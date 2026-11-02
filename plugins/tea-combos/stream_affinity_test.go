package main

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func streamAffinityPlugin(t *testing.T, combo Combo) *combosPlugin {
	t.Helper()
	plugin := newAffinityPlugin(t, newFakeClock())
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return plugin
}

func runStream(t *testing.T, fake *fakeStreamHost, plugin *combosPlugin, req pluginapi.ExecutorRequest) error {
	t.Helper()
	_, err := plugin.executeStream(withPluginStreamID(context.Background(), "s"), req)
	if err == nil {
		fake.waitForClose(t)
	}
	return err
}

func TestStreamAffinityStickyRoundRobin(t *testing.T) {
	fake := newFakeStreamHost(t)
	for _, model := range []string{"m0", "m1", "m2"} {
		fake.scripts[model] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	}
	plugin := streamAffinityPlugin(t, mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1", "m2"))

	for _, session := range []string{"session-a", "session-a", "session-b", "session-a", "session-b"} {
		if err := runStream(t, fake, plugin, sessionRequest("fleet", session)); err != nil {
			t.Fatalf("stream(%s): %v", session, err)
		}
	}
	want := []string{"m0", "m0", "m2", "m0", "m2"}
	if got := fake.openedModels(); !equalStrings(got, want) {
		t.Fatalf("opened = %v, want %v (bound session must not rotate, another session must)", got, want)
	}
}

func TestStreamAffinityRebindsOnRetryableFailureBeforeFirstByte(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.scripts["m1"] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	plugin := streamAffinityPlugin(t, mustCombo(t, "fleet", StrategyFailover, "m0", "m1"))

	// m0 is down: the session falls to m1 and rebinds there.
	fake.openErrors["m0"] = retryableHostError(http.StatusServiceUnavailable)
	if err := runStream(t, fake, plugin, sessionRequest("fleet", "s1")); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	// m0 recovers, but the bound session stays on m1 and never touches m0.
	delete(fake.openErrors, "m0")
	fake.scripts["m0"] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	if err := runStream(t, fake, plugin, sessionRequest("fleet", "s1")); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	want := []string{"m0", "m1", "m1"}
	if got := fake.openedModels(); !equalStrings(got, want) {
		t.Fatalf("opened = %v, want %v", got, want)
	}
}

func TestStreamAffinityNonRetryableStopLeavesBindingUnchanged(t *testing.T) {
	fake := newFakeStreamHost(t)
	fake.scripts["m0"] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	fake.scripts["m1"] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	plugin := streamAffinityPlugin(t, mustCombo(t, "fleet", StrategyFailover, "m0", "m1"))

	if err := runStream(t, fake, plugin, sessionRequest("fleet", "s1")); err != nil {
		t.Fatalf("bind stream: %v", err)
	}
	fake.openErrors["m0"] = &hostError{Code: "host_call_failed", Message: "invalid request", Status: http.StatusBadRequest}
	err := runStream(t, fake, plugin, sessionRequest("fleet", "s1"))
	var comboErr *comboError
	if !errors.As(err, &comboErr) || comboErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want 400 comboError", err)
	}
	if got := fake.openedModels(); !equalStrings(got, []string{"m0", "m0"}) {
		t.Fatalf("opened = %v, want the bound member only, no fallback past a client error", got)
	}
	delete(fake.openErrors, "m0")
	if err := runStream(t, fake, plugin, sessionRequest("fleet", "s1")); err != nil {
		t.Fatalf("after-stop stream: %v", err)
	}
	if got := fake.openedModels(); !equalStrings(got, []string{"m0", "m0", "m0"}) {
		t.Fatalf("opened = %v, binding must still point at m0", got)
	}
}

func TestStreamAffinityDisabledBehavesAsPlainStrategy(t *testing.T) {
	fake := newFakeStreamHost(t)
	for _, model := range []string{"m0", "m1"} {
		fake.scripts[model] = []pluginapi.HostModelStreamReadResponse{chunk("x"), {Done: true}}
	}
	off := false
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "m0", "m1")
	combo.SessionAffinity = &off
	plugin := streamAffinityPlugin(t, combo)

	for range 4 {
		if err := runStream(t, fake, plugin, sessionRequest("fleet", "s1")); err != nil {
			t.Fatalf("stream: %v", err)
		}
	}
	if got := fake.openedModels(); !equalStrings(got, []string{"m0", "m1", "m0", "m1"}) {
		t.Fatalf("opened = %v, want plain rotation with affinity off", got)
	}
}
