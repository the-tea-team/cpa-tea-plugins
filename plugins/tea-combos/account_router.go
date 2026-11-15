package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// snapshotSource hands the account router the latest published quota snapshot. It does not
// judge freshness; the router owns that decision so a stale source can never widen a pool.
type snapshotSource interface {
	freshSnapshot(now time.Time) (quota.Snapshot, time.Duration, error)
}

// accountPlanRequest is everything one member attempt needs to pick accounts. It is built per
// invocation; nothing in it is shared between requests.
type accountPlanRequest struct {
	Combo  Combo
	Member Member
	// Bound is the credential an account-affinity binding preferred, or "".
	Bound string
}

// accountPlan is an ordered, bounded list of credential IDs to pin, one attempt each.
type accountPlan struct {
	Provider    string
	Strategy    string
	Credentials []string
	// Bound reports that the preferred credential was still eligible and leads the plan.
	Bound bool
}

// accountRouter turns a member's explicit accountPool into an ordered list of pins. All of its
// shared state is synchronized rotation counters; request data lives only in the returned plan.
type accountRouter struct {
	src snapshotSource
	now func() time.Time
	rr  *quota.SmoothRR
}

func newAccountRouter(src snapshotSource) *accountRouter {
	return &accountRouter{src: src, now: time.Now, rr: quota.NewSmoothRR()}
}

// accountGroup is one quota account and the credentials that draw on it.
type accountGroup struct {
	id          string
	credentials []string
	weight      float64
}

// plan resolves the member's pool against a fresh snapshot. A failed plan never carries pins and
// callers must not recover from it by dropping the pin: that would let the request escape the
// pool the operator declared.
func (r *accountRouter) plan(req accountPlanRequest) (accountPlan, *comboError) {
	member := req.Member
	strategy := member.EffectiveAccountStrategy(req.Combo)
	if r == nil || r.src == nil {
		return accountPlan{}, &comboError{Status: http.StatusInternalServerError, Message: "account routing is not available for explicit accountStrategy"}
	}
	if strategy != AccountStrategyRoundRobin && strategy != AccountStrategyQuotaAware {
		return accountPlan{}, &comboError{Status: http.StatusInternalServerError, Message: fmt.Sprintf("accountStrategy %q does not pin accounts", strategy)}
	}
	unavailable := func(format string, args ...any) (accountPlan, *comboError) {
		return accountPlan{}, &comboError{Status: http.StatusServiceUnavailable, Retryable: true, Message: fmt.Sprintf(format, args...)}
	}
	if member.AccountPool == "" {
		return unavailable("member %s has no accountPool", member.Model)
	}

	now := r.now()
	snap, maxAge, err := r.src.freshSnapshot(now)
	if err != nil {
		return unavailable("quota snapshot unavailable for accountPool %q: %v", member.AccountPool, err)
	}
	if snap.GeneratedAt.IsZero() || snap.GeneratedAt.After(now) || (maxAge > 0 && now.Sub(snap.GeneratedAt) > maxAge) {
		return unavailable("quota snapshot is stale for accountPool %q", member.AccountPool)
	}

	var pool *quota.Pool
	for i := range snap.Pools {
		if snap.Pools[i].ID == member.AccountPool {
			pool = &snap.Pools[i]
			break
		}
	}
	if pool == nil {
		return unavailable("accountPool %q is not in the quota snapshot", member.AccountPool)
	}
	if member.Provider != "" && !strings.EqualFold(member.Provider, pool.Provider) {
		return unavailable("accountPool %q serves provider %q, not %q", member.AccountPool, pool.Provider, member.Provider)
	}
	provider := pool.Provider

	credentials := quota.PoolCredentials(snap, pool.ID, member.Model, now)
	if len(credentials) == 0 {
		return unavailable("accountPool %q has no usable credential for model %s", member.AccountPool, member.Model)
	}

	groups := r.groupAccounts(snap, credentials, member.Model, strategy, now, maxAge)
	if len(groups) == 0 {
		return accountPlan{}, &comboError{Status: http.StatusTooManyRequests, Retryable: true, Message: fmt.Sprintf("every account in accountPool %q is exhausted", member.AccountPool)}
	}

	stateKey := accountStateKey(req.Combo, member, strategy)
	plan := accountPlan{Provider: provider, Strategy: strategy}
	chosen := -1
	if req.Bound != "" {
		for i, g := range groups {
			for _, id := range g.credentials {
				if id == req.Bound {
					chosen, plan.Bound = i, true
				}
			}
		}
	}
	if chosen < 0 {
		weights := make(map[string]float64, len(groups))
		for _, g := range groups {
			weights[g.id] = g.weight
		}
		pick := r.rr.Pick(stateKey+"\x00accounts", weights)
		for i, g := range groups {
			if g.id == pick {
				chosen = i
			}
		}
		if chosen < 0 {
			chosen = 0
		}
	}

	ordered := make([]accountGroup, 0, len(groups))
	ordered = append(ordered, groups[chosen])
	rest := make([]accountGroup, 0, len(groups)-1)
	for i, g := range groups {
		if i != chosen {
			rest = append(rest, g)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].weight != rest[j].weight {
			return rest[i].weight > rest[j].weight
		}
		return rest[i].id < rest[j].id
	})
	ordered = append(ordered, rest...)

	lists := make([][]string, len(ordered))
	for gi, g := range ordered {
		lists[gi] = g.credentials
		if gi == 0 {
			lists[gi] = r.rotate(stateKey+"\x00cred\x00"+g.id, g.credentials, plan.Bound, req.Bound)
		}
	}
	plan.Credentials = interleaveCredentials(lists, maxAccountAttempts)
	return plan, nil
}

// interleaveCredentials emits the first credential of every account, then the second of every
// account, and so on, up to limit. Credentials that share a quota account fail together, so a
// second credential of the same account is a poor escape from a quota failure; taking them last
// also keeps one account with many credentials from consuming the whole attempt bound before
// another account is tried.
func interleaveCredentials(lists [][]string, limit int) []string {
	var out []string
	for depth := 0; len(out) < limit; depth++ {
		added := false
		for _, ids := range lists {
			if depth >= len(ids) {
				continue
			}
			added = true
			out = append(out, ids[depth])
			if len(out) >= limit {
				return out
			}
		}
		if !added {
			break
		}
	}
	return out
}

// groupAccounts deduplicates credentials into quota accounts and scores them. Credentials that
// carry no account id cannot be deduplicated and each count as their own account with unknown
// quota. Priority is deliberately ignored: ordering is by id.
func (r *accountRouter) groupAccounts(snap quota.Snapshot, credentials []quota.Credential, model, strategy string, now time.Time, maxAge time.Duration) []accountGroup {
	accounts := make(map[string]quota.Account, len(snap.Accounts))
	for _, a := range snap.Accounts {
		accounts[a.ID] = a
	}
	byID := map[string]*accountGroup{}
	var order []string
	for _, c := range credentials {
		id := c.AccountID
		if id == "" {
			id = "credential:" + c.ID
		}
		g := byID[id]
		if g == nil {
			g = &accountGroup{id: id}
			byID[id] = g
			order = append(order, id)
		}
		g.credentials = append(g.credentials, c.ID)
	}
	sort.Strings(order)

	type scored struct {
		group *accountGroup
		value float64
		known bool
	}
	var kept []scored
	knownSum, knownCount := 0.0, 0
	for _, id := range order {
		g := byID[id]
		sort.Strings(g.credentials)
		s := scored{group: g}
		if strategy == AccountStrategyQuotaAware {
			if account, ok := accounts[id]; ok {
				score := quota.AccountScore(account, model, now, maxAge)
				if score.Known && score.Exhausted {
					continue // known-empty accounts are never pinned
				}
				if score.Known {
					s.known, s.value = true, score.Value
					knownSum += score.Value
					knownCount++
				}
			}
		}
		kept = append(kept, s)
	}
	// Unknown quota is neutral: it receives the mean of the known accounts, never a
	// manufactured advantage, and membership never grows beyond the pool.
	neutral := 1.0
	if knownCount > 0 && knownSum > 0 {
		neutral = knownSum / float64(knownCount)
	}
	groups := make([]accountGroup, 0, len(kept))
	for _, s := range kept {
		weight := 1.0
		if strategy == AccountStrategyQuotaAware {
			weight = neutral
			if s.known {
				weight = s.value
			}
			if weight < 1e-9 {
				weight = 1e-9
			}
		}
		s.group.weight = weight
		groups = append(groups, *s.group)
	}
	return groups
}

// rotate spreads load across the credentials of the chosen account without letting a stale
// binding be displaced: a bound credential always leads.
func (r *accountRouter) rotate(key string, ids []string, bound bool, boundID string) []string {
	out := append([]string(nil), ids...)
	if len(out) < 2 {
		return out
	}
	if bound {
		for i, id := range out {
			if id == boundID {
				out[0], out[i] = out[i], out[0]
				sort.Strings(out[1:])
				return out
			}
		}
	}
	weights := make(map[string]float64, len(out))
	for _, id := range out {
		weights[id] = 1
	}
	first := r.rr.Pick(key, weights)
	for i, id := range out {
		if id == first {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	sort.Strings(out[1:])
	return out
}

// accountStateKey is the shared rotation scope. It names the combo and the member's model,
// provider, pool and effective strategy, so two combos that reuse one model with different
// account policies never share counters.
func accountStateKey(combo Combo, member Member, strategy string) string {
	return strings.Join([]string{combo.Name, member.Model, member.Provider, member.AccountPool, strategy}, "\x00")
}
