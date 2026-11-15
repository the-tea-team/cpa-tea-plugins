package main

import (
	"embed"
	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"net/http"
	"strings"
	"time"
)

//go:embed ui/index.html
var ui embed.FS

func managementRegister(req pluginapi.ManagementRegistrationRequest) ([]byte, error) {
	base := strings.TrimRight(req.BasePath, "/")
	if base == "" {
		base = "/v0/management"
	}
	routes := []map[string]string{}
	for _, v := range []struct{ method, path string }{{"GET", "summary"}, {"GET", "snapshot"}, {"POST", "refresh"}, {"GET", "decisions"}, {"GET", "overrides"}, {"PUT", "overrides"}} {
		routes = append(routes, map[string]string{"Method": v.method, "Path": base + "/plugins/tea-quota-router/" + v.path})
	}
	return envelope(map[string]any{"routes": routes, "resources": []map[string]string{{"Path": "/capacity", "Menu": "Capacity & resets"}}})
}
func response(status int, body any) pluginapi.ManagementResponse {
	b, _ := json.Marshal(body)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: b}
}
func (p *runtimeState) management(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if strings.HasSuffix(req.Path, "/capacity") {
		b, err := ui.ReadFile("ui/index.html")
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, Body: b}, err
	}
	path := strings.TrimPrefix(req.Path, "/v0/management/plugins/tea-quota-router/")
	switch path {
	case "snapshot":
		if req.Method == "GET" {
			return response(200, p.snapshotCopy()), nil
		}
	case "decisions":
		if req.Method == "GET" {
			return response(200, p.scheduler.history()), nil
		}
	case "summary":
		if req.Method == "GET" {
			window := req.Query.Get("window")
			if window == "" {
				window = "24h"
			}
			durations := map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}
			dur, ok := durations[window]
			if !ok {
				return response(400, map[string]string{"error": "invalid window"}), nil
			}
			rows, err := p.store.query(time.Now().Add(-dur))
			if err != nil {
				return response(500, map[string]string{"error": "usage unavailable"}), nil
			}
			s := p.snapshotCopy()
			p.mu.RLock()
			c := p.cfg
			p.mu.RUnlock()
			return response(200, p.summaryData(s, rows, c, window)), nil
		}
	case "overrides":
		if req.Method == "GET" {
			return response(200, p.overrideDoc()), nil
		}
		if req.Method == "PUT" {
			status, body := overridesPutResponse(p.saveOverridesResult(req.Body))
			return response(status, body), nil
		}
	case "refresh":
		if req.Method == "POST" {
			if p.configCopy().FixtureSnapshotPath != "" {
				if err := p.loadFixture(); err != nil {
					return response(503, map[string]string{"error": "fixture unavailable"}), nil
				}
			} else {
				p.triggerRefresh()
			}
			return response(202, map[string]string{"status": "refreshed"}), nil
		}
	}
	return response(404, map[string]string{"error": "not found"}), nil
}
