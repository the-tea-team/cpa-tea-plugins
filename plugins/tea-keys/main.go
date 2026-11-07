package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginName = "Tea Keys"

var pluginVersion = "0.1.0"

// teaKeysPlugin authenticates plugin-managed client keys and enforces each key's
// client-facing model allowlist on execution requests and on model catalogs.
type teaKeysPlugin struct {
	cfg *compiledConfig
}

// active is the plugin instance currently registered with CPA. Reconfigure swaps it
// only after the new configuration parses, so a bad reload leaves the old rules in place.
var active atomic.Pointer[teaKeysPlugin]

func newTeaKeysPlugin(configYAML []byte) (*teaKeysPlugin, error) {
	cfg, err := newCompiledConfig(configYAML)
	if err != nil {
		return nil, err
	}
	return &teaKeysPlugin{cfg: cfg}, nil
}

func buildPlugin(configYAML []byte, _ string) (pluginapi.Plugin, error) {
	plugin, err := newTeaKeysPlugin(configYAML)
	if err != nil {
		return pluginapi.Plugin{}, err
	}
	active.Store(plugin)
	return pluginRegistration(plugin), nil
}

func pluginRegistration(plugin *teaKeysPlugin) pluginapi.Plugin {
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "The Tea Team",
			GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "require_managed_keys",
					Type:        pluginapi.ConfigFieldTypeBoolean,
					Default:     true,
					Description: "Make tea-keys the exclusive frontend auth provider and reject keys that are not listed here.",
				},
				{
					Name:        "keys",
					Type:        pluginapi.ConfigFieldTypeArray,
					Description: "Managed client keys with an optional per-key model allowlist.",
				},
			},
		},
		Capabilities: pluginapi.Capabilities{
			FrontendAuthProvider:             plugin,
			FrontendAuthProviderExclusive:    plugin.cfg.requireManagedKeys,
			FrontendAuthProviderOmitBody:     true,
			RequestInterceptor:               plugin,
			RequestInterceptorOmitBodyBefore: true,
			RequestInterceptorOmitBodyAfter:  true,
			ResponseInterceptor:              plugin,
		},
	}
}

func currentPlugin() *teaKeysPlugin {
	return active.Load()
}

// Identifier is the frontend auth provider identifier CPA exposes in usage records.
func (p *teaKeysPlugin) Identifier() string {
	return "tea-keys"
}

// Authenticate accepts a managed key and reports its display name as the principal.
// Unmanaged keys are left to the other access providers unless RequireManagedKeys
// makes tea-keys exclusive.
func (p *teaKeysPlugin) Authenticate(_ context.Context, req pluginapi.FrontendAuthRequest) (pluginapi.FrontendAuthResponse, error) {
	if p == nil || p.cfg == nil {
		return pluginapi.FrontendAuthResponse{}, nil
	}

	headerValues := headerCredentialCandidates(req.Headers)
	for _, value := range headerValues {
		if key := p.cfg.lookupString(value); key != nil {
			return pluginapi.FrontendAuthResponse{
				Authenticated: true,
				Principal:     key.name,
				Metadata: map[string]string{
					"key_id": key.name,
					"plugin": "tea-keys",
				},
			}, nil
		}
	}

	queryValues := queryCredentialCandidates(req.Query)
	managedQuery := false
	for _, value := range queryValues {
		if p.cfg.lookupString(value) != nil {
			managedQuery = true
			break
		}
	}
	if managedQuery {
		// The model-list response interceptor only receives request headers, so a
		// query-string credential could list models the key is not allowed to use.
		// Reject it instead of allowing a filtering bypass.
		return denyAuth(http.StatusUnauthorized, "Model-restricted API keys must be sent in the Authorization, X-Api-Key or X-Goog-Api-Key header"), nil
	}

	if p.cfg.requireManagedKeys {
		if len(headerValues) == 0 && len(queryValues) == 0 {
			// Let CPA report its normal "Missing API key" response.
			return pluginapi.FrontendAuthResponse{}, nil
		}
		return denyAuth(http.StatusUnauthorized, "Invalid API key"), nil
	}
	return pluginapi.FrontendAuthResponse{}, nil
}

// InterceptRequestBeforeAuth rejects an execution request before credential selection
// when the calling key is not allowed to use the requested model.
func (p *teaKeysPlugin) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	if p == nil || p.cfg == nil {
		return pluginapi.RequestInterceptResponse{}, nil
	}

	key := p.cfg.keyForIntercept(req)
	if key == nil {
		if p.cfg.requireManagedKeys {
			return denialResponse(req.SourceFormat, http.StatusUnauthorized, "unmanaged_api_key",
				"This API key is not managed by tea-keys"), nil
		}
		return pluginapi.RequestInterceptResponse{}, nil
	}

	model := strings.TrimSpace(req.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(req.Model)
	}
	if model == "" {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	if key.allows(model) {
		return pluginapi.RequestInterceptResponse{}, nil
	}
	return denialResponse(req.SourceFormat, http.StatusForbidden, "model_not_allowed",
		fmt.Sprintf("Model %q is not allowed for this API key", model)), nil
}

// InterceptRequestAfterAuth is intentionally a no-op: the before-auth hook already
// rejected disallowed models, and re-checking the selected upstream model would reject
// legitimate aliases.
func (p *teaKeysPlugin) InterceptRequestAfterAuth(_ context.Context, req pluginapi.RequestInterceptRequest) (pluginapi.RequestInterceptResponse, error) {
	return pluginapi.RequestInterceptResponse{}, nil
}

// InterceptResponse filters model catalogs to the calling key's allowlist. CPA routes
// /v1/models, /v1beta/models and the Codex/Grok catalogs through this hook with empty
// Model and RequestedModel fields; ordinary completion responses carry a model and are
// left alone.
func (p *teaKeysPlugin) InterceptResponse(_ context.Context, req pluginapi.ResponseInterceptRequest) (pluginapi.ResponseInterceptResponse, error) {
	if p == nil || p.cfg == nil {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	if strings.TrimSpace(req.RequestedModel) != "" || strings.TrimSpace(req.Model) != "" {
		return pluginapi.ResponseInterceptResponse{}, nil
	}

	key := p.cfg.keyForHeaders(req.RequestHeaders)
	if key == nil {
		if !p.cfg.requireManagedKeys {
			return pluginapi.ResponseInterceptResponse{}, nil
		}
		// Fail closed: an unmanaged caller must not receive the unfiltered catalog.
		filtered, changed := filterModelList(req.Body, func(string) bool { return false })
		if !changed {
			return pluginapi.ResponseInterceptResponse{}, nil
		}
		headers, body := responseWithModelList(req.ResponseHeaders, filtered)
		return pluginapi.ResponseInterceptResponse{Headers: headers, Body: body}, nil
	}
	if key.allowAll {
		return pluginapi.ResponseInterceptResponse{}, nil
	}

	filtered, changed := filterModelList(req.Body, key.allows)
	if !changed {
		return pluginapi.ResponseInterceptResponse{}, nil
	}
	headers, body := responseWithModelList(req.ResponseHeaders, filtered)
	return pluginapi.ResponseInterceptResponse{Headers: headers, Body: body}, nil
}

func denyAuth(status int, message string) pluginapi.FrontendAuthResponse {
	return pluginapi.FrontendAuthResponse{
		Authenticated:  false,
		DenyStatusCode: status,
		DenyMessage:    message,
	}
}

func denialResponse(sourceFormat string, status int, code, message string) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		ResponseBody:    errorBody(sourceFormat, status, code, message),
	}
}

func errorBody(sourceFormat string, status int, code, message string) []byte {
	var payload any
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "claude":
		errorType := "permission_error"
		if status == http.StatusUnauthorized {
			errorType = "authentication_error"
		}
		payload = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errorType,
				"message": message,
			},
		}
	case "gemini":
		errorStatus := "PERMISSION_DENIED"
		if status == http.StatusUnauthorized {
			errorStatus = "UNAUTHENTICATED"
		}
		payload = map[string]any{
			"error": map[string]any{
				"code":    status,
				"message": message,
				"status":  errorStatus,
			},
		}
	default:
		errorType := "invalid_request_error"
		if status == http.StatusUnauthorized {
			errorType = "authentication_error"
		}
		payload = map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    errorType,
				"param":   nil,
				"code":    code,
			},
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"error":{"message":"model access denied","type":"invalid_request_error"}}`)
	}
	return encoded
}
