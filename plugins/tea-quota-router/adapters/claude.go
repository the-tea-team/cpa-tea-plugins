package adapters

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// Claude parses a Claude OAuth usage payload (GET /api/oauth/usage) together
// with an optional OAuth profile payload (GET /api/oauth/profile). Either
// body may be nil/empty; an invalid usage body is an error, while an
// unparseable profile simply leaves the plan unknown. Identity fields from
// the profile (uuid, email, names) are never copied into the result.
//
// Plan detection: only an explicit organization.rate_limit_tier is trusted
// (PlanSource "reported"). default_claude_max_5x -> plan "max5" with a 5x
// plan-relative capacity estimate; default_claude_max_20x -> "max20" with a
// 5-hour estimate only (the weekly multiple is not a documented comparable
// weight). A generic Max tier keeps plan "max" with unknown capacity, and a
// bare has_claude_max flag likewise. See README.md.
func Claude(usage, profile []byte, observedAt time.Time) (quota.Account, error) {
	acct := quota.Account{
		Provider:      "claude",
		BillingType:   "subscription",
		ObservedAt:    observedAt,
		RefreshStatus: "ok",
	}
	if len(strings.TrimSpace(string(usage))) == 0 {
		return quota.Account{}, fmt.Errorf("adapters: claude usage: empty body")
	}
	root, err := decodeObject(usage, "claude usage")
	if err != nil {
		return quota.Account{}, err
	}
	if _, isErr := field(root, "error"); isErr {
		return quota.Account{}, fmt.Errorf("adapters: claude usage: upstream error envelope")
	}

	plan, capacities := claudePlan(profile)
	acct.Plan = plan.name
	if plan.name != "" {
		acct.PlanSource = "reported"
	}

	windows := claudeTopLevelWindows(root, observedAt, capacities)
	windows = append(windows, claudeLimitWindows(root, windows, observedAt)...)
	sortWindows(windows)
	acct.Windows = dedupeWindows(windows)

	if eu, ok := subObject(root, "extra_usage"); ok {
		if fin, ok := claudeExtraUsage(eu, observedAt); ok {
			acct.Financial = append(acct.Financial, fin)
		}
	}
	return acct, nil
}

// claudePlanName maps an explicit supported tier string to a normalized plan
// and per-window plan-relative capacity estimates. Capacities are estimates
// (CapacitySource "plan_estimate" is applied by the caller), never reported
// limits. The map keys are global window IDs.
type claudePlanInfo struct {
	name string
}

func claudePlan(profile []byte) (claudePlanInfo, map[string]float64) {
	if len(strings.TrimSpace(string(profile))) == 0 {
		return claudePlanInfo{}, nil
	}
	root, err := decodeObject(profile, "claude profile")
	if err != nil {
		return claudePlanInfo{}, nil
	}
	org, _ := subObject(root, "organization")
	account, _ := subObject(root, "account")

	if org != nil {
		if tier, ok := stringField(org, "rate_limit_tier", "rateLimitTier"); ok && tier != "" {
			switch normalizedTier(tier) {
			case "default_claude_max_5x", "claude_max_5x", "max_5x", "max5":
				return claudePlanInfo{"max5"}, map[string]float64{"five-hour": 5, "seven-day": 5}
			case "default_claude_max_20x", "claude_max_20x", "max_20x", "max20":
				// Weekly capacity for Max 20x is deliberately not guessed.
				return claudePlanInfo{"max20"}, map[string]float64{"five-hour": 20}
			case "default_claude_pro", "claude_pro", "pro":
				return claudePlanInfo{"pro"}, map[string]float64{"five-hour": 1, "seven-day": 1}
			}
			// Any other explicit tier that still says "max" is a generic
			// Max: known family, unknown multiple.
			if strings.Contains(normalizedTier(tier), "max") {
				return claudePlanInfo{"max"}, nil
			}
			if strings.Contains(normalizedTier(tier), "pro") {
				return claudePlanInfo{"pro"}, map[string]float64{"five-hour": 1, "seven-day": 1}
			}
			// Unrecognized explicit tier: keep the raw family unknown rather
			// than inventing a plan from a boolean flag.
			return claudePlanInfo{}, nil
		}
	}
	// Flag-only fallback (no explicit tier string).
	if account != nil {
		if v, ok := boolField(account, "has_claude_max"); ok && v {
			return claudePlanInfo{"max"}, nil
		}
		if v, ok := boolField(account, "has_claude_pro"); ok && v {
			return claudePlanInfo{"pro"}, map[string]float64{"five-hour": 1, "seven-day": 1}
		}
	}
	if org != nil {
		if orgType, ok := stringField(org, "organization_type"); ok {
			switch strings.ToLower(orgType) {
			case "claude_max":
				return claudePlanInfo{"max"}, nil
			case "claude_pro":
				return claudePlanInfo{"pro"}, map[string]float64{"five-hour": 1, "seven-day": 1}
			}
		}
	}
	return claudePlanInfo{}, nil
}

func normalizedTier(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)
	return s
}

var claudeTopLevel = []struct {
	key      string
	id       string
	duration int64
	models   []string
}{
	{"five_hour", "five-hour", 5 * 3600, nil},
	{"seven_day", "seven-day", 7 * 24 * 3600, nil},
	{"seven_day_oauth_apps", "seven-day-oauth-apps", 7 * 24 * 3600, []string{UnmatchedScopePrefix + "oauth-apps"}},
	{"seven_day_opus", "seven-day-opus", 7 * 24 * 3600, []string{"claude-opus*"}},
	{"seven_day_sonnet", "seven-day-sonnet", 7 * 24 * 3600, []string{"claude-sonnet*"}},
	{"seven_day_cowork", "seven-day-cowork", 7 * 24 * 3600, []string{UnmatchedScopePrefix + "cowork"}},
	{"iguana_necktie", "seven-day-fable", 7 * 24 * 3600, []string{"claude-fable*"}},
}

func claudeTopLevelWindows(root map[string]json.RawMessage, observedAt time.Time, capacities map[string]float64) []quota.Window {
	var out []quota.Window
	for _, spec := range claudeTopLevel {
		winObj, ok := subObject(root, spec.key)
		if !ok {
			continue
		}
		w := quota.Window{
			ID:              spec.id,
			DurationSeconds: spec.duration,
			UsedFraction:    percentFraction(winObj, "utilization", "percent"),
			ResetAt:         resetAt(winObj, observedAt),
			ObservedAt:      observedAt,
			Models:          append([]string(nil), spec.models...),
		}
		if len(spec.models) == 0 {
			if c, ok := capacities[spec.id]; ok {
				w.Capacity = floatPtr(c)
				w.CapacitySource = "plan_estimate"
			}
		}
		out = append(out, w)
	}
	return out
}

// claudeLimitWindows parses the newer `limits` array: base session/weekly
// entries fill windows the legacy keys did not already provide, and
// weekly-scoped entries become model-scoped windows. A limit whose fields are
// all unknown contributes nothing.
func claudeLimitWindows(root map[string]json.RawMessage, existing []quota.Window, observedAt time.Time) []quota.Window {
	raw, ok := field(root, "limits")
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	have := map[string]bool{}
	for _, w := range existing {
		have[w.ID+"\x00"+strings.Join(w.Models, "\x00")] = true
	}
	var out []quota.Window
	for _, item := range items {
		var lim map[string]json.RawMessage
		if err := json.Unmarshal(item, &lim); err != nil || lim == nil {
			continue
		}
		if v, ok := boolField(lim, "is_active", "isActive"); ok && !v {
			continue
		}
		kind := normalizedTier(firstString(lim, "kind"))
		group := normalizedTier(firstString(lim, "group"))
		frac := percentFraction(lim, "percent", "utilization")
		reset := resetAt(lim, observedAt)
		if frac == nil && reset.IsZero() {
			continue
		}
		scoped := kind == "weekly_scoped" || kind == "weekly_model_scoped" ||
			(kind == "model_scoped" && group == "weekly")
		if scoped {
			models, label := claudeScopeModels(lim)
			if len(models) == 0 && label == "" {
				continue
			}
			id := "seven-day-scoped"
			if ls := slug(label); ls != "" {
				id += "-" + ls
			}
			if len(models) == 0 {
				// Label-only scope: an empty model list means "global" to the
				// scheduler and would exhaust every model. Use an opaque
				// sentinel that matches no real model ID instead.
				s := slug(label)
				if s == "" {
					s = "unlabeled"
				}
				models = []string{UnmatchedScopePrefix + s}
			}
			key := id + "\x00" + strings.Join(models, "\x00")
			if have[key] {
				continue
			}
			have[key] = true
			out = append(out, quota.Window{
				ID: id, DurationSeconds: 7 * 24 * 3600, UsedFraction: frac,
				ResetAt: reset, ObservedAt: observedAt, Models: models,
			})
			continue
		}
		// A declared scope, app, or surface (including null or unfamiliar
		// variants) is not evidence of a global limit.
		declaredScope := false
		for key := range lim {
			key = strings.ToLower(key)
			if strings.HasPrefix(key, "scope") || strings.HasPrefix(key, "app") || strings.HasPrefix(key, "surface") {
				declaredScope = true
				break
			}
		}
		if declaredScope {
			continue
		}
		var id string
		var duration int64
		switch {
		case kind == "session" && (group == "" || group == "session"):
			id, duration = "five-hour", 5*3600
		case (kind == "weekly" || kind == "weekly_all") && (group == "" || group == "weekly" || group == "weekly_all"):
			id, duration = "seven-day", 7*24*3600
		default:
			continue
		}
		key := id + "\x00"
		if have[key] {
			continue
		}
		have[key] = true
		out = append(out, quota.Window{
			ID: id, DurationSeconds: duration, UsedFraction: frac,
			ResetAt: reset, ObservedAt: observedAt,
		})
	}
	return out
}

func claudeScopeModels(lim map[string]json.RawMessage) (models []string, label string) {
	scope, ok := subObject(lim, "scope")
	if !ok {
		return nil, ""
	}
	modelObj, ok := subObject(scope, "model")
	if !ok {
		return nil, ""
	}
	if raw, ok := field(modelObj, "id", "model_id", "modelId"); ok {
		var id string
		if json.Unmarshal(raw, &id) == nil && id != "" {
			if strings.ContainsAny(id, "?*[]") || strings.IndexFunc(id, unicode.IsSpace) >= 0 {
				models = []string{UnmatchedScopePrefix + "invalid-model"}
			} else {
				models = []string{id}
			}
		}
	}
	label, _ = stringField(modelObj, "display_name", "displayName", "name")
	return models, label
}

// UnmatchedScopePrefix starts the opaque sentinel model pattern given to a
// scoped limit whose upstream scope is app-only or has only a display label
// and no model ID.
// No model ID begins with it, so the window matches nothing; core must not
// treat the limit as global (an empty Models list) or it would wrongly
// exhaust all models.
const UnmatchedScopePrefix = "scope-unmatched/"

// IsUnmatchedScope reports whether a window uses an opaque unmatched scope
// sentinel produced by this package.
func IsUnmatchedScope(models []string) bool {
	return len(models) == 1 && strings.HasPrefix(models[0], UnmatchedScopePrefix)
}

func firstString(m map[string]json.RawMessage, keys ...string) string {
	s, _ := stringField(m, keys...)
	return s
}

func floatPtr(f float64) *float64 { return &f }

// claudeExtraUsage converts enabled extra-usage credit figures (reported in
// cents by the OAuth usage payload) into an upstream-spend financial entry.
// The figures are usage against an optional monthly credit, not a prepaid
// balance, so remaining credit is never fabricated here.
func claudeExtraUsage(eu map[string]json.RawMessage, observedAt time.Time) (quota.Financial, bool) {
	if v, ok := boolField(eu, "is_enabled"); ok && !v {
		return quota.Financial{}, false
	}
	used, present, invalid := numberField(eu, "used_credits")
	if !present || invalid || used < 0 || math.IsNaN(used) {
		return quota.Financial{}, false
	}
	return quota.Financial{
		Kind: "upstream-spend", ScopeID: "claude-extra-usage", Currency: "USD",
		Source: sourceClaudeUsage, Amount: used / 100, ObservedAt: observedAt,
	}, true
}

// parseCentsString is retained for Anthropic cost reports; see reports.go.
func parseCentsString(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f / 100, true
}
