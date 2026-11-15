package adapters

import (
	"fmt"
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func TestClaudeOver100Exhausted(t *testing.T) {
	for _, value := range []string{"103", "100.4"} {
		t.Run(value, func(t *testing.T) {
			a, err := Claude([]byte(fmt.Sprintf(`{"five_hour":{"utilization":%s}}`, value)), nil, obs)
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Windows) != 1 || a.Windows[0].UsedFraction == nil || *a.Windows[0].UsedFraction != 1 {
				t.Fatalf("window = %+v", a.Windows)
			}
			if !quota.AccountScore(a, "claude-sonnet-4", obs, time.Hour).Exhausted {
				t.Fatal("expected exhausted")
			}
		})
	}
}

func TestClaudeInvalidPercentUnknown(t *testing.T) {
	for _, value := range []string{`-1`, `"NaN"`, `"+Inf"`, `"-Inf"`} {
		t.Run(value, func(t *testing.T) {
			a, err := Claude([]byte(fmt.Sprintf(`{"five_hour":{"utilization":%s}}`, value)), nil, obs)
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Windows) != 1 || a.Windows[0].UsedFraction != nil {
				t.Fatalf("invalid percent = %+v", a.Windows)
			}
		})
	}
}

func TestClaudeDeclaredScopesNeverBecomeGlobal(t *testing.T) {
	for _, fields := range []string{
		`"app":"cowork"`, `"surface":"cowork"`, `"scope":null,"app":"cowork"`,
		`"scope":null,"surface":"cowork"`, `"scope":null`, `"scope":{}`,
		`"app":null`, `"surface":{"id":"cowork"}`, `"app_id":"cowork"`, `"surface_id":"cowork"`,
	} {
		t.Run(fields, func(t *testing.T) {
			payload := fmt.Sprintf(`{"limits":[{"kind":"weekly","group":"weekly","percent":100,%s}]}`, fields)
			a, err := Claude([]byte(payload), nil, obs)
			if err != nil {
				t.Fatal(err)
			}
			if quota.AccountScore(a, "claude-sonnet-4", obs, time.Hour).Exhausted {
				t.Fatalf("declared scope exhausted ordinary model: %+v", a.Windows)
			}
		})
	}
}

func TestClaudeUnsafeModelIDsDoNotExhaust(t *testing.T) {
	for _, id := range []string{"*", "claude-*", "claude-?", "claude-[ab]", "claude model", " claude", "claude\u00a0model"} {
		t.Run(id, func(t *testing.T) {
			payload := fmt.Sprintf(`{"limits":[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"id":%q}}}]}`, id)
			a, err := Claude([]byte(payload), nil, obs)
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Windows) != 1 || !IsUnmatchedScope(a.Windows[0].Models) {
				t.Fatalf("unsafe ID window = %+v", a.Windows)
			}
			if quota.AccountScore(a, "claude-sonnet-4", obs, time.Hour).Exhausted {
				t.Fatal("unsafe ID exhausted ordinary model")
			}
		})
	}
}
