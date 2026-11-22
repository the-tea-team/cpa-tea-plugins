package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// typedSnap builds one account+credential per id with the given provider and
// auth type ("" = unidentified).
type typedAcct struct {
	acct
	authType string
}

func typedSnap(provider string, m map[string]typedAcct) quota.Snapshot {
	s := quota.Snapshot{SchemaVersion: 1}
	for id, a := range m {
		w := quota.Window{ID: "session", UsedFraction: a.used, ResetAt: a.reset, ObservedAt: a.obs}
		s.Accounts = append(s.Accounts, quota.Account{ID: id, Provider: provider, Windows: []quota.Window{w}})
		s.Credentials = append(s.Credentials, quota.Credential{ID: id, AccountID: id, Provider: provider, AuthType: a.authType})
	}
	return s
}

func provCand(provider, id string, prio int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Priority: prio, Provider: provider, Status: "active"}
}

func provReq(provider, model string, cs ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: provider, Providers: []string{provider}, Model: model, Candidates: cs}
}

func exhausted() acct { return acct{used: f(1), reset: affNow.Add(time.Hour), obs: affNow} }

func seenOver(n int, pick func() pluginapi.SchedulerPickResponse) map[string]int {
	seen := map[string]int{}
	for i := 0; i < n; i++ {
		seen[pick().AuthID]++
	}
	return seen
}

func TestAcrossPrioritiesScoresEveryTier(t *testing.T) {
	snap := typedSnap("mock", map[string]typedAcct{"a": {fresh(0.1), ""}, "b": {fresh(0.1), ""}, "c": {fresh(0.1), ""}})
	req := provReq("mock", "m", provCand("mock", "a", 5), provCand("mock", "b", 1), provCand("mock", "c", 0))

	s := newScheduler()
	got := seenOver(60, func() pluginapi.SchedulerPickResponse {
		return s.pickWith(req, snap, "active", qar, affNow, time.Hour, true)
	})
	if got["a"] == 0 || got["b"] == 0 || got["c"] == 0 {
		t.Fatalf("across priorities must rotate all tiers: %v", got)
	}

	s = newScheduler()
	got = seenOver(60, func() pluginapi.SchedulerPickResponse {
		return s.pickWith(req, snap, "active", qar, affNow, time.Hour, false)
	})
	if len(got) != 1 || got["a"] != 60 {
		t.Fatalf("max-tier mode must stay on top tier: %v", got)
	}

	s = newScheduler()
	got = seenOver(60, func() pluginapi.SchedulerPickResponse {
		return s.pick(req, snap, "active", qar, affNow, time.Hour)
	})
	if len(got) != 1 || got["a"] != 60 {
		t.Fatalf("pick wrapper must keep max-tier signature/behavior: %v", got)
	}
}

func TestAcrossPrioritiesFallsToLowerTierWhenTopExhausted(t *testing.T) {
	snap := typedSnap("mock", map[string]typedAcct{"top": {exhausted(), ""}, "low": {fresh(0.1), ""}})
	req := provReq("mock", "m", provCand("mock", "top", 9), provCand("mock", "low", 1))

	if got := newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, true); !got.Handled || got.AuthID != "low" {
		t.Fatalf("across: want low tier selected, got %+v", got)
	}
	if got := newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false); got.Handled || got.Reject {
		t.Fatalf("max-tier compat: top exhausted must delegate, got %+v", got)
	}
}

func TestAcrossPrioritiesAllExhaustedRejects(t *testing.T) {
	snap := typedSnap("mock", map[string]typedAcct{"top": {exhausted(), ""}, "low": {exhausted(), ""}})
	req := provReq("mock", "m", provCand("mock", "top", 9), provCand("mock", "low", 1))
	if got := newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, true); !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" {
		t.Fatalf("all exhausted across tiers must reject: %+v", got)
	}
}

func TestAcrossPrioritiesKeepsDisabledOut(t *testing.T) {
	snap := typedSnap("mock", map[string]typedAcct{"a": {fresh(0.1), ""}, "b": {fresh(0.1), ""}})
	off := provCand("mock", "b", 1)
	off.Status = "disabled"
	req := provReq("mock", "m", provCand("mock", "a", 5), off)
	s := newScheduler()
	got := seenOver(30, func() pluginapi.SchedulerPickResponse {
		return s.pickWith(req, snap, "active", qar, affNow, time.Hour, true)
	})
	if len(got) != 1 || got["a"] != 30 {
		t.Fatalf("disabled candidate selected: %v", got)
	}
}

func TestSubscriptionGuardScoresOnlyOAuth(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		for _, across := range []bool{false, true} {
			t.Run(provider, func(t *testing.T) {
				snap := typedSnap(provider, map[string]typedAcct{
					"o1":  {fresh(0.1), "oauth"},
					"o2":  {fresh(0.1), "oauth"},
					"key": {fresh(0.0), "api_key"},
					"unk": {fresh(0.0), ""},
				})
				// API and unidentified sit in the TOP tier: the guard must apply
				// before max-tier selection, not after.
				req := provReq(provider, "m", provCand(provider, "o1", 1), provCand(provider, "o2", 1), provCand(provider, "key", 9), provCand(provider, "unk", 9))
				s := newScheduler()
				got := seenOver(60, func() pluginapi.SchedulerPickResponse {
					return s.pickWith(req, snap, "active", qar, affNow, time.Hour, across)
				})
				if got["key"] != 0 || got["unk"] != 0 || got[""] != 0 {
					t.Fatalf("across=%v non-OAuth scored/selected: %v", across, got)
				}
				if got["o1"] == 0 || got["o2"] == 0 {
					t.Fatalf("across=%v both OAuth accounts must rotate: %v", across, got)
				}
				for id, sc := range s.history()[len(s.history())-1].Scores {
					if id == "key" || id == "unk" {
						t.Fatalf("non-OAuth candidate %q was scored", id)
					}
					_ = sc
				}
			})
		}
	}
}

func TestSubscriptionGuardRejectsWhenOAuthExhaustedEvenWithPaidLeft(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			snap := typedSnap(provider, map[string]typedAcct{
				"o1":  {exhausted(), "oauth"},
				"o2":  {exhausted(), "oauth"},
				"key": {fresh(0.0), "api_key"},
				"unk": {fresh(0.0), ""},
			})
			req := provReq(provider, "m", provCand(provider, "o1", 1), provCand(provider, "o2", 1), provCand(provider, "key", 9), provCand(provider, "unk", 9))
			for _, across := range []bool{false, true} {
				if got := newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, across); !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" || got.AuthID != "" {
					t.Fatalf("across=%v OAuth exhausted must reject despite paid candidates: %+v", across, got)
				}
				if got := newScheduler().pickWith(req, snap, "shadow", qar, affNow, time.Hour, across); got.Handled || got.Reject {
					t.Fatalf("shadow must not alter production: %+v", got)
				}
			}
		})
	}
}

func TestSubscriptionGuardAPIOnlyInheritsCore(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			// Even a known-exhausted API credential must not be rejected: it is
			// not a subscription route, so core keeps ownership.
			snap := typedSnap(provider, map[string]typedAcct{"key1": {exhausted(), "api_key"}, "key2": {fresh(0.1), "api_key"}, "unk": {exhausted(), ""}})
			req := provReq(provider, "m", provCand(provider, "key1", 1), provCand(provider, "key2", 2), provCand(provider, "unk", 3))
			for _, across := range []bool{false, true} {
				s := newScheduler()
				if got := s.pickWith(req, snap, "active", qar, affNow, time.Hour, across); got.Handled || got.Reject || got.AuthID != "" {
					t.Fatalf("across=%v API-only route must inherit core: %+v", across, got)
				}
				if h := s.history(); len(h) != 1 || h[0].SelectedID != "" {
					t.Fatalf("API-only route must not record a selection: %+v", h)
				}
			}
		})
	}
}

func TestSubscriptionGuardDerivesProviderFromCandidates(t *testing.T) {
	snap := typedSnap("claude", map[string]typedAcct{"o": {fresh(0.1), "oauth"}, "key": {fresh(0.0), "api_key"}})
	req := provReq("", "m", provCand("claude", "o", 1), provCand("claude", "key", 1))
	req.Providers = nil
	s := newScheduler()
	got := seenOver(30, func() pluginapi.SchedulerPickResponse {
		return s.pickWith(req, snap, "active", qar, affNow, time.Hour, true)
	})
	if got["o"] != 30 {
		t.Fatalf("empty request provider must still guard via candidates: %v", got)
	}
}

func TestSubscriptionGuardLeavesOtherProvidersAlone(t *testing.T) {
	snap := typedSnap("mock", map[string]typedAcct{"o": {fresh(0.1), "oauth"}, "key": {fresh(0.1), "api_key"}, "unk": {fresh(0.1), ""}})
	req := provReq("mock", "m", provCand("mock", "o", 1), provCand("mock", "key", 1), provCand("mock", "unk", 1))
	s := newScheduler()
	got := seenOver(60, func() pluginapi.SchedulerPickResponse {
		return s.pickWith(req, snap, "active", qar, affNow, time.Hour, false)
	})
	if got["o"] == 0 || got["key"] == 0 || got["unk"] == 0 {
		t.Fatalf("non claude/codex providers must score every credential: %v", got)
	}
}

func TestSchedulerPickHonoursAcrossFlagFromConfig(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		wantBoth    bool
	}{
		{"flag false", "", false},
		{"flag true", "quotaAcrossPriorities: true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer shutdown()
			cfg := []byte("dataDir: " + t.TempDir() + "\nschedulerMode: active\nproviderStrategies: {mock: quota-aware-round-robin}\n" + tc.extra)
			in, _ := json.Marshal(map[string]any{"config_yaml": cfg})
			if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, in); err != nil {
				t.Fatal(err)
			}
			req := provReq("mock", "m", provCand("mock", "hi", 5), provCand("mock", "lo", 1))
			raw, _ := json.Marshal(req)
			seen := map[string]int{}
			for i := 0; i < 40; i++ {
				out, err := abiHandleMethod(pluginabi.MethodSchedulerPick, raw)
				if err != nil {
					t.Fatal(err)
				}
				var env struct {
					Result pluginapi.SchedulerPickResponse `json:"result"`
				}
				if err := json.Unmarshal(out, &env); err != nil {
					t.Fatal(err)
				}
				seen[env.Result.AuthID]++
			}
			if tc.wantBoth && (seen["hi"] == 0 || seen["lo"] == 0) {
				t.Fatalf("flag true must rotate across tiers: %v", seen)
			}
			if !tc.wantBoth && (seen["hi"] != 40) {
				t.Fatalf("flag false must stay on top tier: %v", seen)
			}
		})
	}
}

func TestSchedulerResetBoostShiftsShare(t *testing.T) {
	now := time.Now()
	win := func(sessUsed, weekUsed float64, weekReset time.Duration) []quota.Window {
		c := 5.0
		su, wu := sessUsed, weekUsed
		return []quota.Window{
			{ID: "five_hour", DurationSeconds: 18000, UsedFraction: &su, ResetAt: now.Add(3 * time.Hour), ObservedAt: now, Capacity: &c, CapacitySource: "reported"},
			{ID: "seven_day", DurationSeconds: 604800, UsedFraction: &wu, ResetAt: now.Add(weekReset), ObservedAt: now, Capacity: &c, CapacitySource: "reported"},
		}
	}
	snap := quota.Snapshot{SchemaVersion: 1,
		Accounts: []quota.Account{
			{ID: "near", Provider: "mock", Windows: win(0.16, 0.84, 2*time.Hour)},
			{ID: "far", Provider: "mock", Windows: win(0.06, 0.29, 83*time.Hour)},
		},
		Credentials: []quota.Credential{{ID: "near", AccountID: "near", Provider: "mock"}, {ID: "far", AccountID: "far", Provider: "mock"}},
	}
	req := pluginapi.SchedulerPickRequest{Provider: "mock", Model: "m", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "near"}, {ID: "far"}}}
	count := func(opts quota.ScoreOptions) int {
		s := newScheduler()
		n := 0
		for i := 0; i < 100; i++ {
			if got := s.pickWithOptions(req, snap, "active", "quota-aware-round-robin", now, time.Hour, true, opts); got.AuthID == "near" {
				n++
			}
		}
		return n
	}
	plain := count(quota.ScoreOptions{})
	boosted := count(quota.ScoreOptions{ResetBoost: quota.ResetBoost{HorizonHours: 24, MaxMultiplier: 6}})
	if boosted < 80 || boosted <= plain {
		t.Fatalf("near-reset picks out of 100: plain=%d boosted=%d", plain, boosted)
	}
	t.Logf("near-reset picks out of 100: plain=%d boosted=%d", plain, boosted)
}
