package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func callABI(t *testing.T, method string, payload any) abiEnvelope {
	t.Helper()
	var raw []byte
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s request: %v", method, err)
		}
	}
	body, err := abiHandleMethod(method, raw)
	if err != nil {
		t.Fatalf("abiHandleMethod(%s): %v", method, err)
	}
	var envelope abiEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode %s envelope: %v", method, err)
	}
	return envelope
}

func TestRegistrationDeclaresOnlyRouterAndExecutor(t *testing.T) {
	config := []byte(fmt.Sprintf("statePath: %q\ncombos:\n  fleet:\n    members:\n      - model: gpt-a\n", filepath.Join(t.TempDir(), "state.json")))
	envelope := callABI(t, pluginabi.MethodPluginRegister, abiLifecycleRequest{ConfigYAML: config})
	if !envelope.OK || envelope.Error != nil {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	var registration abiRegistration
	if err := json.Unmarshal(envelope.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	caps := registration.Capabilities
	if !caps.ModelRouter || !caps.Executor {
		t.Fatalf("router/executor not declared: %+v", caps)
	}
	if caps.ManagementAPI || caps.Scheduler || caps.QuotaProvider || caps.FrontendAuth {
		t.Fatalf("slice A declared an unimplemented capability: %+v", caps)
	}
	if caps.ExecutorModelScope != "both" {
		t.Fatalf("executor_model_scope = %q", caps.ExecutorModelScope)
	}
	wantFormats := []string{"openai", "claude", "gemini", "codex"}
	if strings.Join(caps.ExecutorInputFormats, ",") != strings.Join(wantFormats, ",") ||
		strings.Join(caps.ExecutorOutputFormats, ",") != strings.Join(wantFormats, ",") {
		t.Fatalf("formats = %v/%v", caps.ExecutorInputFormats, caps.ExecutorOutputFormats)
	}
}

func TestABIRoutesAndExecutesCombo(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	config := []byte(fmt.Sprintf("statePath: %q\ncombos:\n  fleet:\n    members:\n      - model: member-a\n", statePath))
	if envelope := callABI(t, pluginabi.MethodPluginRegister, abiLifecycleRequest{ConfigYAML: config}); !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}

	route := callABI(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{RequestedModel: "fleet"})
	if !route.OK {
		t.Fatalf("route failed: %+v", route.Error)
	}
	var routeResponse pluginapi.ModelRouteResponse
	if err := json.Unmarshal(route.Result, &routeResponse); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if !routeResponse.Handled || routeResponse.Target != pluginID {
		t.Fatalf("route response = %+v", routeResponse)
	}

	var callbackID string
	stubHostExecute(t, func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		callbackID = hostCallbackIDFromContext(ctx)
		if req.Model != "member-a" {
			return nil, fmt.Errorf("unexpected host model %q", req.Model)
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"served":"member-a"}`)}, nil
	})

	execute := callABI(t, pluginabi.MethodExecutorExecute, abiExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)},
		HostCallbackID:  "callback-1",
	})
	if !execute.OK {
		t.Fatalf("execute failed: %+v", execute.Error)
	}
	var response pluginapi.ExecutorResponse
	if err := json.Unmarshal(execute.Result, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(response.Payload) != `{"served":"member-a"}` {
		t.Fatalf("payload = %s", response.Payload)
	}
	if callbackID != "callback-1" {
		t.Fatalf("host callback id = %q, want callback-1", callbackID)
	}
}

func TestABIErrorEnvelopeCarriesStatus(t *testing.T) {
	config := []byte(fmt.Sprintf("statePath: %q\ncombos:\n  fleet:\n    members:\n      - model: member-a\n", filepath.Join(t.TempDir(), "state.json")))
	if envelope := callABI(t, pluginabi.MethodPluginRegister, abiLifecycleRequest{ConfigYAML: config}); !envelope.OK {
		t.Fatalf("registration failed: %+v", envelope.Error)
	}
	stubHostExecute(t, func(context.Context, pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		return nil, &hostError{Status: http.StatusBadRequest, Message: "client error"}
	})

	envelope := callABI(t, pluginabi.MethodExecutorExecute, abiExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)},
	})
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("expected an error envelope, got %+v", envelope)
	}
	if envelope.Error.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("http_status = %d, want 400", envelope.Error.HTTPStatus)
	}
	if envelope.Error.Retryable {
		t.Fatal("client error must not be marked retryable")
	}
}

func TestABIIdentifier(t *testing.T) {
	envelope := callABI(t, pluginabi.MethodExecutorIdentifier, nil)
	if !envelope.OK {
		t.Fatalf("identifier failed: %+v", envelope.Error)
	}
	var identifier map[string]string
	if err := json.Unmarshal(envelope.Result, &identifier); err != nil {
		t.Fatalf("decode identifier: %v", err)
	}
	if identifier["identifier"] != pluginID {
		t.Fatalf("identifier = %q", identifier["identifier"])
	}
}
