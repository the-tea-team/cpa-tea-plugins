package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// sharedAccountSnapshot is the monopoly shape: acctA owns eight credentials (a1..a8) that share
// one allowance, acctB owns a single credential. One pool lists all nine.
func sharedAccountSnapshot() quota.Snapshot {
	snap := accountTestSnapshot(accountTestNow)
	snap.Credentials = nil
	var ids []string
	for i := 1; i <= 8; i++ {
		id := fmt.Sprintf("a%d", i)
		snap.Credentials = append(snap.Credentials, quota.Credential{ID: id, AccountID: "acctA", Provider: "claude", Models: []string{"claude-*"}})
		ids = append(ids, id)
	}
	snap.Credentials = append(snap.Credentials, quota.Credential{ID: "b1", AccountID: "acctB", Provider: "claude", Models: []string{"claude-*"}})
	ids = append(ids, "b1")
	snap.Pools = []quota.Pool{{ID: "claude-ab", Provider: "claude", CredentialIDs: ids}}
	return snap
}

func TestAccountPlanInterleavesFirstCredentialOfEachAccountBeforeDuplicates(t *testing.T) {
	for _, strategy := range []string{AccountStrategyRoundRobin, AccountStrategyQuotaAware} {
		t.Run(strategy, func(t *testing.T) {
			r := accountRouterFor(sharedAccountSnapshot())
			combo, member := explicitComboMember(strategy, "claude-ab")
			for i := 0; i < 12; i++ {
				ids := mustPlan(t, r, combo, member).Credentials
				if len(ids) != maxAccountAttempts {
					t.Fatalf("plan must stay bounded at %d, got %v", maxAccountAttempts, ids)
				}
				seen := map[string]bool{}
				for _, id := range ids {
					if seen[id] {
						t.Fatalf("duplicate credential in plan: %v", ids)
					}
					seen[id] = true
				}
				// Whichever account leads, the other account's first credential is attempt two:
				// a second credential on the same quota account is a poor escape from a quota fail.
				if accountOf(ids[0]) == accountOf(ids[1]) {
					t.Fatalf("attempt two must switch account: %v", ids)
				}
				if !seen["b1"] {
					t.Fatalf("b1 must survive the %d-attempt bound: %v", maxAccountAttempts, ids)
				}
			}
		})
	}
}

func TestAccountPlanInterleavesDuplicatesRoundRobinAcrossAccounts(t *testing.T) {
	snap := accountTestSnapshot(accountTestNow) // acctA: a1,a2  acctB: b1  acctC: c1
	r := accountRouterFor(snap)
	combo, member := explicitComboMember(AccountStrategyRoundRobin, "claude-oauth")
	for i := 0; i < 9; i++ {
		ids := mustPlan(t, r, combo, member).Credentials
		// depth 0 covers all three accounts before any depth-1 duplicate.
		first := map[string]bool{}
		for _, id := range ids[:3] {
			first[id[:1]] = true // a1/a2 -> "a", b1 -> "b", c1 -> "c"
		}
		if len(first) != 3 || len(ids) != 4 {
			t.Fatalf("first three attempts must cover three accounts: %v", ids)
		}
		if ids[3] != "a2" && ids[3] != "a1" {
			t.Fatalf("the only duplicate credential comes last: %v", ids)
		}
	}
}

func TestAccountFailoverReachesAccountBAfterA1QuotaFailNonStream(t *testing.T) {
	host := &accountHost{reply: func(call recordedCall) (*pluginapi.HostModelExecutionResponse, error) {
		if strings.HasPrefix(call.req.AuthID, "a") {
			return quotaFailure()
		}
		return okReply("from-" + call.req.AuthID)
	}}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(sharedAccountSnapshot())
	resp, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	if err != nil || string(resp.Payload) != "from-b1" {
		t.Fatalf("resp=%q err=%v calls=%v", resp.Payload, err, host.authIDs())
	}
	if n := len(host.calls); n > 2 {
		t.Fatalf("b1 must be reached right after the first account-A quota failure: %v", host.authIDs())
	}
}

func TestAccountFailoverReachesAccountBAfterA1QuotaFailStream(t *testing.T) {
	h := newAccountStreamHost(t)
	for i := 1; i <= 8; i++ {
		h.openErr[fmt.Sprintf("a%d", i)] = &hostError{Message: "rate limited", Status: 429, Retryable: true}
	}
	h.scripts["b1"] = []pluginapi.HostModelStreamReadResponse{{Payload: []byte("from-b1")}, {Done: true}}
	p := streamAccountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(sharedAccountSnapshot())
	handle, emitted, err := runAccountStream(p, cbCtx(), "c")
	if err != nil || handle == nil {
		t.Fatalf("handle=%v err=%v opens=%v", handle, err, h.openedAuth())
	}
	handle.Close()
	if len(emitted) != 1 || emitted[0] != "from-b1" {
		t.Fatalf("emitted=%v opens=%v", emitted, h.openedAuth())
	}
	if n := len(h.openedAuth()); n > 2 {
		t.Fatalf("b1 must be reached right after the first account-A quota failure: %v", h.openedAuth())
	}
}

func TestAccountFailoverStillBoundedAtEightWithNineCredentials(t *testing.T) {
	host := &accountHost{reply: func(recordedCall) (*pluginapi.HostModelExecutionResponse, error) { return quotaFailure() }}
	host.install(t)
	p := accountPlugin(t, pinnedCombo("c"))
	p.accounts = accountRouterFor(sharedAccountSnapshot())
	_, err := p.executeCombo(cbCtx(), pinnedReq("c"), "c")
	var ce *comboError
	if err == nil || !errors.As(err, &ce) || ce.Status != http.StatusTooManyRequests {
		t.Fatalf("err=%v", err)
	}
	if len(host.calls) != maxAccountAttempts {
		t.Fatalf("attempts = %d, want %d: %v", len(host.calls), maxAccountAttempts, host.authIDs())
	}
}
