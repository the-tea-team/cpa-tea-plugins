package adapters

import (
	"testing"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

func TestClaudeAppWindowsNeverExhaustModels(t *testing.T) {
	for _, key := range []string{"seven_day_cowork", "seven_day_oauth_apps"} {
		t.Run(key, func(t *testing.T) {
			a, err := Claude([]byte(`{"`+key+`":{"utilization":100}}`), nil, obs)
			if err != nil {
				t.Fatal(err)
			}
			if len(a.Windows) != 1 || !IsUnmatchedScope(a.Windows[0].Models) {
				t.Fatalf("not unmatched scope: %+v", a.Windows)
			}
			if sc := quota.AccountScore(a, "claude-sonnet-4", obs, time.Hour); sc.Exhausted {
				t.Fatalf("app limit exhausted model: %+v", sc)
			}
		})
	}
	a, err := Claude([]byte(`{"seven_day":{"utilization":100}}`), nil, obs)
	if err != nil {
		t.Fatal(err)
	}
	if sc := quota.AccountScore(a, "claude-sonnet-4", obs, time.Hour); !sc.Exhausted {
		t.Fatalf("global limit not exhausted: %+v", sc)
	}
}
