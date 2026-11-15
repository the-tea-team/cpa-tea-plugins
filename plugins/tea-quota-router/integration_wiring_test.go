package main

import (
	"context"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"testing"
	"time"
)

func TestNativeInventoryCallsBothSafeMethods(t *testing.T) {
	calls := []string{}
	fn := func(method string, payload any) ([]byte, error) {
		calls = append(calls, method)
		if method == "host.auth.list" {
			return []byte(`{"files":[]}`), nil
		}
		return []byte(`{"credentials":[{"id":"claude:apikey:demo","auth_index":"opaque","provider":"claude","models":["m"]}]}`), nil
	}
	inv := nativeInventoryWith(fn)
	files, models, err := inv(context.Background())
	if err != nil || len(files) != 0 || len(models) != 1 || len(calls) != 2 {
		t.Fatalf("inventory: %v %v %v", files, models, err)
	}
}
func TestCollectorLifecyclePublishesNativeInventory(t *testing.T) {
	shutdown()
	defer shutdown()
	p, e := setup([]byte("dataDir: " + t.TempDir() + "\n"))
	if e != nil {
		t.Fatal(e)
	}
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return nil, []pluginapi.HostAuthModels{{ID: "openai-compatibility:demo:hash", AuthIndex: "idx", Provider: "openai-compatible-demo", Models: []string{"m"}}}, nil
	}
	p.startCollector(inv)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.snapshotCopy().Credentials) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("collector never published inventory")
}
func TestOverridesReadApplyAndRestart(t *testing.T) {
	shutdown()
	defer shutdown()
	dir := t.TempDir()
	cfg := []byte("dataDir: " + dir + "\n")
	p, e := setup(cfg)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	used := .2
	s := quota.Snapshot{SchemaVersion: 1, GeneratedAt: now, Accounts: []quota.Account{{ID: "a", Provider: "claude", Windows: []quota.Window{{ID: "five-hour", DurationSeconds: 18000, UsedFraction: &used, ObservedAt: now, ResetAt: now.Add(time.Hour)}}}}, Credentials: []quota.Credential{{ID: "c", AccountID: "a", Provider: "claude", Models: []string{"m"}}}, Pools: []quota.Pool{}}
	if e = p.applySnapshot(s); e != nil {
		t.Fatal(e)
	}
	raw := []byte(`{"credentials":{"c":{"label":"Demo User","capacityByWindow":{"five-hour":5}}},"pools":[]}`)
	if e = p.saveOverrides(raw); e != nil {
		t.Fatal(e)
	}
	ss := p.snapshotCopy()
	if len(ss.Accounts) != 1 || ss.Accounts[0].Windows[0].Capacity == nil || *ss.Accounts[0].Windows[0].Capacity != 5 {
		t.Fatalf("not applied: %+v", ss)
	}
	r, e := p.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/plugins/tea-quota-router/overrides"})
	if e != nil || r.StatusCode != 200 {
		t.Fatalf("get %d %v", r.StatusCode, e)
	}
	var d overrideDocument
	if json.Unmarshal(r.Body, &d) != nil || d.Credentials["c"].Label != "Demo User" {
		t.Fatal("readback wrong")
	}
	shutdown()
	p, e = setup(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if p.cfg.AccountOverrides["c"].Label != "Demo User" {
		t.Fatal("override lost on restart")
	}
}
