package quota

import (
	"fmt"
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
	return AccountScoreWith(a, model, now, maxAge, ScoreOptions{})
}

// ResetBoost favours accounts whose long quota window (weekly) is about to
// reset, because whatever they have left is lost at the reset. A window
// qualifies only when it is longer than the horizon, so short session windows
// (which always reset within a day) never trigger it. By default the
// multiplier is a flat MaxMultiplier for the whole horizon, so a soft value
// (2-3x) spreads the extra traffic over the last day instead of spiking it
// at the end. Ramp=true instead grows it linearly from 1 at the horizon to
// MaxMultiplier at the reset. Several accounts may be boosted at once. The
// zero value disables the boost.
type ResetBoost struct {
	HorizonHours  float64 `json:"horizonHours" yaml:"horizonHours"`
	MaxMultiplier float64 `json:"maxMultiplier" yaml:"maxMultiplier"`
	Ramp          bool    `json:"ramp,omitempty" yaml:"ramp,omitempty"`
}

// Enabled reports whether the boost changes any score.
func (b ResetBoost) Enabled() bool {
	return finite(b.HorizonHours) && finite(b.MaxMultiplier) && b.HorizonHours > 0 && b.MaxMultiplier > 1
}

// Validate rejects settings that cannot mean anything sensible. The zero
// value is valid (disabled).
func (b ResetBoost) Validate() error {
	if b == (ResetBoost{}) {
		return nil
	}
	if !finite(b.HorizonHours) || b.HorizonHours <= 0 || b.HorizonHours > 24*30 {
		return fmt.Errorf("resetBoost.horizonHours must be in (0, 720]")
	}
	if !finite(b.MaxMultiplier) || b.MaxMultiplier < 1 || b.MaxMultiplier > 100 {
		return fmt.Errorf("resetBoost.maxMultiplier must be in [1, 100]")
	}
	return nil
}

// multiplier returns the boost for one fresh window, or 1 when it does not qualify.
func (b ResetBoost) multiplier(w Window, now time.Time) float64 {
	if !b.Enabled() || w.ResetAt.IsZero() || w.DurationSeconds <= 0 {
		return 1
	}
	horizon := b.HorizonHours * 3600
	if float64(w.DurationSeconds) <= horizon {
		return 1
	}
	ttr := w.ResetAt.Sub(now).Seconds()
	if ttr <= 0 || ttr >= horizon {
		return 1
	}
	if !b.Ramp {
		return b.MaxMultiplier
	}
	return 1 + (b.MaxMultiplier-1)*(1-ttr/horizon)
}

// ScoreOptions tunes AccountScoreWith. The zero value reproduces AccountScore.
type ScoreOptions struct {
	ResetBoost ResetBoost
	// DrainToZero drops the headroom guard, so an account keeps its full
	// weight until a window is actually exhausted. Use it when leftover quota
	// is worth more than the occasional 429 at the very end of a window.
	DrainToZero bool
}

// AccountScoreWith is AccountScore plus optional tuning. The reset boost is
// applied after the headroom guard and never revives an exhausted account.
func AccountScoreWith(a Account, model string, now time.Time, maxAge time.Duration, opts ScoreOptions) Score {
	var weightedSum, weightTotal float64
	boost := 1.0
	boostWindow := ""
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
		if g := math.Min(1, r/0.10); !opts.DrainToZero && g < guard {
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
		if m := opts.ResetBoost.multiplier(w, now); m > boost {
			boost, boostWindow = m, w.ID
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
	v := (weightedSum / weightTotal) * guard * boost
	if !finite(v) || v < 0 {
		v = 0
	}
	reasons = append(reasons, "scored from fresh known windows")
	if boost > 1 {
		reasons = append(reasons, fmt.Sprintf("reset boost x%.2f: window %s resets soon", boost, boostWindow))
	}
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
