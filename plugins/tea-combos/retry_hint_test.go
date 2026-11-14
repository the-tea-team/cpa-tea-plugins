package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestParseRetryHint(t *testing.T) {
	past := time.Now().Add(-2 * time.Hour).UTC().Format(http.TimeFormat)
	future := time.Now().Add(90 * time.Minute).UTC().Format(http.TimeFormat)

	cases := []struct {
		name    string
		headers map[string][]string
		message string
		want    *time.Duration
	}{
		{"none", nil, "server overloaded", nil},
		{"header seconds", map[string][]string{"Retry-After": {"120"}}, "", durPtr(2 * time.Minute)},
		{"header lowercase key", map[string][]string{"retry-after": {"45"}}, "", durPtr(45 * time.Second)},
		{"header zero", map[string][]string{"Retry-After": {"0"}}, "", durPtr(0)},
		{"header date future", map[string][]string{"Retry-After": {future}}, "", nil}, // range-checked below
		{"header date past", map[string][]string{"Retry-After": {past}}, "", durPtr(0)},
		{"prose retry-after", nil, `{"error":{"code":"insufficient_quota","retry-after": 3600}}`, durPtr(time.Hour)},
		{"prose try again in hours", nil, "Rate limit reached. Try again in 4.5 hours.", durPtr(4*time.Hour + 30*time.Minute)},
		{"prose try again in minutes", nil, "quota window, try again in 20 minutes", durPtr(20 * time.Minute)},
		{"header beats prose", map[string][]string{"Retry-After": {"10"}}, "try again in 3 days", durPtr(10 * time.Second)},
		{"unrelated numeric headers ignored", map[string][]string{"Content-Length": {"42"}, "X-RateLimit-Limit": {"3000"}}, "server overloaded", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRetryHint(tc.headers, tc.message)
			if tc.name == "header date future" {
				if got == nil || *got < 80*time.Minute || *got > 100*time.Minute {
					t.Fatalf("future date hint = %v, want ~90m", got)
				}
				return
			}
			if tc.want == nil && got != nil {
				t.Fatalf("hint = %v, want nil", *got)
			}
			if tc.want != nil && (got == nil || *got != *tc.want) {
				t.Fatalf("hint = %v, want %v", got, *tc.want)
			}
		})
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestCoolDurationHonoursAndClampsHints(t *testing.T) {
	plugin := newTestPlugin(t)
	combo := mustCombo(t, "fleet", StrategyFailover, "m")

	if got := plugin.coolDuration(combo, nil, "plain 502"); got != 30*time.Second {
		t.Fatalf("no hint = %v, want the 30s default", got)
	}
	if got := plugin.coolDuration(combo, map[string][]string{"Retry-After": {"120"}}, ""); got != 2*time.Minute {
		t.Fatalf("120s hint = %v, want 2m", got)
	}
	if got := plugin.coolDuration(combo, map[string][]string{"Retry-After": {"604800"}}, "weekly window"); got != 15*time.Minute {
		t.Fatalf("weekly hint = %v, want clamped to maxCooldown 15m", got)
	}
	if got := plugin.coolDuration(combo, map[string][]string{"Retry-After": {"0"}}, ""); got != 0 {
		t.Fatalf("zero hint = %v, want no cooling", got)
	}
}

func TestExecuteShortRetryAfterParksBrieflyNotForDefaultWindow(t *testing.T) {
	plugin := newTestPlugin(t)
	now := time.Now()
	plugin.cooldown.now = func() time.Time { return now }
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "throttled", "steady")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "throttled" {
			return &pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusTooManyRequests,
				Headers:    map[string][]string{"Retry-After": {"1"}},
				Body:       []byte(`{"error":{"message":"slow down"}}`),
			}, nil
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}
	for range 2 {
		if _, err := plugin.Execute(context.Background(), request); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	// Inside the 1s hint window the member is still parked...
	if strings.Join(attempts, ",") != "throttled,steady,steady" {
		t.Fatalf("attempts = %v, want throttled parked inside its 1s hint window", attempts)
	}
	now = now.Add(2 * time.Second)
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("Execute after hint: %v", err)
	}
	// ...and it returns after 2s, not after the 30s default: the hint set the window.
	if strings.Join(attempts, ",") != "throttled,steady,steady,throttled,steady" {
		t.Fatalf("attempts = %v, want throttled selectable again 2s after a 1s hint", attempts)
	}
}

func TestExecuteLongHintParksMember(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "cooling-lane", "steady")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "cooling-lane" {
			return &pluginapi.HostModelExecutionResponse{
				StatusCode: http.StatusTooManyRequests,
				Headers:    map[string][]string{"Retry-After": {"600"}},
				Body:       []byte(`{"error":{"message":"quota window"}}`),
			}, nil
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}
	for range 2 {
		if _, err := plugin.Execute(context.Background(), request); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if strings.Join(attempts, ",") != "cooling-lane,steady,steady" {
		t.Fatalf("attempts = %v, want the 600s hint to park cooling-lane for round two", attempts)
	}
}
