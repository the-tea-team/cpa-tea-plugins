package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"testing"
)

func TestStrategyForMixedRouteWithOneProvider(t *testing.T) {
	r := pluginapi.SchedulerPickRequest{Providers: []string{"mock"}, Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "mock"}}}
	if strategyFor(r, map[string]string{"mock": "quota-aware-round-robin"}) != "quota-aware-round-robin" {
		t.Fatal("single provider lost on mixed route")
	}
	r.Providers = append(r.Providers, "paid")
	r.Candidates = append(r.Candidates, pluginapi.SchedulerAuthCandidate{ID: "b", Provider: "paid"})
	if strategyFor(r, map[string]string{"mock": "quota-aware-round-robin"}) != "inherit" {
		t.Fatal("mixed unauthorized policy expansion")
	}
}
