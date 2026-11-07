package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func mustPlugin(t *testing.T, config string) *teaKeysPlugin {
	t.Helper()
	plugin, err := newTeaKeysPlugin([]byte(config))
	if err != nil {
		t.Fatalf("newTeaKeysPlugin() error = %v", err)
	}
	return plugin
}

func TestConfigModelsOmittedAllowsEverything(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key: sk-alice
`)
	key := plugin.cfg.lookupString("sk-alice")
	if key == nil {
		t.Fatal("alice not found")
	}
	if !key.allows("anything") {
		t.Fatal("omitted models should allow every model")
	}
}

func TestConfigEmptyModelsDeniesEverything(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key: sk-alice
    models: []
`)
	key := plugin.cfg.lookupString("sk-alice")
	if key == nil {
		t.Fatal("alice not found")
	}
	if key.allows("gpt-4o") {
		t.Fatal("empty models should deny every model")
	}
}

func TestConfigWildcardOnly(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key: sk-alice
    models: ["*"]
`)
	key := plugin.cfg.lookupString("sk-alice")
	if key == nil || !key.allowAll || !key.allows("anything") {
		t.Fatal("wildcard-only list should allow every model")
	}
}

func TestConfigKeySHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("sk-alice"))
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key_sha256: `+strings.ToUpper(hex.EncodeToString(sum[:]))+`
    models: ["gpt-4o"]
`)
	if plugin.cfg.lookupString("sk-alice") == nil {
		t.Fatal("uppercase hex fingerprint was not accepted")
	}
}

func TestConfigRejectsBadKeys(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "neither key nor hash",
			config: "keys:\n  - models: [\"gpt-4o\"]\n",
			want:   "exactly one of key and key_sha256",
		},
		{
			name:   "both key and hash",
			config: "keys:\n  - key: x\n    key_sha256: " + strings.Repeat("a", 64) + "\n",
			want:   "exactly one of key and key_sha256",
		},
		{
			name:   "bad hex",
			config: "keys:\n  - key_sha256: \"zz\"\n",
			want:   "not valid hex",
		},
		{
			name:   "short hash",
			config: "keys:\n  - key_sha256: \"abcd\"\n",
			want:   "must be 64 hex characters",
		},
		{
			name:   "duplicate fingerprint",
			config: "keys:\n  - key: x\n  - key: x\n",
			want:   "duplicate key fingerprint",
		},
		{
			name:   "duplicate name",
			config: "keys:\n  - name: alice\n    key: x\n  - name: alice\n    key: y\n",
			want:   "duplicate name",
		},
		{
			name:   "empty model pattern",
			config: "keys:\n  - key: x\n    models: [\"\"]\n",
			want:   "pattern is empty",
		},
		{
			name:   "wildcard mixed with other patterns",
			config: "keys:\n  - key: x\n    models: [\"*\", \"gpt-4o\"]\n",
			want:   "must be the only pattern",
		},
		{
			name:   "managed keys required but empty",
			config: "require_managed_keys: true\nkeys: []\n",
			want:   "no keys are configured",
		},
		{
			name:   "default requires managed keys and rejects empty",
			config: "keys: []\n",
			want:   "no keys are configured",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newTeaKeysPlugin([]byte(test.config))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestConfigDefaultNameIsNotTheKey(t *testing.T) {
	plugin := mustPlugin(t, "keys:\n  - key: sk-very-secret\n")
	key := plugin.cfg.lookupString("sk-very-secret")
	if key == nil {
		t.Fatal("key not found")
	}
	if strings.Contains(key.name, "very-secret") {
		t.Fatalf("default name %q leaks the key", key.name)
	}
	if !strings.HasPrefix(key.name, "key-") {
		t.Fatalf("default name = %q, want key- prefix", key.name)
	}
}

func TestKeyForHeadersForms(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key: sk-alice
`)
	tests := []struct {
		name    string
		headers http.Header
		want    bool
	}{
		{name: "bearer", headers: http.Header{"Authorization": []string{"Bearer sk-alice"}}, want: true},
		{name: "raw authorization", headers: http.Header{"Authorization": []string{"sk-alice"}}, want: true},
		{name: "x-api-key", headers: http.Header{"X-Api-Key": []string{"sk-alice"}}, want: true},
		{name: "x-goog-api-key", headers: http.Header{"X-Goog-Api-Key": []string{"sk-alice"}}, want: true},
		{name: "unknown", headers: http.Header{"Authorization": []string{"Bearer nope"}}, want: false},
		{name: "nil", headers: nil, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := plugin.cfg.keyForHeaders(test.headers)
			if (got != nil) != test.want {
				t.Fatalf("keyForHeaders() = %v, want found=%v", got, test.want)
			}
		})
	}
}

func TestKeyForHeadersIgnoresQuery(t *testing.T) {
	plugin := mustPlugin(t, "keys:\n  - key: sk-alice\n")
	if key := plugin.cfg.keyForHeaders(http.Header{}); key != nil {
		t.Fatal("keyForHeaders must not read query credentials")
	}
	values := url.Values{"key": []string{"sk-alice"}}
	if got := queryCredentialCandidates(values); len(got) != 1 || got[0] != "sk-alice" {
		t.Fatalf("queryCredentialCandidates() = %v", got)
	}
}

func TestKeyForInterceptCallerScope(t *testing.T) {
	plugin := mustPlugin(t, `
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o"]
`)
	tests := []struct {
		name  string
		req   pluginapi.RequestInterceptRequest
		found bool
	}{
		{
			name:  "core-authenticated raw key scope",
			req:   pluginapi.RequestInterceptRequest{CallerScope: callerScope("sk-alice")},
			found: true,
		},
		{
			name:  "metadata scope",
			req:   pluginapi.RequestInterceptRequest{Metadata: map[string]any{pluginapi.CallerScopeMetadataKey: callerScope("alice")}},
			found: true,
		},
		{
			name:  "plugin-authenticated name scope",
			req:   pluginapi.RequestInterceptRequest{CallerScope: callerScope("alice")},
			found: true,
		},
		{
			name: "unknown scope",
			req:  pluginapi.RequestInterceptRequest{CallerScope: callerScope("sk-unknown")},
		},
		{
			name: "empty scope",
			req:  pluginapi.RequestInterceptRequest{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			key := plugin.cfg.keyForIntercept(test.req)
			if (key != nil) != test.found {
				t.Fatalf("keyForIntercept() = %v, want found=%v", key, test.found)
			}
		})
	}
}

func TestCallerScopeMatchesCPA(t *testing.T) {
	tests := map[string]string{
		"sk-alice": "3bdece7f935e649ab302aec3a53bb0bfaca62166684bc6e4e9aaad9bf8139766",
		"alice":    "74da4a0970ecf0b03932ee5793ec12bcf4e6dbccaaac29bb4fd147f78f85fc91",
	}
	for value, want := range tests {
		if got := callerScope(value); got != want {
			t.Errorf("callerScope(%q) = %q, want %q", value, got, want)
		}
	}
	if got := callerScope("  "); got != "" {
		t.Errorf("callerScope(blank) = %q, want empty", got)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := map[string]string{
		"Bearer abc": "abc",
		"bearer abc": "abc",
		" abc ":      "abc",
		"Bearer":     "Bearer",
		"Bearer    ": "",
		"Basic abc":  "Basic abc",
	}
	for input, want := range tests {
		if got := extractBearerToken(input); got != want {
			t.Errorf("extractBearerToken(%q) = %q, want %q", input, got, want)
		}
	}
}
