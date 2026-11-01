package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func keyFor(t *testing.T, combo Combo, request pluginapi.ExecutorRequest) string {
	t.Helper()
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		return ""
	}
	return key
}

func TestSessionKeyPrecedence(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
	base := pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}
	withHeaders := func(values map[string]string) pluginapi.ExecutorRequest {
		request := base
		request.Headers = http.Header{}
		for name, value := range values {
			request.Headers.Set(name, value)
		}
		return request
	}
	withPayload := func(payload string) pluginapi.ExecutorRequest {
		request := base
		request.Payload = []byte(payload)
		return request
	}

	cases := []struct {
		name string
		// request carries every lower-priority source too; want equals the request carrying only
		// the expected source, proving precedence.
		request pluginapi.ExecutorRequest
		only    pluginapi.ExecutorRequest
	}{
		{
			name: "canonical metadata before every header",
			request: func() pluginapi.ExecutorRequest {
				request := withHeaders(map[string]string{
					"X-Claude-Code-Session-Id": "claude-session",
					"Session-Id":               "codex-session",
					"X-Session-ID":             "opencode-session",
				})
				request.Metadata = map[string]any{"canonical_session_id": "canonical-id"}
				return request
			}(),
			only: func() pluginapi.ExecutorRequest {
				request := base
				request.Metadata = map[string]any{"canonical_session_id": "canonical-id"}
				return request
			}(),
		},
		{
			name: "Claude Code header before codex and body",
			request: withHeaders(map[string]string{
				"X-Claude-Code-Session-Id": "claude-session",
				"Session-Id":               "codex-session",
			}),
			only: withHeaders(map[string]string{"X-Claude-Code-Session-Id": "claude-session"}),
		},
		{
			name: "Codex header before OpenCode and body",
			request: func() pluginapi.ExecutorRequest {
				request := withHeaders(map[string]string{
					"Session-Id":   "codex-session",
					"X-Session-ID": "opencode-session",
				})
				request.Payload = []byte(`{"model":"fleet","prompt_cache_key":"pck-body"}`)
				return request
			}(),
			only: withHeaders(map[string]string{"Session-Id": "codex-session"}),
		},
		{
			name: "OpenCode header before body",
			request: func() pluginapi.ExecutorRequest {
				request := withHeaders(map[string]string{"X-Session-ID": "opencode-session"})
				request.Payload = []byte(`{"model":"fleet","prompt_cache_key":"pck-body"}`)
				return request
			}(),
			only: withHeaders(map[string]string{"X-Session-ID": "opencode-session"}),
		},
		{
			name:    "prompt_cache_key before conversation ids",
			request: withPayload(`{"model":"fleet","prompt_cache_key":"pck-body","conversation_id":"conv-body"}`),
			only:    withPayload(`{"model":"fleet","prompt_cache_key":"pck-body"}`),
		},
		{
			name:    "conversation object before legacy conversation ids",
			request: withPayload(`{"model":"fleet","conversation":{"id":"conv-object"},"conversation_id":"conv-legacy"}`),
			only:    withPayload(`{"model":"fleet","conversation":{"id":"conv-object"}}`),
		},
		{
			name:    "nested request body is read when there is no contents array",
			request: withPayload(`{"model":"fleet","request":{"prompt_cache_key":"nested-pck"}}`),
			only:    withPayload(`{"model":"fleet","prompt_cache_key":"nested-pck"}`),
		},
		{
			name:    "first user message is the last resort",
			request: withPayload(`{"model":"fleet","messages":[{"role":"user","content":"hello there"}]}`),
			only:    withPayload(`{"model":"fleet","messages":[{"role":"user","content":"hello there"}]}`),
		},
		{
			name:    "Gemini contents user part",
			request: withPayload(`{"model":"fleet","contents":[{"role":"user","parts":[{"text":"gemini hello"}]}]}`),
			only:    withPayload(`{"model":"fleet","contents":[{"role":"user","parts":[{"text":"gemini hello"}]}]}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := keyFor(t, combo, tc.request)
			want := keyFor(t, combo, tc.only)
			if want == "" {
				t.Fatal("expected request must derive a key")
			}
			if got != want {
				t.Fatalf("key = %q, want %q (precedence differs)", got, want)
			}
		})
	}
}

func TestSessionKeyDerivationTable(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	cases := []struct {
		name    string
		request pluginapi.ExecutorRequest
		derived bool
	}{
		{name: "no identity", request: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}},
		{name: "empty canonical", request: pluginapi.ExecutorRequest{Model: "fleet", Metadata: map[string]any{"canonical_session_id": "  "}}},
		{name: "canonical", request: pluginapi.ExecutorRequest{Model: "fleet", Metadata: map[string]any{"canonical_session_id": "canonical"}}, derived: true},
		{name: "claude header", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{"X-Claude-Code-Session-Id": {"c"}}}, derived: true},
		{name: "claude agent header", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{
			"X-Claude-Code-Session-Id": {"c"},
			"X-Claude-Code-Agent-Id":   {"helper"},
		}}, derived: true},
		{name: "codex session", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{"Session-Id": {"s"}}}, derived: true},
		{name: "codex thread", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{"Thread-Id": {"t"}}}, derived: true},
		{name: "opencode session", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{"X-Session-Id": {"o"}}}, derived: true},
		{name: "prompt cache key", request: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"prompt_cache_key":"k"}`)}, derived: true},
		{name: "conversation id", request: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"conversation_id":"c"}`)}, derived: true},
		{name: "first user message", request: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, derived: true},
		{name: "control characters rejected", request: pluginapi.ExecutorRequest{Model: "fleet", Headers: http.Header{"X-Session-Id": {"bad\x00id"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, ok := deriveSessionKey(combo, tc.request)
			if ok != tc.derived {
				t.Fatalf("derived = %v, want %v", ok, tc.derived)
			}
			if tc.derived && (len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "") {
				t.Fatalf("key %q is not a hex SHA-256 digest", key)
			}
			if !tc.derived && key != "" {
				t.Fatalf("key = %q, want empty", key)
			}
		})
	}
}

func TestSessionKeySelectorHintMatchesComboKey(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	request := pluginapi.ExecutorRequest{
		Model:   "fleet",
		Headers: http.Header{"X-Session-Id": {"shared-session"}},
	}
	key, ok := deriveSessionKey(combo, request)
	if !ok {
		t.Fatal("key not derived")
	}
	if hint := sessionKey(request); hint != key {
		t.Fatalf("selector hint %q != combo key %q", hint, key)
	}
}

func TestSessionKeySameInputSameKeyDifferentComboDifferentKey(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0")
	other := mustCombo(t, "other", StrategyRoundRobin, "m0")
	request := pluginapi.ExecutorRequest{
		Model:   "fleet",
		Headers: http.Header{"X-Session-Id": {"shared-session"}},
	}

	first := keyFor(t, combo, request)
	second := keyFor(t, combo, request)
	if first == "" || first != second {
		t.Fatalf("same input must produce the same key: %q / %q", first, second)
	}
	different := keyFor(t, other, request)
	if different == first {
		t.Fatal("the same session in two combos must not share a binding")
	}
}
