package adapters

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// Accounts produced by adapters must survive shared quota validation once
// core fills the ID, and must JSON-serialize without any identity material
// that was present in the synthetic source payloads.
func TestAdapterOutputsValidateAndDoNotLeak(t *testing.T) {
	claudeAcct, err := Claude([]byte(claudeUsageMax5), []byte(claudeProfileMax5), obs)
	if err != nil {
		t.Fatal(err)
	}
	codexAcct, err := Codex([]byte(codexUsage), obs)
	if err != nil {
		t.Fatal(err)
	}
	keyAcct, err := OpenRouterKey([]byte(openRouterKey), obs)
	if err != nil {
		t.Fatal(err)
	}
	credits, err := OpenRouterCredits([]byte(`{"data":{"total_credits":1,"total_usage":0}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	keyAcct.Financial = append(keyAcct.Financial, credits...)

	snap := quota.Snapshot{SchemaVersion: quota.SchemaVersion, GeneratedAt: obs}
	for i, a := range []quota.Account{claudeAcct, codexAcct, keyAcct} {
		a.ID = []string{"a-claude", "a-codex", "a-openrouter"}[i]
		snap.Accounts = append(snap.Accounts, a)
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"example.invalid", "user_synthetic", "ws-synthetic", "sk-or-v1"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("account %s leaks %q: %s", a.ID, leak, raw)
			}
		}
	}
	if err := quota.Validate(snap); err != nil {
		t.Fatalf("adapter outputs must validate after core assigns IDs: %v", err)
	}
}

// Scoped window model lists must be deterministic and sorted for stable
// snapshot diffs regardless of upstream ordering.
func TestScopedWindowOrderingDeterministic(t *testing.T) {
	body := `{"seven_day_opus":{"utilization":1,"resets_at":"2026-11-22T12:00:00Z"},"five_hour":{"utilization":2,"resets_at":"2026-11-15T17:00:00Z"},"seven_day":{"utilization":3,"resets_at":"2026-11-22T12:00:00Z"}}`
	a, err := Claude([]byte(body), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(a.Windows))
	for _, w := range a.Windows {
		ids = append(ids, w.ID)
	}
	if !sort.IsSorted(sort.StringSlice(ids)) {
		t.Fatalf("windows not sorted: %v", ids)
	}
}
