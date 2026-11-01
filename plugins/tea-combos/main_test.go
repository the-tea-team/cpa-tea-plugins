package main

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func boolPtr(value bool) *bool { return &value }

func mustCombo(t *testing.T, name, strategy string, models ...string) Combo {
	t.Helper()
	members := make([]Member, 0, len(models))
	for _, model := range models {
		members = append(members, Member{Model: model})
	}
	combo := Combo{Name: name, Strategy: strategy, Members: members}.Normalize()
	if err := combo.Validate(); err != nil {
		t.Fatalf("invalid test combo %q: %v", name, err)
	}
	return combo
}

func newTestPlugin(t *testing.T) *combosPlugin {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return &combosPlugin{store: store, roundRobin: newRoundRobinSelector()}
}

func stubHostExecute(t *testing.T, fn func(context.Context, pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error)) {
	t.Helper()
	previous := hostModelExecute
	hostModelExecute = fn
	t.Cleanup(func() { hostModelExecute = previous })
}

func TestBuildPluginMetadata(t *testing.T) {
	plugin, err := buildPlugin([]byte("statePath: "+filepath.Join(t.TempDir(), "state.json")+"\n"), "")
	if err != nil {
		t.Fatalf("buildPlugin: %v", err)
	}
	if plugin.Metadata.Version != pluginVersion {
		t.Fatalf("metadata version %q != pluginVersion %q", plugin.Metadata.Version, pluginVersion)
	}
	if plugin.Metadata.Name != pluginName {
		t.Fatalf("metadata name %q != pluginName %q", plugin.Metadata.Name, pluginName)
	}
	if plugin.Capabilities.ModelRouter == nil || plugin.Capabilities.Executor == nil {
		t.Fatal("router and executor capabilities must be wired")
	}
	fields := map[string]bool{}
	for _, field := range plugin.Metadata.ConfigFields {
		fields[field.Name] = true
	}
	if !fields["statePath"] || !fields["combos"] {
		t.Fatalf("config fields = %v, want statePath and combos", fields)
	}
}

func TestResolveStatePathFailsLoudly(t *testing.T) {
	_, err := resolveStatePath("", "")
	if err == nil || !strings.Contains(err.Error(), "statePath") {
		t.Fatalf("resolveStatePath error = %v, want an error naming statePath", err)
	}
	_, err = resolveStatePath("", "/tmp/cliproxy-pluginhost/pid-123")
	if err == nil || !strings.Contains(err.Error(), "statePath") {
		t.Fatalf("resolveStatePath shadow error = %v, want an error naming statePath", err)
	}
	path, err := resolveStatePath("", "/plugins")
	if err != nil || path != filepath.Join("/plugins", defaultStateFileName) {
		t.Fatalf("resolveStatePath default = %q, %v", path, err)
	}
}

func TestIsEphemeralPluginDir(t *testing.T) {
	cases := map[string]bool{
		"/tmp/cliproxy-pluginhost/pid-123":   true,
		"/var/tmp/cliproxy-pluginhost/pid-9": true,
		"/opt/cli-proxy-api/plugins":         false,
		"/plugins":                           false,
	}
	for dir, want := range cases {
		if got := isEphemeralPluginDir(dir); got != want {
			t.Fatalf("isEphemeralPluginDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestRouteModel(t *testing.T) {
	plugin := newTestPlugin(t)
	enabled := mustCombo(t, "fleet", StrategyRoundRobin, "gpt-a", "gpt-b")
	disabled := mustCombo(t, "paused", StrategyRoundRobin, "gpt-a")
	disabled.Enabled = boolPtr(false)
	empty := Combo{Name: "empty", Members: nil, Enabled: boolPtr(true)}
	for _, combo := range []Combo{enabled, disabled, empty} {
		plugin.store.state.Combos[combo.Name] = combo
	}

	cases := []struct {
		model   string
		handled bool
	}{
		{model: "fleet", handled: true},
		{model: "paused", handled: false},
		{model: "empty", handled: false},
		{model: "not-a-combo", handled: false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			response, err := plugin.RouteModel(context.Background(), pluginapi.ModelRouteRequest{RequestedModel: tc.model})
			if err != nil {
				t.Fatalf("RouteModel: %v", err)
			}
			if response.Handled != tc.handled {
				t.Fatalf("Handled = %v, want %v", response.Handled, tc.handled)
			}
			if tc.handled && (response.TargetKind != pluginapi.ModelRouteTargetExecutor || response.Target != pluginID) {
				t.Fatalf("route target = %q/%q", response.TargetKind, response.Target)
			}
		})
	}
}

func TestExecuteEmptyMemberList(t *testing.T) {
	plugin := newTestPlugin(t)
	plugin.store.state.Combos["empty"] = Combo{Name: "empty", Members: nil, Enabled: boolPtr(true)}
	called := false
	stubHostExecute(t, func(context.Context, pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		called = true
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK}, nil
	})

	_, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "empty"})
	if err == nil {
		t.Fatal("Execute with an empty member list must fail")
	}
	var comboErr *comboError
	if !errors.As(err, &comboErr) || comboErr.Status != http.StatusNotFound {
		t.Fatalf("error = %v, want a 404 comboError", err)
	}
	if called {
		t.Fatal("host must not be called for an empty combo")
	}
}

func TestExecuteAllMembersFailRetryably(t *testing.T) {
	plugin := newTestPlugin(t)
	combo := mustCombo(t, "fleet", StrategyFailover, "bad-a", "bad-b")
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		return nil, &hostError{Status: http.StatusServiceUnavailable, Retryable: true, Message: "upstream unavailable"}
	})

	_, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:   "fleet",
		Payload: []byte(`{"model":"fleet"}`),
	})
	var comboErr *comboError
	if !errors.As(err, &comboErr) {
		t.Fatalf("error = %v, want comboError", err)
	}
	if !comboErr.Retryable || comboErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("comboError = %+v, want retryable 503", comboErr)
	}
	if len(attempts) != 2 || attempts[0] != "bad-a" || attempts[1] != "bad-b" {
		t.Fatalf("attempts = %v, want both members once", attempts)
	}
	if !strings.Contains(comboErr.Message, "attempts=") {
		t.Fatalf("error message lacks an attempt trace: %s", comboErr.Message)
	}
}

func TestExecuteNonRetryableStopsChain(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "bad", "good")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "bad" {
			return nil, &hostError{Status: http.StatusBadRequest, Message: "bad request"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	})

	_, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)})
	var comboErr *comboError
	if !errors.As(err, &comboErr) {
		t.Fatalf("error = %v, want comboError", err)
	}
	if comboErr.Status != http.StatusBadRequest || comboErr.Retryable {
		t.Fatalf("comboError = %+v, want non-retryable 400", comboErr)
	}
	if len(attempts) != 1 || attempts[0] != "bad" {
		t.Fatalf("attempts = %v, second member must never be called", attempts)
	}
}

func TestExecuteRoundRobinRotation(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1", "m2")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var served []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		served = append(served, req.Model)
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	for range 4 {
		if _, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	want := []string{"m0", "m1", "m2", "m0"}
	if strings.Join(served, ",") != strings.Join(want, ",") {
		t.Fatalf("served = %v, want %v", served, want)
	}
}

func TestExecuteFailoverOrder(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "first", "second", "third")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "first" {
			return nil, &hostError{Status: http.StatusTooManyRequests, Retryable: true, Message: "slow down"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"served":"second"}`)}, nil
	})

	response, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(response.Payload) != `{"served":"second"}` {
		t.Fatalf("payload = %s", response.Payload)
	}
	if strings.Join(attempts, ",") != "first,second" {
		t.Fatalf("attempts = %v, want ordered failover", attempts)
	}
}

func TestExecutePassesProtocolsAndRewritesModel(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "target-model")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var got pluginapi.HostModelExecutionRequest
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		got = req
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK}, nil
	})

	payload := []byte(`{"model":"fleet","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{
		Model:        "fleet",
		SourceFormat: "openai",
		Format:       "claude",
		Headers:      map[string][]string{"x-test": {"1"}},
		Payload:      payload,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.EntryProtocol != "openai" || got.ExitProtocol != "claude" {
		t.Fatalf("protocols = %q/%q", got.EntryProtocol, got.ExitProtocol)
	}
	if got.Model != "target-model" {
		t.Fatalf("host model = %q, want member model", got.Model)
	}
	if strings.Contains(string(got.Body), `"model":"fleet"`) || !strings.Contains(string(got.Body), `"model":"target-model"`) {
		t.Fatalf("body model not rewritten: %s", got.Body)
	}
	if !strings.Contains(string(got.Body), `"temperature":0.2`) {
		t.Fatalf("body lost unrelated fields: %s", got.Body)
	}
}
