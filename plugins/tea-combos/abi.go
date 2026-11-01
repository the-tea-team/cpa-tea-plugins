package main

/*
#cgo CFLAGS: -D_GNU_SOURCE
#cgo LDFLAGS: -ldl
#include <stdint.h>
#include <stdlib.h>
#include <stddef.h>
#include <string.h>
#include <dlfcn.h>

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

extern int teaCombosPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void teaCombosPluginFree(void*, size_t);
extern void teaCombosPluginShutdown(void);

static cliproxy_host_api* tea_combos_host;

static void tea_combos_set_host(cliproxy_host_api* host) {
	tea_combos_host = host;
}

static int tea_combos_call_host(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	if (tea_combos_host == NULL || tea_combos_host->call == NULL) {
		return -1;
	}
	return tea_combos_host->call(tea_combos_host->host_ctx, method, req, len, out);
}

static void tea_combos_free_host(void* ptr, size_t len) {
	if (tea_combos_host != NULL && tea_combos_host->free_buffer != NULL) {
		tea_combos_host->free_buffer(ptr, len);
	}
}

static int tea_combos_library_anchor;

// tea_combos_library_path resolves the loaded plugin library so the default state file can sit
// beside it. It returns -1 when the host did not load us from a filesystem path.
static int tea_combos_library_path(char* out, size_t out_len) {
	Dl_info info;
	if (dladdr((void*)&tea_combos_library_anchor, &info) == 0 || info.dli_fname == NULL) {
		return -1;
	}
	size_t n = strlen(info.dli_fname);
	if (n + 1 > out_len) {
		return -1;
	}
	memcpy(out, info.dli_fname, n + 1);
	return 0;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The host decodes a flat capability-flag struct, not pluginapi.Capabilities. Keep these JSON
// names in sync with internal/pluginhost/rpc_schema.go in CPA v8.0.51.
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

// CPA silently skips an executor route unless the plugin declares the protocol formats it
// accepts and emits. Slice A declares only the two capabilities it implements.
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
// response with a non-2xx status, so retryable and http_status drive combo fallback.
type hostError struct {
	Method    string
	Code      string
	Message   string
	Retryable bool
	Status    int
}

func (e *hostError) Error() string { return e.Message }

// abiExecutorRequest mirrors CPA's wrapper: the callback id rides beside the executor request.
type abiExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostCallbackIDKey struct{}

func withHostCallbackID(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, hostCallbackIDKey{}, id)
}

func hostCallbackIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(hostCallbackIDKey{}).(string)
	return id
}

var currentPlugin atomic.Pointer[combosPlugin]

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.tea_combos_set_host(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.teaCombosPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.teaCombosPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.teaCombosPluginShutdown)
	return 0
}

//export teaCombosPluginCall
func teaCombosPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
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

//export teaCombosPluginFree
func teaCombosPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export teaCombosPluginShutdown
func teaCombosPluginShutdown() {
	if plugin := currentPlugin.Load(); plugin != nil {
		plugin.shutdown()
	}
}

// abiHandleMethod recovers from panics: a panic crossing the cgo export would take CPA down,
// so every hook failure must become an error envelope instead.
func abiHandleMethod(method string, request []byte) (raw []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			raw = nil
			err = fmt.Errorf("plugin panic: %v", recovered)
		}
	}()

	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req abiLifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, err
			}
		}
		// Persist pending bindings before reloading state, so a reconfigure cannot lose a
		// conversation that was bound since the last write-behind flush.
		if previous := currentPlugin.Load(); previous != nil && previous.affinity != nil {
			_ = previous.affinity.Flush()
		}
		plugin, err := newCombosPlugin(req.ConfigYAML, pluginLibraryDir())
		if err != nil {
			return nil, err
		}
		if previous := currentPlugin.Load(); previous != nil {
			previous.shutdown()
		}
		currentPlugin.Store(plugin)
		return abiOKEnvelope(abiRegistration{
			SchemaVersion: pluginabi.SchemaVersion,
			Metadata:      plugin.plugin().Metadata,
			Capabilities:  abiEnabledCapabilities,
		})
	case pluginabi.MethodModelRoute:
		plugin := currentPlugin.Load()
		if plugin == nil {
			return abiErrorEnvelope("not_ready", "plugin not registered"), nil
		}
		var req pluginapi.ModelRouteRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		resp, err := plugin.RouteModel(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return abiOKEnvelope(resp)
	case pluginabi.MethodExecutorExecute:
		plugin := currentPlugin.Load()
		if plugin == nil {
			return abiErrorEnvelope("not_ready", "plugin not registered"), nil
		}
		var req abiExecutorRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
		ctx := withHostCallbackID(context.Background(), req.HostCallbackID)
		resp, err := plugin.Execute(ctx, req.ExecutorRequest)
		if err != nil {
			var comboErr *comboError
			if errors.As(err, &comboErr) {
				raw, errMarshal := json.Marshal(abiEnvelope{OK: false, Error: &abiError{
					Code:       "combo_failed",
					Message:    comboErr.Message,
					Retryable:  comboErr.Retryable,
					HTTPStatus: comboErr.Status,
				}})
				return raw, errMarshal
			}
			return nil, err
		}
		return abiOKEnvelope(resp)
	case pluginabi.MethodExecutorExecuteStream:
		return handleAbiExecuteStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return abiErrorEnvelope("not_implemented", "tea-combos: token counting is not supported"), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return abiErrorEnvelope("not_implemented", "tea-combos: HTTP bridging is not supported"), nil
	case pluginabi.MethodExecutorIdentifier:
		return abiOKEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodPluginShutdown:
		if plugin := currentPlugin.Load(); plugin != nil {
			plugin.shutdown()
		}
		currentPlugin.Store(nil)
		return abiOKEnvelope(map[string]any{})
	default:
		return abiErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// rpcHostModelExecutionRequest mirrors CPA's host-side wrapper: the callback id rides beside
// the execution request, not inside it.
type rpcHostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// hostModelExecute is a package-level seam so tests can stub the host call without network.
var hostModelExecute = func(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
	return hostModelExecuteRPC(ctx, req)
}

func hostModelExecuteRPC(ctx context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
	raw, err := hostCall(pluginabi.MethodHostModelExecute, rpcHostModelExecutionRequest{
		HostModelExecutionRequest: req,
		HostCallbackID:            hostCallbackIDFromContext(ctx),
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
	rc := C.tea_combos_call_host(cMethod, req, C.size_t(len(raw)), &response)
	if response.ptr != nil {
		defer C.tea_combos_free_host(response.ptr, response.len)
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

// pluginLibraryDir returns the directory that holds the loaded .so, or "" when unknown.
func pluginLibraryDir() string {
	buffer := make([]byte, 4096)
	if C.tea_combos_library_path((*C.char)(unsafe.Pointer(&buffer[0])), C.size_t(len(buffer))) != 0 {
		return ""
	}
	path := C.GoString((*C.char)(unsafe.Pointer(&buffer[0])))
	if path == "" {
		return ""
	}
	return filepath.Dir(path)
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
	buffer := C.CBytes(raw)
	response.ptr = buffer
	response.len = C.size_t(len(raw))
}
