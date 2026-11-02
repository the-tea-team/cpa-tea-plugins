package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// State is the on-disk JSON document. Bindings is reserved for slice B's affinity map; it is
// preserved across writes even though slice A never populates it.
type State struct {
	Combos   map[string]Combo           `json:"combos"`
	Bindings map[string]json.RawMessage `json:"bindings,omitempty"`
}

// Store owns the combo state file and keeps an in-memory copy for concurrent router/executor
// reads. Every mutation persists the whole document atomically before the in-memory copy changes,
// so a failed write never leaves runtime state ahead of disk.
type Store struct {
	path  string
	mu    sync.RWMutex
	state State
}

// OpenStore loads path, treating a missing file as empty state. A malformed file is an error.
func OpenStore(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("combo state path is empty")
	}
	store := &Store{path: path, state: emptyState()}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read combo state %s: %w", path, err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return store, nil
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("parse combo state %s: %w", path, err)
	}
	if state.Combos == nil {
		state.Combos = make(map[string]Combo)
	}
	for name, combo := range state.Combos {
		if strings.TrimSpace(combo.Name) == "" {
			combo.Name = name
		}
		state.Combos[name] = combo.Normalize()
	}
	store.state = state
	return store, nil
}

func emptyState() State {
	return State{Combos: make(map[string]Combo)}
}

// Path returns the file this store persists to.
func (s *Store) Path() string { return s.path }

// Get returns one combo by name.
func (s *Store) Get(name string) (Combo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	combo, ok := s.state.Combos[name]
	if !ok {
		return Combo{}, false
	}
	return cloneCombo(combo), true
}

// List returns every combo, sorted by name.
func (s *Store) List() []Combo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.state.Combos))
	for name := range s.state.Combos {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Combo, 0, len(names))
	for _, name := range names {
		out = append(out, cloneCombo(s.state.Combos[name]))
	}
	return out
}

// Upsert creates or replaces one combo and persists the state file.
func (s *Store) Upsert(combo Combo) error {
	combo = combo.Normalize()
	if err := combo.Validate(); err != nil {
		return err
	}
	return s.mutate(func(next *State) bool {
		next.Combos[combo.Name] = cloneCombo(combo)
		return true
	})
}

// Delete removes one combo and persists the state file. Deleting an unknown combo is a no-op.
func (s *Store) Delete(name string) error {
	name = strings.TrimSpace(name)
	return s.mutate(func(next *State) bool {
		if _, ok := next.Combos[name]; !ok {
			return false
		}
		delete(next.Combos, name)
		return true
	})
}

// Replace swaps the whole combo set and persists it.
func (s *Store) Replace(combos []Combo) error {
	next := make(map[string]Combo, len(combos))
	for _, combo := range combos {
		combo = combo.Normalize()
		if err := combo.Validate(); err != nil {
			return err
		}
		if _, exists := next[combo.Name]; exists {
			return fmt.Errorf("duplicate combo name %q", combo.Name)
		}
		next[combo.Name] = cloneCombo(combo)
	}
	return s.mutate(func(state *State) bool {
		state.Combos = next
		return true
	})
}

// MergeSeed adds seed combos only when a combo of that name is absent. Runtime edits win: a seed
// value never overwrites a combo that already exists in state.
func (s *Store) MergeSeed(combos []Combo) error {
	if len(combos) == 0 {
		return nil
	}
	return s.mutate(func(next *State) bool {
		changed := false
		for _, combo := range combos {
			combo = combo.Normalize()
			if _, exists := next.Combos[combo.Name]; exists {
				continue
			}
			next.Combos[combo.Name] = cloneCombo(combo)
			changed = true
		}
		return changed
	})
}

// mutate applies change to a copy of the state, writes it, then publishes the copy. change
// returns false to skip the write (for example a no-op delete).
func (s *Store) mutate(change func(next *State) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state.clone()
	if !change(&next) {
		return nil
	}
	if err := writeStateAtomic(s.path, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s State) clone() State {
	out := State{
		Combos:   make(map[string]Combo, len(s.Combos)),
		Bindings: make(map[string]json.RawMessage, len(s.Bindings)),
	}
	for name, combo := range s.Combos {
		out.Combos[name] = cloneCombo(combo)
	}
	for key, value := range s.Bindings {
		out.Bindings[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func cloneCombo(combo Combo) Combo {
	out := combo
	out.Members = append([]Member(nil), combo.Members...)
	if combo.Enabled != nil {
		enabled := *combo.Enabled
		out.Enabled = &enabled
	}
	if combo.SessionAffinity != nil {
		affinity := *combo.SessionAffinity
		out.SessionAffinity = &affinity
	}
	return out
}

// writeStateAtomic writes a sibling temp file with mode 0600, fsyncs it, and renames it over the
// destination. The temp file is always removed on failure.
func writeStateAtomic(path string, state State) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode combo state: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare combo state directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tea-combos-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create combo state temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("set combo state mode: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return fmt.Errorf("write combo state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync combo state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close combo state temp file: %w", err)
	}
	closed = true
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace combo state %s: %w", path, err)
	}
	if handle, err := os.Open(dir); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}
