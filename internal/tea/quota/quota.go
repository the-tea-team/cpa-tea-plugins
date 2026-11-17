package quota

import (
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"
)

const SchemaVersion = 1

type Snapshot struct {
	SchemaVersion int          `json:"schemaVersion"`
	Revision      uint64       `json:"revision"`
	GeneratedAt   time.Time    `json:"generatedAt"`
	Accounts      []Account    `json:"accounts"`
	Credentials   []Credential `json:"credentials"`
	Pools         []Pool       `json:"pools"`
}

type Account struct {
	ID            string      `json:"id"`
	Provider      string      `json:"provider"`
	Label         string      `json:"label"`
	BillingType   string      `json:"billingType"`
	Plan          string      `json:"plan"`
	PlanSource    string      `json:"planSource"`
	RefreshStatus string      `json:"refreshStatus"`
	ObservedAt    time.Time   `json:"observedAt"`
	Windows       []Window    `json:"windows"`
	Financial     []Financial `json:"financial"`
}

type Credential struct {
	ID          string    `json:"id"`
	AuthIndex   string    `json:"authIndex"`
	AccountID   string    `json:"accountId"`
	Provider    string    `json:"provider"`
	AuthType    string    `json:"authType"`
	Label       string    `json:"label"`
	Models      []string  `json:"models"`
	Priority    int       `json:"priority"`
	Disabled    bool      `json:"disabled"`
	Unavailable bool      `json:"unavailable"`
	NextRetryAt time.Time `json:"nextRetryAt"`
}

type Window struct {
	ID              string    `json:"id"`
	DurationSeconds int64     `json:"durationSeconds"`
	UsedFraction    *float64  `json:"usedFraction,omitempty"`
	ResetAt         time.Time `json:"resetAt"`
	ObservedAt      time.Time `json:"observedAt"`
	Capacity        *float64  `json:"capacity,omitempty"`
	CapacitySource  string    `json:"capacitySource"`
	Models          []string  `json:"models,omitempty"`
}

type Financial struct {
	Kind       string    `json:"kind"`
	ScopeID    string    `json:"scopeId"`
	Currency   string    `json:"currency"`
	Source     string    `json:"source"`
	Amount     float64   `json:"amount"`
	ObservedAt time.Time `json:"observedAt"`
	ResetAt    time.Time `json:"resetAt"`
}

type Pool struct {
	ID            string   `json:"id" yaml:"id"`
	Label         string   `json:"label" yaml:"label"`
	Provider      string   `json:"provider" yaml:"provider"`
	CredentialIDs []string `json:"credentialIds" yaml:"credentialIds"`
	BillingType   string   `json:"billingType" yaml:"billingType"`
}

type PoolSummary struct {
	ID                string    `json:"id"`
	Label             string    `json:"label"`
	Provider          string    `json:"provider"`
	WindowID          string    `json:"windowId"`
	RemainingFraction *float64  `json:"remainingFraction,omitempty"`
	TotalAccounts     int       `json:"totalAccounts"`
	MeasuredAccounts  int       `json:"measuredAccounts"`
	UsableAccounts    int       `json:"usableAccounts"`
	UnknownAccounts   int       `json:"unknownAccounts"`
	TotalCapacity     float64   `json:"totalCapacity"`
	MeasuredCapacity  float64   `json:"measuredCapacity"`
	UsableCapacity    float64   `json:"usableCapacity"`
	Estimated         bool      `json:"estimated"`
	NextResetAt       time.Time `json:"nextResetAt"`
}

type Score struct {
	Value     float64  `json:"value"`
	Known     bool     `json:"known"`
	Exhausted bool     `json:"exhausted"`
	Reasons   []string `json:"reasons"`
}

var financialKinds = map[string]bool{
	"upstream-spend": true, "prepaid-balance": true, "key-budget-remaining": true,
	"configured-budget-remaining": true, "gateway-estimated-cost": true,
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// canonicalScope returns a sorted, de-duplicated copy of a model-scope set.
// The caller's slice is never mutated.
func canonicalScope(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	c := append([]string(nil), models...)
	sort.Strings(c)
	out := c[:1]
	for _, m := range c[1:] {
		if m != out[len(out)-1] {
			out = append(out, m)
		}
	}
	return out
}

func scopeKey(models []string, sep string) string {
	return strings.Join(canonicalScope(models), sep)
}

func Validate(s Snapshot) error {
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("quota: unsupported schemaVersion %d", s.SchemaVersion)
	}
	acctIDs := map[string]bool{}
	for _, a := range s.Accounts {
		if strings.TrimSpace(a.ID) == "" {
			return fmt.Errorf("quota: account with empty id")
		}
		if acctIDs[a.ID] {
			return fmt.Errorf("quota: duplicate account id %q", a.ID)
		}
		acctIDs[a.ID] = true
		if strings.TrimSpace(a.Provider) == "" {
			return fmt.Errorf("quota: account %q empty provider", a.ID)
		}
		winKeys := map[string]bool{}
		for _, w := range a.Windows {
			if strings.TrimSpace(w.ID) == "" {
				return fmt.Errorf("quota: account %q window with empty id", a.ID)
			}
			key := w.ID + "\x00" + scopeKey(w.Models, "\x00")
			if winKeys[key] {
				return fmt.Errorf("quota: account %q duplicate window %q", a.ID, w.ID)
			}
			winKeys[key] = true
			if w.DurationSeconds < 0 {
				return fmt.Errorf("quota: account %q window %q negative duration", a.ID, w.ID)
			}
			if w.UsedFraction != nil {
				if !finite(*w.UsedFraction) {
					return fmt.Errorf("quota: account %q window %q non-finite usedFraction", a.ID, w.ID)
				}
				if *w.UsedFraction < 0 || *w.UsedFraction > 1 {
					return fmt.Errorf("quota: account %q window %q usedFraction out of [0,1]", a.ID, w.ID)
				}
			}
			if w.Capacity != nil {
				if !finite(*w.Capacity) {
					return fmt.Errorf("quota: account %q window %q non-finite capacity", a.ID, w.ID)
				}
				if *w.Capacity <= 0 {
					return fmt.Errorf("quota: account %q window %q non-positive capacity", a.ID, w.ID)
				}
			}
			for _, m := range w.Models {
				if strings.TrimSpace(m) == "" {
					return fmt.Errorf("quota: account %q window %q empty model", a.ID, w.ID)
				}
			}
		}
		for _, f := range a.Financial {
			if !financialKinds[f.Kind] {
				return fmt.Errorf("quota: account %q unknown financial kind %q", a.ID, f.Kind)
			}
			if !finite(f.Amount) {
				return fmt.Errorf("quota: account %q non-finite financial amount", a.ID)
			}
		}
	}
	credIDs := map[string]bool{}
	credProvider := map[string]string{}
	for _, c := range s.Credentials {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("quota: credential with empty id")
		}
		if credIDs[c.ID] {
			return fmt.Errorf("quota: duplicate credential id %q", c.ID)
		}
		credIDs[c.ID] = true
		credProvider[c.ID] = c.Provider
		if strings.TrimSpace(c.Provider) == "" {
			return fmt.Errorf("quota: credential %q empty provider", c.ID)
		}
		if c.AccountID != "" && !acctIDs[c.AccountID] {
			return fmt.Errorf("quota: credential %q references unknown account %q", c.ID, c.AccountID)
		}
		for _, m := range c.Models {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("quota: credential %q empty model", c.ID)
			}
		}
	}
	poolIDs := map[string]bool{}
	for _, p := range s.Pools {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("quota: pool with empty id")
		}
		if poolIDs[p.ID] {
			return fmt.Errorf("quota: duplicate pool id %q", p.ID)
		}
		poolIDs[p.ID] = true
		if strings.TrimSpace(p.Provider) == "" {
			return fmt.Errorf("quota: pool %q empty provider", p.ID)
		}
		seen := map[string]bool{}
		for _, id := range p.CredentialIDs {
			if !credIDs[id] {
				return fmt.Errorf("quota: pool %q references unknown credential %q", p.ID, id)
			}
			if credProvider[id] != p.Provider {
				return fmt.Errorf("quota: pool %q (provider %q) references credential %q of provider %q", p.ID, p.Provider, id, credProvider[id])
			}
			if seen[id] {
				return fmt.Errorf("quota: pool %q duplicate credential %q", p.ID, id)
			}
			seen[id] = true
		}
		if len(p.CredentialIDs) == 0 && strings.TrimSpace(p.BillingType) == "" {
			return fmt.Errorf("quota: pool %q has neither credentialIds nor provider+billingType membership", p.ID)
		}
	}
	return nil
}

// --- membership helpers ---

func credentialsByID(s Snapshot) map[string]Credential {
	m := make(map[string]Credential, len(s.Credentials))
	for _, c := range s.Credentials {
		m[c.ID] = c
	}
	return m
}

func accountsByID(s Snapshot) map[string]Account {
	m := make(map[string]Account, len(s.Accounts))
	for _, a := range s.Accounts {
		m[a.ID] = a
	}
	return m
}

// poolMemberCredentials resolves membership fail-closed: explicit IDs win when
// present (and must match Pool.Provider); otherwise Provider+BillingType
// matched against accounts. Unknown or cross-provider references simply do not
// resolve (Validate reports them separately).
func poolMemberCredentials(s Snapshot, p Pool) []Credential {
	byID := credentialsByID(s)
	accts := accountsByID(s)
	var out []Credential
	if len(p.CredentialIDs) > 0 {
		for _, id := range p.CredentialIDs {
			if c, ok := byID[id]; ok {
				if c.Provider == p.Provider {
					out = append(out, c)
				}
			}
		}
		return out
	}
	for _, c := range s.Credentials {
		if c.Provider != p.Provider {
			continue
		}
		a, ok := accts[c.AccountID]
		if !ok || a.BillingType != p.BillingType {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PoolHasModelSupport reports whether at least one member credential of the pool has a known model
// inventory. A pool can look populated - its credentials exist - while the host has not published
// which models they serve, and then nothing in it can answer a named model. Callers use this to
// tell "the fleet shrank" from "the inventory is not readable yet".
func PoolHasModelSupport(s Snapshot, p Pool) bool {
	for _, c := range poolMemberCredentials(s, p) {
		if len(c.Models) > 0 {
			return true
		}
	}
	return false
}

func usableCredential(c Credential, now time.Time) bool {
	if c.Disabled || c.Unavailable {
		return false
	}
	if !c.NextRetryAt.IsZero() && c.NextRetryAt.After(now) {
		return false
	}
	return true
}

// observationValid: fresh observation with finite in-range used fraction.
// A reset already in the past invalidates the observation (replenishment must
// be observed, not assumed); a zero ResetAt means unknown reset, not expired.
func observationValid(w Window, now time.Time, maxAge time.Duration) bool {
	if w.UsedFraction == nil || !finite(*w.UsedFraction) || *w.UsedFraction < 0 || *w.UsedFraction > 1 {
		return false
	}
	if w.ObservedAt.IsZero() {
		return false
	}
	if maxAge > 0 && now.Sub(w.ObservedAt) > maxAge {
		return false
	}
	if w.ObservedAt.After(now) {
		return false
	}
	if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
		return false
	}
	return true
}

func scopeMatches(windowModels []string, model string) bool {
	if len(windowModels) == 0 {
		return true
	}
	if model == "" {
		return false // scoped window cannot be proven applicable without a model
	}
	for _, pat := range windowModels {
		if matchPattern(pat, model) {
			return true
		}
	}
	return false
}

func matchPattern(pat, name string) bool {
	if ok, err := path.Match(pat, name); err == nil && ok {
		return true
	}
	return false
}

// Summarize returns one row per pool per window-id+model-scope present among
// its deduplicated member accounts, plus a placeholder row for pools whose
// members have no windows at all.
func Summarize(s Snapshot, now time.Time, maxAge time.Duration) []PoolSummary {
	accts := accountsByID(s)
	pools := append([]Pool(nil), s.Pools...)
	sort.Slice(pools, func(i, j int) bool { return pools[i].ID < pools[j].ID })

	var out []PoolSummary
	for _, p := range pools {
		members := poolMemberCredentials(s, p)

		// Deduplicate quota accounts; remember usability per account.
		acctSeen := map[string]bool{}
		var acctIDs []string
		usable := map[string]bool{}
		for _, c := range members {
			if c.AccountID == "" {
				continue
			}
			if !acctSeen[c.AccountID] {
				acctSeen[c.AccountID] = true
				acctIDs = append(acctIDs, c.AccountID)
			}
			if usableCredential(c, now) {
				usable[c.AccountID] = true
			}
		}
		sort.Strings(acctIDs)

		// Group windows by id+scope.
		groups := map[string][]string{} // key -> acctIDs having that window
		groupWindowID := map[string]string{}
		var groupKeys []string
		for _, aid := range acctIDs {
			a, ok := accts[aid]
			if !ok {
				continue
			}
			seenKey := map[string]bool{}
			for _, w := range a.Windows {
				scope := scopeKey(w.Models, ",")
				key := w.ID + "\x00" + scope
				if seenKey[key] {
					continue
				}
				seenKey[key] = true
				if _, ok := groups[key]; !ok {
					groupKeys = append(groupKeys, key)
					groupWindowID[key] = w.ID
					if scope != "" {
						groupWindowID[key] = w.ID + "|" + scope
					}
				}
				groups[key] = append(groups[key], aid)
			}
		}
		sort.Strings(groupKeys)

		if len(groupKeys) == 0 {
			out = append(out, PoolSummary{
				ID: p.ID, Label: p.Label, Provider: p.Provider, WindowID: "",
				TotalAccounts: len(acctIDs), UnknownAccounts: len(acctIDs),
				UsableAccounts: countUsable(acctIDs, usable),
			})
			continue
		}

		for _, key := range groupKeys {
			row := PoolSummary{
				ID: p.ID, Label: p.Label, Provider: p.Provider, WindowID: groupWindowID[key],
				TotalAccounts:  len(acctIDs),
				UsableAccounts: countUsable(acctIDs, usable),
			}
			var weightedRemaining, measuredCap, usableCap, totalCap float64
			measured := 0
			for _, aid := range acctIDs {
				a := accts[aid]
				// find window for this group
				var win *Window
				for i := range a.Windows {
					w := a.Windows[i]
					scope := scopeKey(w.Models, ",")
					if w.ID+"\x00"+scope == key {
						win = &a.Windows[i]
						break
					}
				}
				if win == nil {
					continue
				}
				knownCap := win.Capacity != nil && finite(*win.Capacity) && *win.Capacity > 0
				if knownCap {
					totalCap += *win.Capacity
				}
				if !observationValid(*win, now, maxAge) || !knownCap {
					continue
				}
				measured++
				measuredCap += *win.Capacity
				weightedRemaining += *win.Capacity * (1 - *win.UsedFraction)
				if win.CapacitySource != "reported" && win.CapacitySource != "operator_override" {
					row.Estimated = true
				}
				if usable[aid] {
					usableCap += *win.Capacity
				}
				if !win.ResetAt.IsZero() && (row.NextResetAt.IsZero() || win.ResetAt.Before(row.NextResetAt)) {
					row.NextResetAt = win.ResetAt
				}
			}
			_ = groups
			row.MeasuredAccounts = measured
			row.UnknownAccounts = row.TotalAccounts - measured
			if row.UnknownAccounts < 0 {
				row.UnknownAccounts = 0
			}
			row.TotalCapacity = totalCap
			row.MeasuredCapacity = measuredCap
			row.UsableCapacity = usableCap
			if measuredCap > 0 {
				r := weightedRemaining / measuredCap
				row.RemainingFraction = &r
			}
			out = append(out, row)
		}
	}
	return out
}

func countUsable(ids []string, usable map[string]bool) int {
	n := 0
	for _, id := range ids {
		if usable[id] {
			n++
		}
	}
	return n
}
