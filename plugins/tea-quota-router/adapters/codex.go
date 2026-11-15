package adapters

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// Codex parses a ChatGPT /wham/usage payload (also accepting the websocket
// `rate_limits` spelling used by CPA's codex quota events). Windows are
// identified by their reported duration, never by positional names:
// 18000s -> "five-hour", 604800s -> "seven-day", anything else keeps its
// duration in the window ID. No capacity is reported by this API, so
// Capacity stays unknown, and credit fields never become financial entries
// (they are message-credit metadata, not spend or prepaid balance).
func Codex(body []byte, observedAt time.Time) (quota.Account, error) {
	root, err := decodeObject(body, "codex usage")
	if err != nil {
		return quota.Account{}, err
	}
	if _, isErr := field(root, "error"); isErr {
		return quota.Account{}, fmt.Errorf("adapters: codex usage: upstream error envelope")
	}
	acct := quota.Account{
		Provider: "codex", BillingType: "subscription",
		ObservedAt: observedAt, RefreshStatus: "ok",
	}
	if plan, ok := stringField(root, "plan_type", "planType"); ok && plan != "" {
		acct.Plan = strings.ToLower(plan)
		acct.PlanSource = "reported"
	}
	recognized := acct.Plan != ""

	var windows []quota.Window
	if rl, ok := subObject(root, "rate_limit", "rateLimit", "rate_limits", "rateLimits"); ok {
		recognized = true
		windows = append(windows, codexRateLimitWindows(rl, "", nil, observedAt)...)
	} else if _, ok := field(root, "rate_limit", "rateLimit", "rate_limits", "rateLimits"); ok {
		recognized = true // explicitly null: known-empty inventory, not an error
	}
	if cr, ok := subObject(root, "code_review_rate_limit", "codeReviewRateLimit", "code_review_rate_limits", "codeReviewRateLimits"); ok {
		recognized = true
		for _, w := range codexRateLimitWindows(cr, "", nil, observedAt) {
			w.ID = "code-review-" + w.ID
			if w.Models == nil {
				w.Models = []string{"code-review"}
			}
			windows = append(windows, w)
		}
	}
	windows = append(windows, codexAdditionalWindows(root, observedAt)...)
	if len(windows) > 0 {
		recognized = true
	}
	sortWindows(windows)
	acct.Windows = dedupeWindows(windows)
	if !recognized {
		return quota.Account{}, fmt.Errorf("adapters: codex usage: %w", errNoData)
	}
	return acct, nil
}

func codexRateLimitWindows(rl map[string]json.RawMessage, prefix string, models []string, observedAt time.Time) []quota.Window {
	var out []quota.Window
	for _, pos := range []string{"primary_window", "secondary_window", "primaryWindow", "secondaryWindow", "primary", "secondary"} {
		winObj, ok := subObject(rl, pos)
		if !ok {
			continue
		}
		if w, ok := codexWindow(winObj, prefix, models, observedAt); ok {
			out = append(out, w)
		}
	}
	return out
}

func codexWindow(winObj map[string]json.RawMessage, prefix string, models []string, observedAt time.Time) (quota.Window, bool) {
	duration := codexDurationSeconds(winObj)
	reset := resetAt(winObj, observedAt)
	frac := percentFraction(winObj, "used_percent", "usedPercent")
	// A window object with no usable usage, reset or duration carries no
	// observation at all.
	if frac == nil && reset.IsZero() && duration == 0 {
		return quota.Window{}, false
	}
	return quota.Window{
		ID:              prefix + codexWindowID(duration),
		DurationSeconds: duration,
		UsedFraction:    frac,
		ResetAt:         reset,
		ObservedAt:      observedAt,
		Models:          append([]string(nil), models...),
	}, true
}

func codexDurationSeconds(winObj map[string]json.RawMessage) int64 {
	if v, present, invalid := numberField(winObj, "limit_window_seconds", "limitWindowSeconds"); present && !invalid && v > 0 {
		return int64(v)
	}
	if v, present, invalid := numberField(winObj, "window_minutes", "windowMinutes"); present && !invalid && v > 0 {
		return int64(v * 60)
	}
	return 0
}

func codexWindowID(duration int64) string {
	switch duration {
	case 18000:
		return "five-hour"
	case 604800:
		return "seven-day"
	case 0:
		return "window"
	default:
		return fmt.Sprintf("window-%ds", duration)
	}
}

// codexAdditionalWindows handles both upstream spellings: an array of
// {limit_name, rate_limit} (HTTP /wham/usage) and an object keyed by limit
// name (websocket events). Limit names are upstream-controlled, so they are
// slugged before use in IDs/scopes and entries with unsafe or empty names
// are dropped after a bounded count.
func codexAdditionalWindows(root map[string]json.RawMessage, observedAt time.Time) []quota.Window {
	raw, ok := field(root, "additional_rate_limits", "additionalRateLimits")
	if !ok {
		return nil
	}
	const maxAdditional = 8
	var out []quota.Window
	add := func(name string, rl map[string]json.RawMessage) {
		id := slug(name)
		if id == "" || len(out) >= maxAdditional {
			return
		}
		out = append(out, codexRateLimitWindows(rl, id+"-", []string{id}, observedAt)...)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, item := range arr {
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(item, &entry); err != nil || entry == nil {
				continue
			}
			name, _ := stringField(entry, "limit_name", "limitName", "name")
			rl, ok := subObject(entry, "rate_limit", "rateLimit")
			if !ok {
				rl = entry
			}
			add(name, rl)
		}
		return out
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		names := make([]string, 0, len(obj))
		for name := range obj {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			var entry map[string]json.RawMessage
			if err := json.Unmarshal(obj[name], &entry); err != nil || entry == nil {
				continue
			}
			rl, ok := subObject(entry, "rate_limit", "rateLimit")
			if !ok {
				rl = entry
			}
			add(name, rl)
		}
	}
	return out
}

func sortStrings(s []string) {
	sort.Strings(s)
}
