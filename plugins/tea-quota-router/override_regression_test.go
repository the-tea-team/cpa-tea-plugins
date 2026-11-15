package main

import (
	"errors"
	"testing"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func TestOverrideConflictThreeMembers(t *testing.T) {
	for _, field := range []string{"plan", "capacity"} {
		t.Run(field, func(t *testing.T) {
			base := stateBase()
			base.Credentials = append(base.Credentials, quota.Credential{ID: "c3", AccountID: "c3", Provider: "claude", Label: "Three", Models: []string{"m"}})
			base.Accounts = append(base.Accounts, quota.Account{ID: "c3", Provider: "claude", Label: "Three", BillingType: "subscription", RefreshStatus: "ok", ObservedAt: stateT0, Windows: []quota.Window{}, Financial: []quota.Financial{}})
			ov := overrideDocument{Credentials: map[string]AccountOverride{"c1": {AccountID: "group"}, "c2": {AccountID: "group"}, "c3": {AccountID: "group"}}}
			a, b := ov.Credentials["c2"], ov.Credentials["c3"]
			if field == "plan" {
				a.Plan = "pro"
				b.Plan = "max"
			} else {
				a.CapacityByWindow = map[string]float64{"weekly": 1}
				b.CapacityByWindow = map[string]float64{"weekly": 2}
			}
			ov.Credentials["c2"], ov.Credentials["c3"] = a, b
			if _, _, err := materializeView(base, ov, true); !errors.Is(err, errInvalidOverrides) {
				t.Fatalf("expected conflict, got %v", err)
			}
		})
	}
}

func TestOverrideImplicitMembershipConflict(t *testing.T) {
	ov := overrideDocument{Credentials: map[string]AccountOverride{"c1": {AccountID: "c2", Plan: "max"}, "c2": {Plan: "pro"}}}
	if _, _, err := materializeView(stateBase(), ov, true); !errors.Is(err, errInvalidOverrides) {
		t.Fatalf("expected conflict: %v", err)
	}
}

func TestRetiredPoolMemberAllowsLabelUpdate(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	body := `{"credentials":{"c1":{"label":"New"}},"pools":[{"id":"chosen","label":"Chosen","provider":"claude","credentialIds":["c1","retired"]}]}`
	if code := putCode(t, p, body); code != 200 {
		t.Fatalf("PUT returned %d", code)
	}
	d := getOverrides(t, p)
	if len(d.Pools) != 1 || len(d.Pools[0].CredentialIDs) != 2 {
		t.Fatalf("configured pool lost retired member: %+v", d.Pools)
	}
}

func TestUnresolvedExplicitPoolNeverBroadens(t *testing.T) {
	for _, strict := range []bool{false, true} {
		ov := overrideDocument{Credentials: map[string]AccountOverride{}, Pools: []quota.Pool{{ID: "chosen", Provider: "claude", BillingType: "subscription", CredentialIDs: []string{"retired"}}}}
		view, warns, err := materializeView(stateBase(), ov, strict)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range view.Pools {
			if p.ID == "chosen" {
				t.Fatalf("unresolved pool broadened: %+v", p)
			}
		}
		if len(warns) == 0 {
			t.Fatal("missing warning")
		}
	}
}
func TestPoolKnownForeignProviderRejected(t *testing.T) {
	ov := overrideDocument{Credentials: map[string]AccountOverride{}, Pools: []quota.Pool{{ID: "chosen", Provider: "claude", CredentialIDs: []string{"x1"}}}}
	if _, _, err := materializeView(stateBase(), ov, true); !errors.Is(err, errInvalidOverrides) {
		t.Fatalf("expected foreign member rejected: %v", err)
	}
}
func TestUnknownOverrideCannotConflictOrAlterGroup(t *testing.T) {
	ov := overrideDocument{Credentials: map[string]AccountOverride{"c1": {AccountID: "group", Plan: "pro"}, "retired": {AccountID: "group", Plan: "max"}}}
	view, _, err := materializeView(stateBase(), ov, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range view.Accounts {
		if a.ID == "group" {
			if a.Plan != "pro" {
				t.Fatalf("unexpected plan: %s", a.Plan)
			}
			return
		}
	}
	t.Fatal("group missing")
}
