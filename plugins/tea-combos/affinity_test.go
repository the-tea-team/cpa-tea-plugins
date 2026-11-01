package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func newAffinityPlugin(t *testing.T, clock *fakeClock) *combosPlugin {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return &combosPlugin{
		store:      store,
		roundRobin: newRoundRobinSelector(),
		affinity:   newAffinityWithClock(nil, nil, clock.Now),
	}
}

func sessionRequest(comboName, sessionID string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{
		Model:   comboName,
		Payload: []byte(`{"model":"` + comboName + `"}`),
		Headers: http.Header{"X-Session-Id": {sessionID}},
	}
}

func succeed(body string) func(context.Context, pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
	return func(_ context.Context, _ pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(body)}, nil
	}
}

func TestAffinityStickinessRoundRobin(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1", "m2")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	var served []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		served = append(served, req.Model)
		mu.Unlock()
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	requests := []string{"session-a", "session-a", "session-b", "session-a", "session-b"}
	for _, session := range requests {
		if _, err := plugin.Execute(context.Background(), sessionRequest("fleet", session)); err != nil {
			t.Fatalf("Execute(%s): %v", session, err)
		}
	}

	want := []string{"m0", "m0", "m2", "m0", "m2"}
	if got := strings.Join(served, ","); got != strings.Join(want, ",") {
		t.Fatalf("served = %v, want %v (bound session must not rotate, another session must)", served, want)
	}
}

func TestAffinityStickinessFailoverAfterRebind(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "m0", "m1", "m2")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	var served []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		served = append(served, req.Model)
		mu.Unlock()
		if req.Model == "m0" {
			return nil, &hostError{Status: http.StatusServiceUnavailable, Retryable: true, Message: "down"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	for range 2 {
		if _, err := plugin.Execute(context.Background(), sessionRequest("fleet", "session-a")); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	// First request tries m0 then binds m1; the second must start at the bound m1.
	if got := strings.Join(served, ","); got != "m0,m1,m1" {
		t.Fatalf("served = %v, want m0,m1 then m1 only", served)
	}
}

func TestAffinityTTLDefaultAndOverride(t *testing.T) {
	cases := []struct {
		name  string
		ttl   string
		wait  time.Duration
		bound bool
	}{
		{name: "default 1h inside", ttl: "", wait: 59 * time.Minute, bound: true},
		{name: "default 1h expired", ttl: "", wait: 61 * time.Minute, bound: false},
		{name: "override 10s inside", ttl: "10s", wait: 9 * time.Second, bound: true},
		{name: "override 10s expired", ttl: "10s", wait: 11 * time.Second, bound: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			plugin := newAffinityPlugin(t, clock)
			combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
			combo.TTL = tc.ttl
			if err := plugin.store.Upsert(combo); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			request := sessionRequest("fleet", "session-a")
			key, ok := deriveSessionKey(combo, request)
			if !ok {
				t.Fatal("session key not derived")
			}
			plugin.affinity.Bind(key, 0, combo)
			clock.Advance(tc.wait)

			_, preferred, bound := plugin.affinity.Resolve(combo, request)
			if bound != tc.bound {
				t.Fatalf("bound = %v, want %v after %s", bound, tc.bound, tc.wait)
			}
			if tc.bound && preferred != 0 {
				t.Fatalf("preferred = %d, want 0", preferred)
			}
		})
	}
}

func TestAffinityRebindsOnRetryableFailure(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "m0", "m1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	failFirst := false
	var served []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		served = append(served, req.Model)
		if req.Model == "m0" && failFirst {
			return nil, &hostError{Status: http.StatusBadGateway, Retryable: true, Message: "boom"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := sessionRequest("fleet", "session-a")
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("first Execute: %v", err)
	}

	mu.Lock()
	failFirst = true
	mu.Unlock()
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	mu.Lock()
	failFirst = false
	served = nil
	mu.Unlock()
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("third Execute: %v", err)
	}

	combo, _ := plugin.store.Get("fleet")
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		t.Fatal("session key not derived")
	}
	_, preferred, bound := plugin.affinity.Resolve(combo, request)
	if !bound || preferred != 1 {
		t.Fatalf("binding = (%d, %v), want member 1 after rebind", preferred, bound)
	}
	if key == "" {
		t.Fatal("key must not be empty for a derived session")
	}
}

func TestAffinityNonRetryableErrorStopsChainAndKeepsBinding(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "m0", "m1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	reject := false
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts = append(attempts, req.Model)
		if req.Model == "m0" && reject {
			return nil, &hostError{Status: http.StatusBadRequest, Message: "client error"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := sessionRequest("fleet", "session-a")
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	mu.Lock()
	reject = true
	attempts = nil
	mu.Unlock()

	_, err := plugin.Execute(context.Background(), request)
	var comboErr *comboError
	if !errors.As(err, &comboErr) || comboErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want non-retryable 400", err)
	}
	if got := strings.Join(attempts, ","); got != "m0" {
		t.Fatalf("attempts = %v, chain must stop at the rejected member", attempts)
	}

	combo, _ := plugin.store.Get("fleet")
	_, preferred, bound := plugin.affinity.Resolve(combo, request)
	if !bound || preferred != 0 {
		t.Fatalf("binding = (%d, %v), want member 0 unchanged", preferred, bound)
	}
}

func TestAffinityAllMembersFailLeavesBindingUnchanged(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	failAll := false
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if failAll {
			return nil, &hostError{Status: http.StatusServiceUnavailable, Retryable: true, Message: "down"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := sessionRequest("fleet", "session-a")
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	mu.Lock()
	failAll = true
	mu.Unlock()
	if _, err := plugin.Execute(context.Background(), request); err == nil {
		t.Fatal("all members failing must return an error")
	}

	combo, _ := plugin.store.Get("fleet")
	_, preferred, bound := plugin.affinity.Resolve(combo, request)
	if !bound || preferred != 0 {
		t.Fatalf("binding = (%d, %v), want member 0 unchanged", preferred, bound)
	}
}

func TestAffinityIdentityChangeInvalidatesBinding(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	request := sessionRequest("fleet", "session-a")
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		t.Fatal("session key not derived")
	}
	plugin.affinity.Bind(key, 0, combo)

	edited := combo
	edited.Members = []Member{{Model: "replacement"}, {Model: "m1"}}
	_, preferred, bound := plugin.affinity.Resolve(edited, request)
	if bound {
		t.Fatalf("binding to a replaced member must be invalid, preferred = %d", preferred)
	}
}

func TestAffinityDisabledBehavesAsPlainStrategy(t *testing.T) {
	plugin := newAffinityPlugin(t, newFakeClock())
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
	combo.SessionAffinity = boolPtr(false)
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var mu sync.Mutex
	var served []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		mu.Lock()
		served = append(served, req.Model)
		mu.Unlock()
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	for range 2 {
		if _, err := plugin.Execute(context.Background(), sessionRequest("fleet", "session-a")); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if got := strings.Join(served, ","); got != "m0,m1" {
		t.Fatalf("served = %v, want plain round-robin rotation", served)
	}
}

func TestAffinityLRUEviction(t *testing.T) {
	clock := newFakeClock()
	affinity := newAffinityWithClock(nil, nil, clock.Now)
	affinity.capacity = 2
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")

	for _, key := range []string{"k0", "k1", "k2"} {
		affinity.Bind(key, 0, combo)
	}

	if len(affinity.entries) != 2 {
		t.Fatalf("entries = %d, want cap 2", len(affinity.entries))
	}
	if _, evicted := affinity.entries["k0"]; evicted {
		t.Fatal("least recently used binding k0 must be evicted")
	}
	for _, key := range []string{"k1", "k2"} {
		if _, exists := affinity.entries[key]; !exists {
			t.Fatalf("binding %q must survive", key)
		}
	}
}

func TestAffinitySlidingTTLRefreshesOnSuccessfulUse(t *testing.T) {
	clock := newFakeClock()
	plugin := newAffinityPlugin(t, clock)
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	combo.TTL = "10s"
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	request := sessionRequest("fleet", "session-a")
	key, _ := deriveSessionKey(combo, request)

	plugin.affinity.Bind(key, 0, combo)
	clock.Advance(9 * time.Second)
	if _, _, bound := plugin.affinity.Resolve(combo, request); !bound {
		t.Fatal("binding must still be live before the ttl")
	}
	// A successful use refreshes the expiry (Bind is only called on success in the executor).
	plugin.affinity.Bind(key, 0, combo)
	clock.Advance(9 * time.Second)
	if _, _, bound := plugin.affinity.Resolve(combo, request); !bound {
		t.Fatal("successful use must slide the expiry")
	}
	clock.Advance(2 * time.Second)
	if _, _, bound := plugin.affinity.Resolve(combo, request); bound {
		t.Fatal("binding must expire once the slid ttl passes")
	}
}

func TestAffinityCloseFlushesPendingBindings(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	affinity := newAffinityWithClock(nil, store.SaveBindings, clock.Now)
	request := sessionRequest("fleet", "session-a")
	key, _ := deriveSessionKey(combo, request)
	affinity.Bind(key, 0, combo)
	affinity.Close()

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reopen: %v", err)
	}
	if len(reopened.LoadBindings()) != 1 {
		t.Fatal("Close must flush pending bindings to the state file")
	}
}

func TestAffinityPersistenceFailureDoesNotBreakRequests(t *testing.T) {
	clock := newFakeClock()
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	failures := 0
	affinity := newAffinityWithClock(nil, func(map[string]AffinityBinding) error {
		failures++
		return errors.New("disk full")
	}, clock.Now)
	request := sessionRequest("fleet", "session-a")
	key, _ := deriveSessionKey(combo, request)

	affinity.Bind(key, 0, combo)
	if err := affinity.Flush(); err == nil {
		t.Fatal("Flush must surface the write error")
	}
	if affinity.LastError() == nil {
		t.Fatal("LastError must remember the failed write")
	}
	if _, preferred, bound := affinity.Resolve(combo, request); !bound || preferred != 0 {
		t.Fatal("a failed write must not drop the in-memory binding")
	}
	// The binding stays dirty and is retried on the next flush.
	if err := affinity.Flush(); err == nil {
		t.Fatal("second Flush must retry the failed write")
	}
	if failures != 2 {
		t.Fatalf("persist attempts = %d, want 2", failures)
	}
}

func TestAffinityPersistenceRoundTrip(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	affinity := newAffinityWithClock(nil, store.SaveBindings, clock.Now)
	request := sessionRequest("fleet", "session-a")
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		t.Fatal("session key not derived")
	}
	affinity.Bind(key, 1, combo)
	if err := affinity.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reopen: %v", err)
	}
	restoredCombo, ok := reopened.Get("fleet")
	if !ok {
		t.Fatal("combo missing after bindings write")
	}
	if len(restoredCombo.Members) != 2 || restoredCombo.Members[0].Model != "m0" {
		t.Fatalf("combo rewritten by bindings write: %+v", restoredCombo)
	}
	restored := newAffinityWithClock(reopened.LoadBindings(), nil, clock.Now)
	_, preferred, bound := restored.Resolve(restoredCombo, request)
	if !bound || preferred != 1 {
		t.Fatalf("restored binding = (%d, %v), want member 1", preferred, bound)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state file mode = %o, want 600", mode)
	}
}

func TestAffinityExpiredBindingsDroppedOnLoad(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	combo.TTL = "10s"
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	affinity := newAffinityWithClock(nil, store.SaveBindings, clock.Now)
	request := sessionRequest("fleet", "session-a")
	key, _ := deriveSessionKey(combo, request)
	affinity.Bind(key, 0, combo)
	if err := affinity.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	clock.Advance(11 * time.Second)
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reopen: %v", err)
	}
	loaded := reopened.LoadBindings()
	if len(loaded) != 1 {
		t.Fatalf("persisted bindings = %d, want 1 (expiry is evaluated on load)", len(loaded))
	}
	restored := newAffinityWithClock(loaded, nil, clock.Now)
	if len(restored.entries) != 0 {
		t.Fatalf("entries = %d, want expired bindings dropped on load", len(restored.entries))
	}
	if _, _, bound := restored.Resolve(combo, request); bound {
		t.Fatal("expired binding must not resolve")
	}
}

func TestAffinityStateFileNeverStoresRawIdentity(t *testing.T) {
	clock := newFakeClock()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	affinity := newAffinityWithClock(nil, store.SaveBindings, clock.Now)

	const prompt = "a very secret prompt the state file must never contain"
	const sessionID = "raw-header-session-value"
	request := pluginapi.ExecutorRequest{
		Model:   "fleet",
		Payload: []byte(`{"model":"fleet","messages":[{"role":"user","content":"` + prompt + `"}]}`),
	}
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		t.Fatal("session key not derived from the first user message")
	}
	affinity.Bind(key, 0, combo)

	headerRequest := sessionRequest("fleet", sessionID)
	headerKey, ok := deriveSessionKey(combo, headerRequest)
	if !ok {
		t.Fatal("session key not derived from the header")
	}
	affinity.Bind(headerKey, 0, combo)
	if err := affinity.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, secret := range []string{prompt, sessionID} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("state file leaked %q: %s", secret, raw)
		}
	}
	var doc struct {
		Bindings map[string]json.RawMessage `json:"bindings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("state file is not JSON: %v", err)
	}
	if len(doc.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2", len(doc.Bindings))
	}
}
