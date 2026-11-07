package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// reverseSelector is a third Selector, unrelated to round-robin and failover. It proves the
// interface is a real seam: the executor loop depends only on the order a Selector returns.
type reverseSelector struct{}

func (reverseSelector) Order(combo Combo, _ string) []int {
	order := make([]int, 0, len(combo.Members))
	for index := len(combo.Members) - 1; index >= 0; index-- {
		order = append(order, index)
	}
	return order
}

var (
	_ Selector = reverseSelector{}
	_ Selector = failoverSelector{}
	_ Selector = (*roundRobinSelector)(nil)
)

func TestThirdSelectorDrivesExecutorOrder(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyFailover, "m0", "m1", "m2")

	tests := []struct {
		name      string
		preferred int
		failing   map[string]bool
		wantOrder []string
		wantUsed  int
	}{
		{name: "dummy order is followed", preferred: -1, wantOrder: []string{"m2"}, wantUsed: 2},
		{name: "preferred member goes first", preferred: 0, wantOrder: []string{"m0"}, wantUsed: 0},
		{name: "falls through in dummy order on retryable failure", preferred: -1, failing: map[string]bool{"m2": true}, wantOrder: []string{"m2", "m1"}, wantUsed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := newTestPlugin(t)
			var attempted []string
			stubHostExecute(t, func(_ context.Context, req pluginapi.HostModelExecutionRequest) (*pluginapi.HostModelExecutionResponse, error) {
				attempted = append(attempted, req.Model)
				if tt.failing[req.Model] {
					return nil, &hostError{Status: http.StatusServiceUnavailable, Retryable: true, Message: "down"}
				}
				return &pluginapi.HostModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
			})

			order := withPreferred(reverseSelector{}.Order(combo, ""), tt.preferred)
			_, used, err := plugin.runAttempts(context.Background(), pluginapi.ExecutorRequest{Model: "fleet", Payload: []byte(`{}`)}, combo, order)
			if err != nil {
				t.Fatalf("runAttempts: %v", err)
			}
			if !equalStrings(attempted, tt.wantOrder) {
				t.Fatalf("attempted = %v, want %v", attempted, tt.wantOrder)
			}
			if used != tt.wantUsed {
				t.Fatalf("used = %d, want %d", used, tt.wantUsed)
			}
		})
	}
}

func TestSelectorContractEveryIndexAtMostOnce(t *testing.T) {
	combo := mustCombo(t, "fleet", StrategyRoundRobin, "a", "b", "c", "d")
	selectors := map[string]Selector{
		"round-robin": newRoundRobinSelector(),
		"failover":    failoverSelector{},
		"reverse":     reverseSelector{},
	}
	for name, selector := range selectors {
		t.Run(name, func(t *testing.T) {
			for range 2 * len(combo.Members) {
				seen := map[int]bool{}
				for _, index := range selector.Order(combo, "session") {
					if index < 0 || index >= len(combo.Members) || seen[index] {
						t.Fatalf("index %d out of range or repeated", index)
					}
					seen[index] = true
				}
				if len(seen) != len(combo.Members) {
					t.Fatalf("covered %d of %d members", len(seen), len(combo.Members))
				}
			}
		})
	}
}
