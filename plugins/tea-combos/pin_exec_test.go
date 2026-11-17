package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// --- fixtures ---

func pinnedMemberCombo(name string, members ...Member) Combo {
	return Combo{Name: name, Strategy: StrategyFailover, Members: members}
}

func inheritFallback(model string) Member {
	return Member{Model: model, Role: "fallback", AccountStrategy: AccountStrategyInherit}
}

func okHost(t *testing.T) *accountHost {
	t.Helper()
	h := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	h.install(t)
	return h
}

func credsOf(host *accountHost) []string {
	host.mu.Lock()
	defer host.mu.Unlock()
	out := make([]string, 0, len(host.calls))
	for _, c := range host.calls {
		out = append(out, c.req.AuthID)
	}
	return out
}

func onlyCreds(t *testing.T, got []string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, id := range allowed {
		ok[id] = true
	}
	for _, id := range got {
		if !ok[id] {
			t.Fatalf("call escaped its target: %v (allowed %v)", got, allowed)
		}
	}
}

// --- non-stream ---

func TestPinExecAccountPinAppliedWithoutPoolOrProvider(t *testing.T) {
	host := okHost(t)
	p := accountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}))
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if len(host.calls) != 1 {
		t.Fatalf("calls = %d", len(host.calls))
	}
	call := host.calls[0]
	if id := call.req.AuthID; id != "a1" && id != "a2" {
		t.Fatalf("AuthID %q is not a credential of acctA", id)
	}
	if call.req.ForcedProvider != "claude" {
		t.Fatalf("ForcedProvider = %q, want the target's provider", call.req.ForcedProvider)
	}
	if call.callbackID != "cb-7" || call.req.CallerScope != "scope-A" {
		t.Fatalf("callback/caller scope lost: %+v", call)
	}
	if call.req.Headers.Get("Authorization") != "Bearer client" || call.req.Query.Get("beta") != "1" {
		t.Fatalf("headers/query not preserved")
	}
}

func TestPinExecConnectionPinAppliedExactly(t *testing.T) {
	host := okHost(t)
	p := accountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", ConnectionID: "a2"}))
	for i := 0; i < 4; i++ {
		if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
			t.Fatalf("executeCombo: %v", err)
		}
	}
	for _, id := range credsOf(host) {
		if id != "a2" {
			t.Fatalf("connection pin must always use a2, got %v", credsOf(host))
		}
	}
}

func TestPinExecAccountPinFailureNeverEscapesToOtherAccountThenMemberFallback(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.Model == "fallback-model" {
			return okReply("served-by-fallback")
		}
		return quotaFailure()
	}}
	host.install(t)
	combo := pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}, inheritFallback("fallback-model"))
	p := accountPlugin(t, combo)
	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "served-by-fallback" {
		t.Fatalf("resp=%q err=%v", resp.Payload, err)
	}
	ids := host.authIDs()
	if len(ids) != 3 || ids[2] != "fallback-model@" {
		t.Fatalf("want a1+a2 then the explicit fallback, got %v", ids)
	}
	onlyCreds(t, []string{strings.TrimPrefix(ids[0], "claude-sonnet@"), strings.TrimPrefix(ids[1], "claude-sonnet@")}, "a1", "a2")
	if ids[0] == ids[1] {
		t.Fatalf("each connection of the account is tried once: %v", ids)
	}
}

func TestPinExecConnectionPinFailureNeverRotatesToSiblings(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return quotaFailure() }}
	host.install(t)
	p := accountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", ConnectionID: "a1"}))
	_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want the member failure", err)
	}
	if got := credsOf(host); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("exact connection must not rotate: %v", got)
	}
}

func TestPinExecNeverUnpinsOnTargetFailure(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	exhausted := accountTestSnapshot(accountTestNow)
	for i := range exhausted.Accounts {
		exhausted.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
	}
	disabled := accountTestSnapshot(accountTestNow)
	for i := range disabled.Credentials {
		disabled.Credentials[i].Disabled = true
	}
	wrongModel := accountTestSnapshot(accountTestNow)
	for i := range wrongModel.Credentials {
		wrongModel.Credentials[i].Models = []string{"claude-haiku"}
	}
	cases := map[string]struct {
		snap   quota.Snapshot
		member Member
	}{
		"stale snapshot":        {stale, Member{Model: "claude-sonnet", ConnectionID: "a1"}},
		"missing account":       {accountTestSnapshot(accountTestNow), Member{Model: "claude-sonnet", AccountID: "ghost"}},
		"missing connection":    {accountTestSnapshot(accountTestNow), Member{Model: "claude-sonnet", ConnectionID: "ghost"}},
		"exhausted account":     {exhausted, Member{Model: "claude-sonnet", AccountID: "acctA"}},
		"exhausted connection":  {exhausted, Member{Model: "claude-sonnet", ConnectionID: "b1"}},
		"disabled":              {disabled, Member{Model: "claude-sonnet", ConnectionID: "a1"}},
		"model not advertised":  {wrongModel, Member{Model: "claude-sonnet", AccountID: "acctA"}},
		"provider mismatch":     {accountTestSnapshot(accountTestNow), Member{Model: "claude-sonnet", AccountID: "acctA", Provider: "codex"}},
		"pool excludes the pin": {accountTestSnapshot(accountTestNow), Member{Model: "claude-sonnet", ConnectionID: "c1", AccountPool: "claude-ab"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host := okHost(t)
			p := accountPlugin(t, pinnedMemberCombo("c", tc.member))
			p.accounts = accountRouterFor(tc.snap)
			_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
			if err == nil {
				t.Fatal("want a member failure")
			}
			if len(host.calls) != 0 {
				t.Fatalf("a failing target must never produce a host call: %v", host.authIDs())
			}
			var ce *comboError
			if !errors.As(err, &ce) || (ce.Status != http.StatusServiceUnavailable && ce.Status != http.StatusTooManyRequests) {
				t.Fatalf("want member-side 503/429, got %v", err)
			}
		})
	}
}

func TestPinExecTargetFailureStillAllowsNextMember(t *testing.T) {
	host := okHost(t)
	combo := pinnedMemberCombo("c", Member{Model: "claude-sonnet", ConnectionID: "ghost"}, inheritFallback("paid"))
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if ids := host.authIDs(); len(ids) != 1 || ids[0] != "paid@" {
		t.Fatalf("only the configured next member may run: %v", ids)
	}
}

func TestPinExecNilSourceFailsClosed(t *testing.T) {
	for name, router := range map[string]*accountRouter{"nil router": nil, "nil source": newAccountRouter(nil)} {
		t.Run(name, func(t *testing.T) {
			host := okHost(t)
			p := accountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}, inheritFallback("paid")))
			p.accounts = router
			if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err == nil {
				t.Fatal("a pin with no snapshot source must fail closed")
			}
			if len(host.calls) != 0 {
				t.Fatalf("no call may escape without a source: %v", host.authIDs())
			}
		})
	}
}

func TestPinExecRequiresCallbackAndRejectsComboMembers(t *testing.T) {
	host := okHost(t)
	p := accountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}))
	if _, err := p.executeCombo(context.Background(), pinnedReq("c"), "c"); err == nil {
		t.Fatal("a pin needs the native callback id")
	}
	self := pinnedMemberCombo("self", Member{Model: "self", AccountID: "acctA"})
	p = accountPlugin(t, self)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("self"), "self"); err == nil {
		t.Fatal("a pinned member may not reference its own combo")
	}
	if len(host.calls) != 0 {
		t.Fatalf("no host call expected: %v", host.authIDs())
	}
}

func TestPinExecNonMemberErrorStopsAtTarget(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		return nil, &hostError{Message: "model_not_allowed", Status: 403}
	}}
	host.install(t)
	combo := pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}, inheritFallback("paid"))
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err == nil {
		t.Fatal("caller denial must stop the combo")
	}
	if len(host.calls) != 1 || p.cooldown.Active("c", combo.Members[0]) {
		t.Fatalf("denial must not rotate or cool: %v", host.authIDs())
	}
}

func TestPinExecSameModelPinMembersAreIsolated(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.AuthID == "a1" || call.req.AuthID == "a2" {
			return quotaFailure()
		}
		return okReply("from-" + call.req.AuthID)
	}}
	host.install(t)
	memberA := Member{Model: "claude-sonnet", AccountID: "acctA"}
	memberB := Member{Model: "claude-sonnet", AccountID: "acctB"}
	combo := pinnedMemberCombo("c", memberA, memberB)
	p := accountPlugin(t, combo)

	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "from-b1" {
		t.Fatalf("resp=%q err=%v", resp.Payload, err)
	}
	if !p.cooldown.Active("c", memberA) {
		t.Fatal("exhausted account A must cool")
	}
	if p.cooldown.Active("c", memberB) {
		t.Fatal("same-model failure on A poisoned B")
	}
	before := len(host.calls)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatalf("second request: %v", err)
	}
	for _, id := range credsOf(host)[before:] {
		if id != "b1" {
			t.Fatalf("cooled A must be skipped while B serves: %v", credsOf(host)[before:])
		}
	}
}

func TestPinExecConcurrentSameModelCombosStayInTheirTargets(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]map[string]bool{}
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		combo := call.req.CallerScope
		if seen[combo] == nil {
			seen[combo] = map[string]bool{}
		}
		seen[combo][call.req.AuthID] = true
		return okReply("ok")
	}}
	host.install(t)
	p := accountPlugin(t,
		pinnedMemberCombo("ca", Member{Model: "claude-sonnet", AccountID: "acctA"}),
		pinnedMemberCombo("cb", Member{Model: "claude-sonnet", ConnectionID: "b1"}),
		pinnedMemberCombo("cc", Member{Model: "claude-sonnet", ConnectionID: "c1"}),
	)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		for _, name := range []string{"ca", "cb", "cc"} {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				req := pinnedReq(name)
				req.Metadata = map[string]any{pluginapi.CallerScopeMetadataKey: name}
				req.Headers = http.Header{"X-Session-Id": {name + "-s"}}
				if _, err := p.executeCombo(cbCtx(), req, name); err != nil {
					t.Errorf("%s: %v", name, err)
				}
			}(name)
		}
	}
	wg.Wait()
	allowed := map[string][]string{"ca": {"a1", "a2"}, "cb": {"b1"}, "cc": {"c1"}}
	for name, ids := range allowed {
		var got []string
		for id := range seen[name] {
			got = append(got, id)
		}
		if len(got) == 0 {
			t.Fatalf("%s never executed", name)
		}
		onlyCreds(t, got, ids...)
	}
}

func TestPinExecBindingFollowsTargetAndEditInvalidatesIt(t *testing.T) {
	host := okHost(t)
	memberA := Member{Model: "claude-sonnet", AccountID: "acctA"}
	p := accountPlugin(t, pinnedMemberCombo("c", memberA))
	req := pinnedReq("c")
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	combo := p.store.state.Combos["c"]
	oldKey := accountBindingKey("scope-A", sessionIdentity(req), combo, combo.Members[0], AccountStrategyRoundRobin)
	bound, ok := p.accountBindings.Lookup(oldKey)
	if !ok || (bound != "a1" && bound != "a2") {
		t.Fatalf("a served pinned request must bind a credential of its account, got %q %v", bound, ok)
	}
	if err := p.store.Upsert(pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctB"}).Normalize()); err != nil {
		t.Fatal(err)
	}
	edited := p.store.state.Combos["c"]
	newKey := accountBindingKey("scope-A", sessionIdentity(req), edited, edited.Members[0], AccountStrategyRoundRobin)
	if newKey == oldKey {
		t.Fatal("editing the pin must change the binding identity")
	}
	if _, ok := p.accountBindings.Lookup(newKey); ok {
		t.Fatal("an edited pin must not inherit the old binding")
	}
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatalf("executeCombo after edit: %v", err)
	}
	if got := credsOf(host); got[len(got)-1] != "b1" {
		t.Fatalf("edited pin must use only its new target: %v", got)
	}
	if got, _ := p.accountBindings.Lookup(newKey); got != "b1" {
		t.Fatalf("new target binding = %q", got)
	}
}

func TestPinExecLegacyBindingFromNoPinMemberIsNotReused(t *testing.T) {
	okHost(t)
	legacy := Member{Model: "claude-sonnet", AccountPool: "claude-ab"}
	legacyCombo := Combo{Name: "c", AccountStrategy: AccountStrategyRoundRobin, Strategy: StrategyFailover, Members: []Member{legacy}}.Normalize()
	req := pinnedReq("c")
	p := accountPlugin(t, legacyCombo)
	p.accountBindings.Bind(accountBindingKey("scope-A", sessionIdentity(req), legacyCombo, legacyCombo.Members[0], AccountStrategyRoundRobin), "b1", time.Hour)

	pinned := legacyCombo
	pinned.Members = []Member{{Model: "claude-sonnet", AccountPool: "claude-ab", AccountID: "acctA"}}
	if err := p.store.Upsert(pinned.Normalize()); err != nil {
		t.Fatal(err)
	}
	host := okHost(t)
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	onlyCreds(t, credsOf(host), "a1", "a2")
	if legacyKey := accountBindingKey("scope-A", sessionIdentity(req), legacyCombo, legacyCombo.Members[0], AccountStrategyRoundRobin); legacyKey == "" {
		t.Fatal("legacy key must still be derivable")
	} else if got, ok := p.accountBindings.Lookup(legacyKey); !ok || got != "b1" {
		t.Fatalf("unrelated legacy bindings must be left alone (not globally cleared): %q %v", got, ok)
	}
}

// --- member affinity identity ---

func TestPinMemberAffinityIdentityIncludesTargets(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	affinity := newAffinityWithClock(nil, nil, func() time.Time { return now })
	req := pinnedReq("c")
	combo := Combo{Name: "c", Members: []Member{{Model: "m", AccountID: "A"}, {Model: "m", AccountID: "B"}}}.Normalize()
	key, _, ok := affinity.Resolve(combo, req)
	if key == "" || ok {
		t.Fatalf("key=%q ok=%v", key, ok)
	}
	affinity.Bind(key, 0, combo)
	if _, idx, ok := affinity.Resolve(combo, req); !ok || idx != 0 {
		t.Fatalf("binding to the first pinned member must resolve: idx=%d ok=%v", idx, ok)
	}

	swapped := Combo{Name: "c", Members: []Member{{Model: "m", AccountID: "B"}, {Model: "m", AccountID: "A"}}}.Normalize()
	if _, idx, ok := affinity.Resolve(swapped, req); ok {
		t.Fatalf("a binding for target A must not attach to target B at the same index (idx=%d)", idx)
	}
	affinity.Bind(key, 0, combo)
	connection := Combo{Name: "c", Members: []Member{{Model: "m", ConnectionID: "A"}, {Model: "m", AccountID: "B"}}}.Normalize()
	if _, _, ok := affinity.Resolve(connection, req); ok {
		t.Fatal("account A and connection A are different targets")
	}
}

func TestPinMemberAffinityLegacyBindingsRemainReadable(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	req := pinnedReq("c")
	combo := Combo{Name: "c", Members: []Member{{Model: "m", Provider: "claude"}, {Model: "n"}}}.Normalize()
	key, _ := deriveSessionKey(combo, req)

	var legacy AffinityBinding
	raw := `{"key":"` + key + `","combo":"c","memberIndex":0,"memberModel":"m","memberProvider":"claude","expiresAt":"2026-11-15T13:00:00Z"}`
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		t.Fatal(err)
	}
	affinity := newAffinityWithClock(map[string]AffinityBinding{key: legacy}, nil, func() time.Time { return now })
	if _, idx, ok := affinity.Resolve(combo, req); !ok || idx != 0 {
		t.Fatalf("a persisted pre-pin binding must keep resolving for an unpinned member: idx=%d ok=%v", idx, ok)
	}

	// Editing that member to add a pin invalidates only this binding.
	edited := Combo{Name: "c", Members: []Member{{Model: "m", Provider: "claude", AccountID: "A"}, {Model: "n"}}}.Normalize()
	if _, _, ok := affinity.Resolve(edited, req); ok {
		t.Fatal("adding a pin must invalidate the old binding")
	}
	other := pinnedReq("c")
	other.Headers = http.Header{"X-Session-Id": {"sess-2"}}
	otherKey, _ := deriveSessionKey(combo, other)
	affinity.Bind(otherKey, 1, combo)
	if _, idx, ok := affinity.Resolve(edited, other); !ok || idx != 1 {
		t.Fatalf("unrelated bindings must survive the edit: idx=%d ok=%v", idx, ok)
	}
}

func TestPinMemberAffinityPersistedFieldsOmittedForLegacy(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	affinity := newAffinityWithClock(nil, nil, func() time.Time { return now })
	combo := Combo{Name: "c", Members: []Member{{Model: "m"}, {Model: "m", ConnectionID: "k1"}}}.Normalize()
	affinity.Bind("k-legacy", 0, combo)
	affinity.Bind("k-pinned", 1, combo)
	legacy, _ := json.Marshal(affinity.entries["k-legacy"].binding)
	pinned, _ := json.Marshal(affinity.entries["k-pinned"].binding)
	if strings.Contains(string(legacy), "accountId") || strings.Contains(string(legacy), "connectionId") {
		t.Fatalf("legacy binding JSON must be unchanged: %s", legacy)
	}
	if !strings.Contains(string(pinned), `"memberConnectionId":"k1"`) {
		t.Fatalf("pinned binding must persist its target: %s", pinned)
	}
}

// --- stream ---

func TestPinStreamAccountPinAppliedAndStaysInAccount(t *testing.T) {
	h := newAccountStreamHost(t)
	for _, id := range []string{"a1", "a2"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("hello")}, {Done: true}}
	}
	p := streamAccountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}))
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	handle.Close()
	if len(emitted) != 1 || emitted[0] != "hello" {
		t.Fatalf("emitted = %v", emitted)
	}
	call := h.opens[0]
	if id := call.req.AuthID; id != "a1" && id != "a2" {
		t.Fatalf("stream AuthID %q is not in acctA", id)
	}
	if call.callbackID != "cb-7" || call.req.CallerScope != "scope-A" || call.req.ForcedProvider != "claude" || !call.req.Stream {
		t.Fatalf("stream pin lost context: %+v cb=%q", call.req, call.callbackID)
	}
	if call.req.Headers.Get("Authorization") != "Bearer client" || call.req.Query.Get("beta") != "1" {
		t.Fatalf("headers/query not preserved")
	}
}

func TestPinStreamConnectionFailureNeverEscapesToSiblings(t *testing.T) {
	h := newAccountStreamHost(t)
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.scripts["a2"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("leak")}, {Done: true}}
	p := streamAccountPlugin(t, pinnedMemberCombo("c", Member{Model: "claude-sonnet", ConnectionID: "a1"}))
	if _, emitted, err := runAccountStream(p, cbCtx(), "c"); err == nil || len(emitted) != 0 {
		t.Fatalf("want the member failure, err=%v emitted=%v", err, emitted)
	}
	if opens := h.openedAuth(); len(opens) != 1 || opens[0] != "claude-sonnet@a1" {
		t.Fatalf("exact connection must not rotate to a sibling: %v", opens)
	}
}

func TestPinStreamAccountFailureStaysInAccountThenNextMember(t *testing.T) {
	h := newAccountStreamHost(t)
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.scripts["a2"] = []pluginapi.HostModelStreamReadResponse{{Error: "reset", Done: true}}
	h.scripts["b1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("leak-b1")}, {Done: true}}
	h.scripts["paid"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("from-paid")}, {Done: true}}
	combo := pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"}, inheritFallback("paid"))
	p := streamAccountPlugin(t, combo)
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	handle.Close()
	if len(emitted) != 1 || emitted[0] != "from-paid" {
		t.Fatalf("emitted = %v", emitted)
	}
	for _, open := range h.openedAuth() {
		switch open {
		case "claude-sonnet@a1", "claude-sonnet@a2", "paid":
		case "paid@":
		default:
			t.Fatalf("stream escaped acctA: %v", h.openedAuth())
		}
	}
}

func TestPinStreamNeverUnpinsOnTargetFailure(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	exhausted := accountTestSnapshot(accountTestNow)
	for i := range exhausted.Accounts {
		exhausted.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
	}
	cases := map[string]struct {
		snap   quota.Snapshot
		member Member
		router *accountRouter
	}{
		"stale":      {snap: stale, member: Member{Model: "claude-sonnet", ConnectionID: "a1"}},
		"missing":    {snap: accountTestSnapshot(accountTestNow), member: Member{Model: "claude-sonnet", AccountID: "ghost"}},
		"exhausted":  {snap: exhausted, member: Member{Model: "claude-sonnet", AccountID: "acctA"}},
		"mismatch":   {snap: accountTestSnapshot(accountTestNow), member: Member{Model: "claude-sonnet", ConnectionID: "a1", Provider: "codex"}},
		"nil source": {snap: accountTestSnapshot(accountTestNow), member: Member{Model: "claude-sonnet", ConnectionID: "a1"}, router: newAccountRouter(nil)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newAccountStreamHost(t)
			p := streamAccountPlugin(t, pinnedMemberCombo("c", tc.member))
			p.accounts = accountRouterFor(tc.snap)
			if tc.router != nil {
				p.accounts = tc.router
			}
			if _, _, err := runAccountStream(p, cbCtx(), "c"); err == nil {
				t.Fatal("want failure")
			}
			if len(h.opens) != 0 {
				t.Fatalf("a failing target must not open any stream: %v", h.openedAuth())
			}
		})
	}
}

func TestPinStreamNoReplayAfterFirstPayloadAndIsolatedCooldown(t *testing.T) {
	h := newAccountStreamHost(t)
	memberA := Member{Model: "claude-sonnet", AccountID: "acctA"}
	memberB := Member{Model: "claude-sonnet", AccountID: "acctB"}
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.openErr["a2"] = h.openErr["a1"]
	h.scripts["b1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("first")}, {Error: "boom", Done: true}}
	p := streamAccountPlugin(t, pinnedMemberCombo("c", memberA, memberB))
	_, _, handle, err := p.executeComboStream(cbCtx(), pinnedReq("c"), "c", -1, func([]byte) error { return nil })
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	if pumpErr := handle.Pump(cbCtx(), "s"); pumpErr == nil {
		t.Fatal("a failure after the first payload must surface, not replay")
	}
	handle.Close()
	opens := h.openedAuth()
	if opens[len(opens)-1] != "claude-sonnet@b1" {
		t.Fatalf("B must serve after A's account failed: %v", opens)
	}
	count := 0
	for _, o := range opens {
		if o == "claude-sonnet@b1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("no replay after the first payload: %v", opens)
	}
	if !p.cooldown.Active("c", memberA) || p.cooldown.Active("c", memberB) {
		t.Fatal("only the failed target may cool")
	}
}

func TestPinStreamBindsPinnedAccountAfterFirstPayload(t *testing.T) {
	h := newAccountStreamHost(t)
	h.scripts["a1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("x")}, {Done: true}}
	h.scripts["a2"] = h.scripts["a1"]
	combo := pinnedMemberCombo("c", Member{Model: "claude-sonnet", AccountID: "acctA"})
	p := streamAccountPlugin(t, combo)
	stored := p.store.state.Combos["c"]
	key := accountBindingKey("scope-A", sessionIdentity(pinnedReq("c")), stored, stored.Members[0], AccountStrategyRoundRobin)
	handle, _, err := runAccountStream(p, cbCtx(), "c")
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	if got, ok := p.accountBindings.Lookup(key); !ok || (got != "a1" && got != "a2") {
		t.Fatalf("binding = %q %v", got, ok)
	}
}
