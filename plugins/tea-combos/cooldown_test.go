package main

import (
	"testing"
	"time"
)

func TestCooldownTrackerCoolExpireAndClear(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newCooldownTracker()
	tracker.now = func() time.Time { return now }

	member := Member{Model: "bad-model"}
	other := Member{Model: "good-model"}

	tracker.Cool("combo", member, 30*time.Second)
	if !tracker.Active("combo", member) {
		t.Fatal("member should be cooling right after Cool")
	}
	if tracker.Active("combo", other) {
		t.Fatal("an untouched member must not be cooling")
	}
	if tracker.Active("other-combo", member) {
		t.Fatal("cooling is per combo; another combo must be unaffected")
	}

	now = now.Add(29 * time.Second)
	if !tracker.Active("combo", member) {
		t.Fatal("member should still cool at 29s of a 30s window")
	}
	now = now.Add(2 * time.Second)
	if tracker.Active("combo", member) {
		t.Fatal("member must recover once the window passes")
	}

	tracker.Cool("combo", member, 30*time.Second)
	tracker.Clear("combo", member)
	if tracker.Active("combo", member) {
		t.Fatal("Clear must lift cooling immediately")
	}
}

func TestCooldownTrackerDisabledDurationIsNoOp(t *testing.T) {
	tracker := newCooldownTracker()
	tracker.Cool("combo", Member{Model: "m"}, 0)
	if tracker.Active("combo", Member{Model: "m"}) {
		t.Fatal("a zero duration must not cool anything")
	}
}

func TestCooldownTrackerNilSafe(t *testing.T) {
	var tracker *cooldownTracker
	tracker.Cool("c", Member{Model: "m"}, time.Minute)
	tracker.Clear("c", Member{Model: "m"})
	if tracker.Active("c", Member{Model: "m"}) {
		t.Fatal("nil tracker is never cooling")
	}
	if got := tracker.Cooling("c", []Member{{Model: "m"}}); got != nil {
		t.Fatalf("nil tracker cooling snapshot = %v, want nil", got)
	}
}

func TestMemberFailureClassification(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		want    bool
	}{
		{"429 rate limit", 429, "rate limit", true},
		{"503 capacity", 503, "overloaded", true},
		{"408 timeout", 408, "timeout", true},
		{"403 openai quota", 403, `{"error":{"type":"permission_error","code":"insufficient_quota"}}`, true},
		{"403 anthropic credit", 403, "credit balance is too low", true},
		{"403 google exhausted", 403, "RESOURCE_EXHAUSTED", true},
		{"401 invalid key", 401, "invalid api key provided", true},
		{"403 unrelated stays fatal", 403, "model is not available for this account", false},
		{"400 bad payload stays fatal", 400, "max_output_tokens must be >= 16", false},
		{"400 unknown member model falls through", 400, "unknown provider for model no-such-model-zz", true},
		{"400 model_not_found falls through", 400, `{"error":{"code":"model_not_found","param":"model"}}`, true},
		{"404 retired model falls through", 404, "The model `gpt-x` does not exist or you do not have access to it.", true},
		{"404 unknown route stays fatal", 404, "no handler for this path", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := memberFailure(tc.status, tc.message); got != tc.want {
				t.Fatalf("memberFailure(%d, %q) = %v, want %v", tc.status, tc.message, got, tc.want)
			}
		})
	}
}
