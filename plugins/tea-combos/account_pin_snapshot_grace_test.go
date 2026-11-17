package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// The merged startup grace must also apply to direct pins, without replacing their target
// boundary with whatever credentials become available first.
func TestPinStartupGracePreservesTarget(t *testing.T) {
	for _, kind := range []string{"account", "connection"} {
		t.Run(kind, func(t *testing.T) {
			src := &sequenceSource{snaps: []quota.Snapshot{coldStartSnapshot(), accountTestSnapshot(accountTestNow)}, maxAge: 10 * time.Minute}
			waits := 0
			combo, member := pinMember("claude-sonnet", func(m *Member) {
				if kind == "account" {
					m.AccountID = "acctB"
				} else {
					m.ConnectionID = "b1"
				}
			})
			plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
			if cerr != nil || len(plan.Credentials) != 1 || plan.Credentials[0] != "b1" {
				t.Fatalf("startup pin escaped or failed: %+v %v", plan, cerr)
			}
			if waits != 1 || src.calls != 2 {
				t.Fatalf("snapshot reads=%d waits=%d", src.calls, waits)
			}
		})
	}
}

func TestPinStartupGraceSamplesTimeAfterWait(t *testing.T) {
	published := accountTestSnapshot(accountTestNow.Add(time.Second))
	src := &sequenceSource{snaps: []quota.Snapshot{coldStartSnapshot(), published}, maxAge: 10 * time.Minute}
	r := newAccountRouter(src)
	now := accountTestNow
	r.now = func() time.Time { return now }
	r.wait = func(context.Context, time.Duration) bool { now = now.Add(time.Second); return true }
	combo, member := pinMember("claude-sonnet", func(m *Member) { m.ConnectionID = "b1" })
	plan, cerr := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if cerr != nil || len(plan.Credentials) != 1 || plan.Credentials[0] != "b1" {
		t.Fatalf("newly published snapshot rejected: %+v %v", plan, cerr)
	}
}

func TestPinStartupGraceRemainsBoundedAndFailClosed(t *testing.T) {
	src := &sequenceSource{snaps: []quota.Snapshot{coldStartSnapshot()}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := pinMember("claude-sonnet", func(m *Member) { m.AccountID = "acctB" })
	plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if cerr == nil || len(plan.Credentials) != 0 || !strings.Contains(cerr.Message, "member claude-sonnet") {
		t.Fatalf("unpublished target must fail closed and identify member: %+v %v", plan, cerr)
	}
	if waits != snapshotGraceAttempts-1 || src.calls != snapshotGraceAttempts {
		t.Fatalf("unbounded grace: snapshot reads=%d waits=%d", src.calls, waits)
	}
}

func TestPinStalePopulatedSnapshotNeverWaitsOrWidens(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	src := &sequenceSource{snaps: []quota.Snapshot{stale, accountTestSnapshot(accountTestNow)}, maxAge: 10 * time.Minute}
	waits := 0
	combo, member := pinMember("claude-sonnet", func(m *Member) { m.ConnectionID = "b1" })
	plan, cerr := graceRouter(src, &waits, true).plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if cerr == nil || len(plan.Credentials) != 0 || !strings.Contains(cerr.Message, "stale") {
		t.Fatalf("stale target must fail closed: %+v %v", plan, cerr)
	}
	if waits != 0 || src.calls != 1 {
		t.Fatalf("stale snapshot paid startup wait: reads=%d waits=%d", src.calls, waits)
	}
}
