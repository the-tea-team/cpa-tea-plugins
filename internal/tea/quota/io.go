package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// --- Smooth weighted round-robin (nginx-style) ---

const maxRRScopes = 1024

type rrScope struct {
	current map[string]float64
}

// SmoothRR is a concurrency-safe smooth weighted round-robin picker.
// State is bounded to maxRRScopes keys; candidates absent from a Pick call
// are removed from that scope.
type SmoothRR struct {
	mu     sync.Mutex
	scopes map[string]*rrScope
	order  []string // insertion order for FIFO eviction
}

func NewSmoothRR() *SmoothRR {
	return &SmoothRR{scopes: make(map[string]*rrScope)}
}

func (r *SmoothRR) Pick(key string, weights map[string]float64) string {
	// filter valid candidates deterministically
	names := make([]string, 0, len(weights))
	for name, w := range weights {
		if w > 0 && finite(w) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)

	r.mu.Lock()
	defer r.mu.Unlock()
	sc, ok := r.scopes[key]
	if !ok {
		if len(r.scopes) >= maxRRScopes {
			oldest := r.order[0]
			r.order = r.order[1:]
			delete(r.scopes, oldest)
		}
		sc = &rrScope{current: make(map[string]float64)}
		r.scopes[key] = sc
		r.order = append(r.order, key)
	}
	// remove stale candidates
	valid := make(map[string]bool, len(names))
	for _, n := range names {
		valid[n] = true
	}
	for n := range sc.current {
		if !valid[n] {
			delete(sc.current, n)
		}
	}
	var total float64
	for _, n := range names {
		total += weights[n]
		sc.current[n] += weights[n]
	}
	best := names[0]
	for _, n := range names[1:] {
		if sc.current[n] > sc.current[best] {
			best = n
		}
	}
	sc.current[best] -= total
	return best
}

// --- Snapshot I/O ---

const maxSnapshotBytes = 8 << 20 // 8 MiB

func ReadSnapshot(path string) (Snapshot, error) {
	f, err := openSnapshot(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Snapshot{}, err
	}
	if err := validateSnapshotFile(info); err != nil {
		return Snapshot{}, err
	}
	if info.Size() > maxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("quota: snapshot %q exceeds %d bytes", path, maxSnapshotBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSnapshotBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(data) > maxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("quota: snapshot %q exceeds %d bytes", path, maxSnapshotBytes)
	}
	var s Snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Snapshot{}, fmt.Errorf("quota: decode snapshot: %w", err)
	}
	// Exactly one JSON value: anything but whitespace after it is rejected.
	if _, err := dec.Token(); err != io.EOF {
		return Snapshot{}, fmt.Errorf("quota: decode snapshot: trailing data after JSON value")
	}
	if err := Validate(s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// WriteSnapshot validates, then atomically replaces path via a temp file in
// the same directory (caller owns the directory), mode 0600.
func WriteSnapshot(path string, s Snapshot) error {
	if err := Validate(s); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(data) > maxSnapshotBytes {
		return fmt.Errorf("quota: snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
