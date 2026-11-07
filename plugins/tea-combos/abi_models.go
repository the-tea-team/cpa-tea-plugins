package main

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// handleAbiModelRegister serves CPA's model.register hook. The host calls it while rebuilding the
// model catalog, so the response is derived from the store at call time rather than cached.
func handleAbiModelRegister(plugin *combosPlugin, request []byte) ([]byte, error) {
	if plugin == nil {
		return abiErrorEnvelope("not_ready", "plugin not registered"), nil
	}
	var req pluginapi.ModelRegistrationRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	resp, err := plugin.RegisterModels(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return abiOKEnvelope(resp)
}
