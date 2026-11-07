package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// managementBodyLimit bounds a combo payload. A combo is a handful of member entries, so 1 MiB
// leaves room for large provider names while refusing anything that looks like an upload.
const managementBodyLimit = 1 << 20

// comboCollectionSuffix is the part of the Management API path this plugin owns. The prefix in
// front of it is host-supplied (ManagementRegistrationRequest.BasePath), so the handler locates
// this marker rather than assuming "/v0/management".
const comboCollectionSuffix = "/plugins/" + pluginID + "/combos"

// RegisterManagement declares the plugin's authenticated Management API routes and its single
// browser-navigable resource. The resource is deliberately separate from the API: resource
// requests are not management-authenticated, so they only ever serve the static HTML shell.
func (p *combosPlugin) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	base := strings.TrimRight(strings.TrimSpace(req.BasePath), "/")
	if base == "" {
		return pluginapi.ManagementRegistrationResponse{}, errors.New("management registration is missing the host base path")
	}
	collection := base + comboCollectionSuffix
	api := comboAPIHandler{store: p.store}

	// CPA v8.0.51 dispatches plugin management routes by exact method and path
	// (internal/pluginhost/management.go: managementRouteKey), and rejects ":" and "*" in a
	// declared path. Per-name paths therefore cannot be registered as patterns; the routes above
	// are the collection, and the item is named by ?name= (or by a trailing path segment when a
	// host dispatches one, which the handler also accepts).
	routes := make([]pluginapi.ManagementRoute, 0, 4)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		routes = append(routes, pluginapi.ManagementRoute{
			Method:  method,
			Path:    collection,
			Handler: api,
		})
	}

	return pluginapi.ManagementRegistrationResponse{
		Routes: routes,
		Resources: []pluginapi.ResourceRoute{{
			// CPA v8.0.51 rejects "/" in normalizeResourceRoute (it trims the path to empty), so
			// the page lives at <ResourceBasePath>/combos. CPAMP follows the menu path, so the
			// sidebar entry still works without a host change.
			Path:        "/combos",
			Menu:        "Combos",
			Description: "Named model combos, strategies and session affinity",
			Handler:     uiHandler{},
		}},
	}, nil
}

// comboAPIHandler serves the authenticated CRUD API. It reads combo config from the same Store
// the router uses, so an edit is visible to RouteModel on the next request without a restart.
type comboAPIHandler struct {
	store *Store
}

// HandleManagement implements pluginapi.ManagementHandler.
func (h comboAPIHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return h.handle(req), nil
}

func (h comboAPIHandler) handle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) > managementBodyLimit {
		return jsonError(http.StatusRequestEntityTooLarge, "request body is too large")
	}
	name, ok := comboNameFromRequest(req)
	if !ok {
		return jsonError(http.StatusNotFound, "management route not found")
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch name {
	case "":
		switch method {
		case http.MethodGet:
			if queryName := strings.TrimSpace(req.Query.Get("name")); queryName != "" {
				return h.get(queryName)
			}
			return h.list()
		case http.MethodPost:
			return h.create(req.Body)
		case http.MethodPut, http.MethodDelete:
			queryName := strings.TrimSpace(req.Query.Get("name"))
			if queryName == "" {
				return jsonError(http.StatusBadRequest, "combo name is required")
			}
			if method == http.MethodPut {
				return h.update(queryName, req.Body)
			}
			return h.remove(queryName)
		default:
			return methodNotAllowed()
		}
	default:
		switch method {
		case http.MethodGet:
			return h.get(name)
		case http.MethodPut:
			return h.update(name, req.Body)
		case http.MethodDelete:
			return h.remove(name)
		default:
			return methodNotAllowed()
		}
	}
}

func (h comboAPIHandler) list() pluginapi.ManagementResponse {
	combos := h.store.List()
	return jsonResponse(http.StatusOK, map[string]any{"combos": combos})
}

func (h comboAPIHandler) get(name string) pluginapi.ManagementResponse {
	combo, ok := h.store.Get(name)
	if !ok {
		return jsonError(http.StatusNotFound, fmt.Sprintf("combo %q not found", name))
	}
	return jsonResponse(http.StatusOK, combo)
}

func (h comboAPIHandler) create(body []byte) pluginapi.ManagementResponse {
	combo, err := decodeCombo(body)
	if err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	combo = combo.Normalize()
	if err := combo.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	if _, exists := h.store.Get(combo.Name); exists {
		return jsonError(http.StatusConflict, fmt.Sprintf("combo %q already exists", combo.Name))
	}
	if err := h.store.Upsert(combo); err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return jsonResponse(http.StatusCreated, combo)
}

func (h comboAPIHandler) update(name string, body []byte) pluginapi.ManagementResponse {
	if _, exists := h.store.Get(name); !exists {
		return jsonError(http.StatusNotFound, fmt.Sprintf("combo %q not found", name))
	}
	combo, err := decodeCombo(body)
	if err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	if bodyName := strings.TrimSpace(combo.Name); bodyName != "" && bodyName != name {
		return jsonError(http.StatusBadRequest, fmt.Sprintf("body name %q does not match path name %q", bodyName, name))
	}
	combo.Name = name
	combo = combo.Normalize()
	if err := combo.Validate(); err != nil {
		return jsonError(http.StatusBadRequest, err.Error())
	}
	if err := h.store.Upsert(combo); err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return jsonResponse(http.StatusOK, combo)
}

func (h comboAPIHandler) remove(name string) pluginapi.ManagementResponse {
	if _, exists := h.store.Get(name); !exists {
		return jsonError(http.StatusNotFound, fmt.Sprintf("combo %q not found", name))
	}
	if err := h.store.Delete(name); err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNoContent}
}

// comboNameFromRequest locates the collection marker in the request path and returns the item
// name that follows it. An empty name is the collection itself. The second result is false when
// the request is not for this plugin's collection at all.
func comboNameFromRequest(req pluginapi.ManagementRequest) (string, bool) {
	path := req.Path
	index := strings.Index(path, comboCollectionSuffix)
	if index < 0 {
		return "", false
	}
	rest := path[index+len(comboCollectionSuffix):]
	if rest == "" {
		return "", true
	}
	if !strings.HasPrefix(rest, "/") {
		return "", false
	}
	name := strings.TrimPrefix(rest, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	return name, true
}

// decodeCombo reads exactly one JSON combo, rejecting unknown fields so a caller cannot smuggle
// data into fields the plugin does not own, and refusing a second JSON value in the same body.
func decodeCombo(body []byte) (Combo, error) {
	if len(body) > managementBodyLimit {
		return Combo{}, fmt.Errorf("request body exceeds %d bytes", managementBodyLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var combo Combo
	if err := decoder.Decode(&combo); err != nil {
		return Combo{}, fmt.Errorf("invalid combo body: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Combo{}, errors.New("invalid combo body: expected a single JSON object")
	}
	return combo, nil
}

func jsonResponse(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "failed to encode response")
	}
	return jsonBody(status, body)
}

func jsonError(status int, message string) pluginapi.ManagementResponse {
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		body = []byte(`{"error":"internal error"}`)
	}
	return jsonBody(status, body)
}

func jsonBody(status int, body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	}
}

func methodNotAllowed() pluginapi.ManagementResponse {
	resp := jsonError(http.StatusMethodNotAllowed, "method not allowed")
	resp.Headers.Set("Allow", "GET, POST, PUT, DELETE")
	return resp
}
