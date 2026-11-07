package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return store
}

func upsertCombo(t *testing.T, store *Store, combo Combo) {
	t.Helper()
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert %q: %v", combo.Name, err)
	}
}

func TestComboModelsFiltersSortsAndShapes(t *testing.T) {
	tests := []struct {
		name string
		seed []Combo
		want []string
	}{
		{
			name: "enabled non-empty combos only, sorted by id",
			seed: []Combo{
				mustCombo(t, "zeta", StrategyRoundRobin, "m1"),
				mustCombo(t, "alpha", StrategyFailover, "m1", "m2"),
				{Name: "disabled", Members: []Member{{Model: "m1"}}, Enabled: boolPtr(false)},
				{Name: "empty", Members: nil},
			},
			want: []string{"alpha", "zeta"},
		},
		{
			name: "enabled empty and disabled non-empty are both excluded",
			seed: []Combo{
				{Name: "enabled-empty", Enabled: boolPtr(true)},
				{Name: "disabled-full", Members: []Member{{Model: "m1"}}, Enabled: boolPtr(false)},
			},
			want: []string{},
		},
		{
			name: "unset enabled defaults to enabled",
			seed: []Combo{{Name: "implicit", Members: []Member{{Model: "m1"}}}},
			want: []string{"implicit"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			for _, combo := range tt.seed {
				upsertCombo(t, store, combo)
			}
			models := comboModels(store)
			got := make([]string, 0, len(models))
			for _, model := range models {
				got = append(got, model.ID)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("model ids = %v, want %v", got, tt.want)
			}
			for _, model := range models {
				if model.Object != "model" {
					t.Fatalf("model %q object = %q", model.ID, model.Object)
				}
				if model.OwnedBy != comboModelOwner {
					t.Fatalf("model %q owned_by = %q", model.ID, model.OwnedBy)
				}
				if model.Created != 0 || model.Type != "" || model.DisplayName != "" || model.Description != "" {
					t.Fatalf("model %q carries invented fields: %+v", model.ID, model)
				}
			}
		})
	}
}

func TestComboModelsReflectsStoreEditsImmediately(t *testing.T) {
	store := openTestStore(t)
	plugin := &combosPlugin{store: store}

	list := func() []string {
		t.Helper()
		resp, err := plugin.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
		if err != nil {
			t.Fatalf("RegisterModels: %v", err)
		}
		ids := make([]string, 0, len(resp.Models))
		for _, model := range resp.Models {
			ids = append(ids, model.ID)
		}
		return ids
	}

	if ids := list(); len(ids) != 0 {
		t.Fatalf("empty store listed %v", ids)
	}

	upsertCombo(t, store, mustCombo(t, "fast-fleet", StrategyRoundRobin, "m1"))
	if ids := list(); fmt.Sprint(ids) != "[fast-fleet]" {
		t.Fatalf("after create = %v", ids)
	}

	updated := mustCombo(t, "fast-fleet", StrategyFailover, "m1", "m2")
	upsertCombo(t, store, updated)
	if ids := list(); fmt.Sprint(ids) != "[fast-fleet]" {
		t.Fatalf("after update = %v", ids)
	}

	upsertCombo(t, store, Combo{Name: "fast-fleet", Members: updated.Members, Enabled: boolPtr(false)})
	if ids := list(); len(ids) != 0 {
		t.Fatalf("after disable = %v", ids)
	}

	if err := store.Delete("fast-fleet"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ids := list(); len(ids) != 0 {
		t.Fatalf("after delete = %v", ids)
	}
}

func TestComboModelsEmptyAndNilStore(t *testing.T) {
	plugin := &combosPlugin{store: openTestStore(t)}
	resp, err := plugin.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatalf("RegisterModels: %v", err)
	}
	if resp.Provider != pluginID {
		t.Fatalf("provider = %q", resp.Provider)
	}
	if resp.Models == nil || len(resp.Models) != 0 {
		t.Fatalf("empty store models = %#v, want an empty non-nil list", resp.Models)
	}

	// A nil store must yield an empty catalog rather than panic: the host calls this hook while
	// the plugin is live, and a panic would fuse the plugin.
	if models := comboModels(nil); models == nil || len(models) != 0 {
		t.Fatalf("nil store models = %#v", models)
	}
}

func TestComboModelsDeduplicatesTrimmedNames(t *testing.T) {
	store := openTestStore(t)
	store.mu.Lock()
	store.state.Combos["fast"] = Combo{Name: "fast", Members: []Member{{Model: "m1"}}}
	store.state.Combos["fast "] = Combo{Name: " fast ", Members: []Member{{Model: "m2"}}}
	store.mu.Unlock()

	models := comboModels(store)
	if len(models) != 1 || models[0].ID != "fast" {
		t.Fatalf("models = %+v, want one deduplicated entry", models)
	}
}

// A combo whose name matches an upstream model id is still published here. The host decides the
// catalog order: a model id already served by a native executor is not republished from a plugin
// registration (internal/pluginhost/adapters_executors.go modelHasNativeExecutor), so the upstream
// entry stays and no duplicate is shown. Routing is unaffected because ModelRouter runs before
// provider resolution, so the combo name still reaches this plugin's executor.
func TestComboModelsPublishesNameCollidingWithUpstreamModel(t *testing.T) {
	store := openTestStore(t)
	upsertCombo(t, store, mustCombo(t, "stream-good", StrategyRoundRobin, "stream-good"))
	models := comboModels(store)
	if len(models) != 1 || models[0].ID != "stream-good" {
		t.Fatalf("collision models = %+v", models)
	}
}

func TestABIModelRegisterEnvelope(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	config := []byte(fmt.Sprintf("statePath: %q\ncombos:\n  fast-fleet:\n    members:\n      - model: m1\n  off:\n    enabled: false\n    members:\n      - model: m1\n", statePath))
	if envelope := callABI(t, pluginabi.MethodPluginRegister, abiLifecycleRequest{ConfigYAML: config}); !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	t.Cleanup(func() { currentPlugin.Store(nil) })

	envelope := callABI(t, pluginabi.MethodModelRegister, pluginapi.ModelRegistrationRequest{})
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("model.register failed: %+v", envelope)
	}
	var registration pluginapi.ModelRegistrationResponse
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode model.register result: %v", err)
	}
	if registration.Provider != pluginID {
		t.Fatalf("provider = %q", registration.Provider)
	}
	if len(registration.Models) != 1 || registration.Models[0].ID != "fast-fleet" {
		t.Fatalf("models = %+v", registration.Models)
	}
	if registration.Models[0].Object != "model" || registration.Models[0].OwnedBy != comboModelOwner {
		t.Fatalf("model fields = %+v", registration.Models[0])
	}
}

func TestABIModelRegisterBeforeRegistrationIsNotReady(t *testing.T) {
	if envelope := callABI(t, pluginabi.MethodPluginShutdown, nil); !envelope.OK {
		t.Fatalf("shutdown failed: %+v", envelope.Error)
	}
	envelope := callABI(t, pluginabi.MethodModelRegister, pluginapi.ModelRegistrationRequest{})
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "not_ready" {
		t.Fatalf("model.register before registration = %+v", envelope)
	}
}

func TestRegistrationDeclaresModelRegistrar(t *testing.T) {
	config := []byte(fmt.Sprintf("statePath: %q\n", filepath.Join(t.TempDir(), "state.json")))
	envelope := callABI(t, pluginabi.MethodPluginRegister, abiLifecycleRequest{ConfigYAML: config})
	if !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !registration.Capabilities.ModelRegistrar {
		t.Fatalf("model_registrar not declared: %+v", registration.Capabilities)
	}
	// The flag must reach the host under CPA's own JSON key, not just be set in Go.
	var raw map[string]any
	payload, err := json.Marshal(registration.Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if raw["model_registrar"] != true {
		t.Fatalf("capabilities JSON = %s", payload)
	}
	t.Cleanup(func() { currentPlugin.Store(nil) })
}
