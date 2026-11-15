package adapters

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// OpenRouterKey parses GET /api/v1/key. The key's own limit/usage and any
// guardrail budgets are different constraint scopes and are reported as
// separate Financial entries, never summed: key limit_remaining is
// key-budget-remaining@key, key usage is upstream-spend@key, and each budget
// keeps its reported scope (for example "member:<guardrail-id>") as
// configured-budget-remaining plus its spend. Responses that repeat the same
// budget (budgets[] and effective_budget) dedupe to one entry per scope.
// The key label and user/workspace identifiers are never copied out.
func OpenRouterKey(body []byte, observedAt time.Time) (quota.Account, error) {
	root, err := decodeObject(body, "openrouter key")
	if err != nil {
		return quota.Account{}, err
	}
	if _, isErr := field(root, "error"); isErr {
		return quota.Account{}, fmt.Errorf("adapters: openrouter key: upstream error envelope")
	}
	data, ok := subObject(root, "data")
	if !ok {
		return quota.Account{}, fmt.Errorf("adapters: openrouter key: %w", errNoData)
	}
	acct := quota.Account{
		Provider: "openrouter", BillingType: "api",
		ObservedAt: observedAt, RefreshStatus: "ok",
	}
	var fins []quota.Financial
	if v, present := moneyField(data, "limit_remaining"); present {
		fins = append(fins, quota.Financial{
			Kind: "key-budget-remaining", ScopeID: "key", Currency: "USD",
			Source: sourceOpenRouterKey, Amount: v, ObservedAt: observedAt,
			// Key payloads carry a reset interval label, not an absolute
			// reset time, so ResetAt stays zero (unknown).
		})
	}
	if v, present := moneyField(data, "usage"); present {
		fins = append(fins, quota.Financial{
			Kind: "upstream-spend", ScopeID: "key", Currency: "USD",
			Source: sourceOpenRouterKey, Amount: v, ObservedAt: observedAt,
		})
	}
	var budgets []map[string]json.RawMessage
	if raw, ok := field(data, "budgets"); ok {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, item := range arr {
				var b map[string]json.RawMessage
				if err := json.Unmarshal(item, &b); err == nil && b != nil {
					budgets = append(budgets, b)
				}
			}
		}
	}
	if eff, ok := subObject(data, "effective_budget"); ok {
		budgets = append(budgets, eff)
	}
	for _, b := range budgets {
		scope := budgetScope(b)
		reset := resetAt(b, observedAt)
		if v, present := moneyField(b, "remaining_usd", "remainingUSD"); present {
			fins = append(fins, quota.Financial{
				Kind: "configured-budget-remaining", ScopeID: scope, Currency: "USD",
				Source: sourceOpenRouterKey, Amount: v, ObservedAt: observedAt, ResetAt: reset,
			})
		}
		if v, present := moneyField(b, "spend_usd", "spendUSD"); present {
			fins = append(fins, quota.Financial{
				Kind: "upstream-spend", ScopeID: scope, Currency: "USD",
				Source: sourceOpenRouterKey, Amount: v, ObservedAt: observedAt, ResetAt: reset,
			})
		}
	}
	fins = dedupeFinancial(fins)
	sortFinancial(fins)
	acct.Financial = fins
	return acct, nil
}

// moneyField requires a present, finite, non-negative amount. Invalid or
// negative figures are treated as unknown rather than clamped.
func moneyField(m map[string]json.RawMessage, keys ...string) (float64, bool) {
	v, present, invalid := numberField(m, keys...)
	if !present || invalid || v < 0 {
		return 0, false
	}
	return v, true
}

func budgetScope(b map[string]json.RawMessage) string {
	scope, _ := stringField(b, "scope")
	id, _ := stringField(b, "guardrail_id", "guardrailId", "budget_id", "budgetId")
	switch {
	case scope != "" && id != "":
		return scope + ":" + id
	case id != "":
		return "budget:" + id
	case scope != "":
		return scope
	default:
		return "budget"
	}
}

// OpenRouterCredits parses GET /api/v1/credits (management key): account-wide
// purchased credits and lifetime usage. The prepaid balance is
// total_credits-total_usage at the single "account" scope shared by every
// key on the account; callers must fetch and store it once per account, not
// per key. A negative balance (overdrawn) is preserved, not clamped.
func OpenRouterCredits(body []byte, observedAt time.Time) ([]quota.Financial, error) {
	root, err := decodeObject(body, "openrouter credits")
	if err != nil {
		return nil, err
	}
	if _, isErr := field(root, "error"); isErr {
		return nil, fmt.Errorf("adapters: openrouter credits: upstream error envelope")
	}
	data, ok := subObject(root, "data")
	if !ok {
		return nil, fmt.Errorf("adapters: openrouter credits: %w", errNoData)
	}
	total, present, invalid := numberField(data, "total_credits")
	if !present || invalid || total < 0 {
		return nil, fmt.Errorf("adapters: openrouter credits: missing or invalid total_credits")
	}
	used, present, invalid := numberField(data, "total_usage")
	if !present || invalid || used < 0 {
		return nil, fmt.Errorf("adapters: openrouter credits: missing or invalid total_usage")
	}
	balance := total - used
	if math.IsNaN(balance) || math.IsInf(balance, 0) {
		return nil, fmt.Errorf("adapters: openrouter credits: non-finite balance")
	}
	return []quota.Financial{
		{Kind: "prepaid-balance", ScopeID: "account", Currency: "USD",
			Source: sourceOpenRouterCred, Amount: balance, ObservedAt: observedAt},
		{Kind: "upstream-spend", ScopeID: "account", Currency: "USD",
			Source: sourceOpenRouterCred, Amount: used, ObservedAt: observedAt},
	}, nil
}

// dataEnvelope unwraps a report page: object with a `data` bucket array plus
// has_more/next_page pagination. Unknown page structure is an error, not an
// empty page, so a schema change cannot silently zero a report.
func dataEnvelope(body []byte, what string) (buckets []map[string]json.RawMessage, page UsagePage, err error) {
	root, err := decodeObject(body, what)
	if err != nil {
		return nil, UsagePage{}, err
	}
	if _, isErr := field(root, "error"); isErr {
		return nil, UsagePage{}, fmt.Errorf("adapters: %s: upstream error envelope", what)
	}
	raw, ok := field(root, "data")
	if !ok {
		return nil, UsagePage{}, fmt.Errorf("adapters: %s: %w", what, errNoData)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, UsagePage{}, fmt.Errorf("adapters: %s: data is not an array", what)
	}
	for _, item := range items {
		var bucket map[string]json.RawMessage
		if err := json.Unmarshal(item, &bucket); err != nil || bucket == nil {
			return nil, UsagePage{}, fmt.Errorf("adapters: %s: bucket is not an object", what)
		}
		buckets = append(buckets, bucket)
	}
	if v, ok := boolField(root, "has_more", "hasMore"); ok {
		page.HasMore = v
	}
	if s, ok := stringField(root, "next_page", "nextPage"); ok {
		page.NextPage = s
	}
	if !page.HasMore {
		page.NextPage = ""
	}
	return buckets, page, nil
}

func bucketResults(bucket map[string]json.RawMessage, what string) ([]map[string]json.RawMessage, error) {
	raw, ok := field(bucket, "results")
	if !ok {
		return nil, nil // bucket with no results: no usage, not an error
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("adapters: %s: results is not an array", what)
	}
	out := make([]map[string]json.RawMessage, 0, len(items))
	for _, item := range items {
		var r map[string]json.RawMessage
		if err := json.Unmarshal(item, &r); err != nil || r == nil {
			return nil, fmt.Errorf("adapters: %s: result is not an object", what)
		}
		out = append(out, r)
	}
	return out, nil
}

// tokenCount requires a present non-negative integer token/request count.
// A malformed count fails the whole page: silently skipping it would under-
// report a billing-scope total.
func tokenCount(m map[string]json.RawMessage, what string, keys ...string) (int64, bool, error) {
	v, present, invalid := numberField(m, keys...)
	if !present {
		return 0, false, nil
	}
	if invalid || v < 0 || v != math.Trunc(v) || v > 9.3e18 {
		return 0, false, fmt.Errorf("adapters: %s: invalid token count", what)
	}
	return int64(v), true, nil
}

func addCount(dst *int64, v int64, what string) error {
	if *dst > math.MaxInt64-v {
		return fmt.Errorf("adapters: %s: token total overflow", what)
	}
	*dst += v
	return nil
}

// OpenAIUsage sums one page of an OpenAI organization usage report
// (for example /v1/organization/usage/completions). input_tokens already
// includes cached tokens, so CacheReadTokens is reported alongside and never
// added into InputTokens a second time. Results without token fields (image,
// audio or vector-store buckets) contribute only their request counts.
func OpenAIUsage(body []byte, observedAt time.Time) (UsagePage, error) {
	_ = observedAt
	buckets, page, err := dataEnvelope(body, "openai usage")
	if err != nil {
		return UsagePage{}, err
	}
	var out UsagePage
	out.NextPage, out.HasMore = page.NextPage, page.HasMore
	for _, bucket := range buckets {
		results, err := bucketResults(bucket, "openai usage")
		if err != nil {
			return UsagePage{}, err
		}
		for _, r := range results {
			for _, spec := range []struct {
				dst  *int64
				keys []string
			}{
				{&out.InputTokens, []string{"input_tokens"}},
				{&out.OutputTokens, []string{"output_tokens"}},
				{&out.CacheReadTokens, []string{"input_cached_tokens"}},
				{&out.Requests, []string{"num_model_requests"}},
			} {
				v, _, err := tokenCount(r, "openai usage", spec.keys...)
				if err != nil {
					return UsagePage{}, err
				}
				if err := addCount(spec.dst, v, "openai usage"); err != nil {
					return UsagePage{}, err
				}
			}
		}
	}
	return out, nil
}

// OpenAICost sums one page of GET /v1/organization/costs into USD. Only
// results whose amount.currency is USD feed AmountUSD (CostPage has no other
// currency slot); other currencies are excluded rather than mislabeled. A
// present but malformed USD amount fails the page.
func OpenAICost(body []byte, observedAt time.Time) (CostPage, error) {
	_ = observedAt
	buckets, page, err := dataEnvelope(body, "openai costs")
	if err != nil {
		return CostPage{}, err
	}
	out := CostPage{NextPage: page.NextPage, HasMore: page.HasMore}
	for _, bucket := range buckets {
		results, err := bucketResults(bucket, "openai costs")
		if err != nil {
			return CostPage{}, err
		}
		for _, r := range results {
			amt, ok := subObject(r, "amount")
			if !ok {
				continue
			}
			currency, _ := stringField(amt, "currency")
			if !strings.EqualFold(currency, "usd") {
				continue
			}
			v, present, invalid := numberField(amt, "value")
			if !present || invalid {
				return CostPage{}, fmt.Errorf("adapters: openai costs: invalid amount value")
			}
			out.AmountUSD += v
			if math.IsNaN(out.AmountUSD) || math.IsInf(out.AmountUSD, 0) {
				return CostPage{}, fmt.Errorf("adapters: openai costs: non-finite total")
			}
		}
	}
	return out, nil
}

// AnthropicUsage sums one page of the legacy Anthropic organization usage
// report (/v1/organizations/usage_report/messages). The token categories are
// disjoint: input = uncached + cache-read + cache-creation (5m + 1h). The
// legacy report carries no request count, so Requests stays 0 rather than
// being invented. Costs are not present here and are never derived.
func AnthropicUsage(body []byte, observedAt time.Time) (UsagePage, error) {
	_ = observedAt
	buckets, page, err := dataEnvelope(body, "anthropic usage")
	if err != nil {
		return UsagePage{}, err
	}
	var out UsagePage
	out.NextPage, out.HasMore = page.NextPage, page.HasMore
	for _, bucket := range buckets {
		results, err := bucketResults(bucket, "anthropic usage")
		if err != nil {
			return UsagePage{}, err
		}
		for _, r := range results {
			uncached, _, err := tokenCount(r, "anthropic usage", "uncached_input_tokens")
			if err != nil {
				return UsagePage{}, err
			}
			cacheRead, _, err := tokenCount(r, "anthropic usage", "cache_read_input_tokens")
			if err != nil {
				return UsagePage{}, err
			}
			output, _, err := tokenCount(r, "anthropic usage", "output_tokens")
			if err != nil {
				return UsagePage{}, err
			}
			var creation int64
			if cc, ok := subObject(r, "cache_creation"); ok {
				for _, k := range []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"} {
					v, _, err := tokenCount(cc, "anthropic usage", k)
					if err != nil {
						return UsagePage{}, err
					}
					if err := addCount(&creation, v, "anthropic usage"); err != nil {
						return UsagePage{}, err
					}
				}
			}
			for _, add := range []struct {
				dst *int64
				v   int64
			}{
				{&out.InputTokens, uncached}, {&out.InputTokens, cacheRead},
				{&out.InputTokens, creation}, {&out.OutputTokens, output},
				{&out.CacheReadTokens, cacheRead},
			} {
				if err := addCount(add.dst, add.v, "anthropic usage"); err != nil {
					return UsagePage{}, err
				}
			}
		}
	}
	return out, nil
}

// AnthropicCost sums one page of the legacy Anthropic cost report
// (/v1/organizations/cost_report). Amounts are decimal strings in cents
// ("123.45" in USD is $1.2345); USD is the only documented currency, and any
// other currency is excluded rather than mislabeled as USD.
func AnthropicCost(body []byte, observedAt time.Time) (CostPage, error) {
	_ = observedAt
	buckets, page, err := dataEnvelope(body, "anthropic cost")
	if err != nil {
		return CostPage{}, err
	}
	out := CostPage{NextPage: page.NextPage, HasMore: page.HasMore}
	for _, bucket := range buckets {
		results, err := bucketResults(bucket, "anthropic cost")
		if err != nil {
			return CostPage{}, err
		}
		for _, r := range results {
			currency, _ := stringField(r, "currency")
			if !strings.EqualFold(currency, "USD") {
				continue
			}
			raw, ok := field(r, "amount")
			if !ok {
				return CostPage{}, fmt.Errorf("adapters: anthropic cost: result missing amount")
			}
			s := strings.TrimSpace(string(raw))
			var text string
			if len(s) > 0 && s[0] == '"' {
				if err := json.Unmarshal(raw, &text); err != nil {
					return CostPage{}, fmt.Errorf("adapters: anthropic cost: invalid amount")
				}
			} else {
				text = s
			}
			usd, ok := parseCentsString(text)
			if !ok {
				return CostPage{}, fmt.Errorf("adapters: anthropic cost: invalid amount")
			}
			out.AmountUSD += usd
			if math.IsNaN(out.AmountUSD) || math.IsInf(out.AmountUSD, 0) {
				return CostPage{}, fmt.Errorf("adapters: anthropic cost: non-finite total")
			}
		}
	}
	return out, nil
}
