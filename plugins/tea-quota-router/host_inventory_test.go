package main

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func authTypeFor(t *testing.T, list []pluginapi.HostAuthFileEntry, models []pluginapi.HostAuthModels, id string) string {
	t.Helper()
	creds, _, err := unionInventory(list, models)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range creds {
		if c.ID == id {
			return c.AuthType
		}
	}
	t.Fatalf("credential %q missing: %+v", id, creds)
	return ""
}

func TestModelsOnlyConfigCredentialsClassifiedAPIKeyFromDocumentedIDs(t *testing.T) {
	for _, tc := range []struct{ id, provider, want string }{
		{"claude:apikey:0123456789ab", "claude", "api_key"},
		{"codex:apikey:0123456789ab", "codex", "api_key"},
		{"codex:apikey:0123456789ab-2", "codex", "api_key"}, // duplicate counter
		{"gemini:apikey:abcdef012345", "gemini", "api_key"},
		{"openai-compatibility:openrouter:0123456789ab", "openai-compatible-openrouter", "api_key"},
		// not documented shapes: stay unknown
		{"claude:apikey:nothex", "claude", ""},
		{"claude:apikey:0123456789ab:extra", "claude", ""},
		{"claude-main.json", "claude", ""},
		{"ai-idx-only", "claude", ""},
		{"openai-compatibility:0123456789ab", "claude", ""},
	} {
		got := authTypeFor(t, nil, []pluginapi.HostAuthModels{{ID: tc.id, AuthIndex: "ai-" + tc.id, Provider: tc.provider, Models: []string{"m"}}}, tc.id)
		if got != tc.want {
			t.Errorf("id %q: AuthType = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestListConfigIDStaysAPIKeyEvenWithoutAccountType(t *testing.T) {
	list := []pluginapi.HostAuthFileEntry{{ID: "claude:apikey:0123456789ab", AuthIndex: "i", Provider: "claude", Email: "p@example.invalid"}}
	if got := authTypeFor(t, list, nil, "claude:apikey:0123456789ab"); got != "api_key" {
		t.Fatalf("AuthType = %q", got)
	}
}

func TestFileJSONAuthOAuthOnlyWithProviderSuffixAndEvidence(t *testing.T) {
	base := pluginapi.HostAuthFileEntry{ID: "main.json", Name: "main.json", AuthIndex: "i", Provider: "claude", Email: "p@example.invalid"}
	mut := func(f func(*pluginapi.HostAuthFileEntry)) pluginapi.HostAuthFileEntry { e := base; f(&e); return e }
	for _, tc := range []struct {
		name string
		e    pluginapi.HostAuthFileEntry
		want string
	}{
		{"claude json with email", base, "oauth"},
		{"codex json with email", mut(func(e *pluginapi.HostAuthFileEntry) { e.Provider = "codex" }), "oauth"},
		{"type field used for provider", mut(func(e *pluginapi.HostAuthFileEntry) { e.Provider, e.Type = "", "Codex" }), "oauth"},
		{"suffix only on name", mut(func(e *pluginapi.HostAuthFileEntry) { e.ID = "host-id" }), "oauth"},
		{"no email evidence", mut(func(e *pluginapi.HostAuthFileEntry) { e.Email = "" }), ""},
		{"no json suffix", mut(func(e *pluginapi.HostAuthFileEntry) { e.ID, e.Name = "main", "main" }), ""},
		{"other provider", mut(func(e *pluginapi.HostAuthFileEntry) { e.Provider = "gemini" }), ""},
		{"runtime only", mut(func(e *pluginapi.HostAuthFileEntry) { e.RuntimeOnly = true }), ""},
		{"custom base url", mut(func(e *pluginapi.HostAuthFileEntry) { e.BaseURL = "https://private.invalid" }), ""},
		{"explicit api_key wins", mut(func(e *pluginapi.HostAuthFileEntry) { e.AccountType = "api_key" }), "api_key"},
		{"explicit oauth", mut(func(e *pluginapi.HostAuthFileEntry) { e.AccountType = "oauth"; e.Email = "" }), "oauth"},
		{"unrecognized account type falls to file evidence", mut(func(e *pluginapi.HostAuthFileEntry) { e.AccountType = "oauth-bridge-api" }), "oauth"},
	} {
		got := authTypeFor(t, []pluginapi.HostAuthFileEntry{tc.e}, nil, tc.e.ID)
		if got != tc.want {
			t.Errorf("%s: AuthType = %q, want %q", tc.name, got, tc.want)
		}
	}
	// unrecognized AccountType without file evidence is unknown, not guessed
	e := mut(func(e *pluginapi.HostAuthFileEntry) { e.AccountType = "oauth-bridge-api"; e.Email = "" })
	if got := authTypeFor(t, []pluginapi.HostAuthFileEntry{e}, nil, e.ID); got != "" {
		t.Fatalf("substring-classified: %q", got)
	}
}

func TestFetchEligibleRequiresPositiveOAuthForClaudeCodex(t *testing.T) {
	for _, tc := range []struct {
		provider, authType string
		want               bool
	}{
		{"claude", "oauth", true}, {"claude", "", false}, {"claude", "api_key", false},
		{"codex", "oauth", true}, {"codex", "", false}, {"codex", "api_key", false},
		{"openrouter", "", true}, {"openrouter", "api_key", true}, {"openrouter", "oauth", false},
		{"openai-compatible-openrouter", "api_key", true}, {"openai-compatible-openrouter", "", true}, {"openai-compatible-openrouter", "oauth", false},
		{"openai-compatible-other", "api_key", false}, {"gemini", "api_key", false},
	} {
		got := fetchEligible(quota.Credential{AuthIndex: "i", Provider: tc.provider, AuthType: tc.authType})
		if got != tc.want {
			t.Errorf("fetchEligible(%s,%q) = %v, want %v", tc.provider, tc.authType, got, tc.want)
		}
	}
	if fetchEligible(quota.Credential{Provider: "claude", AuthType: "oauth"}) || fetchEligible(quota.Credential{AuthIndex: "i", Provider: "claude", AuthType: "oauth", Disabled: true}) {
		t.Fatal("missing auth index / disabled must be ineligible")
	}
}

func TestUnclassifiedAndConfigClaudeCodexNeverProbed(t *testing.T) {
	h := newHarness(t)
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return []pluginapi.HostAuthFileEntry{
				{ID: "mystery", AuthIndex: "ai-claude-1", Provider: "claude"}, // no type, no .json evidence
			}, []pluginapi.HostAuthModels{
				{ID: "claude:apikey:0123456789ab", AuthIndex: "ai-claude-key", Provider: "claude", Models: []string{"claude-sonnet-4"}},
				{ID: "codex:apikey:0123456789ab", AuthIndex: "ai-codex-1", Provider: "codex", Models: []string{"gpt-5"}},
			}, nil
	}
	snap, health, err := h.run(quota.Snapshot{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.m.snapshotCalls()); n != 0 {
		t.Fatalf("subscription endpoints probed %d times: %+v", n, h.m.snapshotCalls())
	}
	for _, id := range []string{"mystery", "claude:apikey:0123456789ab", "codex:apikey:0123456789ab"} {
		if a := acctByID(t, snap, id); a.RefreshStatus != refreshUnsupported {
			t.Errorf("%s status = %q", id, a.RefreshStatus)
		}
	}
	if c := credByID(t, snap, "claude:apikey:0123456789ab"); c.AuthType != "api_key" {
		t.Errorf("config cred authType = %q", c.AuthType)
	}
	if len(setupFor(health, "claude")) == 0 || len(setupFor(health, "codex")) == 0 {
		t.Fatalf("setup = %+v", health.Setup)
	}
}

func TestOpenRouterCompatProviderDispatchesButKeepsProviderIdentity(t *testing.T) {
	h := newHarness(t)
	const id, prov = "openai-compatibility:openrouter:0123456789ab", "openai-compatible-openrouter"
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return nil, []pluginapi.HostAuthModels{{ID: id, AuthIndex: "ai-or-1", Provider: prov, Models: []string{"openrouter/auto"}}}, nil
	}
	snap, health, err := h.run(quota.Snapshot{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	cr := credByID(t, snap, id)
	a := acctByID(t, snap, id)
	if cr.Provider != prov || a.Provider != prov {
		t.Fatalf("provider identity rewritten: cred=%q account=%q", cr.Provider, a.Provider)
	}
	if cr.AuthType != "api_key" || a.RefreshStatus != refreshOK || a.BillingType != "api" || len(a.Financial) == 0 {
		t.Fatalf("cred=%+v account=%+v health=%+v", cr, a, health)
	}
	calls := h.m.snapshotCalls()
	if len(calls) != 1 || calls[0].AuthIndex != "ai-or-1" || calls[0].URL != "https://openrouter.ai/api/v1/key" {
		t.Fatalf("calls = %+v", calls)
	}
	// provider join used by the scheduler: pool/credential provider is the original name
	for _, p := range snap.Pools {
		if p.Provider != prov {
			t.Fatalf("pool provider = %q", p.Provider)
		}
	}
}
