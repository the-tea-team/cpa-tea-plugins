package quota

import (
	"testing"
	"time"
)

func scoredAccount(id string, capVal float64, sessionUsed float64, sessionReset time.Time) Account {
	a := acct(id, capVal, sessionUsed, sessionReset)
	a.Windows = append(a.Windows, Window{
		ID: "weekly", DurationSeconds: 604800, UsedFraction: ptr(sessionUsed),
		ResetAt: t0.Add(7 * 24 * time.Hour), ObservedAt: t0, Capacity: ptr(capVal), CapacitySource: "reported",
	})
	return a
}

func TestScoreUnknownIsNeutralNotCapacity(t *testing.T) {
	a := Account{ID: "a", Provider: "claude"}
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if sc.Value != 1 || sc.Known || sc.Exhausted {
		t.Fatalf("unknown must be neutral 1 with Known=false: %+v", sc)
	}
	if len(sc.Reasons) == 0 {
		t.Fatal("reasons required")
	}
}

func TestScoreStaleIsUnknownNeutral(t *testing.T) {
	a := scoredAccount("a", 5, 0, t0.Add(5*time.Hour))
	for i := range a.Windows {
		a.Windows[i].ObservedAt = t0.Add(-time.Hour)
	}
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if sc.Known || sc.Value != 1 {
		t.Fatalf("stale must not be treated as fresh full: %+v", sc)
	}
}

func TestScoreKnownExhaustedWindowCannotBeRescued(t *testing.T) {
	a := scoredAccount("a", 5, 1, t0.Add(5*time.Hour)) // session exhausted
	a.Windows[1].UsedFraction = ptr(0)                 // weekly full remaining
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if !sc.Exhausted || sc.Value != 0 || !sc.Known {
		t.Fatalf("exhausted session must zero score: %+v", sc)
	}
}

func TestScoreNearResetPreferredAtEqualFraction(t *testing.T) {
	near := AccountScore(scoredAccount("a", 1, 0.5, t0.Add(30*time.Minute)), "m", t0, 10*time.Minute)
	fresh := AccountScore(scoredAccount("b", 1, 0.5, t0.Add(5*time.Hour)), "m", t0, 10*time.Minute)
	if !near.Known || !fresh.Known {
		t.Fatalf("both known: %+v %+v", near, fresh)
	}
	if near.Value <= fresh.Value {
		t.Fatalf("near-reset should score higher: near=%v fresh=%v", near.Value, fresh.Value)
	}
}

func TestScoreFiveXBeatsOneXAtEqualFractionAndReset(t *testing.T) {
	one := AccountScore(scoredAccount("a", 1, 0.5, t0.Add(5*time.Hour)), "m", t0, 10*time.Minute)
	five := AccountScore(scoredAccount("b", 5, 0.5, t0.Add(5*time.Hour)), "m", t0, 10*time.Minute)
	if five.Value <= one.Value*4 {
		t.Fatalf("5x should dominate ~5x: 1x=%v 5x=%v", one.Value, five.Value)
	}
}

func TestScoreNearEmptySuppressedByHeadroomGuard(t *testing.T) {
	almostEmpty := AccountScore(scoredAccount("a", 1, 0.98, t0.Add(10*time.Minute)), "m", t0, 10*time.Minute)
	healthy := AccountScore(scoredAccount("b", 1, 0.5, t0.Add(10*time.Minute)), "m", t0, 10*time.Minute)
	if almostEmpty.Value >= healthy.Value {
		t.Fatalf("near-empty near-reset must not capture traffic: empty=%v healthy=%v", almostEmpty.Value, healthy.Value)
	}
}

func TestScoreUrgencyBounded(t *testing.T) {
	seconds := AccountScore(scoredAccount("a", 1, 0, t0.Add(5*time.Second)), "m", t0, 10*time.Minute)
	atFloor := AccountScore(scoredAccount("a", 1, 0, t0.Add(15*time.Minute)), "m", t0, 10*time.Minute) // 5% of 5h
	if seconds.Value != atFloor.Value {
		t.Fatalf("pace must cap at floor/cap: seconds=%v floor=%v", seconds.Value, atFloor.Value)
	}
	if seconds.Value > 4.01 {
		t.Fatalf("pace cap 4 exceeded for cap=1 full: %v", seconds.Value)
	}
}

func TestScoreModelScopedWindowOnlyAppliesToMatchingModel(t *testing.T) {
	a := scoredAccount("a", 1, 0.5, t0.Add(5*time.Hour))
	a.Windows = append(a.Windows, Window{
		ID: "opus-weekly", DurationSeconds: 604800, UsedFraction: ptr(1),
		ResetAt: t0.Add(24 * time.Hour), ObservedAt: t0, Capacity: ptr(1),
		CapacitySource: "reported", Models: []string{"claude-opus*"},
	})
	opus := AccountScore(a, "claude-opus-4", t0, 10*time.Minute)
	sonnet := AccountScore(a, "claude-sonnet-4", t0, 10*time.Minute)
	if !opus.Exhausted {
		t.Fatalf("opus scoped exhaustion missed: %+v", opus)
	}
	if sonnet.Exhausted || !sonnet.Known || sonnet.Value <= 0 {
		t.Fatalf("sonnet must be unaffected by opus scope: %+v", sonnet)
	}
}

func TestScorePastResetIgnoredNotAssumedReplenished(t *testing.T) {
	a := scoredAccount("a", 1, 0, t0.Add(-time.Minute))
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	// weekly still fresh/known, session ignored
	if !sc.Known {
		t.Fatalf("weekly known should keep Known: %+v", sc)
	}
	// session alone past reset -> unknown
	a.Windows = a.Windows[:1]
	sc = AccountScore(a, "m", t0, 10*time.Minute)
	if sc.Known || sc.Value != 1 {
		t.Fatalf("past-reset only must be unknown neutral: %+v", sc)
	}
}

func TestScoreUnknownCapacityIsNeutralEvenWithUsage(t *testing.T) {
	a := scoredAccount("a", 1, 0.5, t0.Add(time.Hour))
	for i := range a.Windows {
		a.Windows[i].Capacity = nil
	}
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if sc.Known || sc.Value != 1 {
		t.Fatalf("unknown capacity cannot imply real 1x: %+v", sc)
	}
}

// --- PoolCredentials ---

func credSnapshot() Snapshot {
	s := baseSnapshot()
	s.Credentials = []Credential{
		{ID: "cred-b", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-*"}, Priority: 5},
		{ID: "cred-a", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-sonnet"}, Priority: 10},
		{ID: "cred-off", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-sonnet"}, Priority: 99, Disabled: true},
		{ID: "cred-cool", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-sonnet"}, Priority: 98, NextRetryAt: t0.Add(time.Hour)},
		{ID: "cred-unavail", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-sonnet"}, Priority: 97, Unavailable: true},
	}
	s.Pools = []Pool{{ID: "p", Provider: "claude", CredentialIDs: []string{"cred-b", "cred-a", "cred-off", "cred-cool", "cred-unavail"}}}
	return s
}

func ids(cs []Credential) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func TestPoolCredentialsFiltersAndSortsDeterministically(t *testing.T) {
	got := ids(PoolCredentials(credSnapshot(), "p", "claude-sonnet", t0))
	want := []string{"cred-a", "cred-b"} // priority desc, disabled/cooldown/unavailable excluded, wildcard matches
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPoolCredentialsEmptyModelNoFilter(t *testing.T) {
	got := ids(PoolCredentials(credSnapshot(), "p", "", t0))
	if len(got) != 2 {
		t.Fatalf("empty model should keep usable creds: %v", got)
	}
}

func TestPoolCredentialsModelMismatchExcluded(t *testing.T) {
	got := PoolCredentials(credSnapshot(), "p", "gpt-5", t0)
	if len(got) != 0 {
		t.Fatalf("model mismatch must exclude: %v", ids(got))
	}
}

func TestPoolCredentialsUnknownPoolFailsClosed(t *testing.T) {
	if got := PoolCredentials(credSnapshot(), "nope", "claude-sonnet", t0); len(got) != 0 {
		t.Fatalf("unknown pool must be empty: %v", ids(got))
	}
}

func TestPoolCredentialsUnresolvedMembershipFailsClosed(t *testing.T) {
	s := credSnapshot()
	s.Pools[0].CredentialIDs = []string{"ghost", "cred-a"}
	got := ids(PoolCredentials(s, "p", "claude-sonnet", t0))
	if len(got) != 1 || got[0] != "cred-a" {
		t.Fatalf("unresolved member must not widen pool: %v", got)
	}
}

func TestPoolCredentialsRetryElapsedIncluded(t *testing.T) {
	s := credSnapshot()
	s.Credentials[3].NextRetryAt = t0.Add(-time.Minute)
	got := ids(PoolCredentials(s, "p", "claude-sonnet", t0))
	found := false
	for _, id := range got {
		if id == "cred-cool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("elapsed retry should be included: %v", got)
	}
}
