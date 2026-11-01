package main

import (
	"sync"
	"sync/atomic"
)

// Selector decides the order in which a combo's members are attempted. Slice B (session
// affinity) can wrap or replace a Selector without changing the executor loop: the executor
// also accepts an explicit preferred member index and always attempts each member once.
type Selector interface {
	// Order returns member indexes, most preferred first. Every index is returned at most once.
	Order(combo Combo, sessionKey string) []int
}

// roundRobinSelector rotates the starting member per request with a per-combo counter.
type roundRobinSelector struct {
	mu       sync.Mutex
	counters map[string]*atomic.Uint64
}

func newRoundRobinSelector() *roundRobinSelector {
	return &roundRobinSelector{counters: make(map[string]*atomic.Uint64)}
}

func (s *roundRobinSelector) Order(combo Combo, _ string) []int {
	count := len(combo.Members)
	if count == 0 {
		return nil
	}
	s.mu.Lock()
	counter := s.counters[combo.Name]
	if counter == nil {
		counter = &atomic.Uint64{}
		s.counters[combo.Name] = counter
	}
	s.mu.Unlock()
	start := int((counter.Add(1) - 1) % uint64(count))
	return ringOrder(count, start)
}

// failoverSelector always starts at the first configured member.
type failoverSelector struct{}

func (failoverSelector) Order(combo Combo, _ string) []int {
	return ringOrder(len(combo.Members), 0)
}

// selectorForStrategy resolves a configured strategy. An empty strategy is round-robin.
func selectorForStrategy(roundRobin Selector, strategy string) (Selector, error) {
	normalized, err := normalizeStrategy(strategy)
	if err != nil {
		return nil, err
	}
	if normalized == StrategyFailover {
		return failoverSelector{}, nil
	}
	return roundRobin, nil
}

// ringOrder lists every member index once, starting at start and wrapping around.
func ringOrder(count, start int) []int {
	if count == 0 {
		return nil
	}
	start = ((start % count) + count) % count
	order := make([]int, 0, count)
	for offset := 0; offset < count; offset++ {
		order = append(order, (start+offset)%count)
	}
	return order
}

// withPreferred moves preferred to the front while keeping each member once. Slice B passes the
// affinity-bound member here; slice A always passes -1. An out-of-range preferred member is
// ignored so stale bindings cannot break a request.
func withPreferred(order []int, preferred int) []int {
	if preferred < 0 {
		return order
	}
	found := false
	for _, index := range order {
		if index == preferred {
			found = true
			break
		}
	}
	if !found {
		return order
	}
	out := make([]int, 0, len(order))
	out = append(out, preferred)
	for _, index := range order {
		if index != preferred {
			out = append(out, index)
		}
	}
	return out
}
