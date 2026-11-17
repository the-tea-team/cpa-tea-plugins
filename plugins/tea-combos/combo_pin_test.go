package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPinFieldsNormalizeAndRoundTripJSON(t *testing.T) {
	c, err := accountCombo(t, `{"name":"x","members":[
		{"model":"a","accountId":"  acct-1  "},
		{"model":"b","connectionId":" conn-1 "},
		{"model":"c","accountId":"   ","connectionId":"conn-2"},
		{"model":"d"}]}`)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.Members[0].AccountID; got != "acct-1" {
		t.Fatalf("accountId not trimmed: %q", got)
	}
	if got := c.Members[1].ConnectionID; got != "conn-1" {
		t.Fatalf("connectionId not trimmed: %q", got)
	}
	if c.Members[2].AccountID != "" || c.Members[2].ConnectionID != "conn-2" {
		t.Fatalf("blank accountId must normalize to empty: %+v", c.Members[2])
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"accountId":"acct-1"`, `"connectionId":"conn-1"`, `"connectionId":"conn-2"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
	var legacy Member
	encoded, _ := json.Marshal(c.Members[3])
	if strings.Contains(string(encoded), "accountId") || strings.Contains(string(encoded), "connectionId") {
		t.Fatalf("legacy member must not emit pin fields: %s", encoded)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	_ = legacy
}

func TestPinFieldsDecodeYAMLSeed(t *testing.T) {
	cfg, err := parseSeedConfig([]byte(`
combos:
  pinned:
    members:
      - model: claude-sonnet
        accountId: " acct-1 "
      - model: claude-sonnet
        connectionId: conn-9
        provider: claude
`))
	if err != nil {
		t.Fatalf("parseSeedConfig: %v", err)
	}
	m := cfg.Combos[0].Members
	if m[0].AccountID != "acct-1" || m[1].ConnectionID != "conn-9" || m[1].Provider != "claude" {
		t.Fatalf("yaml pins not decoded/normalized: %+v", m)
	}
}

func TestPinMutuallyExclusive(t *testing.T) {
	_, err := accountCombo(t, `{"name":"x","members":[{"model":"a","accountId":"a1","connectionId":"c1"}]}`)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want mutually exclusive", err)
	}
	if _, err := accountCombo(t, `{"name":"x","members":[{"model":"a","accountId":"a1","connectionId":"  "}]}`); err != nil {
		t.Fatalf("a blank sibling is not a second pin: %v", err)
	}
}

func TestPinValidationWithPoolAndStrategy(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"pin needs no pool or strategy", `{"name":"x","members":[{"model":"a","accountId":"a1"}]}`, ""},
		{"connection pin needs no pool", `{"name":"x","members":[{"model":"a","connectionId":"c1"}]}`, ""},
		{"pin overrides explicit member inherit without pool", `{"name":"x","members":[{"model":"a","accountStrategy":"inherit","accountId":"a1"}]}`, ""},
		{"pin under explicit combo strategy needs no pool", `{"name":"x","accountStrategy":"quota-aware-round-robin","members":[{"model":"a","accountId":"a1"},{"model":"b","accountPool":"p"}]}`, ""},
		{"pin plus pool is an allowed extra intersection", `{"name":"x","members":[{"model":"a","connectionId":"c1","accountPool":"p"}]}`, ""},
		{"pin plus pool plus strategy", `{"name":"x","members":[{"model":"a","accountStrategy":"round-robin","accountId":"a1","accountPool":"p"}]}`, ""},
		{"explicit strategy without pin still needs pool", `{"name":"x","accountStrategy":"round-robin","members":[{"model":"a"}]}`, "accountPool"},
		{"pool without pin or strategy still rejected", `{"name":"x","members":[{"model":"a","accountPool":"p"}]}`, "accountPool"},
		{"pool on inherit member without pin still rejected", `{"name":"x","members":[{"model":"a","accountStrategy":"inherit","accountPool":"p"}]}`, "accountPool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := accountCombo(t, tc.raw)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestEffectiveAccountStrategyWithPin(t *testing.T) {
	cases := []struct {
		name   string
		combo  string
		member Member
		want   string
	}{
		{"no pin empty stays core", "", Member{Model: "m"}, ""},
		{"no pin explicit inherit stays inherit", "", Member{Model: "m", AccountStrategy: AccountStrategyInherit}, AccountStrategyInherit},
		{"pin with empty policy is round-robin", "", Member{Model: "m", AccountID: "a"}, AccountStrategyRoundRobin},
		{"connection pin with empty policy is round-robin", "", Member{Model: "m", ConnectionID: "c"}, AccountStrategyRoundRobin},
		{"pin overrides member inherit", "", Member{Model: "m", AccountID: "a", AccountStrategy: AccountStrategyInherit}, AccountStrategyRoundRobin},
		{"pin overrides combo inherit", AccountStrategyInherit, Member{Model: "m", AccountID: "a"}, AccountStrategyRoundRobin},
		{"pin overrides member inherit over explicit combo", AccountStrategyQuotaAware, Member{Model: "m", AccountID: "a", AccountStrategy: AccountStrategyInherit}, AccountStrategyRoundRobin},
		{"pin keeps combo quota-aware", AccountStrategyQuotaAware, Member{Model: "m", ConnectionID: "c"}, AccountStrategyQuotaAware},
		{"pin keeps member quota-aware", "", Member{Model: "m", AccountID: "a", AccountStrategy: AccountStrategyQuotaAware}, AccountStrategyQuotaAware},
		{"pin member round-robin beats combo quota-aware", AccountStrategyQuotaAware, Member{Model: "m", AccountID: "a", AccountStrategy: AccountStrategyRoundRobin}, AccountStrategyRoundRobin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			combo := Combo{Name: "c", AccountStrategy: tc.combo}
			if got := tc.member.EffectiveAccountStrategy(combo); got != tc.want {
				t.Fatalf("EffectiveAccountStrategy = %q, want %q", got, tc.want)
			}
			wantExplicit := tc.want == AccountStrategyRoundRobin || tc.want == AccountStrategyQuotaAware
			if got := tc.member.ExplicitAccountStrategy(combo); got != wantExplicit {
				t.Fatalf("ExplicitAccountStrategy = %v, want %v", got, wantExplicit)
			}
		})
	}
}

func TestPinFieldsPersistThroughStore(t *testing.T) {
	path := t.TempDir() + "/state.json"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	combo := Combo{Name: "pins", Members: []Member{
		{Model: "m", AccountID: " acct-1 "},
		{Model: "m", ConnectionID: "conn-1", AccountPool: "p"},
		{Model: "legacy"},
	}}.Normalize()
	if err := store.Upsert(combo); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("pins")
	if !ok || len(got.Members) != 3 {
		t.Fatalf("combo lost: %+v", got)
	}
	if got.Members[0].AccountID != "acct-1" || got.Members[1].ConnectionID != "conn-1" || got.Members[1].AccountPool != "p" {
		t.Fatalf("pins not persisted: %+v", got.Members)
	}
	if got.Members[2].Pinned() {
		t.Fatalf("legacy member gained a pin: %+v", got.Members[2])
	}
}
