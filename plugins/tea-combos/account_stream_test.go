package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// accountStreamHost is an in-memory host whose streams are scripted per pinned credential.
type accountStreamHost struct {
	mu      sync.Mutex
	opens   []recordedCall
	scripts map[string][]pluginapi.HostModelStreamReadResponse // key: auth id (or model when unpinned)
	openErr map[string]error
	streams map[string]string
	pos     map[string]int
	closed  []string
	emitted []string
	closeCh chan *pluginabi.Error
}

func newAccountStreamHost(t *testing.T) *accountStreamHost {
	t.Helper()
	h := &accountStreamHost{
		scripts: map[string][]pluginapi.HostModelStreamReadResponse{},
		openErr: map[string]error{},
		streams: map[string]string{},
		pos:     map[string]int{},
		closeCh: make(chan *pluginabi.Error, 8),
	}
	pe, pr, pc, pm, pcl := hostModelExecuteStream, hostModelStreamRead, hostModelStreamClose, pluginStreamEmit, pluginStreamClose
	hostModelExecuteStream = func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.opens = append(h.opens, recordedCall{req: req, callbackID: hostCallbackIDFromContext(ctx)})
		key := req.AuthID
		if key == "" {
			key = req.Model
		}
		if err := h.openErr[key]; err != nil {
			return nil, err
		}
		id := fmt.Sprintf("%s#%d", key, len(h.opens))
		h.streams[id] = key
		return &pluginapi.HostModelStreamResponse{StatusCode: 200, StreamID: id, Headers: http.Header{"X-Served-By": {"host"}}}, nil
	}
	hostModelStreamRead = func(_ context.Context, id string) (*pluginapi.HostModelStreamReadResponse, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		script := h.scripts[h.streams[id]]
		i := h.pos[id]
		if i >= len(script) {
			return &pluginapi.HostModelStreamReadResponse{Done: true}, nil
		}
		h.pos[id] = i + 1
		item := script[i]
		return &item, nil
	}
	hostModelStreamClose = func(id string) {
		h.mu.Lock()
		h.closed = append(h.closed, id)
		h.mu.Unlock()
	}
	pluginStreamEmit = func(_ context.Context, _ string, payload []byte) error {
		h.mu.Lock()
		h.emitted = append(h.emitted, string(payload))
		h.mu.Unlock()
		return nil
	}
	pluginStreamClose = func(_ string, detail *pluginabi.Error) { h.closeCh <- detail }
	t.Cleanup(func() {
		hostModelExecuteStream, hostModelStreamRead, hostModelStreamClose, pluginStreamEmit, pluginStreamClose = pe, pr, pc, pm, pcl
	})
	return h
}

func (h *accountStreamHost) openedAuth() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.opens))
	for _, c := range h.opens {
		out = append(out, c.req.Model+"@"+c.req.AuthID)
	}
	return out
}

func (h *accountStreamHost) closedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.closed)
}

func streamAccountPlugin(t *testing.T, combos ...Combo) *combosPlugin {
	t.Helper()
	return accountPlugin(t, combos...)
}

func runAccountStream(p *combosPlugin, ctx context.Context, name string) (*comboStreamHandle, []string, error) {
	var emitted []string
	_, _, handle, err := p.executeComboStream(ctx, pinnedReq(name), name, -1, func(b []byte) error {
		emitted = append(emitted, string(b))
		return nil
	})
	return handle, emitted, err
}

func TestStreamPinsEveryAttemptWithCallbackAndOriginalCaller(t *testing.T) {
	h := newAccountStreamHost(t)
	for _, id := range []string{"a1", "a2", "b1"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("hello")}, {Done: true}}
	}
	p := streamAccountPlugin(t, pinnedCombo("c"))
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	handle.Close()
	if len(emitted) != 1 || emitted[0] != "hello" {
		t.Fatalf("emitted = %v", emitted)
	}
	call := h.opens[0]
	if call.callbackID != "cb-7" || call.req.CallerScope != "scope-A" || call.req.AuthID == "" || call.req.ForcedProvider != "claude" || !call.req.Stream {
		t.Fatalf("stream pin lost context: %+v cb=%q", call.req, call.callbackID)
	}
	if call.req.Headers.Get("Authorization") != "Bearer client" || call.req.Query.Get("beta") != "1" {
		t.Fatalf("headers/query not preserved: %v %v", call.req.Headers, call.req.Query)
	}
}

func TestStreamRequiresCallbackIDForExplicitStrategy(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	_, _, err := runAccountStream(p, context.Background(), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v", err)
	}
	if len(h.opens) != 0 {
		t.Fatalf("no stream may open without a callback id: %v", h.openedAuth())
	}
}

func TestStreamFailsOverAcrossAccountsBeforeFirstPayload(t *testing.T) {
	h := newAccountStreamHost(t)
	combo := pinnedCombo("c")
	combo.Members[0].AccountPool = "claude-oauth" // a1, a2 (shared), b1, c1
	p := streamAccountPlugin(t, combo)
	// Fail by open error and by pre-payload read error; only c1 serves, whatever order is planned.
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.scripts["a2"] = []pluginapi.HostModelStreamReadResponse{{Error: "upstream reset", Done: true}}
	h.scripts["b1"] = []pluginapi.HostModelStreamReadResponse{{Error: "upstream reset", Done: true}}
	h.scripts["c1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("from-c1")}, {Done: true}}
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	handle.Close()
	if len(emitted) != 1 || emitted[0] != "from-c1" {
		t.Fatalf("emitted = %v (opens %v)", emitted, h.openedAuth())
	}
	opens := h.openedAuth()
	seen := map[string]bool{}
	for _, o := range opens {
		if seen[o] {
			t.Fatalf("each credential at most once: %v", opens)
		}
		seen[o] = true
	}
	// a1 failed at open (no stream exists). Every stream that did open must be closed by now:
	// the failed ones before moving on, the served one by handle.Close above.
	streamsOpened := 0
	for _, o := range opens {
		if o != "claude-sonnet@a1" {
			streamsOpened++
		}
	}
	if got := h.closedCount(); got != streamsOpened {
		t.Fatalf("failed host streams must be closed: closed=%d opens=%v", got, opens)
	}
}

func TestStreamNoReplayAfterFirstPayload(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	for _, id := range []string{"a1", "a2", "b1"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("first")}, {Error: "boom", Done: true}}
	}
	_, _, handle, err := p.executeComboStream(cbCtx(), pinnedReq("c"), "c", -1, func([]byte) error { return nil })
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v", handle, err)
	}
	pumpErr := handle.Pump(cbCtx(), "s")
	handle.Close()
	if pumpErr == nil {
		t.Fatal("a failure after the first payload must surface")
	}
	if opens := h.openedAuth(); len(opens) != 1 {
		t.Fatalf("no replay after first payload: %v", opens)
	}
}

func TestStreamBindsAccountOnlyAfterFirstPayload(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	combo := p.store.state.Combos["c"]
	key := accountBindingKey("scope-A", sessionIdentity(pinnedReq("c")), combo, combo.Members[0], AccountStrategyRoundRobin)

	for _, id := range []string{"a1", "a2", "b1"} {
		h.scripts[id] = []pluginapi.HostModelStreamReadResponse{{Error: "reset", Done: true}}
	}
	if _, _, err := runAccountStream(p, cbCtx(), "c"); err == nil {
		t.Fatal("want exhaustion")
	}
	if _, ok := p.accountBindings.Lookup(key); ok {
		t.Fatal("a stream that never produced a payload must not bind")
	}
	h.mu.Lock()
	h.pos = map[string]int{}
	h.scripts["a1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("x")}, {Done: true}}
	h.scripts["a2"] = h.scripts["a1"]
	h.scripts["b1"] = h.scripts["a1"]
	h.mu.Unlock()
	handle, _, err := runAccountStream(p, cbCtx(), "c")
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	if _, ok := p.accountBindings.Lookup(key); !ok {
		t.Fatal("first payload must bind the serving account")
	}
}

func TestStreamMemberCoolsOnlyAfterAllAccountsFail(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	member := p.store.state.Combos["c"].Members[0]
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.scripts["a2"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("ok")}, {Done: true}}
	h.scripts["b1"] = h.scripts["a2"]
	handle, _, err := runAccountStream(p, cbCtx(), "c")
	if err != nil {
		t.Fatal(err)
	}
	handle.Close()
	if p.cooldown.Active("c", member) {
		t.Fatal("member must not cool while another account served")
	}
	for _, id := range []string{"a1", "a2", "b1"} {
		h.openErr[id] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	}
	if _, _, err := runAccountStream(p, cbCtx(), "c"); err == nil {
		t.Fatal("want exhaustion")
	}
	if !p.cooldown.Active("c", member) {
		t.Fatal("member must cool after every account failed")
	}
}

func TestStreamNonMemberErrorStopsAccountRetries(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	for _, id := range []string{"a1", "a2", "b1"} {
		h.openErr[id] = &hostError{Message: "model_not_allowed", Status: 403}
	}
	if _, _, err := runAccountStream(p, cbCtx(), "c"); err == nil {
		t.Fatal("want denial")
	}
	if opens := h.openedAuth(); len(opens) != 1 {
		t.Fatalf("a denial must not rotate accounts: %v", opens)
	}
}

func TestStreamPoolFailureNeverUnpins(t *testing.T) {
	h := newAccountStreamHost(t)
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(stale)
	if _, _, err := runAccountStream(p, cbCtx(), "c"); err == nil {
		t.Fatal("want failure")
	}
	if len(h.opens) != 0 {
		t.Fatalf("stale pool must not open an unpinned stream: %v", h.openedAuth())
	}
}

func TestStreamSelfComboReferenceRejected(t *testing.T) {
	h := newAccountStreamHost(t)
	combo := Combo{Name: "c", AccountStrategy: AccountStrategyRoundRobin, Members: []Member{{Model: "c", AccountPool: "claude-ab"}}}
	p := streamAccountPlugin(t, combo)
	_, _, err := runAccountStream(p, cbCtx(), "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest || len(h.opens) != 0 {
		t.Fatalf("err=%v opens=%v", err, h.openedAuth())
	}
}

func TestStreamAttemptsBoundedAndCancellable(t *testing.T) {
	h := newAccountStreamHost(t)
	p := streamAccountPlugin(t, pinnedCombo("c"))
	ctx, cancel := context.WithCancel(cbCtx())
	h.openErr["a1"] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	h.openErr["a2"] = h.openErr["a1"]
	h.openErr["b1"] = h.openErr["a1"]
	cancel()
	_, _, err := runAccountStream(p, ctx, "c")
	var ce *comboError
	if !errors.As(err, &ce) || ce.Status != http.StatusRequestTimeout || len(h.opens) != 0 {
		t.Fatalf("cancelled stream must not open anything: err=%v opens=%v", err, h.openedAuth())
	}
	if strings.Contains(fmt.Sprint(err), "a1") {
		t.Fatalf("errors must never name credential ids: %v", err)
	}
}
