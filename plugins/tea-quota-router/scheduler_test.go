package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"testing"
	"time"
)

func TestSchedulerShadowAndPriority(t *testing.T) {
	now := time.Now()
	used := 0.2
	s := quota.Snapshot{SchemaVersion: 1, Accounts: []quota.Account{{ID: "a", Provider: "mock", Windows: []quota.Window{{ID: "session", UsedFraction: &used, ObservedAt: now}}}, {ID: "b", Provider: "mock"}}, Credentials: []quota.Credential{{ID: "high", AccountID: "a", Provider: "mock"}, {ID: "low", AccountID: "b", Provider: "mock"}}}
	r := newScheduler()
	req := pluginapi.SchedulerPickRequest{Provider: "mock", Model: "m", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "low", Priority: 1}, {ID: "high", Priority: 2}}}
	got := r.pick(req, s, "shadow", "quota-aware-round-robin", now, time.Hour)
	if got.Handled || len(r.history()) != 1 || r.history()[0].SelectedID != "high" {
		t.Fatalf("shadow: %+v %+v", got, r.history())
	}
	got = r.pick(req, s, "active", "quota-aware-round-robin", now, time.Hour)
	if !got.Handled || got.AuthID != "high" {
		t.Fatalf("active priority: %+v", got)
	}
}

func TestSchedulerExhaustedAndInheritFallBack(t *testing.T) {
	now := time.Now()
	used := 1.0
	// Known exhaustion = fresh fully-used window with a KNOWN FUTURE reset.
	known := quota.Snapshot{SchemaVersion: 1, Accounts: []quota.Account{{ID: "a", Provider: "mock", Windows: []quota.Window{{ID: "w", UsedFraction: &used, ObservedAt: now, ResetAt: now.Add(time.Hour)}}}}, Credentials: []quota.Credential{{ID: "a", AccountID: "a", Provider: "mock"}}}
	// Missing reset is NOT known exhaustion: never rejected, delegates.
	unknown := quota.Snapshot{SchemaVersion: 1, Accounts: []quota.Account{{ID: "a", Provider: "mock", Windows: []quota.Window{{ID: "w", UsedFraction: &used, ObservedAt: now}}}}, Credentials: []quota.Credential{{ID: "a", AccountID: "a", Provider: "mock"}}}
	r := newScheduler()
	req := pluginapi.SchedulerPickRequest{Provider: "mock", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a"}}}
	if got := r.pick(req, known, "active", "quota-aware-round-robin", now, time.Hour); !got.Handled || !got.Reject || got.RejectCode != "auth_unavailable" {
		t.Fatalf("known exhausted must reject, not delegate: %+v", got)
	}
	if got := r.pick(req, known, "shadow", "quota-aware-round-robin", now, time.Hour); got.Handled || got.Reject {
		t.Fatalf("shadow must not change production: %+v", got)
	}
	if got := r.pick(req, unknown, "active", "quota-aware-round-robin", now, time.Hour); got.Handled || got.Reject {
		t.Fatalf("missing reset is not known exhausted; must delegate: %+v", got)
	}
	if got := r.pick(req, known, "active", "inherit", now, time.Hour); got.Handled {
		t.Fatalf("inherit: %+v", got)
	}
	if got := r.pick(req, known, "off", "quota-aware-round-robin", now, time.Hour); got.Handled {
		t.Fatalf("off: %+v", got)
	}
	if got := r.pick(pluginapi.SchedulerPickRequest{}, known, "active", "quota-aware-round-robin", now, time.Hour); got.Handled {
		t.Fatalf("empty: %+v", got)
	}
}
