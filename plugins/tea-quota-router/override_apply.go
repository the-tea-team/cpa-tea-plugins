package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// overrideDoc returns a copy of the committed overrides (GET readback).
func (p *runtimeState) overrideDoc() overrideDocument {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneOverrideDocument(p.overrides)
}

type viewGroup struct {
	id       string
	provider string
	creds    []quota.Credential
	rep      quota.Account
	hasRep   bool
}

// checkOverrideConflicts rejects overrides that share an accountId but
// disagree on a field: two different labels, plans, billing types, or
// capacities for the same window cannot both describe one account.
func checkOverrideConflicts(groups map[string]*viewGroup, ov overrideDocument) error {
	for _, acct := range sortedKeys(groups) {
		var label, plan, billing string
		caps := map[string]float64{}
		for _, cr := range groups[acct].creds {
			o := ov.Credentials[cr.ID]
			for _, field := range []struct {
				name  string
				value string
				prior *string
			}{
				{"labels", cleanLabel(o.Label), &label},
				{"plans", strings.TrimSpace(o.Plan), &plan},
				{"billing types", strings.TrimSpace(o.BillingType), &billing},
			} {
				if field.value == "" {
					continue
				}
				if *field.prior != "" && *field.prior != field.value {
					return fmt.Errorf("%w: conflicting %s for account %q", errInvalidOverrides, field.name, acct)
				}
				*field.prior = field.value
			}
			for k, v := range o.CapacityByWindow {
				if prior, ok := caps[k]; ok && prior != v {
					return fmt.Errorf("%w: conflicting capacity for account %q window %q", errInvalidOverrides, acct, k)
				}
				caps[k] = v
			}
		}
	}
	return nil
}

// materializeView derives the served snapshot from the raw collected base and
// the operator overrides. It is a pure function of its inputs, so removing an
// override restores the base identity, plan, billing and label exactly.
//
// strict (operator PUT) rejects cross-provider grouping, conflicting overrides
// and pools that reference foreign-provider credentials. Non-strict
// (collector publish, recovery) never fails on stale overrides: a conflicting
// credential is kept in its own provider-suffixed account and unresolvable
// pool members are dropped, mirroring the collector, and a warning is
// returned. Revision is left to the caller; GeneratedAt follows the base.
func materializeView(base quota.Snapshot, ov overrideDocument, strict bool) (quota.Snapshot, []string, error) {
	if err := quota.Validate(base); err != nil {
		return quota.Snapshot{}, nil, errors.New("invalid snapshot")
	}
	bad := func(err error) (quota.Snapshot, []string, error) {
		if strict {
			err = errors.Join(errInvalidOverrides, err)
		}
		return quota.Snapshot{}, nil, err
	}
	view := cloneSnapshot(base)
	warns := []string{}

	baseAcct := make(map[string]quota.Account, len(view.Accounts))
	for _, a := range view.Accounts {
		baseAcct[a.ID] = a
	}
	order := make([]int, len(view.Credentials))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := view.Credentials[order[i]], view.Credentials[order[j]]
		if ua, ub := credUsable(a), credUsable(b); ua != ub {
			return ua
		}
		return a.ID < b.ID
	})

	referenced := map[string]bool{}
	groups := map[string]*viewGroup{}
	target := make([]string, len(view.Credentials))
	for _, i := range order {
		cr := view.Credentials[i]
		if cr.AccountID != "" {
			referenced[cr.AccountID] = true
		}
		id := cr.AccountID
		if o, ok := ov.Credentials[cr.ID]; ok && strings.TrimSpace(o.AccountID) != "" {
			id = strings.TrimSpace(o.AccountID)
		}
		if id == "" {
			continue
		}
		if g := groups[id]; g != nil && g.provider != cr.Provider {
			if strict {
				return bad(fmt.Errorf("account %q groups credentials of different providers", id))
			}
			orig := id
			for n := 1; ; n++ {
				id = orig + "@" + cr.Provider
				if n > 1 {
					id += "-" + strconv.Itoa(n)
				}
				if g := groups[id]; g == nil || g.provider == cr.Provider {
					break
				}
			}
			warns = append(warns, "account override groups credentials of different providers; credential kept in its own account")
		}
		g := groups[id]
		if g == nil {
			g = &viewGroup{id: id, provider: cr.Provider}
			groups[id] = g
		}
		g.creds = append(g.creds, cr)
		target[i] = id
		if a, ok := baseAcct[cr.AccountID]; ok && a.Provider == cr.Provider && (!g.hasRep || a.ObservedAt.After(g.rep.ObservedAt)) {
			g.rep, g.hasRep = a, true
		}
	}

	if strict {
		if err := checkOverrideConflicts(groups, ov); err != nil {
			return quota.Snapshot{}, nil, err
		}
	}

	accounts := make([]quota.Account, 0, len(groups)+len(view.Accounts))
	for _, id := range sortedKeys(groups) {
		g := groups[id]
		a := g.rep
		if !g.hasRep {
			a = quota.Account{Provider: g.provider, Label: unknownLabel, BillingType: "unknown"}
			for _, cr := range g.creds {
				if cr.Label != "" && cr.Label != unknownLabel {
					a.Label = cr.Label
					break
				}
			}
		}
		a = cloneAccount(a)
		a.ID = id
		m := mergeOverrides(g.creds, ov.Credentials)
		if m.label != "" {
			a.Label = m.label
		}
		if m.billing != "" {
			a.BillingType = m.billing
		}
		if m.plan != "" {
			a.Plan, a.PlanSource = m.plan, "operator_override"
		}
		a.Windows = applyCapacityOverrides(a.Windows, m.caps)
		if a.Windows == nil {
			a.Windows = []quota.Window{}
		}
		if a.Financial == nil {
			a.Financial = []quota.Financial{}
		}
		accounts = append(accounts, a)
	}
	// Accounts no credential refers to (organization-billing reports) are
	// distinct report scopes and pass through unchanged.
	for _, a := range base.Accounts {
		if referenced[a.ID] {
			continue
		}
		if g := groups[a.ID]; g != nil {
			if strict && g.provider != a.Provider {
				return bad(fmt.Errorf("account %q groups credentials of different providers", a.ID))
			}
			warns = append(warns, "an account override collided with a report-only account; the override group was kept")
			continue
		}
		accounts = append(accounts, cloneAccount(a))
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	view.Accounts = accounts

	for i := range view.Credentials {
		if target[i] != "" {
			view.Credentials[i].AccountID = target[i]
		}
		if o, ok := ov.Credentials[view.Credentials[i].ID]; ok {
			if l := cleanLabel(o.Label); l != "" {
				view.Credentials[i].Label = l
			}
		}
	}

	// Auto pools are always recomputed from the overridden view so a billing
	// override moves credentials between them. Explicit pools carried by the
	// base (fixtures, migrated snapshots) are kept; operator pools are applied
	// last and replace a pool with the same ID.
	var setup []setupItem
	explicit := []quota.Pool{}
	for _, bp := range base.Pools {
		if !strings.HasPrefix(bp.ID, autoPoolPrefix) {
			explicit = append(explicit, bp)
		}
	}
	explicit = append(explicit, cloneOverrideDocument(ov).Pools...)
	// A missing member is retained in the committed document but excluded from
	// the served view. Known foreign-provider members remain invalid on PUT.
	providers := map[string]string{}
	for _, cr := range view.Credentials {
		providers[cr.ID] = cr.Provider
	}
	unresolved := map[string]bool{}
	for _, ep := range explicit {
		if strict && (strings.TrimSpace(ep.ID) == "" || strings.TrimSpace(ep.Provider) == "") {
			return bad(errors.New("pool requires id and provider"))
		}
		resolved := 0
		for _, cid := range ep.CredentialIDs {
			if provider, ok := providers[cid]; ok {
				if provider != strings.TrimSpace(ep.Provider) {
					if strict {
						return bad(fmt.Errorf("pool %q contains credential of another provider", ep.ID))
					}
				} else {
					resolved++
				}
			}
		}
		id := strings.TrimSpace(ep.ID)
		if len(ep.CredentialIDs) > 0 && resolved == 0 {
			unresolved[id] = true
		} else {
			delete(unresolved, id)
		}
	}
	view.Pools = buildPools(view, explicit, &setup)
	if len(unresolved) > 0 {
		filtered := view.Pools[:0]
		for _, pool := range view.Pools {
			if unresolved[pool.ID] {
				continue
			}
			filtered = append(filtered, pool)
		}
		view.Pools = filtered
		for _, id := range sortedKeys(unresolved) {
			setup = append(setup, setupItem{Provider: "pools", Status: "warning", Message: fmt.Sprintf("pool %q has no resolvable members and was omitted", id)})
		}
	}

	for _, s := range setup {
		warns = append(warns, s.Message)
	}
	view.GeneratedAt = base.GeneratedAt
	if err := quota.Validate(view); err != nil {
		return bad(err)
	}
	return view, warns, nil
}

// overridesPutResponse maps a saveOverridesResult outcome to the management
// response: 400 for a rejected request, 500 when the authoritative commit
// failed (nothing changed), and 200 once committed. A degraded shared export
// is reported inside the 200 body, never as a failed request.
func overridesPutResponse(res commitResult, err error) (int, map[string]any) {
	switch {
	case err == nil:
		body := map[string]any{"status": "saved", "generation": res.Generation, "revision": res.Revision, "export": map[string]any{"degraded": res.ExportDegraded}}
		if res.ExportDegraded {
			body["status"] = "saved_export_degraded"
			body["export"] = map[string]any{"degraded": true, "error": res.ExportError}
		}
		return 200, body
	case errors.Is(err, errStateCommit):
		return 500, map[string]any{"error": "state commit failed; no changes were applied"}
	default:
		return 400, map[string]any{"error": "invalid overrides"}
	}
}
