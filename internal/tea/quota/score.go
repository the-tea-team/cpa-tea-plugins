package quota

import (
	"math"
	"sort"
	"strings"
	"time"
)

// AccountScore implements the approved plan §4 scoring contract:
// pace = min(r / max(t/T, 0.05), 4) per window; supply = weighted mean of
// C*pace (weekly 0.7, session 0.3, renormalized over known windows); multiplied
// by the most restrictive headroom guard min(1, r/0.10).
// Unknown capacity/usage yields neutral 1 with Known=false — that is a
// scheduling fallback, never a claim of real 1x capacity.
func AccountScore(a Account, model string, now time.Time, maxAge time.Duration) Score {
	var weightedSum, weightTotal float64
	guard := 1.0
	known := 0
	var reasons []string
	for _, w := range a.Windows {
		if !scopeMatches(w.Models, model) {
			continue
		}
		if !observationValid(w, now, maxAge) {
			reasons = append(reasons, "window "+w.ID+": no fresh valid observation")
			continue
		}
		r := 1 - *w.UsedFraction
		// Exhaustion and headroom depend only on the fresh used fraction, never
		// on whether capacity is known.
		if r <= 0 {
			return Score{Value: 0, Known: true, Exhausted: true,
				Reasons: []string{"window " + w.ID + " exhausted"}}
		}
		if g := math.Min(1, r/0.10); g < guard {
			guard = g
		}
		if w.Capacity == nil || !finite(*w.Capacity) || *w.Capacity <= 0 {
			reasons = append(reasons, "window "+w.ID+": capacity unknown")
			continue
		}
		pace := r // no reset/duration info: no urgency uplift
		if w.DurationSeconds > 0 && !w.ResetAt.IsZero() {
			ttr := w.ResetAt.Sub(now).Seconds()
			frac := ttr / float64(w.DurationSeconds)
			if frac < 0.05 {
				frac = 0.05
			}
			pace = math.Min(r/frac, 4)
		}
		iw := windowImportance(w)
		weightedSum += iw * (*w.Capacity) * pace
		weightTotal += iw
		known++
	}
	if known == 0 || weightTotal == 0 {
		if len(reasons) == 0 {
			reasons = []string{"no applicable windows"}
		}
		return Score{Value: 1, Known: false, Reasons: reasons}
	}
	v := (weightedSum / weightTotal) * guard
	if !finite(v) || v < 0 {
		v = 0
	}
	reasons = append(reasons, "scored from fresh known windows")
	return Score{Value: v, Known: true, Reasons: reasons}
}

func windowImportance(w Window) float64 {
	id := strings.ToLower(w.ID)
	switch {
	case strings.Contains(id, "week") || w.DurationSeconds >= 3*24*3600:
		return 0.7
	case strings.Contains(id, "session") || strings.Contains(id, "hour") ||
		strings.Contains(id, "5h") || (w.DurationSeconds > 0 && w.DurationSeconds <= 24*3600):
		return 0.3
	default:
		return 0.5
	}
}

// PoolCredentials returns usable members of the named pool filtered by model.
// It does NOT claim caller authorization. Unknown pool / unresolved membership
// fails closed. Ordering: priority descending, then ID ascending.
func PoolCredentials(s Snapshot, poolID, model string, now time.Time) []Credential {
	var pool *Pool
	for i := range s.Pools {
		if s.Pools[i].ID == poolID {
			pool = &s.Pools[i]
			break
		}
	}
	if pool == nil {
		return nil
	}
	members := poolMemberCredentials(s, *pool)
	out := make([]Credential, 0, len(members))
	for _, c := range members {
		if !usableCredential(c, now) {
			continue
		}
		// A named model needs positive support: empty credential.Models is
		// unknown support, not a wildcard (fail closed).
		if model != "" && !credentialSupportsModel(c, model) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func credentialSupportsModel(c Credential, model string) bool {
	for _, pat := range c.Models {
		if matchPattern(pat, model) {
			return true
		}
	}
	return false
}
