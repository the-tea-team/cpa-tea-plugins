package quota

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }

func TestSmoothRRWeightedDistribution(t *testing.T) {
	r := NewSmoothRR()
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[r.Pick("k", map[string]float64{"a": 1, "b": 5})]++
	}
	if counts["a"] != 50 || counts["b"] != 250 {
		t.Fatalf("smooth WRR 1:5 over 300 should be 50/250, got %v", counts)
	}
}

func TestSmoothRRDeterministicTies(t *testing.T) {
	r1, r2 := NewSmoothRR(), NewSmoothRR()
	w := map[string]float64{"b": 1, "a": 1}
	for i := 0; i < 10; i++ {
		if got1, got2 := r1.Pick("k", w), r2.Pick("k", w); got1 != got2 {
			t.Fatalf("nondeterministic: %q vs %q", got1, got2)
		}
	}
}

func TestSmoothRRExcludesNonPositiveAndNonFinite(t *testing.T) {
	r := NewSmoothRR()
	for i := 0; i < 5; i++ {
		got := r.Pick("k", map[string]float64{"a": 1, "zero": 0, "neg": -2, "nan": nan(), "inf": inf()})
		if got != "a" {
			t.Fatalf("want a, got %q", got)
		}
	}
	if got := r.Pick("k", map[string]float64{"zero": 0}); got != "" {
		t.Fatalf("all invalid must return empty, got %q", got)
	}
	if got := r.Pick("k", nil); got != "" {
		t.Fatalf("empty weights must return empty, got %q", got)
	}
}

func TestSmoothRRRemovesStaleCandidates(t *testing.T) {
	r := NewSmoothRR()
	r.Pick("k", map[string]float64{"a": 1, "b": 1})
	r.Pick("k", map[string]float64{"a": 1, "b": 1})
	// b removed; all subsequent picks must be a
	for i := 0; i < 4; i++ {
		if got := r.Pick("k", map[string]float64{"a": 1}); got != "a" {
			t.Fatalf("stale candidate leaked: %q", got)
		}
	}
}

func TestSmoothRRKeysIndependent(t *testing.T) {
	r := NewSmoothRR()
	r.Pick("k1", map[string]float64{"a": 1, "b": 1})
	if got := r.Pick("k2", map[string]float64{"a": 1, "b": 1}); got == "" {
		t.Fatal("independent key failed")
	}
}

func TestSmoothRRConcurrent(t *testing.T) {
	r := NewSmoothRR()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				got := r.Pick("k", map[string]float64{"a": 1, "b": 3})
				if got != "a" && got != "b" {
					t.Errorf("bad pick %q", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSmoothRRBoundedScopes(t *testing.T) {
	r := NewSmoothRR()
	for i := 0; i < 5000; i++ {
		r.Pick("key-"+itoa(i), map[string]float64{"a": 1})
	}
	r.mu.Lock()
	n := len(r.scopes)
	r.mu.Unlock()
	if n > 1024 {
		t.Fatalf("scope map unbounded: %d", n)
	}
}

// --- snapshot I/O ---

func TestSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "snapshot-v1.json")
	s := baseSnapshot()
	if err := WriteSnapshot(p, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != s.Revision || len(got.Accounts) != 1 || got.Accounts[0].ID != "acct-1" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("want 0600, got %v", info.Mode().Perm())
	}
	// no temp leftovers
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp leftover %s", e.Name())
		}
	}
}

func TestReadSnapshotRejectsOversize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.json")
	if err := os.WriteFile(p, []byte(strings.Repeat(" ", 8<<20+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(p); err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestReadSnapshotRejectsCorruptAndInvalid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := ReadSnapshot(p); err == nil {
		t.Fatal("expected corrupt JSON error")
	}
	_ = os.WriteFile(p, []byte(`{"schemaVersion":99,"accounts":[],"credentials":[],"pools":[]}`), 0o600)
	if _, err := ReadSnapshot(p); err == nil {
		t.Fatal("expected schema validation error")
	}
}

func TestWriteSnapshotRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	s := baseSnapshot()
	s.SchemaVersion = 42
	if err := WriteSnapshot(filepath.Join(dir, "x.json"), s); err == nil {
		t.Fatal("expected validation error on write")
	}
}

func TestReadSnapshotMissingFile(t *testing.T) {
	if _, err := ReadSnapshot(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func itoa(i int) string { return fmt.Sprintf("%05d", i) }
