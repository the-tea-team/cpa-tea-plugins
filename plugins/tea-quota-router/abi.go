package main

/*
#include <stdint.h>
#include <stdlib.h>
typedef struct {void* ptr; size_t len;} cliproxy_buffer;
typedef int (*hostcallfn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*hostfreefn)(void*, size_t);
typedef struct {uint32_t abi_version; void* host_ctx; hostcallfn call; hostfreefn free_buffer;} host_api;
static host_api* quota_host;
static void set_quota_host(host_api* h) { quota_host = h; }
static int call_quota_host(const char* m, const uint8_t* b, size_t n, cliproxy_buffer* out) {
 if (!quota_host || !quota_host->call) return -1;
 return quota_host->call(quota_host->host_ctx,m,b,n,out);
}
static void free_quota_host(void* b,size_t n) { if(quota_host && quota_host->free_buffer) quota_host->free_buffer(b,n); }
typedef int (*callfn)(char*,uint8_t*,size_t,cliproxy_buffer*);
typedef void (*freefn)(void*,size_t);
typedef void (*shutdownfn)(void);
typedef struct {uint32_t abi_version; callfn call; freefn free_buffer; shutdownfn shutdown;} plugin_api;
extern int quotaCall(char*,uint8_t*,size_t,cliproxy_buffer*);
extern void quotaFree(void*,size_t);
extern void quotaShutdown(void);
*/
import "C"
import (
	"encoding/json"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"time"
	"unsafe"
)

func nativeHostCall(method string, payload any) ([]byte, error) {
	b, e := json.Marshal(payload)
	if e != nil {
		return nil, e
	}
	m := C.CString(method)
	defer C.free(unsafe.Pointer(m))
	var in unsafe.Pointer
	if len(b) > 0 {
		in = C.CBytes(b)
		defer C.free(in)
	}
	var out C.cliproxy_buffer
	code := C.call_quota_host(m, (*C.uint8_t)(in), C.size_t(len(b)), &out)
	if out.ptr != nil {
		defer C.free_quota_host(out.ptr, out.len)
	}
	if code != 0 || out.ptr == nil || out.len > 8<<20 {
		return nil, errors.New("host inventory unavailable")
	}
	data := C.GoBytes(out.ptr, C.int(out.len))
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &env) != nil || !env.OK {
		return nil, errors.New("host inventory rejected")
	}
	return env.Result, nil
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(h unsafe.Pointer, p *C.plugin_api) C.int {
	C.set_quota_host((*C.host_api)(h))
	if p == nil {
		return 1
	}
	p.abi_version = C.uint32_t(pluginabi.ABIVersion)
	p.call = C.callfn(C.quotaCall)
	p.free_buffer = C.freefn(C.quotaFree)
	p.shutdown = C.shutdownfn(C.quotaShutdown)
	return 0
}

//export quotaCall
func quotaCall(m *C.char, req *C.uint8_t, n C.size_t, out *C.cliproxy_buffer) C.int {
	if out == nil {
		return 1
	}
	out.ptr = nil
	out.len = 0
	if m == nil {
		return 1
	}
	var b []byte
	if req != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(req), C.int(n))
	}
	raw, err := abiHandleMethod(C.GoString(m), b)
	if err != nil {
		raw, _ = json.Marshal(map[string]any{"ok": false, "error": map[string]string{"code": "plugin_error", "message": "plugin request failed"}})
	}
	out.ptr = C.CBytes(raw)
	out.len = C.size_t(len(raw))
	if err != nil {
		return 1
	}
	return 0
}

//export quotaFree
func quotaFree(p unsafe.Pointer, _ C.size_t) { C.free(p) }

//export quotaShutdown
func quotaShutdown()                 { shutdown() }
func envelope(v any) ([]byte, error) { return json.Marshal(map[string]any{"ok": true, "result": v}) }
func abiHandleMethod(method string, raw []byte) (result []byte, err error) {
	defer func() {
		if recover() != nil {
			result = nil
			err = errors.New("plugin request failed")
		}
	}()
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var r struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		p, err := setup(r.ConfigYAML)
		if err != nil {
			return nil, err
		}
		p.startCollector(nativeInventoryWith(nativeHostCall))
		cfg := p.configCopy()
		schedulerOn := cfg.SchedulerMode != "off"
		return envelope(map[string]any{"schema_version": pluginabi.SchemaVersion, "metadata": pluginapi.Metadata{Name: "tea-quota-router", Version: "0.3.2", Author: "The Tea Team", GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins"}, "capabilities": map[string]bool{"scheduler": schedulerOn, "scheduler_across_priorities": schedulerOn && cfg.QuotaAcrossPriorities, "management_api": true, "usage_plugin": true}})
	case pluginabi.MethodPluginShutdown:
		shutdown()
		return envelope(map[string]any{})
	case pluginabi.MethodManagementRegister:
		var r pluginapi.ManagementRegistrationRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return managementRegister(r)
	case pluginabi.MethodManagementHandle:
		var r pluginapi.ManagementRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		p := getRuntime()
		if p == nil {
			return nil, errors.New("not registered")
		}
		v, err := p.management(r)
		if err != nil {
			return nil, err
		}
		return envelope(v)
	case pluginabi.MethodUsageHandle:
		var r pluginapi.UsageRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		p := getRuntime()
		if p == nil {
			return nil, errors.New("not registered")
		}
		p.submit(r)
		return envelope(map[string]any{})
	case pluginabi.MethodSchedulerPick:
		var r pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		p := getRuntime()
		if p == nil {
			return nil, errors.New("not registered")
		}
		p.mu.RLock()
		mode := p.cfg.SchedulerMode
		strategy := strategyFor(r, p.cfg.ProviderStrategies)
		age := p.cfg.MaxAgeSeconds
		across := p.cfg.QuotaAcrossPriorities
		opts := quota.ScoreOptions{ResetBoost: p.cfg.ResetBoost, DrainToZero: p.cfg.DrainToZero}
		p.mu.RUnlock()
		return envelope(p.scheduler.pickWithOptions(r, p.snapshotCopy(), mode, strategy, time.Now(), time.Duration(age)*time.Second, across, opts))
	}
	return nil, errors.New("unknown method")
}
