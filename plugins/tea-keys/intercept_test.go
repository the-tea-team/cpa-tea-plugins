package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func authHeaders(key string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + key}}
}

func TestInterceptRequestAllowsModel(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o",
		Headers:        authHeaders("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if resp.Terminate {
		t.Fatalf("allowed model was terminated: %#v", resp)
	}
}

func TestInterceptRequestAllowsThinkingSuffix(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o(8192)",
		Headers:        authHeaders("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if resp.Terminate {
		t.Fatalf("thinking suffix was terminated: %#v", resp)
	}
}

func TestInterceptRequestDeniesModelOpenAI(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o-mini",
		Headers:        authHeaders("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if !resp.Terminate || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403 termination", resp)
	}
	if resp.ResponseHeaders.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", resp.ResponseHeaders.Get("Content-Type"))
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, resp.ResponseBody)
	}
	if body.Error.Code != "model_not_allowed" || body.Error.Type != "invalid_request_error" {
		t.Fatalf("body = %#v", body)
	}
	if !strings.Contains(body.Error.Message, "gpt-4o-mini") {
		t.Fatalf("message = %q, want the model name", body.Error.Message)
	}
}

func TestInterceptRequestDeniesModelClaude(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "claude",
		RequestedModel: "gpt-4o-mini",
		Headers:        authHeaders("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, resp.ResponseBody)
	}
	if body.Type != "error" || body.Error.Type != "permission_error" {
		t.Fatalf("body = %#v", body)
	}
}

func TestInterceptRequestDeniesModelGemini(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "gemini",
		RequestedModel: "gpt-4o-mini",
		Headers:        authHeaders("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.ResponseBody, &body); err != nil {
		t.Fatalf("decode body: %v (%s)", err, resp.ResponseBody)
	}
	if body.Error.Code != http.StatusForbidden || body.Error.Status != "PERMISSION_DENIED" {
		t.Fatalf("body = %#v", body)
	}
}

func TestInterceptRequestUnmanagedAllowed(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "anything",
		Headers:        authHeaders("sk-unknown"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if resp.Terminate {
		t.Fatalf("unmanaged key was terminated in non-exclusive mode: %#v", resp)
	}
}

func TestInterceptRequestUnmanagedDeniedInExclusiveMode(t *testing.T) {
	plugin := mustPlugin(t, `
require_managed_keys: true
keys:
  - key: sk-alice
    models: ["gpt-4o"]
`)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o",
		Headers:        authHeaders("sk-unknown"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if !resp.Terminate || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response = %#v, want 401 termination", resp)
	}
	if !strings.Contains(string(resp.ResponseBody), "unmanaged_api_key") {
		t.Fatalf("body = %s", resp.ResponseBody)
	}
}

func TestInterceptRequestUsesCallerScopeWhenHeadersMissing(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o-mini",
		CallerScope:    callerScope("sk-alice"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if !resp.Terminate || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v, want 403 from caller scope", resp)
	}
}

func TestInterceptRequestNoModelPasses(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat: "openai",
		Headers:      authHeaders("sk-blocked"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if resp.Terminate {
		t.Fatal("a request without a model should not be modelled")
	}
}

func TestInterceptRequestEmptyAllowlistDenies(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o",
		Headers:        authHeaders("sk-blocked"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if !resp.Terminate {
		t.Fatal("an empty allowlist should deny every model")
	}
}

func TestInterceptRequestWildcardAllows(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptRequestBeforeAuth(nil, pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "any-model",
		Headers:        authHeaders("sk-bob"),
	})
	if err != nil {
		t.Fatalf("InterceptRequestBeforeAuth() error = %v", err)
	}
	if resp.Terminate {
		t.Fatal("an unrestricted key should allow any model")
	}
}

func TestInterceptResponseOpenAIFilters(t *testing.T) {
	plugin := authConfig(t)
	body := `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini","object":"model"},{"id":"claude-sonnet-4","object":"model"}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders:  authHeaders("sk-alice"),
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		Body:            []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body == nil {
		t.Fatal("response was not filtered")
	}
	assertModelIDs(t, resp.Body, "gpt-4o", "claude-sonnet-4")
	if got := resp.Headers.Get("Content-Length"); got == "" {
		t.Fatal("Content-Length was not set")
	}
}

func TestInterceptResponseClaudeFiltersAndUpdatesPagination(t *testing.T) {
	plugin := authConfig(t)
	body := `{"data":[{"id":"claude-sonnet-4","type":"model"},{"id":"gpt-4o","type":"model"},{"id":"gemini-pro","type":"model"}],"has_more":true,"first_id":"claude-sonnet-4","last_id":"gemini-pro"}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders:  authHeaders("sk-alice"),
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		Body:            []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	assertModelIDs(t, resp.Body, "claude-sonnet-4", "gpt-4o")

	var parsed struct {
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if parsed.HasMore || parsed.FirstID != "claude-sonnet-4" || parsed.LastID != "gpt-4o" {
		t.Fatalf("pagination = %#v", parsed)
	}
}

func TestInterceptResponseGeminiFilters(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: gemini
    key: sk-gemini
    models: ["gemini-pro"]
`)
	body := `{"models":[{"name":"models/gemini-pro","displayName":"Pro"},{"name":"models/gemini-flash","displayName":"Flash"}],"nextPageToken":"page-2"}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders:  authHeaders("sk-gemini"),
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		Body:            []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	var parsed struct {
		Models        []map[string]any `json:"models"`
		NextPageToken string           `json:"nextPageToken"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(parsed.Models) != 1 || parsed.Models[0]["name"] != "models/gemini-pro" {
		t.Fatalf("models = %#v", parsed.Models)
	}
	if parsed.NextPageToken != "" {
		t.Fatalf("nextPageToken = %q, want removed", parsed.NextPageToken)
	}
}

func TestInterceptResponseCodexFilters(t *testing.T) {
	plugin := authConfig(t)
	body := `{"models":[{"slug":"gpt-4o","display_name":"GPT-4o"},{"slug":"gpt-4o-mini","display_name":"GPT-4o mini"}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders:  authHeaders("sk-alice"),
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		Body:            []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	var parsed struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(parsed.Models) != 1 || parsed.Models[0].Slug != "gpt-4o" {
		t.Fatalf("models = %#v", parsed.Models)
	}
}

func TestInterceptResponseLeavesCompletionAlone(t *testing.T) {
	plugin := authConfig(t)
	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o-mini","choices":[{"message":{"content":"hi"}}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders: authHeaders("sk-alice"),
		Body:           []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body != nil {
		t.Fatalf("completion response was rewritten: %s", resp.Body)
	}
}

func TestInterceptResponseLeavesInvalidJSONAlone(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders: authHeaders("sk-alice"),
		Body:           []byte(`{"object":"list","data":[`),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body != nil {
		t.Fatalf("invalid JSON was rewritten: %s", resp.Body)
	}
}

func TestInterceptResponseUnmanagedAllowed(t *testing.T) {
	plugin := authConfig(t)
	body := `{"object":"list","data":[{"id":"gpt-4o"},{"id":"secret-model"}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders: authHeaders("sk-unknown"),
		Body:           []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body != nil {
		t.Fatalf("unmanaged key list was rewritten: %s", resp.Body)
	}
}

func TestInterceptResponseUnmanagedDeniedInExclusiveMode(t *testing.T) {
	plugin := mustPlugin(t, `
require_managed_keys: true
keys:
  - key: sk-alice
    models: ["gpt-4o"]
`)
	body := `{"object":"list","data":[{"id":"gpt-4o"},{"id":"secret-model"}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders: authHeaders("sk-unknown"),
		Body:           []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body == nil {
		t.Fatal("exclusive-mode unmanaged list was not filtered")
	}
	assertModelIDs(t, resp.Body)
}

func TestInterceptResponseWildcardUnchanged(t *testing.T) {
	plugin := authConfig(t)
	body := `{"object":"list","data":[{"id":"gpt-4o"},{"id":"secret-model"}]}`
	resp, err := plugin.InterceptResponse(nil, pluginapi.ResponseInterceptRequest{
		RequestHeaders: authHeaders("sk-bob"),
		Body:           []byte(body),
	})
	if err != nil {
		t.Fatalf("InterceptResponse() error = %v", err)
	}
	if resp.Body != nil {
		t.Fatalf("unrestricted key list was rewritten: %s", resp.Body)
	}
}

func assertModelIDs(t *testing.T, body []byte, want ...string) {
	t.Helper()
	var parsed struct {
		Data   []json.RawMessage `json:"data"`
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode model list: %v (%s)", err, body)
	}
	entries := append(parsed.Data, parsed.Models...)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entryModelID(entry))
	}
	if len(got) != len(want) {
		t.Fatalf("model IDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model IDs = %v, want %v", got, want)
		}
	}
}
