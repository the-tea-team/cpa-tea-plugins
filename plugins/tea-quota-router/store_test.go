package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStorePersistsSafeAggregation(t *testing.T) {
	s, err := openUsageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.insert(usageSample{At: now, AuthID: "opaque", Provider: "codex", Model: "m", InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.query(now.Add(-time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].InputTokens != 10 || rows[0].EstimatedCostUSD != nil {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if err = s.close(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageStorePruneAndUnsafeFiles(t *testing.T) {
	d := t.TempDir()
	s, err := openUsageStore(d)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, at := range []time.Time{now.Add(-31 * 24 * time.Hour), now.Add(-29 * 24 * time.Hour)} {
		if err := s.insert(usageSample{At: at, AuthID: "opaque"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.prune(now.Add(-30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.query(now.Add(-40 * 24 * time.Hour))
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	s.close()
	if err := os.Chmod(filepath.Join(d, "usage.db"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := openUsageStore(d); err == nil {
		t.Fatal("accepted writable database")
	}
	d = t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(d, "usage.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := openUsageStore(d); err == nil {
		t.Fatal("accepted symlink")
	}
}
