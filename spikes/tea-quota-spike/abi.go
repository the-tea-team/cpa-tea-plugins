package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int teaComboSpikePluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void teaComboSpikePluginFree(void*, size_t);
extern void teaComboSpikePluginShutdown(void);

static cliproxy_host_api* spike_host;

static void spike_set_host(cliproxy_host_api* host) {
	spike_host = host;
}

static int spike_call_host(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	if (spike_host == NULL || spike_host->call == NULL) {
		return -1;
	}
	return spike_host->call(spike_host->host_ctx, method, req, len, out);
}

static void spike_free_host(void* ptr, size_t len) {
	if (spike_host != NULL && spike_host->free_buffer != NULL) {
		spike_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	Scheduler             bool     `json:"scheduler"`
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ManagementAPI         bool     `json:"management_api"`
	QuotaProvider         bool     `json:"quota_provider"`
	FrontendAuth          bool     `json:"frontend_auth_provider"`
	UsagePlugin           bool     `json:"usage_plugin"`
	RequestIntercept      bool     `json:"request_interceptor"`
	ExecutorModelScope    string   `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats  []string `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string `json:"executor_output_formats,omitempty"`
}

// CPA refuses a plugin executor route unless the executor declares the protocol formats it
// accepts and emits. Without these, executorPluginReady() fails and the route is silently skipped.
var abiEnabledCapabilities = abiCapabilities{
	ModelRouter:           true,
	Executor:              true,
	ExecutorModelScope:    "both",
	ExecutorInputFormats:  []string{"openai", "claude", "gemini", "codex"},
	ExecutorOutputFormats: []string{"openai", "claude", "gemini", "codex"},
}

type abiLifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type abiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *abiError       `json:"error,omitempty"`
}

type abiError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// hostError is a failed host callback. CPA encodes upstream failures as errors rather than a
// response with a non-2xx status, so the retryable flag and http_status drive combo fallback.
type hostError struct {
	Method    string
	Code      string
	Message   string
	Retryable bool
	Status    int
}

func (e *hostError) Error() string { return e.Message }

// The host callback id ties our executor call to the callback context CPA opened for it.
// Without it CPA cannot resolve the caller's plugin id, so re-entrancy protection would not work.
type abiExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

var currentExecutor *comboSpikePlugin

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.spike_set_host(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.teaComboSpikePluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.teaComboSpikePluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.teaComboSpikePluginShutdown)
	return 0
}

//export teaComboSpikePluginCall
func teaComboSpikePluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		abiWriteResponse(response, abiErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := abiHandleMethod(C.GoString(method), requestBytes)
	if err != nil {
		abiWriteResponse(response, abiErrorEnvelope("plugin_error", err.Error()))
		return 1
	}
	abiWriteResponse(response, raw)
	return 0
}

//export teaComboSpikePluginFree
func teaComboSpikePluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export teaComboSpikePluginShutdown
func teaComboSpikePluginShutdown() {}

func abiHandleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req abiLifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		plugin, err := buildPlugin(req.ConfigYAML, "")
		if err != nil {
			return nil, err
		}
		// Keep the concrete plugin for later capability calls.
		currentExecutor = &comboSpikePlugin{}
		if err := currentExecutor.Reconfigure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return abiOKEnvelope(abiRegistration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata:      plugin.Metadata,
			Capabilities:  abiEnabledCapabilities,
		})
	case pluginabi.MethodModelRoute:
		if currentExecutor == nil {
			return abiErrorEnvelope("not_ready", "plugin not registered"), nil
		}
		var req pluginapi.ModelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		resp, err := currentExecutor.RouteModel(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return abiOKEnvelope(resp)
	case pluginabi.MethodExecutorExecute:
		if currentExecutor == nil {
			return abiErrorEnvelope("not_ready", "plugin not registered"), nil
		}
		var req abiExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		activeCallbackID = req.HostCallbackID
		resp, err := currentExecutor.Execute(context.Background(), req.ExecutorRequest)
		if err != nil {
			var cErr *comboError
			if errors.As(err, &cErr) {
				raw, mErr := json.Marshal(abiEnvelope{OK: false, Error: &abiError{
					Code: "combo_failed", Message: cErr.Message,
					Retryable: cErr.Retryable, HTTPStatus: cErr.Status,
				}})
				return raw, mErr
			}
			return nil, err
		}
		return abiOKEnvelope(resp)
	case pluginabi.MethodExecutorIdentifier:
		return abiOKEnvelope(map[string]string{"identifier": "tea-combo-spike"})
	case pluginabi.MethodPluginShutdown:
		return abiOKEnvelope(map[string]any{})
	default:
		return abiErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

var activeCallbackID string

// rpcHostModelExecutionRequest mirrors CPA's host-side wrapper: the callback id rides alongside
// the execution request, not inside it.
type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// hostModelExecute asks CPA to execute a real provider/model on our behalf.
func hostModelExecute(req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
	raw, err := hostCall(pluginabi.MethodHostModelExecute, rpcHostModelExecutionRequest{
		HostModelExecutionRequest: req,
		HostCallbackID:            activeCallbackID,
	})
	if err != nil {
		return nil, err
	}
	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode host model response: %w", err)
	}
	return &resp, nil
}

func hostCall(method string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var req *C.uint8_t
	if len(raw) > 0 {
		req = (*C.uint8_t)(C.CBytes(raw))
		defer C.free(unsafe.Pointer(req))
	}
	var response C.cliproxy_buffer
	rc := C.spike_call_host(cMethod, req, C.size_t(len(raw)), &response)
	if response.ptr != nil {
		defer C.spike_free_host(response.ptr, response.len)
	}
	if rc != 0 {
		return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, errors.New("host callback returned no data")
	}
	body := C.GoBytes(response.ptr, C.int(response.len))
	var env abiEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope: %w", err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostError{
				Method:    method,
				Code:      env.Error.Code,
				Message:   env.Error.Message,
				Retryable: env.Error.Retryable,
				Status:    env.Error.HTTPStatus,
			}
		}
		return nil, errors.New("host callback returned not ok")
	}
	return env.Result, nil
}

func abiOKEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(abiEnvelope{OK: true, Result: raw})
}

func abiErrorEnvelope(code, message string) []byte {
	raw, err := json.Marshal(abiEnvelope{OK: false, Error: &abiError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"marshal_error","message":"envelope marshal failed"}}`)
	}
	return raw
}

func abiWriteResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	buf := C.CBytes(raw)
	response.ptr = buf
	response.len = C.size_t(len(raw))
}
