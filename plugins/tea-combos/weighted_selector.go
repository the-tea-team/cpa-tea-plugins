package main

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

type weightedSelector struct {
	rr        *quota.SmoothRR
	mu        sync.RWMutex
	snapshot  quota.Snapshot
	path      string
	maxAge    time.Duration
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newWeightedSelector(path string, age int) *weightedSelector {
	if age == 0 {
		age = 600
	}
	s := &weightedSelector{rr: quota.NewSmoothRR(), path: path, maxAge: time.Duration(age) * time.Second, stop: make(chan struct{}), done: make(chan struct{})}
	if path == "" {
		close(s.done)
		return s
	}
	go func() {
		defer close(s.done)
		s.refresh()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.refresh()
			case <-s.stop:
				return
			}
		}
	}()
	return s
}
func (s *weightedSelector) refresh() {
	snap, err := quota.ReadSnapshot(s.path)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.snapshot = snap
	s.mu.Unlock()
}
func (s *weightedSelector) Close() {
	s.closeOnce.Do(func() {
		if s.path != "" {
			close(s.stop)
			<-s.done
		}
	})
}
func (p *combosPlugin) memberSelector(strategy string) (Selector, error) {
	st, err := normalizeStrategy(strategy)
	if err != nil {
		return nil, err
	}
	if st == StrategyWeighted || st == StrategyQuotaAware {
		if p.weighted == nil {
			return nil, fmt.Errorf("weighted selector not initialized")
		}
		return p.weighted, nil
	}
	return selectorForStrategy(p.roundRobin, st)
}
func (s *weightedSelector) Order(c Combo, _ string) []int {
	if len(c.Members) == 0 {
		return nil
	}
	s.mu.RLock()
	snap := s.snapshot
	s.mu.RUnlock()
	now := time.Now()
	fresh := !snap.GeneratedAt.IsZero() && !snap.GeneratedAt.After(now) && now.Sub(snap.GeneratedAt) <= s.maxAge
	// A pool is one baseline allocation, split across its models by member weights.
	groups := map[string][]int{}
	baseline := map[string]float64{}
	totals := map[string]float64{}
	for i, m := range c.Members {
		g := m.QuotaPool
		if g == "" || c.Strategy != StrategyQuotaAware {
			g = fmt.Sprintf("#%d", i)
		}
		g = m.Role + "/" + g
		w := m.Weight
		if w <= 0 {
			w = 1
		}
		groups[g] = append(groups[g], i)
		totals[g] += w
		if w > baseline[g] {
			baseline[g] = w
		}
	}
	weights := map[string]float64{}
	baselineWeights := map[string]float64{}
	for g, indices := range groups {
		for _, i := range indices {
			m := c.Members[i]
			w := m.Weight
			if w <= 0 {
				w = 1
			}
			factor := 1.0
			if fresh && c.Strategy == StrategyQuotaAware && m.QuotaPool != "" {
				creds := quota.PoolCredentials(snap, m.QuotaPool, m.Model, now)
				ids := map[string]bool{}
				sum := 0.0
				n := 0.0
				for _, cred := range creds {
					if ids[cred.AccountID] {
						continue
					}
					ids[cred.AccountID] = true
					for _, a := range snap.Accounts {
						if a.ID == cred.AccountID {
							score := quota.AccountScore(a, m.Model, now, s.maxAge)
							if score.Known {
								capacity := normalizedCapacity(a, m.Model, now, s.maxAge)
								if score.Exhausted && capacity == 0 {
									capacity = 1
								}
								if capacity > 0 {
									sum += score.Value
									n += capacity
								}
							}
							break
						}
					}
				}
				if n > 0 {
					factor = math.Min(4, sum/n)
				}
			}
			baselineWeights[fmt.Sprint(i)] = baseline[g] * w / totals[g]
			weights[fmt.Sprint(i)] = baselineWeights[fmt.Sprint(i)] * factor
		}
	}
	// Pick primary first; fallback is ordered separately by selectOrder.
	first := map[string]float64{}
	for i, m := range c.Members {
		if m.Role != "fallback" {
			first[fmt.Sprint(i)] = weights[fmt.Sprint(i)]
		}
	}
	if len(first) == 0 {
		first = weights
	}
	chosen := s.rr.Pick(c.Name+"/members", first)
	if chosen == "" {
		for key := range first {
			first[key] = baselineWeights[key]
		}
		chosen = s.rr.Pick(c.Name+"/members", first)
	}
	var idx int
	fmt.Sscan(chosen, &idx)
	out := []int{idx}
	for i := range c.Members {
		if i != idx {
			out = append(out, i)
		}
	}
	return out
}

// normalizedCapacity uses the same applicable fresh windows and importance as
// AccountScore, but removes urgency and headroom to expose their mean capacity.
func normalizedCapacity(a quota.Account, model string, now time.Time, maxAge time.Duration) float64 {
	clean := a
	clean.Windows = nil
	zero := 0.0
	clean.Windows = make([]quota.Window, 0, len(a.Windows))
	for _, w := range a.Windows {
		if !w.ResetAt.IsZero() && !w.ResetAt.After(now) {
			continue
		}
		w.ResetAt = time.Time{}
		if w.UsedFraction != nil && *w.UsedFraction >= 0 && *w.UsedFraction <= 1 {
			w.UsedFraction = &zero
		}
		clean.Windows = append(clean.Windows, w)
	}
	score := quota.AccountScore(clean, model, now, maxAge)
	if !score.Known {
		return 0
	}
	return score.Value
}
