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

extern int tea_metricsPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void tea_metricsPluginFree(void*, size_t);
extern void tea_metricsPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host decodes a flat capability-flag struct, not the pluginapi.Capabilities interface
// values. Keep these JSON names in sync with internal/pluginhost/rpc_schema.go in CPA.
type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	Scheduler        bool `json:"scheduler"`
	ModelRouter      bool `json:"model_router"`
	Executor         bool `json:"executor"`
	ManagementAPI    bool `json:"management_api"`
	QuotaProvider    bool `json:"quota_provider"`
	FrontendAuth     bool `json:"frontend_auth_provider"`
	UsagePlugin      bool `json:"usage_plugin"`
	RequestIntercept bool `json:"request_interceptor"`
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
	Code    string `json:"code"`
	Message string `json:"message"`
}

// abiEnabledCapabilities declares which host interfaces this plugin serves.
// A plugin with no capabilities is rejected by CPA at registration, so the scaffold ships a
// no-op scheduler that always defers to CPA's built-in schedulers. Replace it with your own.
var abiEnabledCapabilities = abiCapabilities{
	Scheduler: true,
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.tea_metricsPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.tea_metricsPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.tea_metricsPluginShutdown)
	return 0
}

//export tea_metricsPluginCall
func tea_metricsPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
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

//export tea_metricsPluginFree
func tea_metricsPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export tea_metricsPluginShutdown
func tea_metricsPluginShutdown() {}

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
		return abiOKEnvelope(abiRegistration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata:      plugin.Metadata,
			Capabilities:  abiEnabledCapabilities,
		})
	case pluginabi.MethodSchedulerPick:
		// No-op: Handled=false hands the request back to CPA's built-in schedulers.
		return abiOKEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	case pluginabi.MethodPluginShutdown:
		return abiOKEnvelope(map[string]any{})
	default:
		// TODO: handle the methods for the capabilities you enabled, e.g.
		// pluginabi.MethodModelRoute, pluginabi.MethodManagementRegister.
		return abiErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
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
