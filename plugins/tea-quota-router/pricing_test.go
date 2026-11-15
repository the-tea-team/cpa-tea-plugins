package main

import (
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"testing"
	"time"
)

func TestConfiguredCostAndValidation(t *testing.T) {
	c, err := parseConfig([]byte("dataDir: /tmp/x\nprices:\n  m: {input: 2, output: 4, cacheRead: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := &runtimeState{scheduler: &scheduler{}}
	rows := []usageRow{{Provider: "codex", Model: "m", InputTokens: 1000000, OutputTokens: 1000000, CacheReadTokens: 200000}, {Provider: "claude", Model: "m", InputTokens: 1000000, CacheReadTokens: 2000000}, {Provider: "codex", Model: "absent", InputTokens: 1}}
	result := p.summaryData(quota.Snapshot{}, rows, c, "day")["usage"].([]usageRow)
	if result[0].EstimatedCostUSD == nil || *result[0].EstimatedCostUSD != 5.8 || result[1].EstimatedCostUSD == nil || *result[1].EstimatedCostUSD != 4 || result[2].EstimatedCostUSD != nil {
		t.Fatalf("rows: %+v", result)
	}
	for _, value := range []string{"-1", ".nan", ".inf"} {
		if _, err := parseConfig([]byte("dataDir: /tmp/x\nprices:\n  m: {input: " + value + "}\n")); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestPriceMissingZeroAndOverride(t *testing.T) {
	c, err := parseConfig([]byte("dataDir: /tmp/x\nprices:\n  m: {input: 2}\n  codex:m: {input: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	rows := []usageRow{{Provider: "codex", Model: "m", InputTokens: 1000000}, {Provider: "claude", Model: "m", InputTokens: 1000000, OutputTokens: 1}, {Provider: "unknown", Model: "m", InputTokens: 1}}
	if got := estimateCost(rows[0], c.Prices); got == nil || *got != 0 {
		t.Fatalf("free override: %v", got)
	}
	if estimateCost(rows[1], c.Prices) != nil || estimateCost(rows[2], c.Prices) != nil {
		t.Fatal("unknown estimate")
	}
}
func TestPerRequestCacheClampAndUnknownAttribution(t *testing.T) {
	s, err := openUsageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	now := time.Now()
	for _, v := range []usageSample{{At: now, AuthID: "old", Provider: "codex", Model: "m", InputTokens: 1, CacheReadTokens: 3}, {At: now, AuthID: "old", Provider: "codex", Model: "m", InputTokens: 10}} {
		if err := s.insert(v); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.query(now.Add(-time.Second))
	if err != nil || len(rows) != 1 || rows[0].Requests != 2 {
		t.Fatalf("%+v %v", rows, err)
	}
	c, err := parseConfig([]byte("dataDir: /tmp/x\nprices:\n  m: {input: 1000000, cacheRead: 0}\n"))
	if err != nil {
		t.Fatal(err)
	}
	p := &runtimeState{scheduler: &scheduler{}}
	data := p.summaryData(quota.Snapshot{Credentials: []quota.Credential{{ID: "old", AccountID: "current"}}}, rows, c, "day")
	got := data["usage"].([]usageRow)[0]
	if got.EstimatedCostUSD == nil || *got.EstimatedCostUSD != 10 || data["health"].(map[string]any)["attributionUnknown"] != int64(2) {
		t.Fatalf("%+v %+v", got, data["health"])
	}
}
