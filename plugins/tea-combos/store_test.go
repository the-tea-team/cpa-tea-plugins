package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRoundTripAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	combo := mustCombo(t, "fleet", StrategyFailover, "a", "b")
	combo.TTL = "45m"
	combo.SessionAffinity = boolPtr(false)
	combo.Enabled = boolPtr(false)
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state file mode = %o, want 600", mode)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.Get("fleet")
	if !ok {
		t.Fatal("combo missing after reopen")
	}
	if got.Strategy != StrategyFailover || got.TTL != "45m" || got.IsEnabled() {
		t.Fatalf("combo = %+v", got)
	}
	if got.SessionAffinity == nil || *got.SessionAffinity {
		t.Fatalf("sessionAffinity = %v", got.SessionAffinity)
	}
	if len(got.Members) != 2 || got.Members[1].Model != "b" {
		t.Fatalf("members = %+v", got.Members)
	}
}

func TestStoreAtomicReplaceLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, name := range []string{"one", "two"} {
		if err := store.Upsert(mustCombo(t, name, StrategyRoundRobin, "m")); err != nil {
			t.Fatalf("Upsert %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("state is not valid JSON: %v", err)
	}
	if len(state.Combos) != 2 {
		t.Fatalf("combos on disk = %d, want 2", len(state.Combos))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != "state.json" {
			t.Fatalf("unexpected leftover file %q", entry.Name())
		}
	}
}

func TestStoreListSorted(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	for _, name := range []string{"zeta", "alpha", "middle"} {
		if err := store.Upsert(mustCombo(t, name, StrategyRoundRobin, "m")); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	list := store.List()
	if len(list) != 3 || list[0].Name != "alpha" || list[1].Name != "middle" || list[2].Name != "zeta" {
		t.Fatalf("List = %+v", list)
	}
}

func TestStoreReplace(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.Upsert(mustCombo(t, "old", StrategyRoundRobin, "m")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.Replace([]Combo{mustCombo(t, "new", StrategyRoundRobin, "m")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, ok := store.Get("old"); ok {
		t.Fatal("Replace kept the old combo")
	}
	if _, ok := store.Get("new"); !ok {
		t.Fatal("Replace lost the new combo")
	}
}

func TestStoreMergeSeedNeverOverwritesRuntimeEdit(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	runtime := mustCombo(t, "shared", StrategyFailover, "runtime-member")
	if err := store.Upsert(runtime); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	seed := []Combo{
		mustCombo(t, "shared", StrategyRoundRobin, "seed-member"),
		mustCombo(t, "fresh", StrategyRoundRobin, "seed-member"),
	}
	if err := store.MergeSeed(seed); err != nil {
		t.Fatalf("MergeSeed: %v", err)
	}
	got, _ := store.Get("shared")
	if len(got.Members) != 1 || got.Members[0].Model != "runtime-member" || got.Strategy != StrategyFailover {
		t.Fatalf("runtime edit was overwritten by seed: %+v", got)
	}
	if _, ok := store.Get("fresh"); !ok {
		t.Fatal("new seed combo was not added")
	}
}

func TestStoreUnwritablePathReturnsError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := &Store{path: filepath.Join(blocker, "state.json"), state: emptyState()}

	if err := store.Upsert(mustCombo(t, "fleet", StrategyRoundRobin, "m")); err == nil {
		t.Fatal("Upsert on an unwritable path must fail")
	}

	store.state.Combos["fleet"] = mustCombo(t, "fleet", StrategyRoundRobin, "m")
	if err := store.Delete("fleet"); err == nil {
		t.Fatal("Delete on an unwritable path must fail")
	}
	if _, ok := store.Get("fleet"); !ok {
		t.Fatal("failed Delete must not change in-memory state")
	}
}

func TestOpenStoreRejectsMalformedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("OpenStore must reject malformed state")
	}
}
