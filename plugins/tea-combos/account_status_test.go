package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func statusOnly(status int, body string) (*pluginapi.HostModelExecutionResponse, error) {
	return &pluginapi.HostModelExecutionResponse{StatusCode: status, Body: []byte(body)}, nil
}

func accountBindingFor(t *testing.T, p *combosPlugin, combo string) string {
	t.Helper()
	c := p.store.state.Combos[combo]
	return accountBindingKey("scope-A", sessionIdentity(pinnedReq(combo)), c, c.Members[0], AccountStrategyRoundRobin)
}

// --- Defect 3: status-only (nil error) host responses >= 400 -------------------------------

func TestStatusOnlyClientErrorsAreFatalWithActualStatusAndNoBinding(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusUnauthorized, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
				return statusOnly(status, `{"error":"client fault"}`)
			}}
			host.install(t)
			combo := pinnedCombo("c")
			combo.Members = append(combo.Members, Member{Model: "other", AccountStrategy: AccountStrategyInherit, Role: "fallback"})
			p := accountPlugin(t, combo)
			resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
			var ce *comboError
			if !errors.As(err, &ce) || ce.Status != status {
				t.Fatalf("want fatal comboError with status %d, got resp=%q err=%v", status, resp.Payload, err)
			}
			if ce.Retryable {
				t.Fatalf("a client error must not be retryable: %+v", ce)
			}
			if len(resp.Payload) != 0 {
				t.Fatalf("a client error must not be returned as a success payload: %q", resp.Payload)
			}
			if len(host.calls) != 1 {
				t.Fatalf("client error must not retry accounts or members: %v", host.authIDs())
			}
			if _, ok := p.accountBindings.Lookup(accountBindingFor(t, p, "c")); ok {
				t.Fatal("a client error must not bind an account")
			}
			if p.cooldown.Active("c", combo.Members[0]) {
				t.Fatal("a caller-side failure must not cool the member")
			}
		})
	}
}

func TestStatusOnlyMemberFailuresStillRetryToNextAccount(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
			if call.req.AuthID == "b1" {
				return okReply("from-b1")
			}
			return statusOnly(status, "")
		}}
		host.install(t)
		p := accountPlugin(t, pinnedCombo("c"))
		p.accounts = accountRouterFor(sharedAccountSnapshot())
		resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
		if err != nil || string(resp.Payload) != "from-b1" {
			t.Fatalf("status %d: resp=%q err=%v calls=%v", status, resp.Payload, err, host.authIDs())
		}
	}
}

func TestStatusOnlyQuotaLanguageOn403IsAMemberFailure(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.AuthID == "b1" {
			return okReply("from-b1")
		}
		return statusOnly(http.StatusForbidden, `{"error":{"type":"insufficient_quota"}}`)
	}}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(sharedAccountSnapshot())
	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "from-b1" {
		t.Fatalf("an account-exhausted 403 body is the member's problem: resp=%q err=%v calls=%v", resp.Payload, err, host.authIDs())
	}
}

func TestNilHostResponseIsGuardedNonStream(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.AuthID == "b1" {
			return okReply("from-b1")
		}
		return nil, nil
	}}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(sharedAccountSnapshot())
	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "from-b1" {
		t.Fatalf("nil response must be a retryable member failure, not a panic or success: resp=%q err=%v", resp.Payload, err)
	}

	host2 := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return nil, nil }}
	host2.install(t)
	p2 := accountPlugin(t, pinnedCombo("c"))
	_, err = p2.executeCombo(cbCtx(), pinnedReq("c"), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusBadGateway {
		t.Fatalf("all-nil must surface 502, got %v", err)
	}
	if _, ok := p2.accountBindings.Lookup(accountBindingFor(t, p2, "c")); ok {
		t.Fatal("nil response must not bind")
	}
}

func TestNilHostResponseIsGuardedForInheritedMembers(t *testing.T) {
	stubHostExecute(t, func(context.Context, pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		return nil, nil
	})
	p := newTestPlugin(t)
	combo := Combo{Name: "c", Strategy: StrategyFailover, Members: []Member{{Model: "m"}}}.Normalize()
	p.store.state.Combos["c"] = combo
	_, err := p.executeCombo(context.Background(), pluginapi.ExecutorRequest{Model: "c", Payload: []byte(`{}`)}, "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusBadGateway {
		t.Fatalf("nil response must be a 502 member failure, got %v", err)
	}
}

func TestStreamStatusOnlyClientErrorsAreFatalAndClosed(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		h := newAccountStreamHost(t)
		open := hostModelExecuteStream
		hostModelExecuteStream = func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
			resp, _ := open(ctx, req)
			resp.StatusCode = status
			return resp, nil
		}
		p := streamAccountPlugin(t, pinnedCombo("c"))
		_, _, err := runAccountStream(p, cbCtx(), "c")
		var ce *comboError
		if !errors.As(err, &ce) || ce.Status != status || ce.Retryable {
			t.Fatalf("status %d: want fatal comboError with that status, got %v", status, err)
		}
		if n := len(h.openedAuth()); n != 1 {
			t.Fatalf("status %d: client error must not rotate accounts: %v", status, h.openedAuth())
		}
		if h.closedCount() != 1 {
			t.Fatalf("status %d: the opened host stream must be closed", status)
		}
		if _, ok := p.accountBindings.Lookup(accountBindingFor(t, p, "c")); ok {
			t.Fatalf("status %d: must not bind", status)
		}
	}
}

func TestStreamNilHostResponseIsGuarded(t *testing.T) {
	h := newAccountStreamHost(t)
	open := hostModelExecuteStream
	hostModelExecuteStream = func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
		if req.AuthID == "b1" {
			return open(ctx, req)
		}
		return nil, nil
	}
	h.scripts["b1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("from-b1")}, {Done: true}}
	p := streamAccountPlugin(t, pinnedCombo("c"))
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil || len(emitted) != 1 || emitted[0] != "from-b1" {
		t.Fatalf("nil stream response must be a retryable failure: handle=%v emitted=%v err=%v", handle, emitted, err)
	}
	handle.Close()
}

// --- Defect 2: explicit-account empty stream before payload -------------------------------

func TestExplicitAccountEmptyStreamClosesRetriesNextAndBindsOnlyServer(t *testing.T) {
	for _, server := range []string{"a1", "a2", "b1"} {
		t.Run("only "+server+" has data", func(t *testing.T) {
			h := newAccountStreamHost(t)
			for _, id := range []string{"a1", "a2", "b1"} {
				if id == server {
					h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("from-" + id)}, {Done: true}}
				} else {
					h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Done: true}} // clean end, no payload
				}
			}
			p := streamAccountPlugin(t, pinnedCombo("c"))
			handle, emitted, err := runAccountStream(p, cbCtx(), "c")
			if err != nil || handle == nil {
				t.Fatalf("handle=%v err=%v opens=%v", handle, err, h.openedAuth())
			}
			handle.Close()
			if len(emitted) != 1 || emitted[0] != "from-"+server {
				t.Fatalf("emitted=%v opens=%v", emitted, h.openedAuth())
			}
			opens := h.openedAuth()
			if h.closedCount() != len(opens) {
				t.Fatalf("every empty stream must be closed before retrying: closed=%d opens=%v", h.closedCount(), opens)
			}
			bound, ok := p.accountBindings.Lookup(accountBindingFor(t, p, "c"))
			if !ok || bound != server {
				t.Fatalf("affinity must bind only the credential that produced data: got %q ok=%v want %q", bound, ok, server)
			}
		})
	}
}

func TestExplicitAccountEmptyStreamEverywhereFailsWithoutBindingAndCoolsMember(t *testing.T) {
	h := newAccountStreamHost(t)
	for _, id := range []string{"a1", "a2", "b1"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Done: true}}
	}
	p := streamAccountPlugin(t, pinnedCombo("c"))
	member := p.store.state.Combos["c"].Members[0]
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	var ce *comboError
	if !errors.As(err, &ce) || handle != nil || len(emitted) != 0 {
		t.Fatalf("all-empty must fail the member: handle=%v emitted=%v err=%v", handle, emitted, err)
	}
	if n := len(h.openedAuth()); n != 3 || h.closedCount() != 3 {
		t.Fatalf("each credential once and every stream closed: opens=%v closed=%d", h.openedAuth(), h.closedCount())
	}
	if _, ok := p.accountBindings.Lookup(accountBindingFor(t, p, "c")); ok {
		t.Fatal("empty streams must never bind")
	}
	if !p.cooldown.Active("c", member) {
		t.Fatal("member must cool after every account came back empty")
	}
}

func TestExplicitAccountEmptyStreamFallsBackToNextMember(t *testing.T) {
	h := newAccountStreamHost(t)
	for _, id := range []string{"a1", "a2", "b1"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Done: true}}
	}
	h.scripts["fallback-model"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("fallback")}, {Done: true}}
	combo := pinnedCombo("c")
	combo.Members = append(combo.Members, Member{Model: "fallback-model", Role: "fallback", AccountStrategy: AccountStrategyInherit})
	p := streamAccountPlugin(t, combo)
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil || len(emitted) != 1 || emitted[0] != "fallback" {
		t.Fatalf("handle=%v emitted=%v err=%v opens=%v", handle, emitted, err, h.openedAuth())
	}
	handle.Close()
}

func TestInheritedEmptyStreamSemanticsUnchanged(t *testing.T) {
	// Only explicit-account routes change: an inherited member's clean empty stream is still a
	// successful member (stream_test.go keeps the full matrix; this pins the member loop).
	h := newAccountStreamHost(t)
	h.scripts["m"] = []pluginapi.HostModelStreamReadResponse{{Done: true}}
	p := newTestPlugin(t)
	p.store.state.Combos["c"] = Combo{Name: "c", Strategy: StrategyFailover, Members: []Member{{Model: "m"}, {Model: "never"}}}.Normalize()
	idx, _, handle, err := p.executeComboStream(cbCtx(), pluginapi.ExecutorRequest{Model: "c", Payload: []byte(`{}`)}, "c", -1, func([]byte) error { return nil })
	if err != nil || handle != nil || idx != 0 {
		t.Fatalf("idx=%d handle=%v err=%v", idx, handle, err)
	}
	if opens := h.openedAuth(); len(opens) != 1 {
		t.Fatalf("inherited empty stream must not rotate: %v", opens)
	}
}

// --- Snapshot filtering: disabled / unavailable / nextRetry --------------------------------

func TestFreshSnapshotFiltersDisabledUnavailableAndCoolingCredentials(t *testing.T) {
	now := accountTestNow
	build := func(mut func(*quota.Snapshot)) quota.Snapshot {
		s := accountTestSnapshot(now)
		mut(&s)
		return s
	}
	cases := []struct {
		name string
		snap quota.Snapshot
		want []string // credentials the plan may pin, as a set
	}{
		{"disabled", build(func(s *quota.Snapshot) { s.Credentials[0].Disabled = true }), []string{"a2", "b1"}},
		{"unavailable", build(func(s *quota.Snapshot) { s.Credentials[1].Unavailable = true }), []string{"a1", "b1"}},
		{"future nextRetry", build(func(s *quota.Snapshot) { s.Credentials[2].NextRetryAt = now.Add(time.Minute) }), []string{"a1", "a2"}},
		{"expired nextRetry is usable again", build(func(s *quota.Snapshot) { s.Credentials[2].NextRetryAt = now.Add(-time.Minute) }), []string{"a1", "a2", "b1"}},
		{"all three filters at once", build(func(s *quota.Snapshot) {
			s.Credentials[0].Disabled = true
			s.Credentials[1].Unavailable = true
			s.Credentials[2].NextRetryAt = now.Add(time.Hour)
		}), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Goes through the real weightedSelector snapshot reader, not a static fake.
			path := t.TempDir() + "/quota.json"
			if err := quota.WriteSnapshot(path, tc.snap); err != nil {
				t.Fatal(err)
			}
			sel := newWeightedSelector(path, 600)
			defer sel.Close()
			sel.refresh()
			r := newAccountRouter(sel)
			r.now = func() time.Time { return now }
			combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
			plan, cerr := r.plan(accountPlanRequest{Combo: combo, Member: member})
			if len(tc.want) == 0 {
				if cerr == nil || len(plan.Credentials) != 0 {
					t.Fatalf("every credential filtered: want failure without pins, got %+v %v", plan, cerr)
				}
				return
			}
			if cerr != nil {
				t.Fatalf("plan: %v", cerr)
			}
			got := map[string]bool{}
			for _, id := range plan.Credentials {
				got[id] = true
			}
			if len(got) != len(tc.want) || len(plan.Credentials) != len(tc.want) {
				t.Fatalf("plan %v, want set %v", plan.Credentials, tc.want)
			}
			for _, id := range tc.want {
				if !got[id] {
					t.Fatalf("plan %v, want set %v", plan.Credentials, tc.want)
				}
			}
		})
	}
}

func TestFreshSnapshotStaleOrMissingNeverWidensPool(t *testing.T) {
	now := accountTestNow
	stale := accountTestSnapshot(now)
	stale.GeneratedAt = now.Add(-time.Hour)
	path := t.TempDir() + "/quota.json"
	if err := quota.WriteSnapshot(path, stale); err != nil {
		t.Fatal(err)
	}
	sel := newWeightedSelector(path, 600)
	defer sel.Close()
	sel.refresh()
	r := newAccountRouter(sel)
	r.now = func() time.Time { return now }
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	if plan, cerr := r.plan(accountPlanRequest{Combo: combo, Member: member}); cerr == nil || len(plan.Credentials) != 0 {
		t.Fatalf("stale snapshot must fail closed: %+v %v", plan, cerr)
	}
	none := newWeightedSelector("", 600)
	defer none.Close()
	r = newAccountRouter(none)
	r.now = func() time.Time { return now }
	if plan, cerr := r.plan(accountPlanRequest{Combo: combo, Member: member}); cerr == nil || len(plan.Credentials) != 0 {
		t.Fatalf("missing snapshot path must fail closed: %+v %v", plan, cerr)
	}
}
