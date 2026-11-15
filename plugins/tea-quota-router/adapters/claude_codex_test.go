package adapters

import (
	"strings"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

var obs = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// Claude: real-shaped synthetic fixtures (CPAMP ClaudeUsagePayload/ClaudeProfileResponse).
const claudeUsageMax5 = `{
  "five_hour": {"utilization": 42.5, "resets_at": "2026-11-15T17:00:00Z"},
  "seven_day": {"utilization": 10, "resets_at": "2026-11-22T12:00:00Z"},
  "seven_day_opus": {"utilization": 25, "resets_at": "2026-11-22T12:00:00Z"},
  "seven_day_sonnet": null,
  "extra_usage": {"is_enabled": true, "monthly_limit": 10000, "used_credits": 2500, "utilization": 25}
}`
const claudeProfileMax5 = `{
  "account": {"uuid": "synthetic-uuid", "email": "person@example.invalid", "has_claude_max": true, "has_claude_pro": false},
  "organization": {"uuid": "synthetic-org", "name": "Example", "organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x", "subscription_status": "active"}
}`

func TestClaudeMax5WindowsAndEstimate(t *testing.T) {
	a, err := Claude([]byte(claudeUsageMax5), []byte(claudeProfileMax5), obs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "claude" || a.BillingType != "subscription" || a.Plan != "max5" || a.PlanSource != "reported" {
		t.Fatalf("account identity fields = %+v", a)
	}
	if a.ID != "" || a.Label != "" {
		t.Fatalf("adapter must not fill ID/label, got %q %q", a.ID, a.Label)
	}
	if strings.Contains(a.Plan+a.PlanSource, "example.invalid") || strings.Contains(a.Plan, "synthetic") {
		t.Fatal("identity leakage in plan fields")
	}
	byID := map[string]int{}
	for i, w := range a.Windows {
		byID[w.ID] = i
		if !w.ObservedAt.Equal(obs) {
			t.Fatalf("window %s observedAt not propagated", w.ID)
		}
	}
	fh, ok := byID["five-hour"]
	if !ok {
		t.Fatalf("missing five-hour in %+v", a.Windows)
	}
	if a.Windows[fh].UsedFraction == nil || *a.Windows[fh].UsedFraction != 0.425 {
		t.Fatalf("five-hour fraction = %v", a.Windows[fh].UsedFraction)
	}
	if a.Windows[fh].DurationSeconds != 18000 || !a.Windows[fh].ResetAt.Equal(time.Date(2026, 11, 15, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("five-hour window = %+v", a.Windows[fh])
	}
	if a.Windows[fh].Capacity == nil || *a.Windows[fh].Capacity != 5 || a.Windows[fh].CapacitySource != "plan_estimate" {
		t.Fatalf("max5 five-hour capacity must be 5 plan_estimate, got %+v", a.Windows[fh])
	}
	sd := a.Windows[byID["seven-day"]]
	if sd.Capacity == nil || *sd.Capacity != 5 || sd.DurationSeconds != 604800 {
		t.Fatalf("max5 seven-day = %+v", sd)
	}
	opus := a.Windows[byID["seven-day-opus"]]
	if len(opus.Models) == 0 {
		t.Fatal("opus scoped window must carry model scope")
	}
	if opus.Capacity != nil {
		t.Fatalf("scoped window capacity must stay unknown, got %v", *opus.Capacity)
	}
	// seven_day_sonnet is null: no zero-substituted window.
	if _, ok := byID["seven-day-sonnet"]; ok {
		t.Fatal("null scoped window must be omitted, not zeroed")
	}
}

func TestClaudeGenericMaxUnknownAndPro(t *testing.T) {
	usage := []byte(`{"five_hour":{"utilization":0,"resets_at":"2026-11-15T17:00:00Z"},"seven_day":null}`)
	profile := []byte(`{"account":{"has_claude_max":true},"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max"}}`)
	a, err := Claude(usage, profile, obs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan != "max" {
		t.Fatalf("generic max plan = %q", a.Plan)
	}
	for _, w := range a.Windows {
		if w.Capacity != nil {
			t.Fatalf("generic Max capacity must be unknown, window %s got %v", w.ID, *w.Capacity)
		}
	}
	if len(a.Windows) != 1 || a.Windows[0].UsedFraction == nil || *a.Windows[0].UsedFraction != 0 {
		t.Fatalf("explicit 0 utilization is valid measured zero, got %+v", a.Windows)
	}

	proProfile := []byte(`{"account":{"has_claude_pro":true},"organization":{"rate_limit_tier":"default_claude_pro"}}`)
	b, err := Claude(usage, proProfile, obs)
	if err != nil {
		t.Fatal(err)
	}
	if b.Plan != "pro" || b.Windows[0].Capacity == nil || *b.Windows[0].Capacity != 1 || b.Windows[0].CapacitySource != "plan_estimate" {
		t.Fatalf("pro = %+v window %+v", b, b.Windows[0])
	}
}

func TestClaudeMax20WeeklyCapacityUnknown(t *testing.T) {
	profile := []byte(`{"organization":{"rate_limit_tier":"default_claude_max_20x"}}`)
	a, err := Claude([]byte(claudeUsageMax5), profile, obs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan != "max20" {
		t.Fatalf("plan = %q", a.Plan)
	}
	for _, w := range a.Windows {
		if w.ID == "seven-day" && w.Capacity != nil {
			t.Fatalf("max20 weekly capacity must not be guessed, got %v", *w.Capacity)
		}
	}
}

func TestClaudeNullMissingInvalidFields(t *testing.T) {
	// Null usage window and invalid utilization type: no zero substitution.
	a, err := Claude([]byte(`{"five_hour":null,"seven_day":{"utilization":"high","resets_at":"not-a-time"}}`), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range a.Windows {
		if w.UsedFraction != nil {
			t.Fatalf("invalid utilization must stay unknown, got %v", *w.UsedFraction)
		}
	}
	if a.Plan != "" || a.PlanSource != "" {
		t.Fatalf("missing profile must leave plan unknown, got %q/%q", a.Plan, a.PlanSource)
	}
	// Negative utilization stays unknown.
	b, err := Claude([]byte(`{"five_hour":{"utilization":-1,"resets_at":"2026-11-15T17:00:00Z"}}`), []byte(`not json`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Windows) != 1 || b.Windows[0].UsedFraction != nil {
		t.Fatalf("out-of-range must be unknown, got %+v", b.Windows)
	}
	// Invalid usage payloads error without echoing the body.
	for _, bad := range []string{``, `{`, `[]`, `{"error":{"type":"x","message":"secret-body-marker"}}`} {
		if _, err := Claude([]byte(bad), nil, obs); err == nil {
			t.Errorf("Claude(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks raw body: %v", err)
		}
	}
}

func TestClaudeLimitsScopedAndFallback(t *testing.T) {
	usage := `{"limits":[
	  {"kind":"weekly_scoped","group":"weekly","percent":16,"resets_at":"2026-11-22T12:00:00Z","is_active":true,"scope":{"model":{"id":"claude-fable-synthetic","display_name":"Fable"}}},
	  {"kind":"session","group":"session","percent":5,"resets_at":"2026-11-15T17:00:00Z","is_active":true},
	  {"kind":"weekly_all","group":"weekly","percent":14,"resets_at":"2026-11-22T12:00:00Z","is_active":true},
	  {"kind":"session","group":"session","percent":99,"resets_at":"2026-11-15T17:00:00Z","is_active":false}
	]}`
	a, err := Claude([]byte(usage), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	var scopedFound bool
	for _, w := range a.Windows {
		ids[w.ID] = true
		if strings.HasPrefix(w.ID, "seven-day-scoped") {
			scopedFound = true
			if len(w.Models) != 1 || w.Models[0] != "claude-fable-synthetic" {
				t.Fatalf("scoped models = %v", w.Models)
			}
			if w.UsedFraction == nil || *w.UsedFraction != 0.16 {
				t.Fatalf("scoped fraction = %v", w.UsedFraction)
			}
		}
		if w.ID == "five-hour" && w.UsedFraction != nil && *w.UsedFraction != 0.05 {
			t.Fatalf("inactive limit must not win, got %v", *w.UsedFraction)
		}
	}
	if !ids["five-hour"] || !ids["seven-day"] || !scopedFound {
		t.Fatalf("limits fallback windows = %+v", a.Windows)
	}
}

// Codex: /wham/usage shape (CPAMP CodexUsagePayload + CPA codex_quota.go).
const codexUsage = `{
  "user_id": "synthetic-user", "account_id": "synthetic-account", "email": "person@example.invalid",
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 31, "limit_window_seconds": 18000, "resets_at": 1763290800},
    "secondary_window": {"used_percent": 12, "limit_window_seconds": 604800, "resets_at": 1763800000}
  },
  "additional_rate_limits": [
    {"limit_name": "gpt-5.3-codex-spark", "rate_limit": {"primary_window": {"used_percent": 3, "limit_window_seconds": 18000, "reset_after_seconds": 900}}}
  ],
  "credits": {"has_credits": true, "unlimited": false, "balance": "9.99"}
}`

func TestCodexWindowsByDuration(t *testing.T) {
	a, err := Codex([]byte(codexUsage), obs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "codex" || a.BillingType != "subscription" || a.Plan != "plus" || a.PlanSource != "reported" {
		t.Fatalf("codex identity = %+v", a)
	}
	if a.ID != "" || a.Label != "" || len(a.Financial) != 0 {
		t.Fatalf("codex must not fill identity or infer credit financials: %+v", a)
	}
	byID := map[string]int{}
	for i, w := range a.Windows {
		byID[w.ID] = i
		if w.Capacity != nil {
			t.Fatalf("codex capacity must stay unknown (%s)", w.ID)
		}
	}
	fh := a.Windows[byID["five-hour"]]
	if fh.UsedFraction == nil || *fh.UsedFraction != 0.31 || fh.DurationSeconds != 18000 || fh.ResetAt.Unix() != 1763290800 {
		t.Fatalf("five-hour = %+v", fh)
	}
	sd := a.Windows[byID["seven-day"]]
	if sd.UsedFraction == nil || *sd.UsedFraction != 0.12 || sd.DurationSeconds != 604800 {
		t.Fatalf("seven-day = %+v", sd)
	}
	scoped, ok := byID["gpt-5.3-codex-spark-five-hour"]
	if !ok {
		t.Fatalf("additional scoped window missing: %+v", a.Windows)
	}
	if len(a.Windows[scoped].Models) != 1 || a.Windows[scoped].Models[0] != "gpt-5.3-codex-spark" {
		t.Fatalf("scoped models = %v", a.Windows[scoped].Models)
	}
	if !a.Windows[scoped].ResetAt.Equal(obs.Add(900 * time.Second)) {
		t.Fatalf("reset_after_seconds must be relative to observedAt, got %v", a.Windows[scoped].ResetAt)
	}
}

func TestCodexSwappedWindowsAndNulls(t *testing.T) {
	// CPA fixture shape: primary can be weekly, secondary the 5h window.
	body := `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":81,"limit_window_seconds":604800,"reset_at":1763800000},"secondary_window":null}}`
	a, err := Codex([]byte(body), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Windows) != 1 || a.Windows[0].ID != "seven-day" || *a.Windows[0].UsedFraction != 0.81 {
		t.Fatalf("duration must decide window id, got %+v", a.Windows)
	}
	// Websocket-style rate_limits with null/missing used_percent stays unknown.
	b, err := Codex([]byte(`{"rate_limits":{"primary":{"used_percent":null,"window_minutes":300,"reset_after_seconds":60},"secondary":{"window_minutes":10080}}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Windows) != 2 {
		t.Fatalf("present windows with unknown usage must be preserved: %+v", b.Windows)
	}
	for _, w := range b.Windows {
		if w.UsedFraction != nil {
			t.Fatalf("null/missing percent must stay unknown, got %v", *w.UsedFraction)
		}
	}
	for _, bad := range []string{``, `{`, `[]`, `{"error":{"message":"secret-body-marker"}}`} {
		if _, err := Codex([]byte(bad), obs); err == nil {
			t.Errorf("Codex(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks body: %v", err)
		}
	}
	// Over-100 percent is clamped to exhausted.
	c, err := Codex([]byte(`{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000}}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Windows) != 1 || c.Windows[0].UsedFraction == nil || *c.Windows[0].UsedFraction != 1 {
		t.Fatalf("101%% must be exhausted: %+v", c.Windows)
	}
}

func TestClaudeLabelOnlyScopedLimitUsesOpaqueSentinelNotGlobal(t *testing.T) {
	usage := `{"limits":[
	  {"kind":"weekly_scoped","group":"weekly","percent":100,"resets_at":"2026-11-22T12:00:00Z","is_active":true,"scope":{"model":{"display_name":"Fable Preview"}}},
	  {"kind":"weekly_scoped","group":"weekly","percent":10,"resets_at":"2026-11-22T12:00:00Z","is_active":true,"scope":{"model":{"display_name":"!!!"}}}
	]}`
	a, err := Claude([]byte(usage), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Windows) != 2 {
		t.Fatalf("windows = %+v", a.Windows)
	}
	for _, w := range a.Windows {
		if len(w.Models) == 0 {
			t.Fatalf("label-only window %q became global (Models empty)", w.ID)
		}
		if !IsUnmatchedScope(w.Models) || !strings.HasPrefix(w.ID, "seven-day-scoped") || strings.HasSuffix(w.ID, "-") {
			t.Fatalf("window %q models %v not an unmatched sentinel", w.ID, w.Models)
		}
	}
	// A 100%-used label-only scope must not exhaust real models in shared scoring.
	acct := a
	acct.ID, acct.Provider = "x", "claude"
	for _, model := range []string{"claude-sonnet-4", "claude-opus-4", "claude-fable-synthetic"} {
		if sc := quota.AccountScore(acct, model, obs, time.Hour); sc.Exhausted {
			t.Fatalf("label-only scope exhausted %s: %+v", model, sc)
		}
	}
	// Exact ID scopes are unchanged.
	b, _ := Claude([]byte(`{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":5,"resets_at":"2026-11-22T12:00:00Z","scope":{"model":{"id":"claude-fable-synthetic","display_name":"Fable"}}}]}`), nil, obs)
	if len(b.Windows) != 1 || IsUnmatchedScope(b.Windows[0].Models) || b.Windows[0].Models[0] != "claude-fable-synthetic" {
		t.Fatalf("id-scoped window changed: %+v", b.Windows)
	}
}
