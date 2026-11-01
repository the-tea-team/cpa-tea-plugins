package main

import (
	"context"
	"encoding/json"
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

// fakeStreamHost is an in-memory CPA host for the streaming seams. Scripts are keyed by member
// model; each opened stream gets its own id and replays that member's script.
type fakeStreamHost struct {
	mu         sync.Mutex
	opened     []string
	scripts    map[string][]pluginapi.HostModelStreamReadResponse
	openErrors map[string]error
	openStatus map[string]int
	headers    http.Header
	readHook   func(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error)
	streams    map[string]string // stream id -> model
	positions  map[string]int
	closed     []string
	emitted    []string
	emitErr    error
	emitSignal chan string
	closeCh    chan *pluginabi.Error
}

func newFakeStreamHost(t *testing.T) *fakeStreamHost {
	t.Helper()
	fake := &fakeStreamHost{
		scripts:    make(map[string][]pluginapi.HostModelStreamReadResponse),
		openErrors: make(map[string]error),
		openStatus: make(map[string]int),
		streams:    make(map[string]string),
		positions:  make(map[string]int),
		headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
		emitSignal: make(chan string, 64),
		closeCh:    make(chan *pluginabi.Error, 16),
	}
	previousExecute := hostModelExecuteStream
	previousRead := hostModelStreamRead
	previousClose := hostModelStreamClose
	previousEmit := pluginStreamEmit
	previousClientClose := pluginStreamClose
	hostModelExecuteStream = func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelStreamResponse, error) {
		return fake.open(req.Model)
	}
	hostModelStreamRead = fake.read
	hostModelStreamClose = fake.close
	pluginStreamEmit = fake.emit
	pluginStreamClose = func(streamID string, detail *pluginabi.Error) {
		fake.mu.Lock()
		fake.closed = append(fake.closed, streamID)
		fake.mu.Unlock()
		fake.closeCh <- detail
	}
	t.Cleanup(func() {
		hostModelExecuteStream = previousExecute
		hostModelStreamRead = previousRead
		hostModelStreamClose = previousClose
		pluginStreamEmit = previousEmit
		pluginStreamClose = previousClientClose
	})
	return fake
}

func (f *fakeStreamHost) open(model string) (*pluginapi.HostModelStreamResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, model)
	if err := f.openErrors[model]; err != nil {
		return nil, err
	}
	status := f.openStatus[model]
	if status == 0 {
		status = http.StatusOK
	}
	id := fmt.Sprintf("%s#%d", model, len(f.opened))
	f.streams[id] = model
	return &pluginapi.HostModelStreamResponse{StatusCode: status, Headers: f.headers.Clone(), StreamID: id}, nil
}

func (f *fakeStreamHost) read(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.readHook != nil {
		hook := f.readHook
		f.mu.Unlock()
		return hook(ctx, streamID)
	}
	model := f.streams[streamID]
	script := f.scripts[model]
	position := f.positions[streamID]
	if position >= len(script) {
		f.mu.Unlock()
		return &pluginapi.HostModelStreamReadResponse{Done: true}, nil
	}
	f.positions[streamID] = position + 1
	chunk := script[position]
	f.mu.Unlock()
	return &chunk, nil
}

func (f *fakeStreamHost) close(streamID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, streamID)
}

func (f *fakeStreamHost) emit(_ context.Context, _ string, payload []byte) error {
	f.mu.Lock()
	if f.emitErr != nil {
		err := f.emitErr
		f.mu.Unlock()
		return err
	}
	f.emitted = append(f.emitted, string(payload))
	f.mu.Unlock()
	f.emitSignal <- string(payload)
	return nil
}

func (f *fakeStreamHost) emittedChunks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.emitted...)
}

func (f *fakeStreamHost) openedModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

// waitForClose drains one plugin-level stream close (the terminal host.stream.close call).
func (f *fakeStreamHost) waitForClose(t *testing.T) *pluginabi.Error {
	t.Helper()
	select {
	case detail := <-f.closeCh:
		return detail
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host stream close")
		return nil
	}
}

func chunk(payload string) pluginapi.HostModelStreamReadResponse {
	return pluginapi.HostModelStreamReadResponse{Payload: []byte(payload)}
}

func retryableHostError(status int) error {
	return &hostError{Method: pluginabi.MethodHostModelExecuteStream, Code: "host_call_failed", Message: "upstream overloaded", Status: status, Retryable: true}
}

func streamTestPlugin(t *testing.T, combo Combo) *combosPlugin {
	t.Helper()
	plugin := newTestPlugin(t)
	if err := plugin.store.MergeSeed([]Combo{combo}); err != nil {
		t.Fatalf("MergeSeed: %v", err)
	}
	return plugin
}

func TestExecuteComboStreamFallbackBeforeFirstByte(t *testing.T) {
	cases := []struct {
		name     string
		combo    Combo
		arrange  func(f *fakeStreamHost)
		wantOpen []string
		want     []string
		served   int
	}{
		{
			name:  "execute_stream error is retryable",
			combo: mustCombo(t, "c", StrategyFailover, "bad", "good"),
			arrange: func(f *fakeStreamHost) {
				f.openErrors["bad"] = retryableHostError(http.StatusServiceUnavailable)
				f.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("a"), chunk("b"), {Done: true}}
			},
			wantOpen: []string{"bad", "good"},
			want:     []string{"a", "b"},
			served:   1,
		},
		{
			name:  "first read error is retryable before any payload",
			combo: mustCombo(t, "c", StrategyFailover, "bad", "good"),
			arrange: func(f *fakeStreamHost) {
				f.readHook = func(_ context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
					if strings.HasPrefix(streamID, "bad#") {
						return &pluginapi.HostModelStreamReadResponse{Error: "upstream reset", Done: true}, nil
					}
					f.mu.Lock()
					defer f.mu.Unlock()
					position := f.positions[streamID]
					script := f.scripts[f.streams[streamID]]
					if position >= len(script) {
						return &pluginapi.HostModelStreamReadResponse{Done: true}, nil
					}
					f.positions[streamID] = position + 1
					return &script[position], nil
				}
				f.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("a"), {Done: true}}
			},
			wantOpen: []string{"bad", "good"},
			want:     []string{"a"},
			served:   1,
		},
		{
			name:  "empty clean stream is a successful member",
			combo: mustCombo(t, "c", StrategyFailover, "empty", "good"),
			arrange: func(f *fakeStreamHost) {
				f.scripts["empty"] = []pluginapi.HostModelStreamReadResponse{{Done: true}}
				f.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("never"), {Done: true}}
			},
			wantOpen: []string{"empty"},
			want:     nil,
			served:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStreamHost(t)
			tc.arrange(fake)
			plugin := streamTestPlugin(t, tc.combo)
			ctx := withPluginStreamID(context.Background(), "client-stream")
			resp, err := plugin.executeStream(ctx, pluginapi.ExecutorRequest{Model: "c"})
			if err != nil {
				t.Fatalf("executeStream: %v", err)
			}
			if resp.Headers.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("headers = %v, want the serving member's headers", resp.Headers)
			}
			fake.waitForClose(t)
			if got := fake.openedModels(); !equalStrings(got, tc.wantOpen) {
				t.Fatalf("opened members = %v, want %v", got, tc.wantOpen)
			}
			if got := fake.emittedChunks(); !equalStrings(got, tc.want) {
				t.Fatalf("emitted chunks = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecuteComboStreamNonRetryableStopsBeforeFirstByte(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.openErrors["bad"] = &hostError{Code: "host_call_failed", Message: "invalid request", Status: http.StatusBadRequest}
	fake.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("never"), {Done: true}}
	plugin := streamTestPlugin(t, mustCombo(t, "c", StrategyFailover, "bad", "good"))

	_, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), pluginapi.ExecutorRequest{Model: "c"})
	var comboErr *comboError
	if !errors.As(err, &comboErr) {
		t.Fatalf("error = %v, want comboError", err)
	}
	if comboErr.Status != http.StatusBadRequest || comboErr.Retryable {
		t.Fatalf("comboError = %+v, want 400 non-retryable", comboErr)
	}
	if got := fake.openedModels(); !equalStrings(got, []string{"bad"}) {
		t.Fatalf("opened members = %v, want only the rejected member", got)
	}
	if got := fake.emittedChunks(); len(got) != 0 {
		t.Fatalf("emitted chunks = %v, want none", got)
	}
}

func TestExecuteComboStreamExhaustedKeepsLastStatusAndTrace(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.openErrors["bad1"] = retryableHostError(http.StatusServiceUnavailable)
	fake.openErrors["bad2"] = retryableHostError(http.StatusTooManyRequests)
	plugin := streamTestPlugin(t, mustCombo(t, "c", StrategyFailover, "bad1", "bad2"))

	_, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), pluginapi.ExecutorRequest{Model: "c"})
	var comboErr *comboError
	if !errors.As(err, &comboErr) {
		t.Fatalf("error = %v, want comboError", err)
	}
	if comboErr.Status != http.StatusTooManyRequests || !comboErr.Retryable {
		t.Fatalf("comboError = %+v, want last status 429 retryable", comboErr)
	}
	if !strings.Contains(comboErr.Message, `"member":"bad1"`) || !strings.Contains(comboErr.Message, `"member":"bad2"`) {
		t.Fatalf("message %q lacks the attempt trace", comboErr.Message)
	}
}

func TestExecuteStreamFailureAfterFirstByteIsFinal(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.readHook = func(_ context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		position := fake.positions[streamID]
		fake.positions[streamID] = position + 1
		switch position {
		case 0:
			return &pluginapi.HostModelStreamReadResponse{Payload: []byte("first")}, nil
		default:
			return &pluginapi.HostModelStreamReadResponse{Error: "upstream died mid-stream", Done: true}, nil
		}
	}
	fake.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("never"), {Done: true}}
	plugin := streamTestPlugin(t, mustCombo(t, "c", StrategyFailover, "cut", "good"))

	resp, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), pluginapi.ExecutorRequest{Model: "c"})
	if err != nil {
		t.Fatalf("executeStream: %v", err)
	}
	if resp.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers = %v", resp.Headers)
	}
	detail := fake.waitForClose(t)
	if detail == nil || !strings.Contains(detail.Message, "mid-stream") {
		t.Fatalf("terminal close detail = %+v, want the mid-stream failure", detail)
	}
	if got := fake.openedModels(); !equalStrings(got, []string{"cut"}) {
		t.Fatalf("opened members = %v, want only the member that already sent bytes", got)
	}
	if got := fake.emittedChunks(); !equalStrings(got, []string{"first"}) {
		t.Fatalf("emitted chunks = %v, want the forwarded first chunk", got)
	}
}

func TestExecuteStreamForwardsEachChunkBeforeReadingNext(t *testing.T) {
	fake := newFakeStreamHost(t)
	firstEmitted := make(chan struct{})
	var once sync.Once
	fake.readHook = func(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
		fake.mu.Lock()
		position := fake.positions[streamID]
		fake.positions[streamID] = position + 1
		fake.mu.Unlock()
		switch position {
		case 0:
			return &pluginapi.HostModelStreamReadResponse{Payload: []byte("one")}, nil
		case 1:
			// A buffering implementation would block here before emitting "one".
			select {
			case <-firstEmitted:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				return nil, errors.New("first chunk was not forwarded before the second read")
			}
			return &pluginapi.HostModelStreamReadResponse{Payload: []byte("two")}, nil
		case 2:
			return &pluginapi.HostModelStreamReadResponse{Payload: []byte("three")}, nil
		default:
			return &pluginapi.HostModelStreamReadResponse{Done: true}, nil
		}
	}
	plugin := streamTestPlugin(t, mustCombo(t, "c", StrategyFailover, "good"))
	// The emit seam signals the blocked read exactly when a chunk is forwarded.
	previousEmit := pluginStreamEmit
	pluginStreamEmit = func(ctx context.Context, streamID string, payload []byte) error {
		err := fake.emit(ctx, streamID, payload)
		once.Do(func() { close(firstEmitted) })
		return err
	}
	t.Cleanup(func() { pluginStreamEmit = previousEmit })

	if _, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), pluginapi.ExecutorRequest{Model: "c"}); err != nil {
		t.Fatalf("executeStream: %v", err)
	}
	fake.waitForClose(t)
	if got := fake.emittedChunks(); !equalStrings(got, []string{"one", "two", "three"}) {
		t.Fatalf("emitted chunks = %v, want ordered pass-through", got)
	}
}

func TestExecuteStreamClosesEveryOpenedHostStream(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(f *fakeStreamHost)
		ctx     func() (context.Context, context.CancelFunc)
	}{
		{
			name: "success",
			arrange: func(f *fakeStreamHost) {
				f.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("a"), {Done: true}}
			},
		},
		{
			name: "failure after first byte",
			arrange: func(f *fakeStreamHost) {
				f.readHook = func(_ context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
					f.mu.Lock()
					defer f.mu.Unlock()
					position := f.positions[streamID]
					f.positions[streamID] = position + 1
					if position == 0 {
						return &pluginapi.HostModelStreamReadResponse{Payload: []byte("a")}, nil
					}
					return &pluginapi.HostModelStreamReadResponse{Error: "boom", Done: true}, nil
				}
			},
		},
		{
			name: "fallback before first byte",
			arrange: func(f *fakeStreamHost) {
				f.openErrors["bad"] = retryableHostError(http.StatusBadGateway)
				f.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("a"), {Done: true}}
			},
		},
		{
			name: "context cancel",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			arrange: func(f *fakeStreamHost) {
				f.readHook = func(ctx context.Context, streamID string) (*pluginapi.HostModelStreamReadResponse, error) {
					f.mu.Lock()
					position := f.positions[streamID]
					f.positions[streamID] = position + 1
					f.mu.Unlock()
					if position == 0 {
						return &pluginapi.HostModelStreamReadResponse{Payload: []byte("a")}, nil
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeStreamHost(t)
			tc.arrange(fake)
			combo := mustCombo(t, "c", StrategyFailover, "bad", "good")
			plugin := streamTestPlugin(t, combo)
			ctx := context.Background()
			cancel := func() {}
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			if _, err := plugin.executeStream(withPluginStreamID(ctx, "s"), pluginapi.ExecutorRequest{Model: "c"}); err != nil {
				// Fallback/all-fail cases can legitimately return a comboError; the close assertion still applies.
				var comboErr *comboError
				if !errors.As(err, &comboErr) {
					t.Fatalf("executeStream: %v", err)
				}
			}
			if tc.name == "context cancel" {
				cancel()
			}
			fake.waitForClose(t)
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				fake.mu.Lock()
				opened, closed := len(fake.streams), len(fake.closed)
				fake.mu.Unlock()
				if closed >= opened && opened > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			fake.mu.Lock()
			opened, closed := len(fake.streams), len(fake.closed)
			fake.mu.Unlock()
			if opened == 0 || closed < opened {
				t.Fatalf("opened host streams = %d, closed = %d; every host stream must be closed", opened, closed)
			}
		})
	}
}

func TestExecuteComboStreamEmptyMembers(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(Combo{Name: "empty", Members: nil}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), pluginapi.ExecutorRequest{Model: "empty"})
	var comboErr *comboError
	if !errors.As(err, &comboErr) || comboErr.Status != http.StatusNotFound {
		t.Fatalf("error = %v, want 404 comboError", err)
	}
}

func TestHandleAbiExecuteStreamEnvelope(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.scripts["good"] = []pluginapi.HostModelStreamReadResponse{chunk("a"), {Done: true}}
	plugin := streamTestPlugin(t, mustCombo(t, "c", StrategyFailover, "good"))
	previousPlugin := currentPlugin.Load()
	currentPlugin.Store(plugin)
	t.Cleanup(func() { currentPlugin.Store(previousPlugin) })

	request, err := json.Marshal(abiExecutorStreamRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "c", SourceFormat: "openai", Format: "openai", Payload: []byte(`{"model":"c"}`)},
		StreamID:        "host-stream",
		HostCallbackID:  "callback-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := handleAbiExecuteStream(request)
	if err != nil {
		t.Fatalf("handleAbiExecuteStream: %v", err)
	}
	var envelope struct {
		OK     bool                            `json:"ok"`
		Result rpcExecutorStreamResponse       `json:"result"`
		Error  *struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("envelope = %s, want ok", raw)
	}
	if got := envelope.Result.Headers["Content-Type"]; len(got) != 1 || got[0] != "text/event-stream" {
		t.Fatalf("headers = %v, want the serving member's headers", envelope.Result.Headers)
	}
	fake.waitForClose(t)
	if got := fake.emittedChunks(); !equalStrings(got, []string{"a"}) {
		t.Fatalf("emitted chunks = %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
