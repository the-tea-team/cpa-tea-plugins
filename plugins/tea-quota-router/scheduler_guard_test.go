package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// guardSnap builds a snapshot of credentials for provider. oauth credentials
// advertise models; api credentials are positively api_key.
type gcred struct {
	authType    string
	models      []string
	disabled    bool
	unavailable bool
}

func guardSnap(provider string, m map[string]gcred) quota.Snapshot {
	s := quota.Snapshot{SchemaVersion: 1, GeneratedAt: affNow.Add(-48 * time.Hour)}
	for id, g := range m {
		w := quota.Window{ID: "session", UsedFraction: f(0.1), ObservedAt: affNow.Add(-48 * time.Hour)}
		s.Accounts = append(s.Accounts, quota.Account{ID: id, Provider: provider, Windows: []quota.Window{w}})
		c := quota.Credential{ID: id, AccountID: id, Provider: provider, AuthType: g.authType, Models: g.models, Disabled: g.disabled, Unavailable: g.unavailable}
		if g.unavailable {
			c.NextRetryAt = affNow.Add(time.Hour)
		}
		s.Credentials = append(s.Credentials, c)
	}
	return s
}

func mustReject(t *testing.T, got pluginapi.SchedulerPickResponse, why string) {
	t.Helper()
	if !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" || got.AuthID != "" {
		t.Fatalf("%s: want Handled+Reject auth_unavailable, got %+v", why, got)
	}
}

func mustInherit(t *testing.T, got pluginapi.SchedulerPickResponse, why string) {
	t.Helper()
	if got.Handled || got.Reject || got.AuthID != "" {
		t.Fatalf("%s: want inherit core, got %+v", why, got)
	}
}

var guardProviders = []string{"claude", "codex"}

// The core already filtered the cooling / attempted OAuth credential out of
// the candidate list; only paid candidates remain. Must not delegate.
func TestGuardRejectsWhenOAuthFilteredByCoreAndPaidRemains(t *testing.T) {
	for _, provider := range guardProviders {
		for _, tc := range []struct {
			name string
			oa   gcred
		}{
			{"cooling", gcred{authType: "oauth", models: []string{"m"}, unavailable: true}},
			{"attempted", gcred{authType: "oauth", models: []string{"m"}}},
			{"glob alias", gcred{authType: "oauth", models: []string{"*"}}},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				snap := guardSnap(provider, map[string]gcred{"o": tc.oa, "key": {authType: "api_key", models: []string{"m"}}, "unk": {models: []string{"m"}}})
				req := provReq(provider, "m", provCand(provider, "key", 9), provCand(provider, "unk", 9))
				for _, across := range []bool{false, true} {
					s := newScheduler()
					mustReject(t, s.pickWith(req, snap, "active", qar, affNow, time.Hour, across), "OAuth filtered by core")
					if h := s.history(); len(h) != 1 || h[0].Reason != reasonRejected || h[0].SelectedID != "" {
						t.Fatalf("rejection must be recorded: %+v", h)
					}
					mustInherit(t, newScheduler().pickWith(req, snap, "shadow", qar, affNow, time.Hour, across), "shadow never alters production")
				}
			})
		}
	}
}

func TestGuardOAuthWithoutProviderOnRequestUsesCandidateProvider(t *testing.T) {
	snap := guardSnap("claude", map[string]gcred{"o": {authType: "oauth", models: []string{"m"}, unavailable: true}, "key": {authType: "api_key", models: []string{"m"}}})
	req := provReq("", "m", provCand("claude", "key", 1))
	req.Providers = nil
	mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, true), "derived provider")
}

func TestGuardIgnoresDisabledOtherProviderAndOtherModelOAuth(t *testing.T) {
	for _, provider := range guardProviders {
		other := "claude"
		if provider == "claude" {
			other = "codex"
		}
		t.Run(provider, func(t *testing.T) {
			paid := provReq(provider, "m", provCand(provider, "key", 1))
			key := map[string]gcred{"key": {authType: "api_key", models: []string{"m"}}}
			with := func(extra gcred) quota.Snapshot {
				snap := guardSnap(provider, key)
				o := guardSnap(provider, map[string]gcred{"o": extra})
				snap.Accounts = append(snap.Accounts, o.Accounts...)
				snap.Credentials = append(snap.Credentials, o.Credentials...)
				return snap
			}
			mustInherit(t, newScheduler().pickWith(paid, with(gcred{authType: "oauth", models: []string{"m"}, disabled: true}), "active", qar, affNow, time.Hour, false), "disabled OAuth is not an offer")
			mustInherit(t, newScheduler().pickWith(paid, with(gcred{authType: "oauth", models: []string{"other-model"}}), "active", qar, affNow, time.Hour, false), "OAuth for a different model")
			otherSnap := guardSnap(other, map[string]gcred{"o": {authType: "oauth", models: []string{"m"}}})
			otherSnap.Accounts = append(otherSnap.Accounts, guardSnap(provider, key).Accounts...)
			otherSnap.Credentials = append(otherSnap.Credentials, guardSnap(provider, key).Credentials...)
			mustInherit(t, newScheduler().pickWith(paid, otherSnap, "active", qar, affNow, time.Hour, false), "other provider's OAuth")
		})
	}
}

func TestGuardPinnedKnownAPICredentialInheritsCore(t *testing.T) {
	for _, provider := range guardProviders {
		for name, pin := range map[string]any{"string": " key ", "bytes": []byte("key")} {
			t.Run(provider+"/"+name, func(t *testing.T) {
				snap := guardSnap(provider, map[string]gcred{"o": {authType: "oauth", models: []string{"m"}, unavailable: true}, "key": {authType: "api_key", models: []string{"m"}}})
				req := provReq(provider, "m", provCand(provider, "key", 1))
				req.Options.Metadata = map[string]any{"pinned_auth_id": pin}
				mustInherit(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "explicit API pin")
				// Same request without the pin is rejected.
				req.Options.Metadata = nil
				mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "unpinned")
			})
		}
	}
}

func TestGuardPinnedNonAPIStillGuarded(t *testing.T) {
	for _, pin := range []string{"unk", "ghost"} {
		t.Run(pin, func(t *testing.T) {
			snap := guardSnap("claude", map[string]gcred{"o": {authType: "oauth", models: []string{"m"}, unavailable: true}, "unk": {models: []string{"m"}}, "key": {authType: "api_key", models: []string{"m"}}})
			req := provReq("claude", "m", provCand("claude", "unk", 1))
			req.Options.Metadata = map[string]any{"pinned_auth_id": pin}
			mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "pin to unknown/unlisted credential is not an API pin")
		})
	}
	// A pin naming an unrelated (non-string) value does not count as a pin.
	snap := guardSnap("claude", map[string]gcred{"o": {authType: "oauth", models: []string{"m"}}, "key": {authType: "api_key", models: []string{"m"}}})
	req := provReq("claude", "m", provCand("claude", "key", 1))
	req.Options.Metadata = map[string]any{"pinned_auth_id": 7}
	mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "malformed pin")
}

func TestGuardGenuineAPIOnlyModelInheritsCore(t *testing.T) {
	for _, provider := range guardProviders {
		t.Run(provider, func(t *testing.T) {
			snap := guardSnap(provider, map[string]gcred{
				"o":   {authType: "oauth", models: []string{"sub-model-*"}, unavailable: true},
				"key": {authType: "api_key", models: []string{"openai/gpt-5", "sub-model-1"}},
			})
			for _, model := range []string{"openai/gpt-5", "paid-only"} {
				req := provReq(provider, model, provCand(provider, "key", 1))
				for _, across := range []bool{false, true} {
					mustInherit(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, across), "API-only model "+model)
				}
			}
			// The overlapping model still rejects.
			req := provReq(provider, "sub-model-1", provCand(provider, "key", 1))
			mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "overlapping model")
		})
	}
}

func TestGuardMissingInventoryFailsClosedForUnpinnedMixedRoute(t *testing.T) {
	for _, provider := range guardProviders {
		t.Run(provider, func(t *testing.T) {
			// OAuth exists and is enabled but its model inventory is missing.
			snap := guardSnap(provider, map[string]gcred{"o": {authType: "oauth", unavailable: true}, "key": {authType: "api_key", models: []string{"m"}}, "unk": {}})
			req := provReq(provider, "m", provCand(provider, "key", 1), provCand(provider, "unk", 1))
			mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "unknown inventory, unpinned")

			pinned := req
			pinned.Options.Metadata = map[string]any{"pinned_auth_id": "key"}
			mustInherit(t, newScheduler().pickWith(pinned, snap, "active", qar, affNow, time.Hour, false), "pinned API despite missing inventory")

			// A disabled inventory-less OAuth is not an offer.
			snap = guardSnap(provider, map[string]gcred{"o": {authType: "oauth", disabled: true}, "key": {authType: "api_key", models: []string{"m"}}})
			mustInherit(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "disabled OAuth without inventory")

			// No OAuth known at all: genuine API provider setup inherits core.
			snap = guardSnap(provider, map[string]gcred{"key": {authType: "api_key"}})
			mustInherit(t, newScheduler().pickWith(req, snap, "active", qar, affNow, time.Hour, false), "no OAuth in snapshot")
		})
	}
}

// Stale snapshot: old quota observations, candidates unknown to the snapshot
// (core's final candidates raced a collector refresh) never open the paid path.
func TestGuardStaleSnapshotAndUnknownFinalCandidates(t *testing.T) {
	for _, provider := range guardProviders {
		t.Run(provider, func(t *testing.T) {
			snap := guardSnap(provider, map[string]gcred{"o": {authType: "oauth", models: []string{"m"}}})
			// Core's final candidates are paid ids the snapshot has never seen.
			req := provReq(provider, "m", provCand(provider, "new-key-1", 1), provCand(provider, "new-key-2", 5))
			for _, age := range []time.Duration{time.Second, 24 * time.Hour * 365} {
				mustReject(t, newScheduler().pickWith(req, snap, "active", qar, affNow, age, true), "stale snapshot / unseen candidates")
			}
			// Snapshot with an OAuth credential that is an offered candidate: still scored, quota stays stale-neutral.
			req = provReq(provider, "m", provCand(provider, "o", 1), provCand(provider, "new-key-1", 9))
			got := newScheduler().pickWith(req, snap, "active", qar, affNow, time.Second, false)
			if !got.Handled || got.AuthID != "o" {
				t.Fatalf("offered stale OAuth must still be selected over paid: %+v", got)
			}
			// Empty snapshot: no inventory proves the candidate API-only; fail closed unless pinned to a known API credential.
			empty := provReq(provider, "m", provCand(provider, "k", 1))
			mustReject(t, newScheduler().pickWith(empty, quota.Snapshot{SchemaVersion: 1}, "active", qar, affNow, time.Hour, false), "empty snapshot")
		})
	}
}

func TestGuardRejectionThroughRuntimeSnapshot(t *testing.T) {
	// The mutable runtime must hand the guard a snapshot copy; a rejection
	// must not depend on scheduler-internal state between picks.
	snap := guardSnap("codex", map[string]gcred{"o": {authType: "oauth", models: []string{"m"}, unavailable: true}, "key": {authType: "api_key"}})
	req := provReq("codex", "m", provCand("codex", "key", 1))
	s := newScheduler()
	for i := 0; i < 5; i++ {
		mustReject(t, s.pickWith(req, snap, "active", qar, affNow, time.Hour, false), "repeat")
	}
}
