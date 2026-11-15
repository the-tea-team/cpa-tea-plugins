package main

import (
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"testing"
	"time"
)

func TestSummaryOpportunitiesRequireUsableFreshWindows(t *testing.T) {
	now := time.Now().UTC()
	used := .2
	one := 1.0
	a := quota.Account{ID: "a", Provider: "claude", Windows: []quota.Window{{ID: "seven-day", UsedFraction: &used, Capacity: &one, ObservedAt: now, ResetAt: now.Add(time.Hour), DurationSeconds: 604800}}}
	s := quota.Snapshot{SchemaVersion: 1, Accounts: []quota.Account{a}, Credentials: []quota.Credential{{ID: "c", AccountID: "a", Provider: "claude"}}}
	if len(opportunities(s, now, 10*time.Minute)) != 1 {
		t.Fatal("missing opportunity")
	}
	s.Credentials[0].Disabled = true
	if len(opportunities(s, now, 10*time.Minute)) != 0 {
		t.Fatal("disabled opportunity")
	}
	s.Credentials[0].Disabled = false
	s.Accounts[0].Windows[0].ObservedAt = now.Add(-time.Hour)
	if len(opportunities(s, now, 10*time.Minute)) != 0 {
		t.Fatal("stale opportunity")
	}
}
