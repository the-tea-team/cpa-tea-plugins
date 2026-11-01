package main

import (
	"context"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// comboModelOwner is the owned_by value on combo entries in the client-facing model catalog.
// It is the plugin id, which is also the provider key the models are registered under.
const comboModelOwner = pluginID

// RegisterModels publishes every enabled, non-empty combo as a model in CPA's catalog, which
// GET /v1/models reads. The host binds this plugin's executor to the first registered provider, so
// the provider key is deliberately the same identifier the router already targets: pluginID.
func (p *combosPlugin) RegisterModels(_ context.Context, _ pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	return pluginapi.ModelRegistrationResponse{
		Provider: pluginID,
		Models:   comboModels(p.store),
	}, nil
}

// comboModels builds the catalog entries from a copy taken under the store lock. Only combos that
// the router would accept are listed: enabled and carrying at least one member. Results are
// deduplicated and sorted by id so the catalog is stable across calls.
func comboModels(store *Store) []pluginapi.ModelInfo {
	if store == nil {
		return []pluginapi.ModelInfo{}
	}
	combos := store.List()
	models := make([]pluginapi.ModelInfo, 0, len(combos))
	seen := make(map[string]struct{}, len(combos))
	for _, combo := range combos {
		name := strings.TrimSpace(combo.Name)
		if name == "" || !combo.IsEnabled() || len(combo.Members) == 0 {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		models = append(models, pluginapi.ModelInfo{
			ID:      name,
			Object:  "model",
			OwnedBy: comboModelOwner,
		})
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}
