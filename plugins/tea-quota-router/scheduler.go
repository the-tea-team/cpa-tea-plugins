package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"path"
	"strings"
	"sync"
	"time"
)

type decision struct {
	At         time.Time              `json:"at"`
	Provider   string                 `json:"provider"`
	Model      string                 `json:"model"`
	SelectedID string                 `json:"selectedId"`
	Mode       string                 `json:"mode"`
	Reason     string                 `json:"reason"`
	Scores     map[string]quota.Score `json:"scores"`
}
type scheduler struct {
	rr        *quota.SmoothRR
	aff       *affinityTable
	mu        sync.Mutex
	decisions []decision
}

func strategyFor(req pluginapi.SchedulerPickRequest, strategies map[string]string) string {
	provider := req.Provider
	if provider == "" {
		for _, c := range req.Candidates {
			if provider == "" {
				provider = c.Provider
			} else if provider != c.Provider {
				return "inherit"
			}
		}
		if provider == "" && len(req.Providers) == 1 {
			provider = req.Providers[0]
		}
	}
	if s := strategies[provider]; s != "" {
		return s
	}
	return "inherit"
}

// isSubscriptionProvider reports the providers whose OAuth credentials are
// subscriptions; for them the guard keeps routing on subscription accounts.
func isSubscriptionProvider(p string) bool { return p == "claude" || p == "codex" }

// pinnedAuthID reads the core's explicit pin (executor.PinnedAuthMetadataKey,
// "pinned_auth_id"); only string/[]byte values count, as in core.
func pinnedAuthID(meta map[string]any) string {
	switch v := meta["pinned_auth_id"].(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	}
	return ""
}

// modelCandidates lists the spellings of a requested model compared with a
// credential's model inventory: as asked, and without a "(thinking)" suffix.
func modelCandidates(model string) []string {
	m := strings.ToLower(strings.TrimSpace(model))
	out := []string{m}
	if i := strings.Index(m, "("); i > 0 && strings.HasSuffix(m, ")") {
		out = append(out, strings.TrimSpace(m[:i]))
	}
	return out
}

// inventorySupports reports whether the credential's inventory (which carries
// route aliases) positively lists the model. Empty inventory is unknown, not
// a match.
func inventorySupports(models []string, model string) bool {
	for _, want := range modelCandidates(model) {
		for _, pat := range models {
			pat = strings.ToLower(strings.TrimSpace(pat))
			if pat == want {
				return true
			}
			if ok, err := path.Match(pat, want); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// subscriptionOfferedElsewhere decides, for a subscription route where core
// offered no OAuth candidate, whether the snapshot shows an enabled OAuth
// credential that serves (or may serve) the model -- even if core-unavailable,
// cooling or already attempted. If so the paid candidates left must not be
// used. It also fails closed when the provider has no classified inventory at
// all and a final candidate is unclassified.
func subscriptionOfferedElsewhere(req pluginapi.SchedulerPickRequest, cands []pluginapi.SchedulerAuthCandidate, snap quota.Snapshot, provider string) bool {
	known := 0
	for _, cr := range snap.Credentials {
		if !strings.EqualFold(strings.TrimSpace(cr.Provider), provider) {
			continue
		}
		if cr.AuthType == "oauth" && !cr.Disabled {
			if len(cr.Models) == 0 || inventorySupports(cr.Models, req.Model) {
				return true
			}
		}
		if cr.AuthType != "" {
			known++
		}
	}
	if known == 0 {
		return len(cands) > 0 // no usable inventory: nothing proves these are API-only
	}
	return false
}

// candidateProvider is the request provider, or the one provider every
// candidate shares when the host leaves it empty (mixed-selection paths).
func candidateProvider(req pluginapi.SchedulerPickRequest) string {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider != "" {
		return provider
	}
	for _, c := range req.Candidates {
		p := strings.ToLower(strings.TrimSpace(c.Provider))
		if provider == "" {
			provider = p
		} else if p != provider {
			return ""
		}
	}
	return provider
}

func newScheduler() *scheduler {
	return &scheduler{rr: quota.NewSmoothRR(), aff: newAffinityTable(affinityTTL, affinityMaxBindings), decisions: []decision{}}
}
func (s *scheduler) history() []decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]decision{}, s.decisions...)
}

func (s *scheduler) record(d decision) {
	s.mu.Lock()
	s.decisions = append(s.decisions, d)
	if len(s.decisions) > 256 {
		s.decisions = append([]decision{}, s.decisions[len(s.decisions)-256:]...)
	}
	s.mu.Unlock()
}

// pick chooses an account. Behavior limits:
//   - off / inherit strategy / no candidates: delegate (Handled=false), no affinity.
//   - Subscription guard (claude/codex): when any candidate is a positively
//     classified OAuth credential, only OAuth candidates are scored; API and
//     unidentified credentials are never scored or selected, and if every
//     OAuth candidate is known exhausted the pick rejects even though paid
//     candidates remain. Routes with no OAuth candidate inherit core.
//   - Candidates are weighted by quota score among the max-priority tier only
//     (every tier when acrossPriorities is true; see pickWith);
//     a candidate with a fresh fully-used window and a KNOWN FUTURE reset is
//     "known exhausted". Fully-used with no/past reset, stale or unknown data
//     is not known exhausted (neutral); unknown-reset full windows are still
//     never weighted.
//   - If every identified candidate is known exhausted: active mode returns
//     Handled+Reject (auth_unavailable); shadow only records "rejected".
//   - Affinity applies only when Options.Metadata carries non-empty
//     caller_scope and canonical/execution session id (see affinityKeyFor).
func (s *scheduler) pick(req pluginapi.SchedulerPickRequest, snap quota.Snapshot, mode, strategy string, now time.Time, maxAge time.Duration) pluginapi.SchedulerPickResponse {
	return s.pickWith(req, snap, mode, strategy, now, maxAge, false)
}

// pickWith is pick with an explicit priority policy: acrossPriorities=false
// keeps the max-tier-only behavior.
func (s *scheduler) pickWith(req pluginapi.SchedulerPickRequest, snap quota.Snapshot, mode, strategy string, now time.Time, maxAge time.Duration, acrossPriorities bool) pluginapi.SchedulerPickResponse {
	return s.pickWithOptions(req, snap, mode, strategy, now, maxAge, acrossPriorities, quota.ScoreOptions{})
}

// pickWithOptions is pickWith with scoring options (for example the reset boost).
func (s *scheduler) pickWithOptions(req pluginapi.SchedulerPickRequest, snap quota.Snapshot, mode, strategy string, now time.Time, maxAge time.Duration, acrossPriorities bool, opts quota.ScoreOptions) pluginapi.SchedulerPickResponse {
	if mode == "off" || strategy != "quota-aware-round-robin" || len(req.Candidates) == 0 {
		s.record(decision{At: now, Provider: req.Provider, Model: req.Model, Mode: mode, Reason: "inherited; accepted providers=" + strings.Join(req.Providers, ","), Scores: map[string]quota.Score{}})
		return pluginapi.SchedulerPickResponse{}
	}
	cands := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.ID != "" && !strings.EqualFold(c.Status, "disabled") {
			cands = append(cands, c)
		}
	}
	if isSubscriptionProvider(candidateProvider(req)) {
		oauth := make(map[string]bool, len(snap.Credentials))
		for _, cr := range snap.Credentials {
			if cr.AuthType == "oauth" {
				oauth[cr.ID] = true
			}
		}
		creds0 := make(map[string]quota.Credential, len(snap.Credentials))
		for _, cr := range snap.Credentials {
			creds0[cr.ID] = cr
		}
		// Explicit pin to a known API credential core offered: inherit core.
		if pin := pinnedAuthID(req.Options.Metadata); pin != "" {
			if cr, ok := creds0[pin]; ok && cr.AuthType == "api_key" {
				for _, c := range cands {
					if c.ID == pin {
						s.record(decision{At: now, Provider: req.Provider, Model: req.Model, Mode: mode, Reason: "inherited; pinned API credential", Scores: map[string]quota.Score{}})
						return pluginapi.SchedulerPickResponse{}
					}
				}
			}
		}
		guarded := make([]pluginapi.SchedulerAuthCandidate, 0, len(cands))
		for _, c := range cands {
			if oauth[c.ID] {
				guarded = append(guarded, c)
			}
		}
		if len(guarded) == 0 {
			if subscriptionOfferedElsewhere(req, cands, snap, candidateProvider(req)) {
				// Core filtered the subscription credential out (cooling,
				// attempted, unavailable); never spill onto paid candidates.
				s.record(decision{At: now, Provider: req.Provider, Model: req.Model, Mode: mode, Reason: reasonRejected, Scores: map[string]quota.Score{}})
				if mode == "active" {
					return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "subscription credential for model is not currently offered; refusing paid fallback"}
				}
				return pluginapi.SchedulerPickResponse{}
			}
			// API-only route model or no subscription inventory: core owns it.
			s.record(decision{At: now, Provider: req.Provider, Model: req.Model, Mode: mode, Reason: "inherited; no subscription candidates", Scores: map[string]quota.Score{}})
			return pluginapi.SchedulerPickResponse{}
		}
		cands = guarded
	}
	max := 0
	for i, c := range cands {
		if i == 0 || c.Priority > max {
			max = c.Priority
		}
	}
	accounts := map[string]quota.Account{}
	for _, a := range snap.Accounts {
		accounts[a.ID] = a
	}
	creds := map[string]quota.Credential{}
	for _, c := range snap.Credentials {
		creds[c.ID] = c
	}
	weights := map[string]float64{}
	scores := map[string]quota.Score{}
	knownExhausted := 0
	var earliestReset time.Time
	for _, c := range cands {
		score := quota.Score{Value: 1}
		var until time.Time
		var exhausted bool
		if cr, ok := creds[c.ID]; ok {
			if a, ok := accounts[cr.AccountID]; ok {
				score = quota.AccountScoreWith(a, req.Model, now, maxAge, opts)
				if until, exhausted = knownExhaustion(a, req.Model, now, maxAge); exhausted {
					score.Exhausted, score.Value, score.Known = true, 0, true
				}
			}
		}
		if exhausted {
			knownExhausted++
			if earliestReset.IsZero() || until.Before(earliestReset) {
				earliestReset = until
			}
		}
		if !acrossPriorities && c.Priority != max {
			continue
		}
		scores[c.ID] = score
		if !score.Exhausted {
			weights[c.ID] = score.Value
		}
	}
	allExhausted := len(cands) > 0 && knownExhausted == len(cands)
	rrKey := req.Provider + "\x00" + req.Model
	key, hasKey := affinityKeyFor(req)

	var id, reason string
	s.aff.mu.Lock()
	s.aff.pruneLocked(now)
	if hasKey {
		if bound, ok := s.aff.getLocked(key, now); ok {
			if w, offered := weights[bound]; offered && w > 0 {
				id, reason = bound, reasonAffinity
				s.aff.bindLocked(key, id, now) // sliding TTL
			} else {
				// Removed, lower priority, exhausted or unscored: break binding.
				s.aff.dropLocked(key)
			}
		}
	}
	if id == "" && !allExhausted {
		if id = s.rr.Pick(rrKey, weights); id != "" {
			reason = reasonProposed
			if hasKey {
				s.aff.bindLocked(key, id, now)
			}
		}
	}
	s.aff.mu.Unlock()

	if id == "" && allExhausted {
		s.record(decision{At: now, Provider: req.Provider, Model: req.Model, Mode: mode, Reason: reasonRejected, Scores: scores})
		if mode == "active" {
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "all candidate accounts are quota-exhausted until " + earliestReset.UTC().Format(time.RFC3339)}
		}
		return pluginapi.SchedulerPickResponse{}
	}
	if id == "" {
		reason = "no eligible measured candidate; delegate"
	}
	s.record(decision{At: now, Provider: req.Provider, Model: req.Model, SelectedID: id, Mode: mode, Reason: reason, Scores: scores})
	if mode == "active" && id != "" {
		return pluginapi.SchedulerPickResponse{AuthID: id, Handled: true}
	}
	return pluginapi.SchedulerPickResponse{}
}
