package main

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"testing"
)

func TestNativeRegistrationAndRoutes(t *testing.T) {
	cfg := []byte("dataDir: " + t.TempDir() + "\n")
	in, _ := json.Marshal(map[string]any{"config_yaml": cfg})
	raw, err := abiHandleMethod(pluginabi.MethodPluginRegister, in)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Capabilities map[string]bool `json:"capabilities"`
		} `json:"result"`
	}
	if err = json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("%s %v", raw, err)
	}
	for _, k := range []string{"scheduler", "management_api", "usage_plugin"} {
		if !env.Result.Capabilities[k] {
			t.Errorf("missing %s", k)
		}
	}
	raw, err = abiHandleMethod(pluginabi.MethodManagementRegister, []byte(`{"BasePath":"/v0/management"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatal("invalid routes")
	}
}
