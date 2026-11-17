package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// settledSnapshot is a pass the collector can publish: one subscription pool whose member knows
// which models it serves.
func settledSnapshot(credentialIDs ...string) quota.Snapshot {
	snap := quota.Snapshot{
		SchemaVersion: quota.SchemaVersion,
		Accounts:      []quota.Account{{ID: "acct", Provider: "claude", BillingType: "subscription"}},
		Pools:         []quota.Pool{{ID: "auto-claude-subscription", Provider: "claude", BillingType: "subscription"}},
	}
	for _, id := range credentialIDs {
		snap.Credentials = append(snap.Credentials, quota.Credential{
			ID: id, AccountID: "acct", Provider: "claude", Models: []string{"claude-*"},
		})
	}
	return snap
}

// unreadableSnapshot is what the host returns in the second or two after registration: credentials
// exist, the pools are built, but no credential knows its models yet.
func unreadableSnapshot(credentialIDs ...string) quota.Snapshot {
	snap := settledSnapshot(credentialIDs...)
	for i := range snap.Credentials {
		snap.Credentials[i].Models = nil
	}
	return snap
}

// countWaiting returns a wait seam that records calls and always reports that the delay elapsed.
func countWaiting(waits *int) func(context.Context, time.Duration) bool {
	return func(context.Context, time.Duration) bool {
		*waits++
		return true
	}
}

func TestFirstPassRetriesUntilPoolsCanAnswerModels(t *testing.T) {
	passes, waits := 0, 0
	snap, health, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			if passes == 1 {
				return unreadableSnapshot("claude-1"), collectorHealth{Status: "ok"}, nil
			}
			return settledSnapshot("claude-1"), collectorHealth{Status: "ok"}, nil
		}, inventorySettled, countWaiting(&waits))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !inventorySettled(snap) {
		t.Fatal("want the retried, model-complete snapshot")
	}
	if health.Status != "ok" {
		t.Fatalf("health %q, want the retried pass report", health.Status)
	}
	if passes != 2 || waits != 1 {
		t.Fatalf("passes=%d waits=%d, want 2 and 1", passes, waits)
	}
}

func TestFirstPassRetriesAnEmptyInventory(t *testing.T) {
	passes, waits := 0, 0
	snap, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			if passes == 1 {
				return quota.Snapshot{SchemaVersion: quota.SchemaVersion}, collectorHealth{Status: "ok"}, nil
			}
			return settledSnapshot("claude-1"), collectorHealth{Status: "ok"}, nil
		}, inventorySettled, countWaiting(&waits))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(snap.Credentials) != 1 {
		t.Fatalf("credentials=%d, want the retried pass", len(snap.Credentials))
	}
	if passes != 2 || waits != 1 {
		t.Fatalf("passes=%d waits=%d, want 2 and 1", passes, waits)
	}
}

func TestFirstPassDoesNotRetryAFailedPass(t *testing.T) {
	passes, waits := 0, 0
	wantErr := errors.New("host inventory unavailable")
	if _, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			return quota.Snapshot{}, collectorHealth{Status: "error"}, wantErr
		}, inventorySettled, countWaiting(&waits)); !errors.Is(err, wantErr) {
		t.Fatalf("err %v, want %v", err, wantErr)
	}
	if passes != 1 || waits != 0 {
		t.Fatalf("passes=%d waits=%d, want 1 and 0", passes, waits)
	}
}

func TestFirstPassDoesNotRetryASettledPass(t *testing.T) {
	passes, waits := 0, 0
	if _, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			return settledSnapshot("claude-1", "claude-2"), collectorHealth{Status: "ok"}, nil
		}, inventorySettled, countWaiting(&waits)); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if passes != 1 || waits != 0 {
		t.Fatalf("passes=%d waits=%d, want 1 and 0", passes, waits)
	}
}

func TestFirstPassStopsAtTheAttemptBudget(t *testing.T) {
	passes, waits := 0, 0
	snap, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			return unreadableSnapshot("claude-1"), collectorHealth{Status: "ok"}, nil
		}, inventorySettled, countWaiting(&waits))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if inventorySettled(snap) {
		t.Fatal("want the unreadable inventory to be published honestly after the budget")
	}
	if passes != startupInventoryAttempts || waits != startupInventoryAttempts-1 {
		t.Fatalf("passes=%d waits=%d, want %d and %d", passes, waits, startupInventoryAttempts, startupInventoryAttempts-1)
	}
}

func TestFirstPassStopsWhenTheContextEndsFirst(t *testing.T) {
	passes, waits := 0, 0
	if _, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			return quota.Snapshot{SchemaVersion: quota.SchemaVersion}, collectorHealth{Status: "ok"}, nil
		}, inventorySettled, func(context.Context, time.Duration) bool {
			waits++
			return false
		}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if passes != 1 || waits != 1 {
		t.Fatalf("passes=%d waits=%d, want 1 and 1", passes, waits)
	}
}

func TestInventorySettledRejectsPoolsWithoutModelSupport(t *testing.T) {
	if inventorySettled(quota.Snapshot{SchemaVersion: quota.SchemaVersion}) {
		t.Fatal("an empty snapshot is not settled")
	}
	snap := settledSnapshot("claude-1")
	snap.Pools = append(snap.Pools, quota.Pool{ID: "auto-codex-subscription", Provider: "codex", BillingType: "subscription"})
	if inventorySettled(snap) {
		t.Fatal("a pool with no member credential is not settled")
	}
}

func TestWaitContextReturnsFalseOnceTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitContext(ctx, time.Millisecond) {
		t.Fatal("want false after cancellation")
	}
}
