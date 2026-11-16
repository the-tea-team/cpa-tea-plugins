package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func targetPlugin(s snapshotSource) *combosPlugin {
	return &combosPlugin{accounts: newAccountRouter(s)}
}
func targetRequest() pluginapi.ManagementRequest {
	return pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/tea-combos/targets"}
}

func TestTargetsManagementDeclaresAuthenticatedGET(t *testing.T) {
	p := targetPlugin(nil)
	r, e := p.RegisterManagement(context.Background(), pluginapi.ManagementRegistrationRequest{BasePath: "/v0/management"})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, route := range r.Routes {
		if strings.HasSuffix(route.Path, "/targets") {
			if route.Method != http.MethodGet {
				t.Fatalf("unexpected method %s", route.Method)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("targets management route missing")
	}
	for _, r := range r.Resources {
		if strings.Contains(r.Path, "targets") {
			t.Fatal("inventory exposed as resource")
		}
	}
}
func TestTargetsProjectionAllowlist(t *testing.T) {
	now := time.Now()
	snap := accountTestSnapshot(now)
	snap.Accounts[0].Label = "Account (Demo User)"
	snap.Credentials[0].Label = "Connection (Demo User)"
	snap.Credentials[0].AuthIndex = "NEVER-EXPOSE-AUTH-INDEX"
	snap.Credentials[0].Disabled = true
	snap.Credentials[1].Unavailable = true
	p := targetPlugin(staticSnapshotSource{snap: snap, maxAge: time.Minute})
	r, e := p.handleManagementRequest(context.Background(), targetRequest())
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("status %d body%s", r.StatusCode, r.Body)
	}
	var j struct {
		Accounts    []map[string]any `json:"accounts"`
		Connections []map[string]any `json:"connections"`
	}
	if e := json.Unmarshal(r.Body, &j); e != nil {
		t.Fatal(e)
	}
	keys := func(m map[string]any) map[string]bool {
		o := map[string]bool{}
		for k := range m {
			o[k] = true
		}
		return o
	}
	for _, a := range j.Accounts {
		if !reflect.DeepEqual(keys(a), map[string]bool{"id": true, "provider": true, "label": true}) {
			t.Fatalf("account leakage %v", keys(a))
		}
	}
	for _, a := range j.Connections {
		if !reflect.DeepEqual(keys(a), map[string]bool{"id": true, "accountId": true, "provider": true, "label": true, "models": true, "disabled": true, "unavailable": true}) {
			t.Fatalf("connection leakage %v", keys(a))
		}
	}
	if len(j.Accounts) != 4 || len(j.Connections) != 5 {
		t.Fatalf("counts %d/%d", len(j.Accounts), len(j.Connections))
	}
	if strings.Contains(string(r.Body), "NEVER-EXPOSE") || strings.Contains(string(r.Body), "authIndex") || strings.Contains(string(r.Body), "priority") {
		t.Fatal("unexpected fields exposed")
	}
	if j.Connections[0]["disabled"] != true || j.Connections[1]["unavailable"] != true {
		t.Fatal("status flags missing")
	}
	if r.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("inventory must not cache")
	}
}
func TestTargetsUnavailableUsesBoundedGeneric503(t *testing.T) {
	now := time.Now()
	for name, source := range map[string]snapshotSource{"none": nil, "failed": staticSnapshotSource{err: errors.New("secret-internal-path")}, "stale": staticSnapshotSource{snap: quota.Snapshot{GeneratedAt: now.Add(-time.Hour)}, maxAge: time.Minute}, "future": staticSnapshotSource{snap: quota.Snapshot{GeneratedAt: now.Add(time.Hour)}, maxAge: time.Minute}} {
		t.Run(name, func(t *testing.T) {
			r, e := targetPlugin(source).handleManagementRequest(context.Background(), targetRequest())
			if e != nil {
				t.Fatal(e)
			}
			if r.StatusCode != 503 || len(r.Body) > 200 || strings.Contains(string(r.Body), "secret") {
				t.Fatalf("%d %s", r.StatusCode, r.Body)
			}
		})
	}
}
func TestTargetsNonGETRejected(t *testing.T) {
	req := targetRequest()
	req.Method = http.MethodPost
	r, e := targetPlugin(nil).handleManagementRequest(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	if r.StatusCode != 405 {
		t.Fatalf("status %d", r.StatusCode)
	}
}
