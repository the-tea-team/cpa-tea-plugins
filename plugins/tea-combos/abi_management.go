package main

import (
	"context"
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// abiManagementRequest mirrors CPA's wrapper around a Management API or resource request. The
// host callback id rides beside the request and matters only for handlers that call back.
type abiManagementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// abiManagementRoute and abiResourceRoute are the wire forms of a route declaration. The host
// decodes into pluginapi types and supplies the handler itself, so handlers must not cross the
// ABI: a func value would decode as an object and the whole registration would be rejected.
type abiManagementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type abiResourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type abiManagementRegistrationResponse struct {
	Routes    []abiManagementRoute `json:"routes,omitempty"`
	Resources []abiResourceRoute   `json:"resources,omitempty"`
}

// handleManagementRegister answers management.register with the plugin's routes and resources.
func handleManagementRegister(plugin *combosPlugin, request []byte) ([]byte, error) {
	var req pluginapi.ManagementRegistrationRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	resp, err := plugin.RegisterManagement(context.Background(), req)
	if err != nil {
		return nil, err
	}
	wire := abiManagementRegistrationResponse{
		Routes:    make([]abiManagementRoute, 0, len(resp.Routes)),
		Resources: make([]abiResourceRoute, 0, len(resp.Resources)),
	}
	for _, route := range resp.Routes {
		wire.Routes = append(wire.Routes, abiManagementRoute{
			Method:      route.Method,
			Path:        route.Path,
			Menu:        route.Menu,
			Description: route.Description,
		})
	}
	for _, resource := range resp.Resources {
		wire.Resources = append(wire.Resources, abiResourceRoute{
			Path:        resource.Path,
			Menu:        resource.Menu,
			Description: resource.Description,
		})
	}
	return abiOKEnvelope(wire)
}

// handleManagementCall answers management.handle for both the authenticated API routes and the
// unauthenticated resource route, dispatching by path.
func handleManagementCall(plugin *combosPlugin, request []byte) ([]byte, error) {
	var req abiManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	resp, err := plugin.handleManagementRequest(context.Background(), req.ManagementRequest)
	if err != nil {
		return nil, err
	}
	return abiOKEnvelope(resp)
}
