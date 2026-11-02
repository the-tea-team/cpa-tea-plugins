package main

import (
	"strings"
)

// claudeCloakPrefix mirrors the prefix CPA uses in Anthropic model listings
// (internal/client/claude/models). The remainder is the real model ID reversed.
const claudeCloakPrefix = "claude-fable-5-dd-"

// modelCandidates returns every client-visible form of a model that an allowlist
// pattern may match. A request may carry a thinking suffix or a cloaked Claude ID;
// a model-list entry may carry a "models/" prefix.
func modelCandidates(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	out := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}

	if uncloaked, ok := uncloakClaudeModel(model); ok {
		// Replace the cloaked form, do not add it as an alternative. Every cloaked
		// ID starts with "claude-", so a "claude-*" allowlist would otherwise match
		// every model in an Anthropic model list.
		add(uncloaked)
		if base, _, hasSuffix := splitThinkingSuffix(uncloaked); hasSuffix {
			add(base)
		}
		return out
	}
	add(model)
	if base, _, ok := splitThinkingSuffix(model); ok {
		add(base)
	}
	if strings.HasPrefix(model, "models/") {
		add(strings.TrimPrefix(model, "models/"))
	}
	return out
}

// splitThinkingSuffix mirrors CPA's thinking.ParseSuffix: a trailing "(value)"
// is treated as a client-side model option, not part of the model name.
func splitThinkingSuffix(model string) (base, suffix string, ok bool) {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen < 0 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:lastOpen], model[lastOpen+1 : len(model)-1], true
}

func uncloakClaudeModel(model string) (string, bool) {
	base, suffix, hasSuffix := splitThinkingSuffix(model)
	if !strings.HasPrefix(base, claudeCloakPrefix) {
		return "", false
	}
	encoded := base[len(claudeCloakPrefix):]
	if encoded == "" {
		return "", false
	}
	resolved := reverseRunes(encoded)
	if hasSuffix {
		resolved += "(" + suffix + ")"
	}
	return resolved, true
}

func reverseRunes(value string) string {
	runes := []rune(value)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

// globMatch matches value against a pattern where '*' matches any sequence
// (including '/') and '?' matches exactly one rune. Matching is case-sensitive.
func globMatch(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == value
	}

	p := []rune(pattern)
	v := []rune(value)
	// dp[j] reports whether the pattern prefix processed so far matches value[:j].
	dp := make([]bool, len(v)+1)
	dp[0] = true
	for _, token := range p {
		next := make([]bool, len(v)+1)
		switch token {
		case '*':
			next[0] = dp[0]
			for j := 1; j <= len(v); j++ {
				next[j] = dp[j] || next[j-1]
			}
		case '?':
			for j := 1; j <= len(v); j++ {
				next[j] = dp[j-1]
			}
		default:
			for j := 1; j <= len(v); j++ {
				next[j] = dp[j-1] && v[j-1] == token
			}
		}
		dp = next
	}
	return dp[len(v)]
}
