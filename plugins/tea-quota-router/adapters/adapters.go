// Package adapters contains pure parsers for upstream quota/billing payloads.
//
// Every function takes a raw response body already fetched by the core
// collector and returns only normalized quota domain values. Adapters perform
// no network, file or environment access, never copy account IDs, labels,
// emails or credential material out of a payload, and never substitute zero
// for a null, missing or invalid measurement: an unknown value stays unknown
// (nil UsedFraction, zero ResetAt, absent Financial entry).
//
// See README.md in this directory for the checked source schema of each
// upstream and the documented parser limitations.
package adapters

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// UsagePage is one page of an organization usage report, summed over all
// buckets/results on that page. The sum retains the report's scope (for
// example an OpenAI organization or an Anthropic organization); it is not a
// per-key value and must not be billed as one. Requests is 0 when the
// upstream report carries no request count (Anthropic legacy usage report).
type UsagePage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	Requests        int64
	NextPage        string
	HasMore         bool
}

// CostPage is one page of an organization cost report summed in USD.
// Anthropic reports amounts as decimal strings in cents; OpenAI reports
// amount.value in whole currency units. Both are normalized to USD here.
type CostPage struct {
	AmountUSD float64
	NextPage  string
	HasMore   bool
}

const (
	sourceClaudeUsage    = "claude-oauth-usage"
	sourceCodexUsage     = "codex-wham-usage"
	sourceOpenRouterKey  = "openrouter-key"
	sourceOpenRouterCred = "openrouter-credits"
)

// ---- low-level JSON helpers -------------------------------------------------
// Payloads are decoded into raw maps so that null, missing and wrongly-typed
// fields remain distinguishable. Numbers may arrive as JSON numbers or as
// numeric strings (both shapes appear in the checked upstream sources).

func decodeObject(body []byte, what string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("adapters: %s: empty body", what)
	}
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("adapters: %s: invalid JSON", what)
	}
	if m == nil {
		return nil, fmt.Errorf("adapters: %s: payload is not an object", what)
	}
	if dec.More() {
		return nil, fmt.Errorf("adapters: %s: trailing data after JSON object", what)
	}
	return m, nil
}

// field returns the raw value for key. present=false means the key is absent
// or explicitly null; adapters treat both as "unknown".
func field(m map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || string(raw) == "null" {
			continue
		}
		return raw, true
	}
	return nil, false
}

func subObject(m map[string]json.RawMessage, keys ...string) (map[string]json.RawMessage, bool) {
	raw, ok := field(m, keys...)
	if !ok {
		return nil, false
	}
	var sub map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sub); err != nil || sub == nil {
		return nil, false
	}
	return sub, true
}

// optNumber parses a JSON number or numeric string. ok=false means absent or
// null (unknown); invalid=true means present but not a finite number.
func optNumber(raw json.RawMessage) (v float64, invalid bool) {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 {
		return 0, true
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, true
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(str), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, true
		}
		return f, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, true
	}
	return f, false
}

func numberField(m map[string]json.RawMessage, keys ...string) (float64, bool, bool) {
	raw, ok := field(m, keys...)
	if !ok {
		return 0, false, false
	}
	v, invalid := optNumber(raw)
	return v, true, invalid
}

func stringField(m map[string]json.RawMessage, keys ...string) (string, bool) {
	raw, ok := field(m, keys...)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func boolField(m map[string]json.RawMessage, keys ...string) (bool, bool) {
	raw, ok := field(m, keys...)
	if !ok {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	return b, true
}

// percentFraction converts a nonnegative upstream percentage to a 0..1
// fraction, clamping over 100. Invalid and negative values stay unknown.
func percentFraction(m map[string]json.RawMessage, keys ...string) *float64 {
	v, present, invalid := numberField(m, keys...)
	if !present || invalid || v < 0 {
		return nil
	}
	f := math.Min(v, 100) / 100
	return &f
}

// parseTimeValue accepts RFC 3339 strings and Unix epoch numbers (seconds,
// or milliseconds when the magnitude requires it). Anything else is unknown.
func parseTimeValue(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return time.Time{}
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return time.Time{}
		}
		str = strings.TrimSpace(str)
		if str == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, str); err == nil {
			return t.UTC()
		}
		// Fall through: some sources emit an epoch inside a string.
		if f, err := strconv.ParseFloat(str, 64); err == nil {
			return epochTime(f)
		}
		return time.Time{}
	}
	if f, invalid := optNumber(raw); !invalid {
		return epochTime(f)
	}
	return time.Time{}
}

func epochTime(f float64) time.Time {
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return time.Time{}
	}
	if f > 1e12 { // milliseconds
		return time.UnixMilli(int64(f)).UTC()
	}
	return time.Unix(int64(f), 0).UTC()
}

func timeField(m map[string]json.RawMessage, keys ...string) time.Time {
	raw, ok := field(m, keys...)
	if !ok {
		return time.Time{}
	}
	return parseTimeValue(raw)
}

// resetAt resolves an absolute reset timestamp, falling back to a relative
// reset_after_seconds measured from observedAt.
func resetAt(m map[string]json.RawMessage, observedAt time.Time) time.Time {
	if t := timeField(m, "resets_at", "reset_at", "resetsAt", "resetAt"); !t.IsZero() {
		return t
	}
	if v, present, invalid := numberField(m, "reset_after_seconds", "resetAfterSeconds", "reset_after", "resetAfter"); present && !invalid && v >= 0 {
		return observedAt.Add(time.Duration(v * float64(time.Second)))
	}
	return time.Time{}
}

func sortWindows(ws []quota.Window) {
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].ID != ws[j].ID {
			return ws[i].ID < ws[j].ID
		}
		return strings.Join(ws[i].Models, ",") < strings.Join(ws[j].Models, ",")
	})
}

func dedupeWindows(ws []quota.Window) []quota.Window {
	seen := map[string]bool{}
	out := ws[:0]
	for _, w := range ws {
		key := w.ID + "\x00" + strings.Join(w.Models, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, w)
	}
	return out
}

func sortFinancial(fs []quota.Financial) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Kind != fs[j].Kind {
			return fs[i].Kind < fs[j].Kind
		}
		return fs[i].ScopeID < fs[j].ScopeID
	})
}

// dedupeFinancial keeps the first entry per (kind, scope, currency): the same
// report scope reported twice (for example an effective_budget mirroring a
// budgets entry) must never be double-counted downstream.
func dedupeFinancial(fs []quota.Financial) []quota.Financial {
	seen := map[string]bool{}
	out := fs[:0]
	for _, f := range fs {
		key := f.Kind + "\x00" + f.ScopeID + "\x00" + f.Currency
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-.")
}

var errNoData = errors.New("no recognizable fields")
