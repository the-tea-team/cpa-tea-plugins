package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func pinMember(model string, mutate func(*Member)) (Combo, Member) {
	m := Member{Model: model}
	if mutate != nil {
		mutate(&m)
	}
	combo := Combo{Name: "c", Members: []Member{m}}.Normalize()
	return combo, combo.Members[0]
}

func planPin(t *testing.T, r *accountRouter, mutate func(*Member)) accountPlan {
	t.Helper()
	combo, member := pinMember("claude-sonnet", mutate)
	plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if err != nil {
		t.Fatalf("plan: %+v", err)
	}
	return plan
}

func planPinErr(t *testing.T, r *accountRouter, mutate func(*Member)) *comboError {
	t.Helper()
	combo, member := pinMember("claude-sonnet", mutate)
	plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
	if err == nil {
		t.Fatalf("plan succeeded with %+v, want a member-side failure", plan)
	}
	if len(plan.Credentials) != 0 {
		t.Fatalf("failed plan must carry no pins: %+v", plan)
	}
	return err
}

func sortedCreds(plan accountPlan) []string {
	out := append([]string(nil), plan.Credentials...)
	sort.Strings(out)
	return out
}

func TestPinAccountNarrowsToThatAccountOnly(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA" })
	if got := sortedCreds(plan); strings.Join(got, ",") != "a1,a2" {
		t.Fatalf("account pin must narrow to acctA's credentials, got %v", plan.Credentials)
	}
	if plan.Provider != "claude" {
		t.Fatalf("provider must be inferred from the target, got %q", plan.Provider)
	}
	if plan.Strategy != AccountStrategyRoundRobin {
		t.Fatalf("a bare pin rotates round-robin, got %q", plan.Strategy)
	}
	b := planPin(t, r, func(m *Member) { m.AccountID = "acctB" })
	if len(b.Credentials) != 1 || b.Credentials[0] != "b1" {
		t.Fatalf("acctB pin = %v, want only b1", b.Credentials)
	}
}

func TestPinConnectionNarrowsToExactlyOneCredential(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	for i := 0; i < 6; i++ {
		plan := planPin(t, r, func(m *Member) { m.ConnectionID = "a1" })
		if len(plan.Credentials) != 1 || plan.Credentials[0] != "a1" {
			t.Fatalf("connection pin must never reach a sibling of the same account: %v", plan.Credentials)
		}
		if plan.Provider != "claude" {
			t.Fatalf("provider = %q", plan.Provider)
		}
	}
}

func TestPinAccountRotatesWithinItsOwnCredentials(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	first := map[string]int{}
	for i := 0; i < 8; i++ {
		plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA" })
		first[plan.Credentials[0]]++
	}
	if first["a1"] == 0 || first["a2"] == 0 || len(first) != 2 {
		t.Fatalf("round-robin within the pinned account expected, leaders = %v", first)
	}
}

func TestPinMemberProviderIsOptionalAndMismatchRejected(t *testing.T) {
	r := accountRouterFor(accountTestSnapshot(accountTestNow))
	if plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA"; m.Provider = "Claude" }); plan.Provider != "claude" {
		t.Fatalf("matching provider (case-insensitive) must plan with the target's provider: %q", plan.Provider)
	}
	for name, mutate := range map[string]func(*Member){
		"account":    func(m *Member) { m.AccountID = "acctA"; m.Provider = "codex" },
		"connection": func(m *Member) { m.ConnectionID = "a1"; m.Provider = "codex" },
	} {
		t.Run(name, func(t *testing.T) {
			if err := planPinErr(t, r, mutate); err.Status != http.StatusServiceUnavailable || !err.Retryable {
				t.Fatalf("provider mismatch must be a retryable member-side 503: %+v", err)
			}
		})
	}
}

func TestPinMissingUnknownAndEmptyTargetsFailClosed(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Accounts = append(snap.Accounts, quota.Account{ID: "acctEmpty", Provider: "claude", BillingType: "subscription"})
	r := accountRouterFor(snap)
	cases := map[string]func(*Member){
		"unknown account":                     func(m *Member) { m.AccountID = "acct-nope" },
		"unknown connection":                  func(m *Member) { m.ConnectionID = "nope" },
		"account without any credential":      func(m *Member) { m.AccountID = "acctEmpty" },
		"account id given as a connection":    func(m *Member) { m.ConnectionID = "acctA" },
		"connection id given as an account":   func(m *Member) { m.AccountID = "a1" },
		"authIndex style id is never guessed": func(m *Member) { m.ConnectionID = "auth-0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := planPinErr(t, r, mutate); err.Status != http.StatusServiceUnavailable || !err.Retryable {
				t.Fatalf("want retryable 503, got %+v", err)
			}
		})
	}
}

func TestPinNeverFallsBackToAuthIndex(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	for i := range snap.Credentials {
		if snap.Credentials[i].ID == "a1" {
			snap.Credentials[i].AuthIndex = "idx-a1"
		}
	}
	r := accountRouterFor(snap)
	planPinErr(t, r, func(m *Member) { m.ConnectionID = "idx-a1" })
}

func TestPinSnapshotFreshnessFailsClosed(t *testing.T) {
	stale := accountTestSnapshot(accountTestNow)
	stale.GeneratedAt = accountTestNow.Add(-time.Hour)
	future := accountTestSnapshot(accountTestNow)
	future.GeneratedAt = accountTestNow.Add(time.Hour)
	zero := accountTestSnapshot(accountTestNow)
	zero.GeneratedAt = time.Time{}
	routers := map[string]*accountRouter{
		"stale":       accountRouterFor(stale),
		"future":      accountRouterFor(future),
		"zero":        accountRouterFor(zero),
		"unavailable": newAccountRouter(staticSnapshotSource{err: fmt.Errorf("no snapshot")}),
	}
	for name, r := range routers {
		r.now = func() time.Time { return accountTestNow }
		for kind, mutate := range map[string]func(*Member){
			"account":    func(m *Member) { m.AccountID = "acctA" },
			"connection": func(m *Member) { m.ConnectionID = "a1" },
		} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				if err := planPinErr(t, r, mutate); err.Status != http.StatusServiceUnavailable || !err.Retryable {
					t.Fatalf("want retryable 503, got %+v", err)
				}
			})
		}
	}
}

func TestPinNilSourceFailsClosed(t *testing.T) {
	var nilRouter *accountRouter
	routers := map[string]*accountRouter{
		"nil router":         nilRouter,
		"nil source":         newAccountRouter(nil),
		"typed nil weighted": newAccountRouter((*weightedSelector)(nil)),
	}
	for name, r := range routers {
		t.Run(name, func(t *testing.T) {
			combo, member := pinMember("claude-sonnet", func(m *Member) { m.ConnectionID = "a1" })
			plan, err := r.plan(context.Background(), accountPlanRequest{Combo: combo, Member: member})
			if err == nil || len(plan.Credentials) != 0 {
				t.Fatalf("a missing source must fail closed: plan=%+v err=%v", plan, err)
			}
		})
	}
}

func TestPinModelSupportIsPositiveAndExact(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials = append(snap.Credentials,
		quota.Credential{ID: "nomodels", AccountID: "acctB", Provider: "claude"},
		quota.Credential{ID: "narrow", AccountID: "acctB", Provider: "claude", Models: []string{"claude-haiku"}},
	)
	r := accountRouterFor(snap)
	for _, id := range []string{"nomodels", "narrow", "x1"} {
		t.Run(id, func(t *testing.T) {
			planPinErr(t, r, func(m *Member) { m.ConnectionID = id })
		})
	}
	planPinErr(t, r, func(m *Member) { m.AccountID = "acctA"; m.Model = "gpt-6" })
	// Credentials without advertised support are dropped from an account pin, not wildcarded.
	plan := planPin(t, r, func(m *Member) { m.AccountID = "acctB" })
	if got := strings.Join(sortedCreds(plan), ","); got != "b1" {
		t.Fatalf("only credentials that advertise the model may be pinned, got %s", got)
	}
}

func TestPinUnusableCredentialsAreNeverPlanned(t *testing.T) {
	mutations := map[string]func(*quota.Credential){
		"disabled":    func(c *quota.Credential) { c.Disabled = true },
		"unavailable": func(c *quota.Credential) { c.Unavailable = true },
		"cooling":     func(c *quota.Credential) { c.NextRetryAt = accountTestNow.Add(time.Minute) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			snap := accountTestSnapshot(accountTestNow)
			for i := range snap.Credentials {
				if snap.Credentials[i].ID == "a1" {
					mutate(&snap.Credentials[i])
				}
			}
			r := accountRouterFor(snap)
			if err := planPinErr(t, r, func(m *Member) { m.ConnectionID = "a1" }); err.Status != http.StatusServiceUnavailable {
				t.Fatalf("exact connection unusable => 503, got %+v", err)
			}
			plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA" })
			if len(plan.Credentials) != 1 || plan.Credentials[0] != "a2" {
				t.Fatalf("account pin must keep only its usable credential a2, got %v", plan.Credentials)
			}
			for i := range snap.Credentials {
				if snap.Credentials[i].ID == "a2" {
					mutate(&snap.Credentials[i])
				}
			}
			r = accountRouterFor(snap)
			if err := planPinErr(t, r, func(m *Member) { m.AccountID = "acctA" }); err.Status != http.StatusServiceUnavailable {
				t.Fatalf("no usable credential in the account => 503, got %+v", err)
			}
		})
	}
}

func TestPinProviderConsistencyBetweenAccountAndCredential(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials = append(snap.Credentials, quota.Credential{ID: "odd", AccountID: "acctA", Provider: "codex", Models: []string{"claude-*"}})
	r := accountRouterFor(snap)
	plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA" })
	if got := strings.Join(sortedCreds(plan), ","); got != "a1,a2" {
		t.Fatalf("a credential of another provider must not join the account pin: %s", got)
	}
	planPinErr(t, r, func(m *Member) { m.ConnectionID = "odd" })
}

func TestPinKnownExhaustedAccountRejectedEvenForRoundRobin(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	for i := range snap.Accounts {
		if snap.Accounts[i].ID == "acctA" {
			snap.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
		}
	}
	r := accountRouterFor(snap)
	for name, mutate := range map[string]func(*Member){
		"account rr":             func(m *Member) { m.AccountID = "acctA" },
		"connection rr":          func(m *Member) { m.ConnectionID = "a1" },
		"account quota-aware":    func(m *Member) { m.AccountID = "acctA"; m.AccountStrategy = AccountStrategyQuotaAware },
		"connection quota-aware": func(m *Member) { m.ConnectionID = "a2"; m.AccountStrategy = AccountStrategyQuotaAware },
		"account explicit rr":    func(m *Member) { m.AccountID = "acctA"; m.AccountStrategy = AccountStrategyRoundRobin },
		"account inherit + pin":  func(m *Member) { m.AccountID = "acctA"; m.AccountStrategy = AccountStrategyInherit },
	} {
		t.Run(name, func(t *testing.T) {
			if err := planPinErr(t, r, mutate); err.Status != http.StatusTooManyRequests || !err.Retryable {
				t.Fatalf("known-exhausted hard pin => retryable 429, got %+v", err)
			}
		})
	}
	// Unaffected siblings stay routable.
	if plan := planPin(t, r, func(m *Member) { m.AccountID = "acctB" }); plan.Credentials[0] != "b1" {
		t.Fatalf("acctB must stay routable: %v", plan.Credentials)
	}
	// Unknown quota is neutral, not exhausted.
	if plan := planPin(t, r, func(m *Member) { m.AccountID = "acctX"; m.Model = "gpt-5" }); plan.Provider != "codex" || plan.Credentials[0] != "x1" {
		t.Fatalf("unknown quota must stay pinnable: %+v", plan)
	}
}

func TestLegacyPoolRoundRobinStillKeepsExhaustedAccounts(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	for i := range snap.Accounts {
		if snap.Accounts[i].ID == "acctB" {
			snap.Accounts[i].Windows = []quota.Window{testWindow(accountTestNow, 1, 1)}
		}
	}
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-ab")
	if got := strings.Join(sortedCreds(mustPlan(t, r, combo, member)), ","); got != "a1,a2,b1" {
		t.Fatalf("legacy round-robin pool behaviour must be unchanged, got %s", got)
	}
}

func TestPinOptionalPoolIsAdditionalIntersection(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	snap.Pools = append(snap.Pools,
		quota.Pool{ID: "only-a1", Provider: "claude", CredentialIDs: []string{"a1"}},
		quota.Pool{ID: "only-b1", Provider: "claude", CredentialIDs: []string{"b1"}},
	)
	r := accountRouterFor(snap)

	plan := planPin(t, r, func(m *Member) { m.AccountID = "acctA"; m.AccountPool = "only-a1" })
	if len(plan.Credentials) != 1 || plan.Credentials[0] != "a1" {
		t.Fatalf("pool must narrow an account pin to a1, got %v", plan.Credentials)
	}
	plan = planPin(t, r, func(m *Member) { m.AccountID = "acctA"; m.AccountPool = "claude-ab" })
	if got := strings.Join(sortedCreds(plan), ","); got != "a1,a2" {
		t.Fatalf("pool containing the account keeps it whole, got %s", got)
	}
	if plan = planPin(t, r, func(m *Member) { m.ConnectionID = "b1"; m.AccountPool = "claude-ab" }); plan.Credentials[0] != "b1" || len(plan.Credentials) != 1 {
		t.Fatalf("connection + member pool = %v", plan.Credentials)
	}
	for name, mutate := range map[string]func(*Member){
		"target outside pool (account)":    func(m *Member) { m.AccountID = "acctB"; m.AccountPool = "only-a1" },
		"target outside pool (connection)": func(m *Member) { m.ConnectionID = "c1"; m.AccountPool = "claude-ab" },
		"pool of another provider":         func(m *Member) { m.AccountID = "acctA"; m.AccountPool = "codex-oauth" },
		"unknown pool is never ignored":    func(m *Member) { m.ConnectionID = "a1"; m.AccountPool = "no-such-pool" },
		"pool does not widen to siblings":  func(m *Member) { m.ConnectionID = "a1"; m.AccountPool = "only-b1" },
	} {
		t.Run(name, func(t *testing.T) {
			if err := planPinErr(t, r, mutate); err.Status != http.StatusServiceUnavailable || !err.Retryable {
				t.Fatalf("want retryable 503, got %+v", err)
			}
		})
	}
}

func TestPinPoolIntersectionEnforcesPoolModelSupport(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	r := accountRouterFor(snap)
	planPinErr(t, r, func(m *Member) { m.AccountID = "acctA"; m.AccountPool = "claude-ab"; m.Model = "gpt-6" })
}

func TestPinDoesNotMutateSnapshot(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow)
	pools := len(snap.Pools)
	r := accountRouterFor(snap)
	planPin(t, r, func(m *Member) { m.AccountID = "acctA" })
	planPin(t, r, func(m *Member) { m.ConnectionID = "a1"; m.AccountPool = "claude-ab" })
	if len(snap.Pools) != pools {
		t.Fatalf("planning must not leave synthetic pools in the shared snapshot")
	}
}

// --- identities ---

func TestPinRotationStateKeyIncludesTargetsButKeepsLegacy(t *testing.T) {
	combo := Combo{Name: "c"}
	legacy := Member{Model: "m", Provider: "claude", AccountPool: "p"}
	want := strings.Join([]string{"c", "m", "claude", "p", AccountStrategyRoundRobin}, "\x00")
	if got := accountStateKey(combo, legacy, AccountStrategyRoundRobin); got != want {
		t.Fatalf("legacy rotation key changed: %q != %q", got, want)
	}
	a := accountStateKey(combo, Member{Model: "m", AccountID: "A"}, AccountStrategyRoundRobin)
	b := accountStateKey(combo, Member{Model: "m", AccountID: "B"}, AccountStrategyRoundRobin)
	c := accountStateKey(combo, Member{Model: "m", ConnectionID: "A"}, AccountStrategyRoundRobin)
	d := accountStateKey(combo, Member{Model: "m"}, AccountStrategyRoundRobin)
	seen := map[string]string{}
	for name, key := range map[string]string{"accA": a, "accB": b, "connA": c, "none": d} {
		if prev, dup := seen[key]; dup {
			t.Fatalf("%s and %s share a rotation key", prev, name)
		}
		seen[key] = name
	}
}

func TestPinAccountBindingKeyIncludesTargetsButKeepsLegacy(t *testing.T) {
	combo := Combo{Name: "c"}
	legacy := Member{Model: "m", Provider: "claude", AccountPool: "p"}
	sum := sha256.Sum256([]byte(accountBindingVersion + "\x00caller\x00sess\x00c\x00m\x00claude\x00p\x00" + AccountStrategyRoundRobin))
	if got := accountBindingKey("caller", "sess", combo, legacy, AccountStrategyRoundRobin); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("legacy binding identity changed")
	}
	keys := map[string]string{}
	for name, m := range map[string]Member{
		"none": {Model: "m"}, "accA": {Model: "m", AccountID: "A"}, "accB": {Model: "m", AccountID: "B"},
		"connA": {Model: "m", ConnectionID: "A"}, "connB": {Model: "m", ConnectionID: "B"},
	} {
		key := accountBindingKey("caller", "sess", combo, m, AccountStrategyRoundRobin)
		if prev, dup := keys[key]; dup {
			t.Fatalf("%s and %s share a binding key", prev, name)
		}
		keys[key] = name
	}
}

func TestPinCooldownIdentityIncludesTargetsButKeepsLegacy(t *testing.T) {
	if got, want := cooldownKey("c", Member{Model: "m", Provider: "p"}), "c\x00p\x00m"; got != want {
		t.Fatalf("legacy cooldown key changed: %q != %q", got, want)
	}
	tracker := newCooldownTracker()
	a := Member{Model: "m", AccountID: "A"}
	b := Member{Model: "m", AccountID: "B"}
	conn := Member{Model: "m", ConnectionID: "A"}
	plain := Member{Model: "m"}
	tracker.Cool("c", a, time.Minute)
	if !tracker.Active("c", a) {
		t.Fatal("pinned member must cool")
	}
	for name, m := range map[string]Member{"other account": b, "same id as connection": conn, "unpinned": plain} {
		if tracker.Active("c", m) {
			t.Fatalf("cooling account A poisoned %s", name)
		}
	}
	tracker.Clear("c", b)
	if !tracker.Active("c", a) {
		t.Fatal("clearing B must not clear A")
	}
}
