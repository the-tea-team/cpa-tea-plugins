package main

import (
	"encoding/json"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemberQuotaConfigurationRoundTrip(t *testing.T) {
	var c Combo
	if err := json.Unmarshal([]byte(`{"name":"x","strategy":"weighted-round-robin","accountStrategy":"inherit","members":[{"model":"a","weight":3,"role":"fallback","quotaPool":"pool"}]}`), &c); err != nil {
		t.Fatal(err)
	}
	c = c.Normalize()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{`"weight":3`, `"role":"fallback"`, `"quotaPool":"pool"`} {
		if !strings.Contains(string(b), v) {
			t.Fatalf("missing %s: %s", v, b)
		}
	}
}

func TestAccountStrategyRejectsUnsupported(t *testing.T) {
	var c Combo
	if err := json.Unmarshal([]byte(`{"name":"x","accountStrategy":"round-robin"}`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize().Validate(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported, got %v", err)
	}
}

func TestWeightedSelectionProportions(t *testing.T) {
	p, err := newCombosPlugin(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	c := Combo{Name: "w", Strategy: "weighted-round-robin", Members: []Member{{Model: "a", Weight: 3}, {Model: "b", Weight: 1}}}.Normalize()
	counts := [2]int{}
	for i := 0; i < 400; i++ {
		s, err := p.memberSelector(c.Strategy)
		if err != nil {
			t.Fatal(err)
		}
		counts[p.selectOrder(c, s, "", -1)[0]]++
	}
	if counts != [2]int{300, 100} {
		t.Fatalf("counts %v", counts)
	}
}

func TestFallbackAffinityDoesNotPromoteAbovePrimary(t *testing.T) {
	p, err := newCombosPlugin(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	c := Combo{Name: "roles", Members: []Member{{Model: "a"}, {Model: "b", Role: "fallback"}}}.Normalize()
	got := p.selectOrder(c, failoverSelector{}, "", 1)
	if len(got) != 2 || got[0] != 0 {
		t.Fatalf("fallback promoted: %v", got)
	}
}

func TestQuotaPoolModelsShareOneAllocation(t *testing.T) {
	p, err := newCombosPlugin(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.shutdown()
	c := Combo{Name: "pools", Strategy: StrategyQuotaAware, Members: []Member{{Model: "a", QuotaPool: "p"}, {Model: "b", QuotaPool: "p"}, {Model: "c", QuotaPool: "q"}}}.Normalize()
	counts := [3]int{}
	for i := 0; i < 400; i++ {
		counts[p.selectOrder(c, p.weighted, "", -1)[0]]++
	}
	if counts != [3]int{100, 100, 200} {
		t.Fatalf("counts %v", counts)
	}
}

func TestWeightedSelectorConcurrentClose(t *testing.T) {
	s := newWeightedSelector(t.TempDir()+"/missing", 1)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close() }()
	}
	wg.Wait()
}

func testQuotaSnapshot(now time.Time, usedA, usedB float64) quota.Snapshot {
	one, five := 1.0, 5.0
	return quota.Snapshot{SchemaVersion: quota.SchemaVersion, GeneratedAt: now,
		Accounts:    []quota.Account{{ID: "a", Provider: "a", Windows: []quota.Window{{ID: "week", Capacity: &one, UsedFraction: &usedA, ObservedAt: now}}}, {ID: "b", Provider: "b", Windows: []quota.Window{{ID: "week", Capacity: &five, UsedFraction: &usedB, ObservedAt: now}}}},
		Credentials: []quota.Credential{{ID: "a", AccountID: "a", Provider: "a", Models: []string{"*"}}, {ID: "b", AccountID: "b", Provider: "b", Models: []string{"*"}}},
		Pools:       []quota.Pool{{ID: "a", Provider: "a", CredentialIDs: []string{"a"}}, {ID: "b", Provider: "b", CredentialIDs: []string{"b"}}}}
}
func TestQuotaProviderCapacityNormalized(t *testing.T) {
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.snapshot = testQuotaSnapshot(time.Now(), .5, .5)
	c := Combo{Name: "normalized", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a"}, {Model: "x", QuotaPool: "b"}}}.Normalize()
	counts := [2]int{}
	for i := 0; i < 200; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [2]int{100, 100} {
		t.Fatalf("counts %v", counts)
	}
}
func TestQuotaExhaustedAvoided(t *testing.T) {
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.snapshot = testQuotaSnapshot(time.Now(), 1, .5)
	c := Combo{Name: "exhausted", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a"}, {Model: "x", QuotaPool: "b"}}}.Normalize()
	for i := 0; i < 100; i++ {
		if got := s.Order(c, "")[0]; got != 1 {
			t.Fatalf("exhausted selected: %d", got)
		}
	}
}

func TestQuotaSnapshotFailureExpiresAndFutureNeutral(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "snapshot.json")
	snap := testQuotaSnapshot(now, .9, .1)
	if err := quota.WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.path = path
	s.refresh()
	s.path = ""
	c := Combo{Name: "stale", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a"}, {Model: "x", QuotaPool: "b"}}}.Normalize()
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	s.path = path
	s.refresh()
	s.path = ""
	s.snapshot.GeneratedAt = now.Add(-time.Hour)
	counts := [2]int{}
	for i := 0; i < 100; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [2]int{50, 50} {
		t.Fatalf("stale counts %v", counts)
	}
	s.snapshot.GeneratedAt = time.Now().Add(time.Hour)
	c.Name = "future"
	counts = [2]int{}
	for i := 0; i < 100; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [2]int{50, 50} {
		t.Fatalf("future counts %v", counts)
	}
}
func TestQuotaDuplicateCredentialNoShareChange(t *testing.T) {
	now := time.Now()
	snap := testQuotaSnapshot(now, .25, .5)
	snap.Credentials = append(snap.Credentials, quota.Credential{ID: "extra", AccountID: "a", Provider: "a", Models: []string{"*"}})
	snap.Pools[0].CredentialIDs = append(snap.Pools[0].CredentialIDs, "extra")
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.snapshot = snap
	c := Combo{Name: "dup", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a"}, {Model: "x", QuotaPool: "b"}}}.Normalize()
	counts := [2]int{}
	for i := 0; i < 100; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [2]int{60, 40} {
		t.Fatalf("counts %v", counts)
	}
}
func TestConcurrentOrderCloseAndNilSelector(t *testing.T) {
	s := newWeightedSelector("", 60)
	c := Combo{Name: "concurrent", Strategy: StrategyWeighted, Members: []Member{{Model: "a"}, {Model: "b"}}}.Normalize()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Order(c, "")
				s.Close()
			}
		}()
	}
	wg.Wait()
	p := &combosPlugin{}
	if _, err := p.memberSelector(StrategyWeighted); err == nil {
		t.Fatal("uninitialized selector accepted")
	}
}
func TestAllExhaustedUseBaselineWeights(t *testing.T) {
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.snapshot = testQuotaSnapshot(time.Now(), 1, 1)
	c := Combo{Name: "all-exhausted", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a", Weight: 3}, {Model: "x", QuotaPool: "b", Weight: 1}}}.Normalize()
	counts := [2]int{}
	for i := 0; i < 100; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [2]int{75, 25} {
		t.Fatalf("counts %v", counts)
	}
}

func TestFreshQuotaPoolDifferentModelsShareAllocation(t *testing.T) {
	s := newWeightedSelector("", 60)
	defer s.Close()
	s.snapshot = testQuotaSnapshot(time.Now(), .5, .5)
	c := Combo{Name: "fresh-models", Strategy: StrategyQuotaAware, Members: []Member{{Model: "x", QuotaPool: "a"}, {Model: "y", QuotaPool: "a"}, {Model: "z", QuotaPool: "b"}}}.Normalize()
	counts := [3]int{}
	for i := 0; i < 400; i++ {
		counts[s.Order(c, "")[0]]++
	}
	if counts != [3]int{100, 100, 200} {
		t.Fatalf("counts %v", counts)
	}
}
