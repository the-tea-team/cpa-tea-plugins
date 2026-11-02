package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func authConfig(t *testing.T) *teaKeysPlugin {
	t.Helper()
	return mustPlugin(t, `
require_managed_keys: false
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o", "claude-*"]
  - name: bob
    key: sk-bob
  - name: blocked
    key: sk-blocked
    models: []
`)
}

func TestAuthenticateManagedHeaderForms(t *testing.T) {
	plugin := authConfig(t)
	tests := []struct {
		name    string
		headers http.Header
	}{
		{name: "bearer", headers: http.Header{"Authorization": []string{"Bearer sk-alice"}}},
		{name: "raw", headers: http.Header{"Authorization": []string{"sk-alice"}}},
		{name: "anthropic", headers: http.Header{"X-Api-Key": []string{"sk-alice"}}},
		{name: "google", headers: http.Header{"X-Goog-Api-Key": []string{"sk-alice"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{Headers: test.headers})
			if err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}
			if !resp.Authenticated || resp.Principal != "alice" {
				t.Fatalf("Authenticate() = %#v, want alice authenticated", resp)
			}
			if resp.Metadata["key_id"] != "alice" {
				t.Fatalf("metadata = %v, want key_id alice", resp.Metadata)
			}
		})
	}
}

func TestAuthenticateDefaultIsExclusive(t *testing.T) {
	plugin := mustPlugin(t, "keys:\n  - name: alice\n    key: sk-alice\n")
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Headers: http.Header{"Authorization": []string{"Bearer sk-unknown"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != http.StatusUnauthorized {
		t.Fatalf("Authenticate() = %#v, want 401 because require_managed_keys defaults to true", resp)
	}
}

func TestAuthenticateUnknownFallsThrough(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Headers: http.Header{"Authorization": []string{"Bearer sk-unknown"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != 0 || resp.DenyMessage != "" {
		t.Fatalf("Authenticate() = %#v, want unhandled", resp)
	}
}

func TestAuthenticateExclusiveRejectsUnknown(t *testing.T) {
	plugin := mustPlugin(t, `
require_managed_keys: true
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o"]
`)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Headers: http.Header{"Authorization": []string{"Bearer sk-unknown"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != http.StatusUnauthorized || !strings.Contains(resp.DenyMessage, "Invalid API key") {
		t.Fatalf("Authenticate() = %#v, want 401 invalid key", resp)
	}
}

func TestAuthenticateExclusiveMissingKeyFallsThroughToHostError(t *testing.T) {
	plugin := mustPlugin(t, `
require_managed_keys: true
keys:
  - key: sk-alice
`)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != 0 {
		t.Fatalf("Authenticate() = %#v, want unhandled so CPA reports Missing API key", resp)
	}
}

func TestAuthenticateManagedQueryKeyRejected(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Query: url.Values{"key": []string{"sk-alice"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != http.StatusUnauthorized {
		t.Fatalf("Authenticate() = %#v, want 401", resp)
	}
	if !strings.Contains(resp.DenyMessage, "header") {
		t.Fatalf("DenyMessage = %q, want header guidance", resp.DenyMessage)
	}
}

func TestAuthenticateHeaderWinsOverManagedQueryKey(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Headers: http.Header{"Authorization": []string{"Bearer sk-alice"}},
		Query:   url.Values{"key": []string{"sk-alice"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if !resp.Authenticated || resp.Principal != "alice" {
		t.Fatalf("Authenticate() = %#v, want alice authenticated", resp)
	}
}

func TestAuthenticateUnknownQueryKeyFallsThrough(t *testing.T) {
	plugin := authConfig(t)
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{
		Query: url.Values{"key": []string{"sk-unknown"}},
	})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated || resp.DenyStatusCode != 0 {
		t.Fatalf("Authenticate() = %#v, want unhandled", resp)
	}
}

func TestAuthenticateNilPlugin(t *testing.T) {
	var plugin *teaKeysPlugin
	resp, err := plugin.Authenticate(nil, pluginapi.FrontendAuthRequest{})
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if resp.Authenticated {
		t.Fatal("nil plugin authenticated a request")
	}
}

func TestIdentifier(t *testing.T) {
	plugin := authConfig(t)
	if got := plugin.Identifier(); got != "tea-keys" {
		t.Fatalf("Identifier() = %q, want tea-keys", got)
	}
}
