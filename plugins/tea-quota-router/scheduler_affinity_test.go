package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

const qar = "quota-aware-round-robin"

var affNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

type acct struct {
	used  *float64
	reset time.Time
	obs   time.Time
}

func f(v float64) *float64 { return &v }

// snapOf builds one account+credential per id (same id), codex provider.
func snapOf(m map[string]acct) quota.Snapshot {
	s := quota.Snapshot{SchemaVersion: 1}
	for id, a := range m {
		w := quota.Window{ID: "session", UsedFraction: a.used, ResetAt: a.reset, ObservedAt: a.obs}
		s.Accounts = append(s.Accounts, quota.Account{ID: id, Provider: "codex", Windows: []quota.Window{w}})
		s.Credentials = append(s.Credentials, quota.Credential{ID: id, AccountID: id, Provider: "codex"})
	}
	return s
}

func fresh(used float64) acct { return acct{used: f(used), obs: affNow} }

func cand(id string, prio int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Priority: prio, Provider: "codex", Status: "active"}
}

func mkReq(model string, md map[string]any, cs ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{Provider: "codex", Providers: []string{"codex"}, Model: model, Options: pluginapi.SchedulerOptions{Metadata: md}, Candidates: cs}
}

func sess(caller, session string) map[string]any {
	return map[string]any{"caller_scope": caller, "canonical_session_id": session}
}

func lastReason(s *scheduler) string {
	h := s.history()
	return h[len(h)-1].Reason
}

func abc() []pluginapi.SchedulerAuthCandidate {
	return []pluginapi.SchedulerAuthCandidate{cand("a", 1), cand("b", 1), cand("c", 1)}
}

func healthy() quota.Snapshot {
	return snapOf(map[string]acct{"a": fresh(0.1), "b": fresh(0.1), "c": fresh(0.1)})
}

func TestAffinitySameSessionStableDespiteWRR(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	req := mkReq("m", sess("callerA", "s1"), abc()...)
	first := s.pick(req, snap, "active", qar, affNow, time.Hour)
	if !first.Handled || first.AuthID == "" || lastReason(s) != "proposed" {
		t.Fatalf("first: %+v %s", first, lastReason(s))
	}
	// Other sessions advance WRR across all accounts.
	for i := 0; i < 30; i++ {
		s.pick(mkReq("m", sess("callerA", fmt.Sprintf("other%d", i)), abc()...), snap, "active", qar, affNow, time.Hour)
	}
	for i := 0; i < 10; i++ {
		got := s.pick(req, snap, "active", qar, affNow.Add(time.Duration(i)*time.Minute), time.Hour)
		if got.AuthID != first.AuthID || lastReason(s) != "affinity" {
			t.Fatalf("iter %d: %q != %q reason=%s", i, got.AuthID, first.AuthID, lastReason(s))
		}
	}
}

func TestAffinityExecutionSessionFallbackAndCanonicalPreferred(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	req := mkReq("m", map[string]any{"caller_scope": "c", "execution_session_id": "x1"}, abc()...)
	first := s.pick(req, snap, "active", qar, affNow, time.Hour)
	for i := 0; i < 5; i++ {
		s.pick(mkReq("m", sess("c", fmt.Sprintf("o%d", i)), abc()...), snap, "active", qar, affNow, time.Hour)
	}
	if got := s.pick(req, snap, "active", qar, affNow, time.Hour); got.AuthID != first.AuthID || lastReason(s) != "affinity" {
		t.Fatalf("execution session not stable: %+v", got)
	}
	k1, _ := affinityKeyFor(mkReq("m", map[string]any{"caller_scope": "c", "canonical_session_id": "z", "execution_session_id": "x1"}))
	k2, _ := affinityKeyFor(mkReq("m", map[string]any{"caller_scope": "c", "canonical_session_id": "z", "execution_session_id": "x2"}))
	if k1 != k2 {
		t.Fatal("canonical id must take precedence over execution id")
	}
}

func TestAffinityDifferentCallerSameSessionIsolated(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	a := s.pick(mkReq("m", sess("callerA", "shared"), abc()...), snap, "active", qar, affNow, time.Hour)
	// callerB's first pick with the same session id must be a fresh WRR proposal,
	// not callerA's binding.
	b := s.pick(mkReq("m", sess("callerB", "shared"), abc()...), snap, "active", qar, affNow, time.Hour)
	if lastReason(s) != "proposed" || b.AuthID == a.AuthID {
		t.Fatalf("callerB inherited callerA binding: a=%s b=%s reason=%s", a.AuthID, b.AuthID, lastReason(s))
	}
	if s.pick(mkReq("m", sess("callerA", "shared"), abc()...), snap, "active", qar, affNow, time.Hour).AuthID != a.AuthID {
		t.Fatal("callerA binding lost")
	}
	if s.pick(mkReq("m", sess("callerB", "shared"), abc()...), snap, "active", qar, affNow, time.Hour).AuthID != b.AuthID {
		t.Fatal("callerB binding lost")
	}
}

func TestAffinityModelAndProviderScopeIsolated(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	a := s.pick(mkReq("m1", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour)
	// m2 has its own WRR scope (may legitimately choose the same account), but
	// must be a fresh proposal, never m1's binding.
	b := s.pick(mkReq("m2", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour)
	if lastReason(s) != "proposed" || s.aff.len() != 2 {
		t.Fatalf("model not isolated: %s %s %s bindings=%d", a.AuthID, b.AuthID, lastReason(s), s.aff.len())
	}
	// Advance m2 only, then prove each model keeps its own binding.
	for i := 0; i < 4; i++ {
		s.pick(mkReq("m2", sess("c", fmt.Sprintf("o%d", i)), abc()...), snap, "active", qar, affNow, time.Hour)
	}
	if got := s.pick(mkReq("m1", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour); got.AuthID != a.AuthID || lastReason(s) != "affinity" {
		t.Fatalf("m1 binding disturbed: %+v", got)
	}
	if got := s.pick(mkReq("m2", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour); got.AuthID != b.AuthID || lastReason(s) != "affinity" {
		t.Fatalf("m2 binding disturbed: %+v", got)
	}
	k1, _ := affinityKeyFor(mkReq("m", sess("c", "s")))
	r := mkReq("m", sess("c", "s"))
	r.Provider = "claude"
	k2, _ := affinityKeyFor(r)
	if k1 == k2 {
		t.Fatal("provider scope must be part of key")
	}
}

func TestAffinityTTLExpiryReweights(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	req := mkReq("m", sess("c", "s"), abc()...)
	first := s.pick(req, snap, "active", qar, affNow, time.Hour)
	// just before TTL: still bound (and sliding TTL refreshes)
	if got := s.pick(req, snap, "active", qar, affNow.Add(affinityTTL-time.Second), time.Hour); got.AuthID != first.AuthID || lastReason(s) != "affinity" {
		t.Fatalf("pre-expiry: %+v", got)
	}
	// idle for a full TTL: expired, WRR reweights (new proposal, may differ)
	at := affNow.Add(affinityTTL - time.Second).Add(affinityTTL)
	s.pick(req, snap, "active", qar, at, time.Hour)
	if lastReason(s) != "proposed" {
		t.Fatalf("expired binding must not be honored: %s", lastReason(s))
	}
	// Prove the pick came from WRR rather than the stale binding: over many
	// expiries the WRR rotates through accounts.
	seen := map[string]bool{}
	now := at
	for i := 0; i < 12; i++ {
		now = now.Add(affinityTTL)
		seen[s.pick(req, snap, "active", qar, now, time.Hour).AuthID] = true
		if lastReason(s) != "proposed" {
			t.Fatalf("iteration %d reason %s", i, lastReason(s))
		}
	}
	if len(seen) != 3 {
		t.Fatalf("WRR did not reweight across expiries: %v", seen)
	}
}

func TestAffinityMissingCallerOrSessionNoBinding(t *testing.T) {
	for name, md := range map[string]map[string]any{
		"nil":             nil,
		"empty":           {},
		"caller only":     {"caller_scope": "c"},
		"session only":    {"canonical_session_id": "s"},
		"blank caller":    {"caller_scope": "  ", "canonical_session_id": "s"},
		"blank session":   {"caller_scope": "c", "canonical_session_id": " ", "execution_session_id": ""},
		"non-string":      {"caller_scope": 7, "canonical_session_id": "s"},
		"headers-only id": nil,
	} {
		s := newScheduler()
		req := mkReq("m", md, abc()...)
		if name == "headers-only id" {
			req.Options.Headers = map[string][]string{"X-Session-Id": {"s"}, "Authorization": {"Bearer k"}}
		}
		seen := map[string]bool{}
		for i := 0; i < 9; i++ {
			seen[s.pick(req, healthy(), "active", qar, affNow, time.Hour).AuthID] = true
			if lastReason(s) != "proposed" {
				t.Fatalf("%s: reason %s", name, lastReason(s))
			}
		}
		if len(seen) != 3 || s.aff.len() != 0 {
			t.Fatalf("%s: affinity leaked: seen=%v bindings=%d", name, seen, s.aff.len())
		}
	}
}

func TestAffinityBreaksOnRemovedUnofferedOrLowerPriorityCandidate(t *testing.T) {
	snap := healthy()
	cases := map[string][]pluginapi.SchedulerAuthCandidate{
		"removed (tried/racing/unavailable)": nil, // filled below
		"demoted to lower priority":          nil,
		"disabled":                           nil,
	}
	for name := range cases {
		s := newScheduler()
		req := mkReq("m", sess("c", "s"), abc()...)
		first := s.pick(req, snap, "active", qar, affNow, time.Hour).AuthID
		var next []pluginapi.SchedulerAuthCandidate
		for _, c := range abc() {
			switch {
			case name[0] == 'r' && c.ID == first: // removed
			case name[0] == 'd' && name[1] == 'e' && c.ID == first: // demoted
				c.Priority = 0
				next = append(next, c)
			case name[0] == 'd' && name[1] == 'i' && c.ID == first:
				c.Status = "disabled"
				next = append(next, c)
			default:
				next = append(next, c)
			}
		}
		got := s.pick(mkReq("m", sess("c", "s"), next...), snap, "active", qar, affNow, time.Hour)
		if got.AuthID == first || got.AuthID == "" || lastReason(s) != "proposed" {
			t.Fatalf("%s: stayed on %s (got %q, %s)", name, first, got.AuthID, lastReason(s))
		}
		// binding moved to the new account and the old one is NOT resurrected
		// when it reappears.
		again := s.pick(mkReq("m", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour)
		if again.AuthID != got.AuthID || lastReason(s) != "affinity" {
			t.Fatalf("%s: rebind not sticky: %s vs %s", name, again.AuthID, got.AuthID)
		}
	}
}

func TestAffinityHigherPriorityCandidateBreaksBinding(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	first := s.pick(mkReq("m", sess("c", "s"), cand("a", 1), cand("b", 1)), snap, "active", qar, affNow, time.Hour).AuthID
	got := s.pick(mkReq("m", sess("c", "s"), cand("a", 1), cand("b", 1), cand("c", 5)), snap, "active", qar, affNow, time.Hour)
	if got.AuthID != "c" || lastReason(s) != "proposed" {
		t.Fatalf("bound %s survived higher-priority c: %+v", first, got)
	}
}

func exhaustedSnap(ids ...string) quota.Snapshot {
	m := map[string]acct{"a": fresh(0.1), "b": fresh(0.1), "c": fresh(0.1)}
	for _, id := range ids {
		m[id] = acct{used: f(1), obs: affNow, reset: affNow.Add(30 * time.Minute)}
	}
	return snapOf(m)
}

func TestAffinityExhaustedBoundAccountRebinds(t *testing.T) {
	s := newScheduler()
	req := mkReq("m", sess("c", "s"), abc()...)
	first := s.pick(req, healthy(), "active", qar, affNow, time.Hour).AuthID
	got := s.pick(req, exhaustedSnap(first), "active", qar, affNow.Add(time.Minute), time.Hour)
	if !got.Handled || got.Reject || got.AuthID == "" || got.AuthID == first || lastReason(s) != "proposed" {
		t.Fatalf("exhausted bound not rebound: first=%s got=%+v %s", first, got, lastReason(s))
	}
	again := s.pick(req, exhaustedSnap(first), "active", qar, affNow.Add(2*time.Minute), time.Hour)
	if again.AuthID != got.AuthID || lastReason(s) != "affinity" {
		t.Fatalf("rebound account not sticky: %+v", again)
	}
}

func TestAllKnownExhaustedRejectsActiveShadowOnlyRecords(t *testing.T) {
	snap := exhaustedSnap("a", "b", "c")
	s := newScheduler()
	got := s.pick(mkReq("m", sess("c", "s"), abc()...), snap, "active", qar, affNow, time.Hour)
	if !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" || got.AuthID != "" || got.RejectReason == "" {
		t.Fatalf("active must reject: %+v", got)
	}
	if lastReason(s) != "rejected" {
		t.Fatalf("reason %s", lastReason(s))
	}
	// without affinity metadata too
	if got := s.pick(mkReq("m", nil, abc()...), snap, "active", qar, affNow, time.Hour); !got.Handled || !got.Reject {
		t.Fatalf("no-affinity active must reject: %+v", got)
	}
	// shadow: no production change, still records the would-be rejection
	sh := newScheduler()
	if got := sh.pick(mkReq("m", nil, abc()...), snap, "shadow", qar, affNow, time.Hour); got.Handled || got.Reject || lastReason(sh) != "rejected" {
		t.Fatalf("shadow: %+v %s", got, lastReason(sh))
	}
	// the wire format the host parses carries Handled+Reject+code
	raw, _ := json.Marshal(pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "x"})
	var back pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(raw, &back); err != nil || !back.Handled || !back.Reject || back.RejectCode != "auth_unavailable" {
		t.Fatalf("wire roundtrip: %s %+v %v", raw, back, err)
	}
	// reset passes: no longer exhausted -> not rejected
	if got := s.pick(mkReq("m", nil, abc()...), snap, "active", qar, affNow.Add(31*time.Minute), 2*time.Hour); got.Reject {
		t.Fatalf("expired reset must not reject: %+v", got)
	}
}

func TestPartialExhaustionNeverRejects(t *testing.T) {
	s := newScheduler()
	got := s.pick(mkReq("m", nil, abc()...), exhaustedSnap("a", "b"), "active", qar, affNow, time.Hour)
	if got.Reject || got.AuthID != "c" {
		t.Fatalf("%+v", got)
	}
}

func TestAllStaleOrUnknownIsNeutral(t *testing.T) {
	future := affNow.Add(time.Hour)
	cases := map[string]quota.Snapshot{
		"stale full":              snapOf(map[string]acct{"a": {used: f(1), obs: affNow.Add(-3 * time.Hour), reset: future}, "b": {used: f(1), obs: affNow.Add(-3 * time.Hour), reset: future}}),
		"full with missing reset": snapOf(map[string]acct{"a": {used: f(1), obs: affNow}, "b": {used: f(1), obs: affNow}}),
		"full with past reset":    snapOf(map[string]acct{"a": {used: f(1), obs: affNow, reset: affNow.Add(-time.Minute)}, "b": {used: f(1), obs: affNow, reset: affNow.Add(-time.Minute)}}),
		"no usedFraction":         snapOf(map[string]acct{"a": {obs: affNow}, "b": {obs: affNow}}),
		"unknown accounts":        {SchemaVersion: 1},
	}
	for name, snap := range cases {
		s := newScheduler()
		got := s.pick(mkReq("m", sess("c", "s"), cand("a", 1), cand("b", 1)), snap, "active", qar, affNow, time.Hour)
		if got.Reject {
			t.Fatalf("%s: rejected on non-known exhaustion: %+v", name, got)
		}
		if lastReason(s) == "rejected" {
			t.Fatalf("%s: recorded rejected", name)
		}
	}
	// stale/unknown observations stay schedulable at neutral weight.
	s := newScheduler()
	if got := s.pick(mkReq("m", nil, cand("a", 1), cand("b", 1)), cases["stale full"], "active", qar, affNow, time.Hour); !got.Handled || got.AuthID == "" {
		t.Fatalf("stale must be schedulable: %+v", got)
	}
	// full window with unknown reset is not rejected; with no other candidate it delegates.
	if got := s.pick(mkReq("m", nil, cand("a", 1), cand("b", 1)), cases["full with missing reset"], "active", qar, affNow, time.Hour); got.Handled {
		t.Fatalf("unknown-reset full must delegate, got %+v", got)
	}
}

func TestAffinityRecordsNoRawIdentifiers(t *testing.T) {
	s := newScheduler()
	secretCaller, secretSession := "CALLER-SECRET-xyz", "SESSION-SECRET-abc"
	req := mkReq("m", sess(secretCaller, secretSession), abc()...)
	req.Options.Headers = map[string][]string{"Authorization": {"Bearer HEADER-SECRET"}}
	s.pick(req, healthy(), "active", qar, affNow, time.Hour)
	s.pick(req, exhaustedSnap("a", "b", "c"), "active", qar, affNow, time.Hour)
	b, _ := json.Marshal(s.history())
	for _, secret := range []string{secretCaller, secretSession, "HEADER-SECRET"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("history leaked %q: %s", secret, b)
		}
	}
	// binding table holds only hashes + auth ids
	s.aff.mu.Lock()
	defer s.aff.mu.Unlock()
	for k, e := range s.aff.byID {
		if len(k) != 32 || strings.Contains(e.Value.(*affinityEntry).authID, "SECRET") {
			t.Fatal("unexpected table contents")
		}
	}
}

func TestAffinityShadowSameDistributionNoProductionChange(t *testing.T) {
	snap := healthy()
	act, sh := newScheduler(), newScheduler()
	for i := 0; i < 12; i++ {
		req := mkReq("m", sess("c", fmt.Sprintf("s%d", i%4)), abc()...)
		a := act.pick(req, snap, "active", qar, affNow, time.Hour)
		b := sh.pick(req, snap, "shadow", qar, affNow, time.Hour)
		if b.Handled || b.AuthID != "" {
			t.Fatalf("shadow changed production: %+v", b)
		}
		if sh.history()[i].SelectedID != a.AuthID {
			t.Fatalf("shadow proposal %q != active %q at %d", sh.history()[i].SelectedID, a.AuthID, i)
		}
	}
}

func TestAffinityBoundedAndTTLCleanup(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	for i := 0; i < affinityMaxBindings+500; i++ {
		s.pick(mkReq("m", sess("c", fmt.Sprintf("s%d", i)), abc()...), snap, "active", qar, affNow.Add(time.Duration(i)*time.Millisecond), time.Hour)
		if n := s.aff.len(); n > affinityMaxBindings {
			t.Fatalf("map grew to %d", n)
		}
	}
	if n := s.aff.len(); n != affinityMaxBindings {
		t.Fatalf("len %d", n)
	}
	// oldest evicted (LRU), newest retained
	s.aff.mu.Lock()
	oldest, _ := affinityKeyFor(mkReq("m", sess("c", "s0")))
	newest, _ := affinityKeyFor(mkReq("m", sess("c", fmt.Sprintf("s%d", affinityMaxBindings+499))))
	_, hasOld := s.aff.byID[oldest]
	_, hasNew := s.aff.byID[newest]
	s.aff.mu.Unlock()
	if hasOld || !hasNew {
		t.Fatalf("LRU eviction wrong old=%v new=%v", hasOld, hasNew)
	}
	// An unrelated pick after the TTL sweeps every expired binding.
	s.pick(mkReq("m", nil, abc()...), snap, "active", qar, affNow.Add(affinityTTL+time.Minute), time.Hour)
	if n := s.aff.len(); n != 0 {
		t.Fatalf("expired bindings not cleaned: %d", n)
	}
	if s.aff.lru.Len() != 0 {
		t.Fatal("lru list not cleaned")
	}
}

func TestAffinityConcurrentRace(t *testing.T) {
	s := newScheduler()
	snap := healthy()
	var wg sync.WaitGroup
	const sessions, workers = 40, 8
	results := make([][]string, sessions)
	var mu sync.Mutex
	start := make(chan struct{})
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < sessions; i++ {
				req := mkReq("m", sess("c", fmt.Sprintf("s%d", i)), abc()...)
				got := s.pick(req, snap, "active", qar, affNow.Add(time.Duration(g)*time.Millisecond), time.Hour)
				if !got.Handled || got.AuthID == "" {
					t.Errorf("bad pick %+v", got)
					return
				}
				mu.Lock()
				results[i] = append(results[i], got.AuthID)
				mu.Unlock()
				if g == 0 && i%7 == 0 { // concurrent churn: expiry sweep + unrelated traffic
					s.pick(mkReq("m", nil, abc()...), snap, "shadow", qar, affNow, time.Hour)
					_ = s.history()
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		if len(r) != workers {
			t.Fatalf("session %d got %d results", i, len(r))
		}
		for _, id := range r {
			if id != r[0] {
				t.Fatalf("session %d bound to multiple accounts: %v", i, r)
			}
		}
	}
	if n := s.aff.len(); n != sessions {
		t.Fatalf("bindings %d want %d", n, sessions)
	}
}
