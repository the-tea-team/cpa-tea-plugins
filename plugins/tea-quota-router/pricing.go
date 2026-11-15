package main

import (
	"math"
	"strings"
)

// Estimate only from configured rates. Output includes reasoning tokens in the
// CPA SDK accounting breakdown, so reasoning must not be added a second time.
func estimateCost(r usageRow, prices map[string]Price) *float64 {
	p, ok := prices[r.Provider+":"+r.Model]
	if !ok {
		p, ok = prices[r.Model]
	}
	if !ok {
		return nil
	}
	input := r.InputTokens
	switch strings.ToLower(r.Provider) {
	case "claude":
	case "codex", "openai":
		if r.uncachedKnown {
			input = r.uncachedTokens
		} else {
			input -= r.CacheReadTokens
			if input < 0 {
				input = 0
			}
		}
	default:
		return nil
	}
	if input < 0 || r.OutputTokens < 0 || r.CacheReadTokens < 0 {
		return nil
	}
	total := 0.0
	for _, x := range []struct {
		n    int64
		rate *float64
	}{{input, p.Input}, {r.OutputTokens, p.Output}, {r.CacheReadTokens, p.CacheRead}} {
		if x.n == 0 {
			continue
		}
		if x.rate == nil || *x.rate < 0 || math.IsNaN(*x.rate) || math.IsInf(*x.rate, 0) {
			return nil
		}
		total += float64(x.n) * *x.rate / 1e6
	}
	if math.IsInf(total, 0) || math.IsNaN(total) {
		return nil
	}
	return &total
}
