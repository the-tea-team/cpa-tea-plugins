package quota

import (
	"testing"
	"time"
)

func acct(id string, capVal, used float64, reset time.Time) Account {
	return Account{
		ID: id, Provider: "claude", BillingType: "subscription", Plan: "x", PlanSource: "reported", ObservedAt: t0,
		Windows: []Window{{
			ID: "session", DurationSeconds: 18000, UsedFraction: ptr(used),
			ResetAt: reset, ObservedAt: t0, Capacity: ptr(capVal), CapacitySource: "reported",
		}},
	}
}

func twoAccountSnapshot() Snapshot {
	s := baseSnapshot()
	a1 := acct("acct-1", 1, 0.8, t0.Add(5*time.Hour)) // 20% left, cap 1
	a5 := acct("acct-5", 5, 0.2, t0.Add(5*time.Hour)) // 80% left, cap 5
	s.Accounts = []Account{a1, a5}
	s.Credentials = []Credential{
		{ID: "cred-1", AccountID: "acct-1", Provider: "claude", Models: []string{"m"}},
		{ID: "cred-5", AccountID: "acct-5", Provider: "claude", Models: []string{"m"}},
	}
	s.Pools = []Pool{{ID: "p", Provider: "claude", BillingType: "subscription", CredentialIDs: []string{"cred-1", "cred-5"}}}
	return s
}

func TestSummarizeWeightedPoolIs70Percent(t *testing.T) {
	sums := Summarize(twoAccountSnapshot(), t0, 10*time.Minute)
	if len(sums) != 1 {
		t.Fatalf("rows=%d", len(sums))
	}
	r := sums[0]
	if r.RemainingFraction == nil || *r.RemainingFraction < 0.699 || *r.RemainingFraction > 0.701 {
		t.Fatalf("want 0.70 weighted, got %v", r.RemainingFraction)
	}
	if r.TotalCapacity != 6 || r.MeasuredCapacity != 6 || r.UsableCapacity != 6 {
		t.Fatalf("capacities %+v", r)
	}
	if r.TotalAccounts != 2 || r.MeasuredAccounts != 2 || r.UnknownAccounts != 0 || r.UsableAccounts != 2 {
		t.Fatalf("counts %+v", r)
	}
	if r.Estimated {
		t.Fatal("reported capacity must not be Estimated")
	}
}

func TestSummarizeDedupSharedAccount(t *testing.T) {
	s := twoAccountSnapshot()
	// second credential for same account must not double capacity
	s.Credentials = append(s.Credentials, Credential{ID: "cred-1b", AccountID: "acct-1", Provider: "claude", Models: []string{"m"}})
	s.Pools[0].CredentialIDs = append(s.Pools[0].CredentialIDs, "cred-1b")
	sums := Summarize(s, t0, 10*time.Minute)
	if sums[0].TotalCapacity != 6 || sums[0].TotalAccounts != 2 {
		t.Fatalf("dedup failed: %+v", sums[0])
	}
	if sums[0].RemainingFraction == nil || *sums[0].RemainingFraction < 0.699 || *sums[0].RemainingFraction > 0.701 {
		t.Fatalf("dedup changed fraction: %v", sums[0].RemainingFraction)
	}
}

func TestSummarizePartialFreshnessPartialCoverage(t *testing.T) {
	s := twoAccountSnapshot()
	s.Accounts[1].Windows[0].ObservedAt = t0.Add(-20 * time.Minute) // stale
	sums := Summarize(s, t0, 10*time.Minute)
	r := sums[0]
	if r.MeasuredAccounts != 1 || r.UnknownAccounts != 1 {
		t.Fatalf("stale must be unknown-in-coverage: %+v", r)
	}
	if r.MeasuredCapacity != 1 || r.TotalCapacity != 6 {
		t.Fatalf("total known vs measured: %+v", r)
	}
	if r.RemainingFraction == nil || *r.RemainingFraction < 0.199 || *r.RemainingFraction > 0.201 {
		t.Fatalf("remaining only over measured: %v", r.RemainingFraction)
	}
	if r.NextResetAt.IsZero() {
		t.Fatal("next reset from fresh account expected")
	}
}

func TestSummarizeStaleOnlyPoolHasRowButNoFraction(t *testing.T) {
	s := twoAccountSnapshot()
	for i := range s.Accounts {
		s.Accounts[i].Windows[0].ObservedAt = t0.Add(-time.Hour)
	}
	sums := Summarize(s, t0, 10*time.Minute)
	if len(sums) != 1 {
		t.Fatalf("known pool must keep row, rows=%d", len(sums))
	}
	if sums[0].RemainingFraction != nil {
		t.Fatal("stale must not fabricate fraction")
	}
	if sums[0].NextResetAt.IsZero() == false && sums[0].MeasuredAccounts != 0 {
		t.Fatal("bad")
	}
}

func TestSummarizeDisabledAccountExcludedFromUsable(t *testing.T) {
	s := twoAccountSnapshot()
	s.Credentials[1].Disabled = true
	sums := Summarize(s, t0, 10*time.Minute)
	r := sums[0]
	if r.UsableAccounts != 1 || r.UsableCapacity != 1 {
		t.Fatalf("disabled not 0%% remaining but not usable: %+v", r)
	}
	if r.MeasuredAccounts != 2 {
		t.Fatalf("disabled still measured for total: %+v", r)
	}
}

func TestSummarizePerWindowIndependent(t *testing.T) {
	s := twoAccountSnapshot()
	for i := range s.Accounts {
		s.Accounts[i].Windows = append(s.Accounts[i].Windows, Window{
			ID: "weekly", DurationSeconds: 604800, UsedFraction: ptr(0.5),
			ResetAt: t0.Add(7 * 24 * time.Hour), ObservedAt: t0,
			Capacity: ptr(2), CapacitySource: "plan_estimate",
		})
	}
	sums := Summarize(s, t0, 10*time.Minute)
	if len(sums) != 2 {
		t.Fatalf("want 2 window rows, got %+v", sums)
	}
	var sess, wk *PoolSummary
	for i := range sums {
		if sums[i].WindowID == "session" {
			sess = &sums[i]
		}
		if sums[i].WindowID == "weekly" {
			wk = &sums[i]
		}
	}
	if sess == nil || wk == nil {
		t.Fatalf("missing rows %+v", sums)
	}
	if *sess.RemainingFraction < 0.699 || *sess.RemainingFraction > 0.701 {
		t.Fatalf("session %v", *sess.RemainingFraction)
	}
	if *wk.RemainingFraction < 0.499 || *wk.RemainingFraction > 0.501 {
		t.Fatalf("weekly %v", *wk.RemainingFraction)
	}
	if !wk.Estimated {
		t.Fatal("plan_estimate capacity must mark Estimated")
	}
}

func TestSummarizeModelScopedWindowSeparateRow(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows = append(s.Accounts[0].Windows, Window{
		ID: "weekly", DurationSeconds: 604800, UsedFraction: ptr(0),
		ResetAt: t0.Add(24 * time.Hour), ObservedAt: t0, Capacity: ptr(3),
		CapacitySource: "reported", Models: []string{"claude-opus*"},
	})
	sums := Summarize(s, t0, 10*time.Minute)
	found := false
	for _, r := range sums {
		if r.WindowID == "weekly|claude-opus*" {
			found = true
			if r.RemainingFraction == nil || *r.RemainingFraction != 1 {
				t.Fatalf("scoped row %v", r.RemainingFraction)
			}
		}
	}
	if !found {
		t.Fatalf("scoped row missing: %+v", sums)
	}
}

func TestSummarizePastResetInvalidatesObservation(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows[0].ResetAt = t0.Add(-time.Minute)
	sums := Summarize(s, t0, 10*time.Minute)
	if sums[0].MeasuredAccounts != 0 || sums[0].RemainingFraction != nil {
		t.Fatalf("past reset must be re-observed, not assumed replenished: %+v", sums[0])
	}
}

func TestSummarizeEmptyPoolViaBillingType(t *testing.T) {
	s := baseSnapshot()
	s.Pools = []Pool{{ID: "by-type", Provider: "claude", BillingType: "subscription"}}
	sums := Summarize(s, t0, 10*time.Minute)
	if len(sums) != 1 || sums[0].TotalAccounts != 1 {
		t.Fatalf("provider+billing membership failed: %+v", sums)
	}
}

func TestSummarizePoolWithNoWindowsKeepsRow(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows = nil
	sums := Summarize(s, t0, 10*time.Minute)
	if len(sums) != 1 || sums[0].TotalAccounts != 1 || sums[0].UnknownAccounts != 1 || sums[0].RemainingFraction != nil {
		t.Fatalf("all-missing pool row wrong: %+v", sums)
	}
}
