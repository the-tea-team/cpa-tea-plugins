package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// sequenceSource replays snapshots in order and keeps returning the last one afterwards, which is
// how a collector that published nothing until its first pass looks to the router.
type sequenceSource struct {
	snaps  []quota.Snapshot
	maxAge time.Duration
	calls  int
}

func (s *sequenceSource) freshSnapshot(time.Time) (quota.Snapshot, time.Duration, error) {
	last := len(s.snaps) - 1
	if s.calls > last {
		s.calls++
		return s.snaps[last], s.maxAge, nil
	}
	snap := s.snaps[s.calls]
	s.calls++
	return snap, s.maxAge, nil
}

// coldStartSnapshot is the shape tea-quota-router writes before the host's auth inventory is
// readable: valid schema, no accounts, no pools, no observation time.
func coldStartSnapshot() quota.Snapshot {
	return quota.Snapshot{SchemaVersion: quota.SchemaVersion}
}

// graceRouter counts the waits a plan performs; allow=false makes the first wait report that the
// request has no patience left.
func graceRouter(src snapshotSource, waits *int, allow bool) *accountRouter {
	r := newAccountRouter(src)
	r.now = func() time.Time { return accountTestNow }
	r.wait = func(context.Context, time.Duration) bool {
		*waits++
		return allow
	}
	return r
}

func TestPlanWaitsForTheFirstPopulatedSnapshot(t *testing.T) {
	src := &sequenceSource{
		snaps:  []quota.Snapshot{coldStartSnapshot(), accountTestSnapshot(accountTestNow)},
		maxAge: 10 * time.Minute,
	}
	waits := 0
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if cerr != nil {
		t.Fatalf("plan: %v", cerr)
	}
	if len(plan.Credentials) == 0 {
		t.Fatal("want pinned credentials once the collector has published")
	}
	if src.calls != 2 || waits != 1 {
		t.Fatalf("snapshot reads=%d waits=%d, want 2 and 1", src.calls, waits)
	}
}

func TestPlanGivesUpAfterTheGraceWindow(t *testing.T) {
	src := &sequenceSource{snaps: []quota.Snapshot{coldStartSnapshot()}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if cerr == nil || len(plan.Credentials) != 0 {
		t.Fatalf("an unpublished snapshot must fail closed: %+v %v", plan, cerr)
	}
	if !strings.Contains(cerr.Message, "claude-ab") {
		t.Fatalf("message %q, want the pool to be named", cerr.Message)
	}
	if src.calls != snapshotGraceAttempts || waits != snapshotGraceAttempts-1 {
		t.Fatalf("snapshot reads=%d waits=%d, want %d and %d", src.calls, waits, snapshotGraceAttempts, snapshotGraceAttempts-1)
	}
}

func TestPlanDoesNotWaitWhenThePoolIsAbsentFromAPopulatedSnapshot(t *testing.T) {
	src := &sequenceSource{snaps: []quota.Snapshot{accountTestSnapshot(accountTestNow)}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "no-such-pool")
	if plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member}); cerr == nil || len(plan.Credentials) != 0 {
		t.Fatalf("an unknown pool is a configuration fault and must fail at once: %+v %v", plan, cerr)
	}
	if waits != 0 || src.calls != 1 {
		t.Fatalf("snapshot reads=%d waits=%d, want 1 and 0", src.calls, waits)
	}
}

func TestPlanDoesNotWaitOnAStalePopulatedSnapshot(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	src := &sequenceSource{snaps: []quota.Snapshot{stale}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	if plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member}); cerr == nil || len(plan.Credentials) != 0 {
		t.Fatalf("a stale snapshot must fail closed: %+v %v", plan, cerr)
	}
	if waits != 0 {
		t.Fatalf("waits=%d, a wedged collector must not stall every request", waits)
	}
}

func TestPlanStopsWaitingWhenTheCallerGoesAway(t *testing.T) {
	src := &sequenceSource{snaps: []quota.Snapshot{coldStartSnapshot()}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	if _, cerr := graceRouter(src, &waits, false).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member}); cerr == nil {
		t.Fatal("want a failure when the request cannot wait any longer")
	}
	if waits != 1 || src.calls != 1 {
		t.Fatalf("snapshot reads=%d waits=%d, want 1 and 1", src.calls, waits)
	}
}
