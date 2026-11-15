package adapters

import (
	"math"
	"strings"
	"testing"
	"time"
)

// OpenRouter /api/v1/key official example shape (identifiers synthetic).
const openRouterKey = `{"data":{
  "label":"sk-or-v1-synthetic","is_management_key":false,"is_free_tier":false,
  "expires_at":"2027-12-31T23:59:59Z","creator_user_id":"user_synthetic","workspace_id":"ws-synthetic",
  "limit":100,"limit_remaining":74.5,"limit_reset":"monthly",
  "usage":25.5,"usage_daily":25.5,"usage_weekly":20,"usage_monthly":25.5,
  "budgets":[{"budget_id":null,"guardrail_id":"g-synthetic","limit_usd":50,"remaining_usd":17.5,"spend_usd":32.5,"reset_interval":"weekly","resets_at":"2026-11-22T00:00:00Z","scope":"member"}],
  "effective_budget":{"budget_id":null,"guardrail_id":"g-synthetic","limit_usd":50,"remaining_usd":17.5,"spend_usd":32.5,"reset_interval":"weekly","resets_at":"2026-11-22T00:00:00Z","scope":"member"}
}}`

func TestOpenRouterKeyBudgetsAndSpend(t *testing.T) {
	a, err := OpenRouterKey([]byte(openRouterKey), obs)
	if err != nil {
		t.Fatal(err)
	}
	if a.Provider != "openrouter" || a.BillingType != "api" || a.Plan != "" || a.ID != "" || a.Label != "" {
		t.Fatalf("key account = %+v", a)
	}
	// Key scope: budget remaining + lifetime spend; budgets/effective_budget
	// describing the same guardrail dedupe to one configured-budget entry.
	want := map[string]float64{
		"key-budget-remaining|key":                       74.5,
		"upstream-spend|key":                             25.5,
		"configured-budget-remaining|member:g-synthetic": 17.5,
		"upstream-spend|member:g-synthetic":              32.5,
	}
	got := map[string]float64{}
	for _, f := range a.Financial {
		if f.Currency != "USD" || f.Source == "" || !f.ObservedAt.Equal(obs) {
			t.Fatalf("financial metadata incomplete: %+v", f)
		}
		if strings.Contains(f.ScopeID, "ws-synthetic") || strings.Contains(f.ScopeID, "user_synthetic") {
			t.Fatalf("identity leakage in scope: %+v", f)
		}
		got[f.Kind+"|"+f.ScopeID] = f.Amount
		if f.Kind == "configured-budget-remaining" && !f.ResetAt.Equal(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("budget reset not preserved: %+v", f)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("financial = %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v want %v", k, got[k], v)
		}
	}
}

func TestOpenRouterKeyUnlimitedAndNulls(t *testing.T) {
	// limit null = unlimited key: no budget-remaining fabricated, spend kept.
	a, err := OpenRouterKey([]byte(`{"data":{"limit":null,"limit_remaining":null,"usage":0,"expires_at":null}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Financial) != 1 || a.Financial[0].Kind != "upstream-spend" || a.Financial[0].Amount != 0 {
		t.Fatalf("explicit zero spend is measured, null limit must not fabricate: %+v", a.Financial)
	}
	// Missing usage: nothing at all, no zero substitution.
	b, err := OpenRouterKey([]byte(`{"data":{"is_free_tier":true}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Financial) != 0 {
		t.Fatalf("missing fields must stay absent: %+v", b.Financial)
	}
	for _, bad := range []string{``, `{`, `[]`, `{}`, `{"data":null}`, `{"error":{"code":401,"message":"secret-body-marker"}}`} {
		if _, err := OpenRouterKey([]byte(bad), obs); err == nil {
			t.Errorf("OpenRouterKey(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks body: %v", err)
		}
	}
	// Invalid (non-numeric / negative) amounts are dropped, valid ones kept.
	c, err := OpenRouterKey([]byte(`{"data":{"limit_remaining":"lots","usage":-4,"usage_monthly":3}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Financial) != 0 {
		t.Fatalf("invalid amounts must be dropped: %+v", c.Financial)
	}
}

func TestOpenRouterCreditsPrepaidScope(t *testing.T) {
	fs, err := OpenRouterCredits([]byte(`{"data":{"total_credits":100.5,"total_usage":25.75}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("credits financials = %+v", fs)
	}
	byKind := map[string]float64{}
	for _, f := range fs {
		byKind[f.Kind] = f.Amount
		if f.ScopeID != "account" || f.Currency != "USD" {
			t.Fatalf("credits must retain account scope: %+v", f)
		}
	}
	if byKind["prepaid-balance"] != 74.75 || byKind["upstream-spend"] != 25.75 {
		t.Fatalf("credits = %v", byKind)
	}
	// Negative balance stays as reported (overdrawn), not clamped to zero.
	fs, err = OpenRouterCredits([]byte(`{"data":{"total_credits":10,"total_usage":12.5}}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.Kind == "prepaid-balance" && f.Amount != -2.5 {
			t.Fatalf("overdrawn balance = %v", f.Amount)
		}
	}
	for _, bad := range []string{``, `{}`, `{"data":{"total_credits":"x"}}`, `{"error":{"message":"secret-body-marker"}}`} {
		if _, err := OpenRouterCredits([]byte(bad), obs); err == nil {
			t.Errorf("OpenRouterCredits(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks body: %v", err)
		}
	}
}

// OpenAI organization usage/costs page shapes (official API reference).
const openAIUsagePage = `{"object":"page","data":[
  {"object":"bucket","start_time":1730419200,"end_time":1730505600,"results":[
    {"object":"organization.usage.completions.result","input_tokens":1000,"input_cached_tokens":200,"output_tokens":300,"num_model_requests":7},
    {"object":"organization.usage.embeddings.result","input_tokens":50,"num_model_requests":2}
  ]},
  {"object":"bucket","start_time":1730505600,"end_time":1730592000,"results":[
    {"object":"organization.usage.completions.result","input_tokens":10,"output_tokens":5,"num_model_requests":1}
  ]}
],"has_more":true,"next_page":"page_synthetic"}`

func TestOpenAIUsagePageSums(t *testing.T) {
	p, err := OpenAIUsage([]byte(openAIUsagePage), obs)
	if err != nil {
		t.Fatal(err)
	}
	// input_tokens already includes cached tokens: cache is reported, not re-added.
	if p.InputTokens != 1060 || p.OutputTokens != 305 || p.CacheReadTokens != 200 || p.Requests != 10 {
		t.Fatalf("page = %+v", p)
	}
	if !p.HasMore || p.NextPage != "page_synthetic" {
		t.Fatalf("pagination = %+v", p)
	}
	p, err = OpenAIUsage([]byte(`{"object":"page","data":[],"has_more":false,"next_page":null}`), obs)
	if err != nil {
		t.Fatal(err)
	}
	if p.HasMore || p.NextPage != "" || p.InputTokens != 0 {
		t.Fatalf("empty page = %+v", p)
	}
	for _, bad := range []string{``, `[]`, `{"object":"page"}`, `{"data":[{"results":[{"input_tokens":"lots"}]}]}`, `{"error":{"message":"secret-body-marker"}}`} {
		if _, err := OpenAIUsage([]byte(bad), obs); err == nil {
			t.Errorf("OpenAIUsage(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks body: %v", err)
		}
	}
}

func TestOpenAICostPageSumsUSDOnly(t *testing.T) {
	body := `{"object":"page","data":[{"object":"bucket","start_time":1730419200,"end_time":1730505600,"results":[
	  {"object":"organization.costs.result","amount":{"value":12.34,"currency":"usd"}},
	  {"object":"organization.costs.result","amount":{"value":0.66,"currency":"usd"}},
	  {"object":"organization.costs.result","amount":{"value":99,"currency":"eur"}}
	]}],"has_more":false,"next_page":null}`
	p, err := OpenAICost([]byte(body), obs)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(p.AmountUSD-13.0) > 1e-9 {
		t.Fatalf("non-USD amounts must be excluded from AmountUSD, got %v", p.AmountUSD)
	}
	if _, err := OpenAICost([]byte(`{"data":[{"results":[{"amount":{"value":"lots","currency":"usd"}}]}]}`), obs); err == nil {
		t.Fatal("invalid amount must error, not silently total")
	}
}

// Anthropic legacy usage/cost report shapes (official API reference).
const anthropicUsagePage = `{"data":[
  {"starting_at":"2025-08-01T00:00:00Z","ending_at":"2025-08-02T00:00:00Z","results":[
    {"uncached_input_tokens":1500,"cache_read_input_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":0},"output_tokens":500},
    {"uncached_input_tokens":10,"cache_read_input_tokens":0,"output_tokens":5}
  ]}
],"has_more":true,"next_page":"page_synthetic"}`

func TestAnthropicUsagePageSums(t *testing.T) {
	p, err := AnthropicUsage([]byte(anthropicUsagePage), obs)
	if err != nil {
		t.Fatal(err)
	}
	// Input = uncached + cache-read + cache-creation (disjoint categories).
	if p.InputTokens != 1810 || p.OutputTokens != 505 || p.CacheReadTokens != 200 || p.Requests != 0 {
		t.Fatalf("page = %+v", p)
	}
	if !p.HasMore || p.NextPage != "page_synthetic" {
		t.Fatalf("pagination = %+v", p)
	}
	for _, bad := range []string{``, `{"data":[{"results":[{"uncached_input_tokens":-1}]}]}`, `{"error":{"message":"secret-body-marker"}}`} {
		if _, err := AnthropicUsage([]byte(bad), obs); err == nil {
			t.Errorf("AnthropicUsage(%q) must error", bad)
		} else if strings.Contains(err.Error(), "secret-body-marker") {
			t.Errorf("error leaks body: %v", err)
		}
	}
}

func TestAnthropicCostPageCentsToUSD(t *testing.T) {
	// amount is a decimal string in cents: "123.45" USD == $1.2345.
	body := `{"data":[{"starting_at":"2025-08-01T00:00:00Z","ending_at":"2025-08-02T00:00:00Z","results":[
	  {"amount":"123.45","currency":"USD"},{"amount":"76.55","currency":"USD"}
	]}],"has_more":false,"next_page":null}`
	p, err := AnthropicCost([]byte(body), obs)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(p.AmountUSD-2.0) > 1e-9 || p.HasMore || p.NextPage != "" {
		t.Fatalf("cost page = %+v", p)
	}
	if _, err := AnthropicCost([]byte(`{"data":[{"results":[{"amount":"lots","currency":"USD"}]}]}`), obs); err == nil {
		t.Fatal("invalid amount must error")
	}
}
