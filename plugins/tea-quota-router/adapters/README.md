# Quota/billing adapters

Pure parsers for upstream quota and billing payloads. Each function takes a
raw response body already fetched by the core collector and returns only
normalized `internal/tea/quota` values (or a summed report page). Adapters do
no network, file or environment access; account IDs, labels and credential
material in a payload are never copied out, and errors never embed the raw
body. Null, missing and invalid measurements stay unknown — they are never
replaced with zero. All fixtures in the tests are synthetic and real-shaped.

## Sources checked (2026-11-15)

| Adapter | Upstream | Source |
|---|---|---|
| `Claude` | `GET https://api.anthropic.com/api/oauth/usage` + `GET https://api.anthropic.com/api/oauth/profile` | CPA-Manager-Plus `apps/web/src/utils/quota/providerRequests.ts`, `constants.ts` (`CLAUDE_USAGE_URL`, `CLAUDE_USAGE_WINDOW_KEYS`) and `apps/web/src/types/quota.ts` (`ClaudeUsagePayload`, `ClaudeProfileResponse`) at revision `a67ed8834a914ab24c36d2490fa6bb6d4166724c`; tier strings (`default_claude_max_5x`, `default_claude_max_20x`, `default_claude_pro`) corroborated against `organization.rate_limit_tier` from `/api/oauth/profile`. |
| `Codex` | `GET https://chatgpt.com/backend-api/wham/usage` | CPA v8.0.69 source `internal/runtime/executor/helps/codex_quota.go` and `codex_quota_test.go` (read-only cache `cpa-v8.0.69`), plus CPAMP `CodexUsagePayload`/`codexQuota.ts` at the revision above. |
| `OpenRouterKey` | `GET https://openrouter.ai/api/v1/key` | Official docs: https://openrouter.ai/docs/api/api-reference/api-keys/get-current-key |
| `OpenRouterCredits` | `GET https://openrouter.ai/api/v1/credits` (management key) | Official docs: https://openrouter.ai/docs/api/api-reference/credits/get-credits |
| `OpenAIUsage` | `GET https://api.openai.com/v1/organization/usage/completions` (and sibling usage endpoints) | Official docs: https://platform.openai.com/docs/api-reference/usage |
| `OpenAICost` | `GET https://api.openai.com/v1/organization/costs` | Same OpenAI Usage/Costs reference (`organization.costs.result`, `amount.value`/`amount.currency`). |
| `AnthropicUsage` | `GET https://api.anthropic.com/v1/organizations/usage_report/messages` (legacy) | Official docs: https://platform.claude.com/docs/en/build-with-claude/usage-cost-api and API reference `GET /v1/organizations/usage_report/messages`. |
| `AnthropicCost` | `GET https://api.anthropic.com/v1/organizations/cost_report` (legacy) | Same Anthropic Usage and Cost API docs; amounts are decimal strings in cents. |

Plan notes: the approved plan records that CPAMP classifies `has_claude_max`
only as a generic `plan_max` and does not establish 5x versus 20x, so this
adapter trusts only an explicit `organization.rate_limit_tier`. The OpenAI
and Anthropic organization reports retain their report scope: a summed page
is an organization total, not a per-key value, and is never emitted here as a
key-level budget or balance.

## Behavior contracts

- **Claude** — provider `claude`, billing `subscription`. Windows
  `five-hour` (18000s) and `seven-day` (604800s) from `five_hour`/`seven_day`,
  scoped weekly windows from `seven_day_opus`/`seven_day_sonnet`/
  `seven_day_cowork`/`seven_day_oauth_apps`/`iguana_necktie`, plus the newer
  `limits[]` array (base session/weekly fill gaps only; `weekly_scoped`
  entries become `seven-day-scoped-<model>` windows). Explicit tiers:
  `default_claude_max_5x` → plan `max5`, 5x `plan_estimate` capacity on both
  global windows; `default_claude_max_20x` → `max20`, 20x estimate on the
  five-hour window only; `default_claude_pro` → `pro`, 1x estimates. A
  generic Max tier or a bare `has_claude_max` flag → plan `max`, capacity
  unknown. Scoped windows never receive a plan capacity. Enabled
  `extra_usage.used_credits` (cents) becomes one `upstream-spend` entry at
  scope `claude-extra-usage`.
- **Codex** — provider `codex`, billing `subscription`, plan from
  `plan_type` verbatim (lowercased). Windows are identified by reported
  duration (18000s → `five-hour`, 604800s → `seven-day`, other durations keep
  their seconds in the ID), from both the `/wham/usage` shape
  (`rate_limit.primary_window`…) and the websocket shape
  (`rate_limits.primary`, `window_minutes`, `reset_after_seconds`). Additional
  limits (array or name-keyed object, max 8 names) become model-scoped
  windows; code-review limits are prefixed `code-review-`. Capacity is never
  reported by this API and stays unknown; `credits` fields never become
  financial entries.
- **OpenRouterKey** — provider `openrouter`, billing `api`.
  `limit_remaining` → `key-budget-remaining@key`, lifetime `usage` →
  `upstream-spend@key`, and each guardrail budget keeps its reported scope
  (`<scope>:<guardrail-id>`) as `configured-budget-remaining` plus budgeted
  `upstream-spend`, with the budget's reset preserved. `budgets[]` and
  `effective_budget` repeats of the same guardrail dedupe to one entry.
- **OpenRouterCredits** — returns `prepaid-balance@account`
  (`total_credits − total_usage`, negative preserved) and
  `upstream-spend@account`. This is an account-wide scope shared by all keys:
  fetch and store it once per account.
- **Report pages** (`OpenAIUsage`, `OpenAICost`, `AnthropicUsage`,
  `AnthropicCost`) — return one page of summed totals plus `has_more` /
  `next_page` for the core collector to drive pagination. OpenAI
  `input_tokens` already includes cached tokens, so `CacheReadTokens` is
  reported but not re-added. Anthropic input is the disjoint sum
  uncached + cache-read + cache-creation; the legacy Anthropic usage report
  has no request count, so `Requests` stays 0. Anthropic cost amounts are
  cents strings converted to USD. Only USD amounts feed `AmountUSD`.

## Limitations

- Claude plan capacities are relative weights labeled `plan_estimate`, not
  provider-reported token limits; per-window weights may not be comparable
  across model scopes, and Max 20x weekly weight is deliberately unknown.
- Claude `limits[]` entries with a model display name but no ID, and the
  app-only Cowork and OAuth apps windows, use an opaque unmatched model-scope
  sentinel. They remain visible but cannot exhaust ordinary model requests;
  their precise scope cannot be matched by the model scheduler.
- Codex durations other than 18000/604800 keep an opaque `window-<n>s` ID;
  monthly windows are not renamed to a calendar concept.
- OpenRouter key payloads give no absolute reset for the key's own budget
  (`limit_reset` is an interval label), so that entry's `ResetAt` is unknown.
- Report adapters sum a single page only, across every bucket/result on it,
  regardless of `group_by` dimensions; grouping/attribution stays with core.
  Non-USD cost results are excluded from `AmountUSD` (neither API documents
  non-USD billing today, so this is a guard, not a conversion).
- Anthropic's newer Usage and Cost Analytics API is not parsed here; the
  legacy report endpoints above remain supported per the official docs.
