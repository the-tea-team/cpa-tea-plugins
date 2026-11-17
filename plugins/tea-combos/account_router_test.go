package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

var accountTestNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

type staticSnapshotSource struct {
	snap   quota.Snapshot
	maxAge time.Duration
	err    error
}

func (s staticSnapshotSource) freshSnapshot(time.Time) (quota.Snapshot, time.Duration, error) {
	if s.err != nil {
		return quota.Snapshot{}, 0, s.err
	}
	return s.snap, s.maxAge, nil
}

func fptr(v float64) *float64 { return &v }

func testWindow(now time.Time, capacity, used float64) quota.Window {
	return quota.Window{ID: "week", DurationSeconds: 7 * 24 * 3600, Capacity: fptr(capacity), UsedFraction: fptr(used), ResetAt: now.Add(72 * time.Hour), ObservedAt: now}
}

// accountTestSnapshot models one provider with three quota accounts: acctA has two credentials
// (a1, a2) that share one allowance, acctB has b1, acctC has c1 with a very high priority.
func accountTestSnapshot(now time.Time) quota.Snapshot {
	return quota.Snapshot{
		SchemaVersion: quota.SchemaVersion,
		GeneratedAt:   now,
		Accounts: []quota.Account{
			{ID: "acctA", Provider: "claude", BillingType: "subscription", Windows: []quota.Window{testWindow(now, 1, 0.5)}},
			{ID: "acctB", Provider: "claude", BillingType: "subscription", Windows: []quota.Window{testWindow(now, 1, 0.5)}},
			{ID: "acctC", Provider: "claude", BillingType: "subscription", Windows: []quota.Window{testWindow(now, 1, 0.5)}},
			{ID: "acctX", Provider: "codex", BillingType: "subscription"},
		},
		Credentials: []quota.Credential{
			{ID: "a1", AccountID: "acctA", Provider: "claude", Models: []string{"claude-*"}},
			{ID: "a2", AccountID: "acctA", Provider: "claude", Models: []string{"claude-*"}},
			{ID: "b1", AccountID: "acctB", Provider: "claude", Models: []string{"claude-*"}},
			{ID: "c1", AccountID: "acctC", Provider: "claude", Models: []string{"claude-*"}, Priority: 100},
			{ID: "x1", AccountID: "acctX", Provider: "codex", Models: []string{"gpt-*"}},
		},
		Pools: []quota.Pool{
			{ID: "claude-oauth", Provider: "claude", CredentialIDs: []string{"a1", "a2", "b1", "c1"}},
			{ID: "claude-ab", Provider: "claude", CredentialIDs: []string{"a1", "a2", "b1"}},
			{ID: "codex-oauth", Provider: "codex", CredentialIDs: []string{"x1"}},
		},
	}
}

func accountRouterFor(snap quota.Snapshot) *accountRouter {
	r := newAccountRouter(staticSnapshotSource{snap: snap, maxAge: 10 * time.Minute})
	r.now = func() time.Time { return accountTestNow }
	return r
}

func explicitComboMember(strategy, pool string) (Combo, Member) {
	member := Member{Model: "claude-sonnet", AccountPool: pool}
	combo := Combo{Name: "c", AccountStrategy: strategy, Members: []Member{member}}.Normalize()
	return combo, combo.Members[0]
}

func mustPlan(t *testing.T, r *accountRouter, combo Combo, member Member) accountPlan {
	t.Helper()
	plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return plan
}

func TestAccountPlanPinsOnlyPoolCredentials(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	plan := mustPlan(t, r, combo, member)
	seen := map[string]bool{}
	for _, id := range plan.Credentials {
		seen[id] = true
	}
	if len(plan.Credentials) != 3 || seen["c1"] || seen["x1"] || !seen["a1"] || !seen["a2"] || !seen["b1"] {
		t.Fatalf("plan escaped or lost pool membership: %v", plan.Credentials)
	}
	if plan.Provider != "claude" {
		t.Fatalf("provider = %q, want the pool's provider", plan.Provider)
	}
}

func TestAccountPlanRoundRobinIsEqualPerQuotaAccountNotPerCredential(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		first := mustPlan(t, r, combo, member).Credentials[0]
		account := "acctA"
		if first == "b1" {
			account = "acctB"
		}
		counts[account]++
	}
	if counts["acctA"] != 200 || counts["acctB"] != 200 {
		t.Fatalf("two credentials sharing one quota account must not double its share: %v", counts)
	}
}

func TestAccountPlanTriesOtherAccountBeforeSecondCredentialOfSameAccount(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	for i := 0; i < 10; i++ {
		ids := mustPlan(t, r, combo, member).Credentials
		joined := strings.Join(ids, ",")
		if len(ids) != 3 {
			t.Fatalf("unexpected plan %s", joined)
		}
		switch ids[0] {
		case "a1", "a2":
			if ids[1] != "b1" {
				t.Fatalf("account B must come before A's second credential: %s", joined)
			}
		case "b1":
			if ids[1] != "a1" && ids[1] != "a2" {
				t.Fatalf("unexpected order %s", joined)
			}
		}
	}
}

func TestAccountPlanIgnoresNumericPriority(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-oauth")
	firsts := map[string]int{}
	for i := 0; i < 300; i++ {
		firsts[mustPlan(t, r, combo, member).Credentials[0]]++
	}
	// Three accounts (A, B, C) rotate equally; c1's priority 100 must not pin it first.
	if firsts["c1"] != 100 {
		t.Fatalf("priority leaked into selection: %v", firsts)
	}
}

func TestAccountPlanQuotaAwareUsesAccountScoreAndDedupesAccounts(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Accounts[0].Windows = []quota.Window{testWindow(accountTestNow, 3, 0.5)} // acctA: 3x capacity
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyQuotaAware, "claude-ab")
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		first := mustPlan(t, r, combo, member).Credentials[0]
		if first == "b1" {
			counts["B"]++
		} else {
			counts["A"]++
		}
	}
	// A is scored 3x B once per ACCOUNT (a1+a2 do not count twice): 300 vs 100.
	if counts["A"] != 300 || counts["B"] != 100 {
		t.Fatalf("quota-aware allocation: %v", counts)
	}
}

func TestAccountPlanQuotaAwareDropsKnownExhaustedAccounts(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Accounts[0].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)} // acctA exhausted
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyQuotaAware, "claude-ab")
	for i := 0; i < 20; i++ {
		ids := mustPlan(t, r, combo, member).Credentials
		if len(ids) != 1 || ids[0] != "b1" {
			t.Fatalf("exhausted account credentials must not be pinned: %v", ids)
		}
	}
}

func TestAccountPlanQuotaAwareAllExhaustedFailsRetryablyWithoutPins(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	for i := range snap.Accounts {
		snap.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
	}
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyQuotaAware, "claude-ab")
	plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if err == nil || !err.Retryable || err.Status != http.StatusTooManyRequests || len(plan.Credentials) != 0 {
		t.Fatalf("plan = %+v err = %v, want retryable 429 and zero pins", plan, err)
	}
}

func TestAccountPlanUnknownQuotaIsNeutralInsideMembershipAndAddsNothing(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Accounts[1].Windows = nil // acctB: no observation at all
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyQuotaAware, "claude-ab")
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		ids := mustPlan(t, r, combo, member).Credentials
		counts[ids[0]]++
		for _, id := range ids {
			if id == "c1" || id == "x1" {
				t.Fatalf("unknown quota must not widen membership: %v", ids)
			}
		}
	}
	if counts["b1"] == 0 {
		t.Fatalf("unknown account must stay selectable (neutral), got %v", counts)
	}
}

func TestAccountPlanFiltersIneligibleCredentials(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials[0].Disabled = true                               // a1
	snap.Credentials[1].NextRetryAt = accountTestNow.Add(time.Minute) // a2 cooling
	snap.Credentials[2].Models = []string{"gpt-*"}                    // b1 cannot serve the route model
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-oauth")
	ids := mustPlan(t, r, combo, member).Credentials
	if len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("only c1 is eligible, got %v", ids)
	}
}

func TestAccountPlanBoundsAttemptsToEight(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials = nil
	var ids []string
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("k%02d", i)
		snap.Credentials = append(snap.Credentials, quota.Credential{ID: id, AccountID: "acctA", Provider: "claude", Models: []string{"claude-*"}})
		ids = append(ids, id)
	}
	snap.Pools = []quota.Pool{{ID: "big", Provider: "claude", CredentialIDs: ids}}
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "big")
	if got := len(mustPlan(t, r, combo, member).Credentials); got != maxAccountAttempts {
		t.Fatalf("plan lists %d credentials, want at most %d", got, maxAccountAttempts)
	}
}

func TestAccountPlanFailsClosedAndNeverUnpins(t *testing.T) {
	good := accountTestSnapshot(accountTestNow)
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	cases := []struct {
		name   string
		router *accountRouter
		pool   string
		member func(*Member)
		status int
		fatal  bool
	}{
		{"unknown pool", accountRouterFor(good), "nope", nil, http.StatusServiceUnavailable, false},
		{"provider mismatch", accountRouterFor(good), "claude-ab", func(m *Member) { m.Provider = "codex" }, http.StatusServiceUnavailable, false},
		{"stale snapshot", accountRouterFor(stale), "claude-ab", nil, http.StatusServiceUnavailable, false},
		{"snapshot unavailable", func() *accountRouter {
			r := newAccountRouter(staticSnapshotSource{err: fmt.Errorf("no snapshot")})
			r.now = func() time.Time { return accountTestNow }
			return r
		}(), "claude-ab", nil, http.StatusServiceUnavailable, false},
		{"no router", nil, "claude-ab", nil, http.StatusInternalServerError, true},
		{"model unsupported by every credential", accountRouterFor(good), "claude-ab", func(m *Member) { m.Model = "gpt-6" }, http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			combo, member := explicitComboMember(AccountStrategyRoundRobin, tc.pool)
			if tc.member != nil {
				tc.member(&member)
			}
			plan, err := tc.router.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
			if err == nil {
				t.Fatalf("expected failure, got plan %+v", plan)
			}
			if err.Status != tc.status || err.Retryable == tc.fatal {
				t.Fatalf("err = %+v, want status %d fatal=%v", err, tc.status, tc.fatal)
			}
			if len(plan.Credentials) != 0 {
				t.Fatalf("a failed plan must carry no pins (never unpin to recover): %v", plan.Credentials)
			}
		})
	}
}

func TestAccountPlanUsesPoolProviderEvenWhenMemberSpellsItDifferently(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	member.Provider = "Claude"
	plan := mustPlan(t, r, combo, member)
	if plan.Provider != "claude" {
		t.Fatalf("pinned attempts force the pool's canonical provider, got %q", plan.Provider)
	}
}

func TestAccountPlanPrefersBoundCredentialOnlyWhenStillEligible(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member, Bound: "b1"})
	if err != nil || plan.Credentials[0] != "b1" || len(plan.Credentials) != 3 {
		t.Fatalf("bound credential should lead: %+v %v", plan, err)
	}
	plan, err = r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member, Bound: "c1"})
	if err != nil || plan.Bound || len(plan.Credentials) != 3 {
		t.Fatalf("a binding outside the pool must be ignored: %+v %v", plan, err)
	}
}

func TestAccountPlanStateIsIndependentPerComboAndPolicy(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	comboX, memberX := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	comboX.Name = "x"
	comboY, memberY := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	comboY.Name = "y"
	// Interleave: if state were shared, x would see every other account.
	var xs, ys []string
	for i := 0; i < 6; i++ {
		xs = append(xs, accountOf(mustPlan(t, r, comboX, memberX).Credentials[0]))
		ys = append(ys, accountOf(mustPlan(t, r, comboY, memberY).Credentials[0]))
	}
	for i := 1; i < len(xs); i++ {
		if xs[i] == xs[i-1] || ys[i] == ys[i-1] {
			t.Fatalf("each combo must alternate on its own counter: x=%v y=%v", xs, ys)
		}
	}
	if fmt.Sprint(xs) != fmt.Sprint(ys) {
		t.Fatalf("identical independent state should produce identical sequences: x=%v y=%v", xs, ys)
	}
}

func accountOf(cred string) string {
	if cred == "b1" {
		return "B"
	}
	return "A"
}
