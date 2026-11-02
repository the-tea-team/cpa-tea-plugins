package main

import (
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
		want    bool
	}{
		{"gpt-4o", "gpt-4o", true},
		{"gpt-4o", "gpt-4o-mini", false},
		{"gpt-*", "gpt-4o", true},
		{"gpt-*", "gpt-4o-mini", true},
		{"gpt-*", "claude-3", false},
		{"*-mini", "gpt-4o-mini", true},
		{"gpt-?-mini", "gpt-4-mini", true},
		{"gpt-?-mini", "gpt-40-mini", false},
		{"*", "anything/with/slashes", true},
		{"*/gpt-4o", "openai/gpt-4o", true},
		{"*/gpt-4o", "anthropic/gpt-4o", true},
		{"claude-*", "claude-sonnet-4", true},
		{"claude-*", "CLAUDE-sonnet-4", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, test := range tests {
		if got := globMatch(test.pattern, test.value); got != test.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", test.pattern, test.value, got, test.want)
		}
	}
}

func TestUncloakClaudeModel(t *testing.T) {
	real := "claude-sonnet-4"
	cloaked := claudeCloakPrefix + reverseRunes(real)
	got, ok := uncloakClaudeModel(cloaked)
	if !ok || got != real {
		t.Fatalf("uncloakClaudeModel(%q) = %q, %v; want %q, true", cloaked, got, ok, real)
	}

	withSuffix := cloaked + "(8192)"
	got, ok = uncloakClaudeModel(withSuffix)
	if !ok || got != real+"(8192)" {
		t.Fatalf("uncloakClaudeModel(%q) = %q, %v; want %q, true", withSuffix, got, ok, real+"(8192)")
	}

	if _, ok := uncloakClaudeModel("gpt-4o"); ok {
		t.Fatal("ordinary model was treated as cloaked")
	}
	if _, ok := uncloakClaudeModel(claudeCloakPrefix); ok {
		t.Fatal("empty cloaked payload was accepted")
	}
}

func TestModelCandidates(t *testing.T) {
	cloaked := claudeCloakPrefix + reverseRunes("claude-opus-4")
	tests := []struct {
		model string
		want  []string
	}{
		{model: "gpt-4o", want: []string{"gpt-4o"}},
		{model: "gpt-4o(8192)", want: []string{"gpt-4o(8192)", "gpt-4o"}},
		{model: "models/gemini-2.5-pro", want: []string{"models/gemini-2.5-pro", "gemini-2.5-pro"}},
		{model: cloaked, want: []string{"claude-opus-4"}},
		{model: cloaked + "(4096)", want: []string{"claude-opus-4(4096)", "claude-opus-4"}},
	}
	for _, test := range tests {
		got := modelCandidates(test.model)
		if len(got) != len(test.want) {
			t.Fatalf("modelCandidates(%q) = %v, want %v", test.model, got, test.want)
		}
		for i := range test.want {
			if got[i] != test.want[i] {
				t.Fatalf("modelCandidates(%q) = %v, want %v", test.model, got, test.want)
			}
		}
	}
}

func TestAPIKeyAllows(t *testing.T) {
	key := &apiKey{models: []string{"gpt-4o", "claude-*", "*/gemini-pro"}}
	cloaked := claudeCloakPrefix + reverseRunes("claude-sonnet-4")

	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4o", true},
		{"gpt-4o(8192)", true},
		{"gpt-4o-mini", false},
		{"claude-sonnet-4", true},
		{cloaked, true},
		{cloaked + "(8192)", true},
		{claudeCloakPrefix + reverseRunes("gpt-4o-mini"), false},
		{"openai/gemini-pro", true},
		{"gemini-pro", false},
		{"models/gemini-pro", true},
	}
	for _, test := range tests {
		if got := key.allows(test.model); got != test.want {
			t.Errorf("allows(%q) = %v, want %v", test.model, got, test.want)
		}
	}
}

func TestAPIKeyAllowsDeniesWhenNoPatterns(t *testing.T) {
	key := &apiKey{}
	if key.allows("gpt-4o") {
		t.Fatal("a key with no patterns and allowAll=false must deny")
	}
}

func TestSplitThinkingSuffix(t *testing.T) {
	tests := []struct {
		model string
		base  string
		ok    bool
	}{
		{"gpt-4o", "gpt-4o", false},
		{"gpt-4o(8192)", "gpt-4o", true},
		{"gpt-4o()", "gpt-4o", true},
		{"gpt-4o(not-closed", "gpt-4o(not-closed", false},
		{"weird(name)(8192)", "weird(name)", true},
	}
	for _, test := range tests {
		base, suffix, ok := splitThinkingSuffix(test.model)
		if base != test.base || ok != test.ok {
			t.Errorf("splitThinkingSuffix(%q) = %q, %q, %v; want %q, ok=%v", test.model, base, suffix, ok, test.base, test.ok)
		}
	}
	if !strings.HasPrefix(claudeCloakPrefix, "claude-") {
		t.Fatal("cloak prefix changed unexpectedly")
	}
}
