package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func passSnapshot(credentialIDs ...string) (quota.Snapshot, collectorHealth, error) {
	snap := quota.Snapshot{SchemaVersion: quota.SchemaVersion}
	for _, id := range credentialIDs {
		snap.Credentials = append(snap.Credentials, quota.Credential{ID: id, Provider: "claude"})
	}
	return snap, collectorHealth{Status: "ok"}, nil
}

// countWaiting returns a wait seam that records calls and always reports that the delay elapsed.
func countWaiting(waits *int) func(context.Context, time.Duration) bool {
	return func(context.Context, time.Duration) bool {
		*waits++
		return true
	}
}

func TestFirstPassRetriesAnEmptyInventory(t *testing.T) {
	passes, waits := 0, 0
	snap, health, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			if passes == 1 {
				return passSnapshot()
			}
			return passSnapshot("claude-1")
		}, countWaiting(&waits))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(snap.Credentials) != 1 {
		t.Fatalf("credentials=%d, want the retried pass", len(snap.Credentials))
	}
	if health.Status != "ok" {
		t.Fatalf("health %q, want the retried pass report", health.Status)
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
		}, countWaiting(&waits)); !errors.Is(err, wantErr) {
		t.Fatalf("err %v, want %v", err, wantErr)
	}
	if passes != 1 || waits != 0 {
		t.Fatalf("passes=%d waits=%d, want 1 and 0", passes, waits)
	}
}

func TestFirstPassDoesNotRetryAPopulatedPass(t *testing.T) {
	passes, waits := 0, 0
	if _, _, err := firstPassWithRetry(context.Background(), startupInventoryAttempts, startupInventoryDelay,
		func(context.Context) (quota.Snapshot, collectorHealth, error) {
			passes++
			return passSnapshot("claude-1", "codex-1")
		}, countWaiting(&waits)); err != nil {
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
			return passSnapshot()
		}, countWaiting(&waits))
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(snap.Credentials) != 0 {
		t.Fatalf("credentials=%d, want the empty inventory to be reported honestly", len(snap.Credentials))
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
			return passSnapshot()
		}, func(context.Context, time.Duration) bool {
			waits++
			return false
		}); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if passes != 1 || waits != 1 {
		t.Fatalf("passes=%d waits=%d, want 1 and 1", passes, waits)
	}
}

func TestWaitContextReturnsFalseOnceTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitContext(ctx, time.Millisecond) {
		t.Fatal("want false after cancellation")
	}
}
