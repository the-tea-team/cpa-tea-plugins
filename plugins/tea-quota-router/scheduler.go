package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
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
//   - Candidates are weighted by quota score among the max-priority tier only;
//     a candidate with a fresh fully-used window and a KNOWN FUTURE reset is
//     "known exhausted". Fully-used with no/past reset, stale or unknown data
//     is not known exhausted (neutral); unknown-reset full windows are still
//     never weighted.
//   - If every identified candidate is known exhausted: active mode returns
//     Handled+Reject (auth_unavailable); shadow only records "rejected".
//   - Affinity applies only when Options.Metadata carries non-empty
//     caller_scope and canonical/execution session id (see affinityKeyFor).
func (s *scheduler) pick(req pluginapi.SchedulerPickRequest, snap quota.Snapshot, mode, strategy string, now time.Time, maxAge time.Duration) pluginapi.SchedulerPickResponse {
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
				score = quota.AccountScore(a, req.Model, now, maxAge)
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
		if c.Priority != max {
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
