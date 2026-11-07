package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func decodeEnvelope(t *testing.T, raw []byte) abiEnvelope {
	t.Helper()
	var envelope abiEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return envelope
}

func lifecycleRequest(t *testing.T, config string) []byte {
	t.Helper()
	raw, err := json.Marshal(abiLifecycleRequest{ConfigYAML: []byte(config)})
	if err != nil {
		t.Fatalf("marshal lifecycle request: %v", err)
	}
	return raw
}

func TestABIRegisterDeclaresCapabilities(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	config := `
require_managed_keys: true
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o"]
`
	raw, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, config))
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("registration failed: %s", raw)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	if registration.Metadata.Name != pluginName || registration.Metadata.Version != pluginVersion {
		t.Fatalf("metadata = %#v", registration.Metadata)
	}
	caps := registration.Capabilities
	if !caps.FrontendAuth || !caps.FrontendAuthProviderExclusive || !caps.FrontendAuthOmitBody {
		t.Fatalf("frontend auth capabilities = %#v", caps)
	}
	if !caps.RequestIntercept || !caps.RequestInterceptOmitBodyBefore || !caps.RequestInterceptOmitBodyAfter {
		t.Fatalf("request interceptor capabilities = %#v", caps)
	}
	if !caps.ResponseIntercept {
		t.Fatalf("response interceptor capability missing: %#v", caps)
	}
	if currentPlugin() == nil {
		t.Fatal("plugin was not activated")
	}
}

func TestABIRegisterDefaultsToExclusive(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	raw, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, "keys:\n  - key: sk-test\n"))
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if !envelope.OK {
		t.Fatalf("registration failed: %s", raw)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if !registration.Capabilities.FrontendAuthProviderExclusive {
		t.Fatalf("require_managed_keys defaulted to false: %#v", registration.Capabilities)
	}
}

func TestABIReconfigureKeepsOldConfigOnError(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, `keys: [{name: alice, key: sk-alice}]`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	old := currentPlugin()
	if old == nil {
		t.Fatal("plugin was not activated")
	}

	raw, err := abiHandleMethod(pluginabi.MethodPluginReconfigure, lifecycleRequest(t, `keys: [{name: broken}]`))
	if err == nil {
		t.Fatalf("invalid reconfigure unexpectedly succeeded: %s", raw)
	}
	if currentPlugin() != old {
		t.Fatal("invalid reconfigure replaced the active plugin")
	}
	if currentPlugin().cfg.lookupString("sk-alice") == nil {
		t.Fatal("old configuration was lost")
	}
}

func TestABIFrontendAuthIdentifier(t *testing.T) {
	raw, err := abiHandleMethod(pluginabi.MethodFrontendAuthIdentifier, nil)
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	var identifier abiIdentifierResponse
	if err := json.Unmarshal(envelope.Result, &identifier); err != nil {
		t.Fatalf("decode identifier: %v", err)
	}
	if identifier.Identifier != "tea-keys" {
		t.Fatalf("identifier = %q, want tea-keys", identifier.Identifier)
	}
}

func TestABIFrontendAuthAuthenticate(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, `keys: [{name: alice, key: sk-alice}]`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	request, _ := json.Marshal(pluginapi.FrontendAuthRequest{
		Headers: http.Header{"X-Api-Key": []string{"sk-alice"}},
	})
	raw, err := abiHandleMethod(pluginabi.MethodFrontendAuthAuthenticate, request)
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	var response pluginapi.FrontendAuthResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode auth response: %v", err)
	}
	if !response.Authenticated || response.Principal != "alice" {
		t.Fatalf("response = %#v", response)
	}
}

func TestABIRequestInterceptBefore(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, `
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o"]
`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	request, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		SourceFormat:   "openai",
		RequestedModel: "gpt-4o-mini",
		Headers:        authHeaders("sk-alice"),
	})
	raw, err := abiHandleMethod(pluginabi.MethodRequestInterceptBefore, request)
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode intercept response: %v", err)
	}
	if !response.Terminate || response.StatusCode != http.StatusForbidden {
		t.Fatalf("response = %#v", response)
	}
}

func TestABIResponseInterceptAfter(t *testing.T) {
	t.Cleanup(func() { active.Store(nil) })
	if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, `
keys:
  - name: alice
    key: sk-alice
    models: ["gpt-4o"]
`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	request, _ := json.Marshal(pluginapi.ResponseInterceptRequest{
		RequestHeaders:  authHeaders("sk-alice"),
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		Body:            []byte(`{"object":"list","data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`),
	})
	raw, err := abiHandleMethod(pluginabi.MethodResponseInterceptAfter, request)
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	var response pluginapi.ResponseInterceptResponse
	if err := json.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatalf("decode response intercept: %v", err)
	}
	assertModelIDs(t, response.Body, "gpt-4o")
}

func TestABIUnknownMethod(t *testing.T) {
	raw, err := abiHandleMethod("not.a.method", nil)
	if err != nil {
		t.Fatalf("abiHandleMethod() error = %v", err)
	}
	envelope := decodeEnvelope(t, raw)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "unknown_method" {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestABIShutdownClearsActive(t *testing.T) {
	if _, err := abiHandleMethod(pluginabi.MethodPluginRegister, lifecycleRequest(t, `keys: [{key: sk-alice}]`)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if currentPlugin() == nil {
		t.Fatal("plugin was not activated")
	}
	if _, err := abiHandleMethod(pluginabi.MethodPluginShutdown, nil); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if currentPlugin() != nil {
		t.Fatal("shutdown did not clear the active plugin")
	}
}
