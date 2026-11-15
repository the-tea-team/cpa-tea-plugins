package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func accountCombo(t *testing.T, raw string) (Combo, error) {
	t.Helper()
	var c Combo
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c = c.Normalize()
	return c, c.Validate()
}

func TestAccountFieldsRoundTripJSON(t *testing.T) {
	c, err := accountCombo(t, `{"name":"x","accountStrategy":"quota-aware-round-robin","members":[
		{"model":"a","accountPool":" pool-a "},
		{"model":"b","accountStrategy":"round-robin","accountPool":"pool-b"},
		{"model":"c","accountStrategy":"inherit"}]}`)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Members[0].AccountPool != "pool-a" {
		t.Fatalf("accountPool not trimmed: %q", c.Members[0].AccountPool)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"accountPool":"pool-a"`, `"accountStrategy":"round-robin"`, `"accountStrategy":"inherit"`, `"accountStrategy":"quota-aware-round-robin"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}

func TestAccountStrategyValidation(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"unknown combo strategy", `{"name":"x","accountStrategy":"bogus","members":[{"model":"a"}]}`, "accountStrategy"},
		{"unknown member strategy", `{"name":"x","members":[{"model":"a","accountStrategy":"bogus","accountPool":"p"}]}`, "accountStrategy"},
		{"explicit combo strategy needs pool on every applicable member", `{"name":"x","accountStrategy":"round-robin","members":[{"model":"a","accountPool":"p"},{"model":"b"}]}`, "accountPool"},
		{"quotaPool is observation only and never satisfies the pool requirement", `{"name":"x","accountStrategy":"quota-aware-round-robin","members":[{"model":"a","quotaPool":"p"}]}`, "accountPool"},
		{"member override needs its own pool", `{"name":"x","members":[{"model":"a","accountStrategy":"round-robin"}]}`, "accountPool"},
		{"member explicit inherit exempts the member", `{"name":"x","accountStrategy":"round-robin","members":[{"model":"a","accountPool":"p"},{"model":"b","accountStrategy":"inherit"}]}`, ""},
		{"inherit combo with explicit member override", `{"name":"x","accountStrategy":"inherit","members":[{"model":"a","accountStrategy":"quota-aware-round-robin","accountPool":"p"},{"model":"b"}]}`, ""},
		{"empty strategy is core behaviour", `{"name":"x","members":[{"model":"a"}]}`, ""},
		{"pool without an explicit strategy is rejected", `{"name":"x","members":[{"model":"a","accountPool":"p"}]}`, "accountPool"},
		{"pool on an explicit inherit member is rejected", `{"name":"x","accountStrategy":"round-robin","members":[{"model":"a","accountStrategy":"inherit","accountPool":"p"}]}`, "accountPool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := accountCombo(t, tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestSeedConfigParsesAccountFields(t *testing.T) {
	cfg, err := parseSeedConfig([]byte(`
combos:
  general:
    accountStrategy: quota-aware-round-robin
    members:
      - model: claude-a
        accountPool: claude-oauth
      - model: local
        accountStrategy: inherit
`))
	if err != nil {
		t.Fatalf("parseSeedConfig: %v", err)
	}
	c := cfg.Combos[0]
	if c.AccountStrategy != AccountStrategyQuotaAware || c.Members[0].AccountPool != "claude-oauth" || c.Members[1].AccountStrategy != AccountStrategyInherit {
		t.Fatalf("yaml tags not honoured: %+v", c)
	}
}

func TestManagementAPIAcceptsAccountFields(t *testing.T) {
	combo, err := decodeCombo([]byte(`{"name":"m","accountStrategy":"round-robin","members":[{"model":"a","accountPool":"p"}]}`))
	if err != nil {
		t.Fatalf("decodeCombo: %v", err)
	}
	if combo.Members[0].AccountPool != "p" {
		t.Fatalf("accountPool dropped: %+v", combo)
	}
}
