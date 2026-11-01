package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSeedConfigMapAndDefaults(t *testing.T) {
	raw := []byte(`
statePath: /var/lib/cpa/combos.json
combos:
  fast:
    members:
      - model: gpt-4o
      - model: claude-sonnet
        provider: anthropic
  reliable:
    strategy: failover
    ttl: 30m
    sessionAffinity: false
    enabled: false
    members:
      - model: gpt-4o
`)
	cfg, err := parseSeedConfig(raw)
	if err != nil {
		t.Fatalf("parseSeedConfig: %v", err)
	}
	if cfg.StatePath != "/var/lib/cpa/combos.json" {
		t.Fatalf("statePath = %q", cfg.StatePath)
	}
	if len(cfg.Combos) != 2 {
		t.Fatalf("combos = %d, want 2", len(cfg.Combos))
	}
	fast := cfg.Combos[0]
	if fast.Name != "fast" || fast.Strategy != StrategyRoundRobin || !fast.IsEnabled() {
		t.Fatalf("fast defaults = %+v", fast)
	}
	if fast.Members[1].Provider != "anthropic" {
		t.Fatalf("member provider = %q", fast.Members[1].Provider)
	}
	reliable := cfg.Combos[1]
	if reliable.Strategy != StrategyFailover || reliable.TTL != "30m" || reliable.IsEnabled() {
		t.Fatalf("reliable = %+v", reliable)
	}
	if reliable.SessionAffinity == nil || *reliable.SessionAffinity {
		t.Fatalf("sessionAffinity = %v", reliable.SessionAffinity)
	}
}

func TestParseSeedConfigList(t *testing.T) {
	cfg, err := parseSeedConfig([]byte(`
combos:
  - name: one
    members:
      - model: m1
`))
	if err != nil {
		t.Fatalf("parseSeedConfig: %v", err)
	}
	if len(cfg.Combos) != 1 || cfg.Combos[0].Name != "one" {
		t.Fatalf("combos = %+v", cfg.Combos)
	}
}

func TestParseSeedConfigRejectsBadCombos(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unknown strategy", raw: "combos:\n  c:\n    strategy: weighted\n    members: [{model: m}]\n", want: "unknown strategy"},
		{name: "duplicate names", raw: "combos:\n  - name: c\n    members: [{model: m}]\n  - name: c\n    members: [{model: n}]\n", want: "duplicate"},
		{name: "empty model", raw: "combos:\n  c:\n    members: [{model: \"\"}]\n", want: "empty model"},
		{name: "name with slash", raw: "combos:\n  a/b:\n    members: [{model: m}]\n", want: "must not contain"},
		{name: "bad ttl", raw: "combos:\n  c:\n    ttl: soon\n    members: [{model: m}]\n", want: "invalid ttl"},
		{name: "negative ttl", raw: "combos:\n  c:\n    ttl: -1m\n    members: [{model: m}]\n", want: "must be positive"},
		{name: "key name mismatch", raw: "combos:\n  c:\n    name: other\n    members: [{model: m}]\n", want: "does not match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSeedConfig([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseSeedConfigEmpty(t *testing.T) {
	cfg, err := parseSeedConfig(nil)
	if err != nil {
		t.Fatalf("parseSeedConfig(nil): %v", err)
	}
	if len(cfg.Combos) != 0 || cfg.StatePath != "" {
		t.Fatalf("cfg = %+v, want zero value", cfg)
	}
}

func TestRewriteModelOnlyTouchesModel(t *testing.T) {
	input := []byte(`{"model":"combo","temperature":0.7,"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	output := rewriteModel(input, "member-model")

	var before map[string]json.RawMessage
	var after map[string]json.RawMessage
	if err := json.Unmarshal(input, &before); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	if err := json.Unmarshal(output, &after); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if string(after["model"]) != `"member-model"` {
		t.Fatalf("model = %s", after["model"])
	}
	for key, value := range before {
		if key == "model" {
			continue
		}
		if string(after[key]) != string(value) {
			t.Fatalf("field %q changed: %s != %s", key, after[key], value)
		}
	}
	if len(after) != len(before) {
		t.Fatalf("field count changed: %d != %d", len(after), len(before))
	}
}

func TestRewriteModelLeavesUnparseableBodyAlone(t *testing.T) {
	input := []byte("not json")
	if got := rewriteModel(input, "m"); string(got) != string(input) {
		t.Fatalf("rewriteModel changed a non-JSON body: %q", got)
	}
}
