package main

import (
	"encoding/json"
)

// LoadBindings decodes the reserved bindings section of the state file. Malformed entries are
// skipped rather than treated as a state-file failure: a corrupt binding must never stop combos
// from loading. Bindings whose combo has since been deleted stay in memory until resolved and
// are dropped by the affinity cache's expiry or LRU pressure.
func (s *Store) LoadBindings() map[string]AffinityBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.state.Bindings) == 0 {
		return nil
	}
	out := make(map[string]AffinityBinding, len(s.state.Bindings))
	for key, raw := range s.state.Bindings {
		var binding AffinityBinding
		if err := json.Unmarshal(raw, &binding); err != nil {
			continue
		}
		binding.Key = key
		out[key] = binding
	}
	return out
}

// SaveBindings replaces the state file's bindings section without touching the combo set. The
// existing atomic write path is reused, so the file keeps mode 0600 and a temp-file rename.
// Combo values already in memory are written back byte-for-byte as they were loaded.
func (s *Store) SaveBindings(bindings map[string]AffinityBinding) error {
	encoded := make(map[string]json.RawMessage, len(bindings))
	for key, binding := range bindings {
		raw, err := json.Marshal(binding)
		if err != nil {
			continue
		}
		encoded[key] = raw
	}
	return s.mutate(func(next *State) bool {
		next.Bindings = encoded
		return true
	})
}
