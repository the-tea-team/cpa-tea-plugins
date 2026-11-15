package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// recordedCall is one recorded host.model.execute / execute_stream request.
type recordedCall struct {
	req        pluginapi.HostModelExecutionRequest
	callbackID string
}

type accountHost struct {
	mu    sync.Mutex
	calls []recordedCall
	// reply decides the outcome per call; credential is req.AuthID ("" when unpinned).
	reply func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error)
}

func (h *accountHost) install(t *testing.T) {
	t.Helper()
	stubHostExecute(t, func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		call := recordedCall{req: req, callbackID: hostCallbackIDFromContext(ctx)}
		h.mu.Lock()
		h.calls = append(h.calls, call)
		h.mu.Unlock()
		return h.reply(call)
	})
}

func (h *accountHost) authIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.calls))
	for _, c := range h.calls {
		out = append(out, c.req.Model+"@"+c.req.AuthID)
	}
	return out
}

func okReply(body string) (*pluginapi.HostModelExecutionResponse, error) {
	return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(body)}, nil
}

func quotaFailure() (*pluginapi.HostModelExecutionResponse, error) {
	return nil, &hostError{Message: "rate limited", Status: http.StatusTooManyRequests, Retryable: true}
}

func accountPlugin(t *testing.T, combos ...Combo) *combosPlugin {
	t.Helper()
	p := newTestPlugin(t)
	for _, c := range combos {
		c = c.Normalize()
		if err := c.Validate(); err != nil {
			t.Fatalf("invalid combo %s: %v", c.Name, err)
		}
		p.store.state.Combos[c.Name] = c
	}
	p.accounts = accountRouterFor(accountTestSnapshot(accountTestNow))
	p.accountBindings = newAccountBindings(func() time.Time { return accountTestNow })
	return p
}

func pinnedCombo(name string) Combo {
	return Combo{Name: name, AccountStrategy: AccountStrategyRoundRobin, Strategy: StrategyFailover,
		Members: []Member{{Model: "claude-sonnet", AccountPool: "claude-ab"}}}
}

func pinnedReq(combo string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{
		Model:   combo,
		Payload: []byte(`{"model":"` + combo + `"}`),
		Headers: http.Header{"X-Session-Id": {"sess-1"}, "Authorization": {"Bearer client"}},
		Query:   url.Values{"beta": {"1"}},
		Metadata: map[string]any{
			pluginapi.CallerScopeMetadataKey: "scope-A",
		},
	}
}

func cbCtx() context.Context { return withHostCallbackID(context.Background(), "cb-7") }

func TestExplicitStrategyPinsNativeCredentialWithCallbackAndOriginalCaller(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	req := pinnedReq("c")
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if len(host.calls) != 1 {
		t.Fatalf("calls = %d", len(host.calls))
	}
	call := host.calls[0]
	if id := call.req.AuthID; id != "a1" && id != "a2" && id != "b1" {
		t.Fatalf("AuthID %q is not a pool credential", id)
	}
	if call.req.ForcedProvider != "claude" {
		t.Fatalf("ForcedProvider = %q, want the pool provider", call.req.ForcedProvider)
	}
	if call.callbackID != "cb-7" {
		t.Fatalf("native host callback id lost: %q", call.callbackID)
	}
	if call.req.CallerScope != "scope-A" {
		t.Fatalf("original caller scope not preserved: %q", call.req.CallerScope)
	}
	if call.req.Headers.Get("Authorization") != "Bearer client" || call.req.Headers.Get("X-Session-Id") != "sess-1" || call.req.Query.Get("beta") != "1" {
		t.Fatalf("headers/query not preserved: %v %v", call.req.Headers, call.req.Query)
	}
}

func TestExplicitStrategyNeverSynthesizesCallerScope(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	req := pinnedReq("c")
	req.Metadata = nil
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if got := host.calls[0].req.CallerScope; got != "" {
		t.Fatalf("caller scope was synthesized: %q", got)
	}
}

func TestExplicitStrategyRequiresNativeCallbackID(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	_, err := p.executeCombo(context.Background(), pinnedReq("c"), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusInternalServerError || !strings.Contains(ce.Message, "callback") {
		t.Fatalf("err = %v, want 500 naming the callback id", err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("no host call may happen without a callback id: %v", host.authIDs())
	}
}

func TestInheritedMembersStayUnpinnedAndUnscoped(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	combo := Combo{Name: "plain", Strategy: StrategyFailover, Members: []Member{{Model: "meta-model"}}}
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(context.Background(), pinnedReq("plain"), "plain"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if c := host.calls[0]; c.req.AuthID != "" || c.req.CallerScope != "" {
		t.Fatalf("inherited member must behave as before: %+v", c.req)
	}
}

func TestExplicitInheritMemberInsideExplicitComboIsUnpinned(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.Model == "claude-sonnet" {
			return quotaFailure()
		}
		return okReply("ok")
	}}
	host.install(t)
	combo := pinnedCombo("mix")
	combo.Members = append(combo.Members, Member{Model: "local-model", AccountStrategy: AccountStrategyInherit, Role: "fallback"})
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("mix"), "mix"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	ids := host.authIDs()
	last := ids[len(ids)-1]
	if last != "local-model@" {
		t.Fatalf("fallback member must run unpinned, got %v", ids)
	}
}

func TestAccountFailoverTriesEachListedCredentialOnceThenMemberFallback(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.Model == "fallback-model" {
			return okReply("served-by-fallback")
		}
		return quotaFailure()
	}}
	host.install(t)
	combo := pinnedCombo("c")
	combo.Members = append(combo.Members, Member{Model: "fallback-model", Role: "fallback", AccountStrategy: AccountStrategyInherit})
	p := accountPlugin(t, combo)
	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "served-by-fallback" {
		t.Fatalf("resp=%q err=%v", resp.Payload, err)
	}
	ids := host.authIDs()
	if len(ids) != 4 {
		t.Fatalf("want 3 pinned attempts (one per pool credential) then the fallback, got %v", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids[:3] {
		if seen[id] || !strings.HasPrefix(id, "claude-sonnet@") {
			t.Fatalf("each credential must be tried exactly once: %v", ids)
		}
		seen[id] = true
	}
	if ids[3] != "fallback-model@" {
		t.Fatalf("fallback must not inherit the pool pin: %v", ids)
	}
}

func TestAccountAttemptsBoundedToEight(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials = nil
	var ids []string
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("k%02d", i)
		snap.Credentials = append(snap.Credentials, quota.Credential{ID: id, AccountID: "acctA", Provider: "claude", Models: []string{"claude-*"}})
		ids = append(ids, id)
	}
	snap.Pools = []quota.Pool{{ID: "claude-ab", Provider: "claude", CredentialIDs: ids}}
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return quotaFailure() }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(snap)
	_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v", err)
	}
	if len(host.calls) != maxAccountAttempts {
		t.Fatalf("attempts = %d, want %d", len(host.calls), maxAccountAttempts)
	}
}

func TestNonMemberErrorsStopAccountRetries(t *testing.T) {
	cases := map[string]func() (*pluginapi.HostModelExecutionResponse, error){
		"malformed request": func() (*pluginapi.HostModelExecutionResponse, error) {
			return nil, &hostError{Message: "bad request", Status: 400}
		},
		"caller denial 403": func() (*pluginapi.HostModelExecutionResponse, error) {
			return nil, &hostError{Message: "model_not_allowed", Status: 403}
		},
		"unauthorized 401": func() (*pluginapi.HostModelExecutionResponse, error) {
			return nil, &hostError{Message: "no key", Status: 401}
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return reply() }}
			host.install(t)
			combo := pinnedCombo("c")
			combo.Members = append(combo.Members, Member{Model: "other", AccountStrategy: AccountStrategyInherit, Role: "fallback"})
			p := accountPlugin(t, combo)
			_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
			if err == nil {
				t.Fatal("want an error")
			}
			if len(host.calls) != 1 {
				t.Fatalf("non-member error must not retry accounts or members: %v", host.authIDs())
			}
			if p.cooldown.Active("c", combo.Members[0]) {
				t.Fatal("a caller-side failure must not cool the member")
			}
		})
	}
}

func TestMemberCoolsOnlyAfterAllAccountsFail(t *testing.T) {
	calls := 0
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		calls++
		if calls < 3 {
			return quotaFailure()
		}
		return okReply("ok")
	}}
	host.install(t)
	combo := pinnedCombo("c")
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if p.cooldown.Active("c", combo.Members[0]) {
		t.Fatal("member must not cool while another account served")
	}
	host.reply = func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return quotaFailure() }
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err == nil {
		t.Fatal("want exhaustion")
	}
	if !p.cooldown.Active("c", combo.Members[0]) {
		t.Fatal("member must cool once every permitted account failed")
	}
}

func TestPoolFailureNeverUnpins(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	exhausted := accountTestSnapshot(accountTestNow)
	for i := range exhausted.Accounts {
		exhausted.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
	}
	cases := map[string]quota.Snapshot{"stale": stale, "exhausted": exhausted}
	for name, snap := range cases {
		t.Run(name, func(t *testing.T) {
			host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("leak") }}
			host.install(t)
			combo := pinnedCombo("c")
			combo.AccountStrategy = AccountStrategyQuotaAware
			p := accountPlugin(t, combo)
			p.accounts = accountRouterFor(snap)
			_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
			if err == nil {
				t.Fatal("want failure")
			}
			if len(host.calls) != 0 {
				t.Fatalf("a bad pool must not fall back to an unpinned call: %v", host.authIDs())
			}
		})
	}
}

func TestPoolFailureStillAllowsConfiguredMemberFallback(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("fb") }}
	host.install(t)
	combo := pinnedCombo("c")
	combo.Members = append(combo.Members, Member{Model: "paid", Role: "fallback", AccountStrategy: AccountStrategyInherit})
	p := accountPlugin(t, combo)
	p.accounts = accountRouterFor(stale)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatalf("executeCombo: %v", err)
	}
	if ids := host.authIDs(); len(ids) != 1 || ids[0] != "paid@" {
		t.Fatalf("only the explicit fallback member may run: %v", ids)
	}
}

func TestSelfAndKnownComboReferencesRejectedForExplicitStrategies(t *testing.T) {
	for _, model := range []string{"c", "other-combo"} {
		t.Run(model, func(t *testing.T) {
			host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("x") }}
			host.install(t)
			combo := Combo{Name: "c", AccountStrategy: AccountStrategyRoundRobin, Members: []Member{{Model: model, AccountPool: "claude-ab"}}}
			p := accountPlugin(t, combo, mustCombo(t, "other-combo", StrategyFailover, "claude-sonnet"))
			_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
			var ce *comboError
			if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
				t.Fatalf("err = %v", err)
			}
			if len(host.calls) != 0 {
				t.Fatal("combo reference must never reach the host")
			}
		})
	}
}

func TestContextCancellationStopsAccountRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(cbCtx())
	host := &accountHost{}
	host.reply = func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		cancel()
		return quotaFailure()
	}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	_, err := p.executeCombo(ctx, pinnedReq("c"), "c")
	if err == nil {
		t.Fatal("want an error")
	}
	if len(host.calls) != 1 {
		t.Fatalf("cancelled context must stop retries: %v", host.authIDs())
	}
}

func TestAccountAffinityBindsOnlyOnSuccess(t *testing.T) {
	var failOn string
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.AuthID == failOn {
			return quotaFailure()
		}
		return okReply("ok")
	}}
	host.install(t)
	combo := pinnedCombo("c")
	p := accountPlugin(t, combo)
	req := pinnedReq("c")
	key := accountBindingKey("scope-A", sessionIdentity(req), p.store.state.Combos["c"], p.store.state.Combos["c"].Members[0], AccountStrategyRoundRobin)
	if key == "" {
		t.Fatal("test key empty")
	}

	// A failing request binds nothing.
	host.reply = func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return quotaFailure() }
	if _, err := p.executeCombo(cbCtx(), req, "c"); err == nil {
		t.Fatal("want exhaustion")
	}
	if _, ok := p.accountBindings.Lookup(key); ok {
		t.Fatal("a failed completion must not bind")
	}

	// A success binds the serving credential and the next request leads with it.
	host.reply = func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if call.req.AuthID == failOn {
			return quotaFailure()
		}
		return okReply("ok")
	}
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatal(err)
	}
	bound, ok := p.accountBindings.Lookup(key)
	if !ok {
		t.Fatal("success must bind")
	}
	for i := 0; i < 4; i++ {
		host.calls = nil
		if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
			t.Fatal(err)
		}
		if host.calls[0].req.AuthID != bound {
			t.Fatalf("bound credential must lead: got %s want %s", host.calls[0].req.AuthID, bound)
		}
	}

	// The bound credential failing drops the binding; the next credential's success rebinds.
	failOn = bound
	host.calls = nil
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatal(err)
	}
	rebound, ok := p.accountBindings.Lookup(key)
	if !ok || rebound == bound {
		t.Fatalf("failed binding must be replaced, got %q (was %q)", rebound, bound)
	}
}

func TestAccountAffinityNeverBindsWithoutCallerScopeOrAcrossCallers(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	req := pinnedReq("c")
	req.Metadata = nil
	if _, err := p.executeCombo(cbCtx(), req, "c"); err != nil {
		t.Fatal(err)
	}
	if n := len(p.accountBindings.entries); n != 0 {
		t.Fatalf("missing caller scope must not create a binding, have %d", n)
	}
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatal(err)
	}
	if n := len(p.accountBindings.entries); n != 1 {
		t.Fatalf("bindings = %d", n)
	}
	other := pinnedReq("c")
	other.Metadata = map[string]any{pluginapi.CallerScopeMetadataKey: "scope-B"}
	if _, err := p.executeCombo(cbCtx(), other, "c"); err != nil {
		t.Fatal(err)
	}
	if n := len(p.accountBindings.entries); n != 2 {
		t.Fatalf("callers must not share bindings, have %d", n)
	}
}

func TestAccountAffinityDisabledDoesNotBind(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	combo := pinnedCombo("c")
	combo.SessionAffinity = boolPtr(false)
	p := accountPlugin(t, combo)
	if _, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c"); err != nil {
		t.Fatal(err)
	}
	if n := len(p.accountBindings.entries); n != 0 {
		t.Fatalf("affinity off must not bind, have %d", n)
	}
}

func TestSameModelCombosKeepIndependentAccountStateConcurrently(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	one := pinnedCombo("one") // pool claude-ab
	two := Combo{Name: "two", AccountStrategy: AccountStrategyRoundRobin, Strategy: StrategyFailover,
		Members: []Member{{Model: "claude-sonnet", AccountPool: "claude-oauth"}}}
	p := accountPlugin(t, one, two)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		for _, name := range []string{"one", "two"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := pinnedReq(name)
				req.Metadata = map[string]any{pluginapi.CallerScopeMetadataKey: fmt.Sprintf("scope-%d", i)}
				if _, err := p.executeCombo(cbCtx(), req, name); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	// Pool isolation: combo "one" may never pin c1 (outside claude-ab); combo "two" may.
	sawC1 := false
	for _, call := range host.calls {
		if call.req.AuthID == "c1" {
			sawC1 = true
		}
	}
	if !sawC1 {
		t.Fatal("combo two (claude-oauth) should have rotated onto c1")
	}
}

func TestPoolIsolationBetweenCombosSharingAModel(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return okReply("ok") }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("one"))
	for i := 0; i < 30; i++ {
		if _, err := p.executeCombo(cbCtx(), pinnedReq("one"), "one"); err != nil {
			t.Fatal(err)
		}
	}
	for _, call := range host.calls {
		if call.req.AuthID == "c1" || call.req.AuthID == "x1" {
			t.Fatalf("combo one escaped its pool with %s", call.req.AuthID)
		}
	}
}
