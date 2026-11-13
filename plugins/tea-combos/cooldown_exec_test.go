package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func indexes(order []int) string {
	parts := make([]string, 0, len(order))
	for _, index := range order {
		parts = append(parts, strconv.Itoa(index))
	}
	return strings.Join(parts, ",")
}

func TestExecuteQuotaFailureFallsThroughAndCools(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "tiers", StrategyFailover, "quota-model", "healthy-model")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "quota-model" {
			return nil, &hostError{Status: http.StatusForbidden, Message: `{"error":{"type":"permission_error","code":"insufficient_quota"}}`}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"served":"healthy-model"}`)}, nil
	})

	response, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "tiers", Payload: []byte(`{"model":"tiers"}`)})
	if err != nil {
		t.Fatalf("a member's quota 403 must not fail the request: %v", err)
	}
	if string(response.Payload) != `{"served":"healthy-model"}` {
		t.Fatalf("payload = %s", response.Payload)
	}
	if strings.Join(attempts, ",") != "quota-model,healthy-model" {
		t.Fatalf("attempts = %v, want fall-through past the quota member", attempts)
	}
	if !plugin.cooldown.Active("tiers", Member{Model: "quota-model"}) {
		t.Fatal("the quota member must be cooling after its failure")
	}

	attempts = nil
	if _, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "tiers", Payload: []byte(`{"model":"tiers"}`)}); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if strings.Join(attempts, ",") != "healthy-model" {
		t.Fatalf("second round attempts = %v, want the cooled member skipped by selection", attempts)
	}
}

func TestExecuteFailsOpenWhenEveryMemberCools(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "slow-a", "slow-b")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		return nil, &hostError{Status: http.StatusServiceUnavailable, Retryable: true, Message: "still down"}
	})

	request := pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)}
	if _, err := plugin.Execute(context.Background(), request); err == nil {
		t.Fatal("first Execute must report exhaustion")
	}
	if _, err := plugin.Execute(context.Background(), request); err == nil {
		t.Fatal("second Execute must report exhaustion")
	}
	if strings.Join(attempts, ",") != "slow-a,slow-b,slow-a,slow-b" {
		t.Fatalf("attempts = %v, want every member attempted again while all cool (fail open)", attempts)
	}
}

func TestAffinitySkipsCooledBinding(t *testing.T) {
	plugin := newTestPlugin(t)
	combo := mustCombo(t, "sticky", StrategyRoundRobin, "m-a", "m-b")
	combo.SessionAffinity = boolPtr(true)
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := pluginapi.ExecutorRequest{Model: "sticky", Payload: []byte(`{"model":"sticky","prompt_cache_key":"sess-1"}`)}
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if strings.Join(attempts, ",") != "m-a" {
		t.Fatalf("attempts = %v, want round-robin start m-a", attempts)
	}

	plugin.cooldown.Cool("sticky", Member{Model: "m-a"}, time.Minute)
	attempts = nil
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("Execute with cooled binding: %v", err)
	}
	if strings.Join(attempts, ",") != "m-b" {
		t.Fatalf("attempts = %v, want the cooled binding skipped in favour of m-b", attempts)
	}

	attempts = nil
	if _, err := plugin.Execute(context.Background(), request); err != nil {
		t.Fatalf("third Execute: %v", err)
	}
	if strings.Join(attempts, ",") != "m-b" {
		t.Fatalf("attempts = %v, want the binding rebound to m-b", attempts)
	}
}

func TestComboCooldownOverrideDisablesCooling(t *testing.T) {
	plugin := newTestPlugin(t)
	combo := mustCombo(t, "hot", StrategyFailover, "flaky", "steady")
	zero := 0
	combo.CooldownSeconds = &zero
	if err := plugin.store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var attempts []string
	stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		attempts = append(attempts, req.Model)
		if req.Model == "flaky" {
			return nil, &hostError{Status: http.StatusTooManyRequests, Retryable: true, Message: "slow down"}
		}
		return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
	})

	request := pluginapi.ExecutorRequest{Model: "hot", Payload: []byte(`{"model":"hot"}`)}
	for range 2 {
		if _, err := plugin.Execute(context.Background(), request); err != nil {
			t.Fatalf("Execute: %v", err)
		}
	}
	if strings.Join(attempts, ",") != "flaky,steady,flaky,steady" {
		t.Fatalf("attempts = %v, want flaky retried every request when cooldownSeconds is 0", attempts)
	}
}

func TestSelectOrderFiltersCooledMembers(t *testing.T) {
	plugin := newTestPlugin(t)
	combo := mustCombo(t, "fleet", StrategyFailover, "m0", "m1", "m2")
	plugin.cooldown.Cool("fleet", Member{Model: "m1"}, time.Minute)

	if got := indexes(plugin.selectOrder(combo, failoverSelector{}, "", -1)); got != "0,2" {
		t.Fatalf("order with m1 cooling = %s, want 0,2", got)
	}
	if got := indexes(plugin.selectOrder(combo, failoverSelector{}, "", 1)); got != "0,2" {
		t.Fatalf("order preferring the cooled member = %s, want 0,2 (preference dropped)", got)
	}
	if got := indexes(plugin.selectOrder(combo, failoverSelector{}, "", 2)); got != "2,0" {
		t.Fatalf("order preferring a healthy member = %s, want 2,0", got)
	}
}

func TestExecuteExhaustedMessageNamesCoolingMembers(t *testing.T) {
	plugin := newTestPlugin(t)
	if err := plugin.store.Upsert(mustCombo(t, "fleet", StrategyFailover, "only")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	stubHostExecute(t, func(_ context.Context, _ pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
		return nil, &hostError{Status: http.StatusTooManyRequests, Retryable: true, Message: "slow down"}
	})

	_, err := plugin.Execute(context.Background(), pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{"model":"fleet"}`)})
	var comboErr *comboError
	if !errors.As(err, &comboErr) {
		t.Fatalf("error = %v, want comboError", err)
	}
	if !strings.Contains(comboErr.Message, fmt.Sprintf("cooling=[only]")) {
		t.Fatalf("exhausted message lacks the cooling snapshot: %s", comboErr.Message)
	}
}

func TestParseSeedConfigCooldownSeconds(t *testing.T) {
	cfg, err := parseSeedConfig([]byte("statePath: /tmp/state.json\ncooldownSeconds: 45\n"))
	if err != nil {
		t.Fatalf("parseSeedConfig: %v", err)
	}
	if cfg.CooldownSeconds == nil || *cfg.CooldownSeconds != 45 {
		t.Fatalf("cooldownSeconds = %v, want 45", cfg.CooldownSeconds)
	}
	if _, err := parseSeedConfig([]byte("cooldownSeconds: -5\n")); err == nil {
		t.Fatal("negative cooldownSeconds must be rejected")
	}
	cfg, err = parseSeedConfig([]byte("statePath: /tmp/state.json\n"))
	if err != nil {
		t.Fatalf("parseSeedConfig without cooldownSeconds: %v", err)
	}
	if cfg.CooldownSeconds != nil {
		t.Fatalf("absent cooldownSeconds must stay nil (plugin default), got %v", *cfg.CooldownSeconds)
	}
}

func TestComboCooldownSecondsValidation(t *testing.T) {
	negative := -1
	combo := Combo{Name: "c", Members: []Member{{Model: "m"}}, CooldownSeconds: &negative}
	if err := combo.Normalize().Validate(); err == nil {
		t.Fatal("negative per-combo cooldownSeconds must be rejected")
	}
	zero := 0
	combo = Combo{Name: "c", Members: []Member{{Model: "m"}}, CooldownSeconds: &zero}
	if err := combo.Normalize().Validate(); err != nil {
		t.Fatalf("zero cooldownSeconds must be valid: %v", err)
	}
}
