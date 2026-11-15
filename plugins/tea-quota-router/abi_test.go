package main

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"testing"
)

func TestOffModeDoesNotRegisterScheduler(t *testing.T) {
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		t.Run(method, func(t *testing.T) {
			defer shutdown()
			cfg := []byte("dataDir: " + t.TempDir() + "\nschedulerMode: off\n")
			in, _ := json.Marshal(map[string]any{"config_yaml": cfg})
			raw, err := abiHandleMethod(method, in)
			if err != nil {
				t.Fatal(err)
			}
			var env struct {
				Result struct {
					Capabilities map[string]bool `json:"capabilities"`
				} `json:"result"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatal(err)
			}
			if env.Result.Capabilities["scheduler"] {
				t.Fatal("off mode must not occupy the host scheduler slot")
			}
			for _, k := range []string{"management_api", "usage_plugin"} {
				if !env.Result.Capabilities[k] {
					t.Errorf("observer mode missing %s", k)
				}
			}
		})
	}
}

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
