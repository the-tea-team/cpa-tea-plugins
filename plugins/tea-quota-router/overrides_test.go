package main

import (
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"net/http"
	"testing"
)

func TestOverridesPersistAndRejectUnknownFields(t *testing.T) {
	shutdown()
	dir := t.TempDir()
	p, e := setup([]byte("dataDir: " + dir + "\n"))
	if e != nil {
		t.Fatal(e)
	}
	defer shutdown()
	req := pluginapi.ManagementRequest{Method: http.MethodPut, Path: "/v0/management/plugins/tea-quota-router/overrides", Body: []byte(`{"credentials":{"opaque":{"accountId":"account","label":"Safe","capacityByWindow":{"weekly":2}}},"pools":[]}`)}
	r, e := p.management(req)
	if e != nil || r.StatusCode != 200 {
		t.Fatalf("%+v %v", r, e)
	}
	var v overrideDocument
	b, e := osReadOverride(dir)
	if e != nil || json.Unmarshal(b, &v) != nil || v.Credentials["opaque"].AccountID != "account" {
		t.Fatalf("persist %s %v", b, e)
	}
	req.Body = []byte(`{"credentials":{},"pools":[],"schedulerMode":"active"}`)
	r, _ = p.management(req)
	if r.StatusCode != 400 {
		t.Fatal("unknown field accepted")
	}
}
