package quota

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// --- (1) exhaustion independent of capacity; unknown-capacity guard ---

func TestScoreFreshFullyConsumedUnknownCapacityIsExhausted(t *testing.T) {
	a := Account{ID: "a", Provider: "claude", Windows: []Window{{
		ID: "session", DurationSeconds: 18000, UsedFraction: ptr(1),
		ResetAt: t0.Add(time.Hour), ObservedAt: t0, // Capacity nil
	}}}
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if !sc.Exhausted || sc.Value != 0 || !sc.Known {
		t.Fatalf("fresh 100%% consumed with nil capacity must be Exhausted: %+v", sc)
	}
}

func TestScoreUnknownCapacityExhaustedWindowCannotBeRescuedByKnownWindow(t *testing.T) {
	a := scoredAccount("a", 5, 0, t0.Add(5*time.Hour)) // both windows 100% remaining, cap 5
	a.Windows[0].Capacity = nil                        // session: capacity unknown
	a.Windows[0].UsedFraction = ptr(1)                 // session: fully consumed
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if !sc.Exhausted || sc.Value != 0 {
		t.Fatalf("exhausted unknown-capacity window must zero the score: %+v", sc)
	}
}

func TestScoreNearEmptyUnknownCapacityWindowConstrainsKnownWindows(t *testing.T) {
	reset := t0.Add(5 * time.Hour)
	known := Account{ID: "k", Provider: "claude", Windows: []Window{{
		ID: "session", DurationSeconds: 18000, UsedFraction: ptr(0.5),
		ResetAt: reset, ObservedAt: t0, Capacity: ptr(1), CapacitySource: "reported",
	}}}
	base := AccountScore(known, "m", t0, 10*time.Minute)

	withUnknown := known
	withUnknown.Windows = append(append([]Window(nil), known.Windows...), Window{
		ID: "weekly", DurationSeconds: 604800, UsedFraction: ptr(0.95), // 5% left, capacity unknown
		ResetAt: t0.Add(7 * 24 * time.Hour), ObservedAt: t0,
	})
	got := AccountScore(withUnknown, "m", t0, 10*time.Minute)
	if !got.Known || got.Exhausted {
		t.Fatalf("known window remains scoreable: %+v", got)
	}
	want := base.Value * 0.5 // guard = min(1, 0.05/0.10)
	if math.Abs(got.Value-want) > 1e-9 {
		t.Fatalf("near-empty unknown-capacity window must apply headroom guard: got %v want %v (base %v)", got.Value, want, base.Value)
	}
}

func TestScoreOnlyUnknownCapacityStaysNeutralUnknown(t *testing.T) {
	a := Account{ID: "a", Provider: "claude", Windows: []Window{{
		ID: "session", DurationSeconds: 18000, UsedFraction: ptr(0.5),
		ResetAt: t0.Add(time.Hour), ObservedAt: t0,
	}}}
	sc := AccountScore(a, "m", t0, 10*time.Minute)
	if sc.Known || sc.Exhausted || sc.Value != 1 {
		t.Fatalf("healthy unknown-capacity only must stay neutral: %+v", sc)
	}
}

// --- (2) PoolCredentials: unknown support is not wildcard ---

func TestPoolCredentialsEmptyCredentialModelsFailClosedForNamedModel(t *testing.T) {
	s := baseSnapshot()
	s.Credentials = []Credential{
		{ID: "cred-open", AccountID: "acct-1", Provider: "claude", Models: nil, Priority: 1},
		{ID: "cred-ok", AccountID: "acct-1", Provider: "claude", Models: []string{"claude-sonnet"}, Priority: 1},
	}
	s.Pools = []Pool{{ID: "p", Provider: "claude", CredentialIDs: []string{"cred-open", "cred-ok"}}}
	if got := ids(PoolCredentials(s, "p", "claude-sonnet", t0)); !reflect.DeepEqual(got, []string{"cred-ok"}) {
		t.Fatalf("empty credential.Models must not support a named model: %v", got)
	}
	if got := ids(PoolCredentials(s, "p", "", t0)); !reflect.DeepEqual(got, []string{"cred-ok", "cred-open"}) {
		t.Fatalf("empty requested model means no model filter: %v", got)
	}
}

// --- (3) canonical model-scope ordering ---

func TestValidateRejectsDuplicateWindowWithPermutedModelScope(t *testing.T) {
	s := baseSnapshot()
	w1 := s.Accounts[0].Windows[0]
	w1.Models = []string{"a", "b"}
	w2 := w1
	w2.Models = []string{"b", "a"}
	s.Accounts[0].Windows = []Window{w1, w2}
	if err := Validate(s); err == nil {
		t.Fatal("same window id with permuted model scope is a duplicate")
	}
}

func TestSummarizeGroupsPermutedModelScopesAndDoesNotMutateInput(t *testing.T) {
	s := twoAccountSnapshot()
	s.Accounts[0].Windows[0].Models = []string{"b", "a"}
	s.Accounts[1].Windows[0].Models = []string{"a", "b"}
	rows := Summarize(s, t0, 10*time.Minute)
	if len(rows) != 1 {
		t.Fatalf("permuted identical scopes must share one row, got %d: %+v", len(rows), rows)
	}
	if rows[0].MeasuredAccounts != 2 || rows[0].WindowID != "session|a,b" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
	if !reflect.DeepEqual(s.Accounts[0].Windows[0].Models, []string{"b", "a"}) ||
		!reflect.DeepEqual(s.Accounts[1].Windows[0].Models, []string{"a", "b"}) {
		t.Fatalf("caller slices mutated: %v %v", s.Accounts[0].Windows[0].Models, s.Accounts[1].Windows[0].Models)
	}
}

func TestValidateDoesNotMutateModelScopeSlices(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows[0].Models = []string{"z", "a"}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Accounts[0].Windows[0].Models, []string{"z", "a"}) {
		t.Fatalf("Validate mutated caller slice: %v", s.Accounts[0].Windows[0].Models)
	}
}

// --- (4) explicit pool IDs respect Pool.Provider ---

func crossProviderSnapshot() Snapshot {
	s := baseSnapshot()
	s.Accounts = append(s.Accounts, Account{ID: "acct-o", Provider: "openai", BillingType: "subscription"})
	s.Credentials = append(s.Credentials, Credential{ID: "cred-o", AccountID: "acct-o", Provider: "openai", Models: []string{"claude-sonnet"}})
	s.Pools[0].CredentialIDs = []string{"cred-1", "cred-o"}
	return s
}

func TestValidateRejectsCrossProviderExplicitPoolReference(t *testing.T) {
	if err := Validate(crossProviderSnapshot()); err == nil {
		t.Fatal("pool claude referencing openai credential must be rejected")
	}
}

func TestPoolCredentialsAndSummarizeFilterCrossProviderExplicitIDs(t *testing.T) {
	s := crossProviderSnapshot() // intentionally unvalidated: resolver must still fail closed
	if got := ids(PoolCredentials(s, "claude-sub", "claude-sonnet", t0)); !reflect.DeepEqual(got, []string{"cred-1"}) {
		t.Fatalf("cross-provider credential leaked into pool: %v", got)
	}
	rows := Summarize(s, t0, 10*time.Minute)
	if len(rows) != 1 || rows[0].TotalAccounts != 1 {
		t.Fatalf("cross-provider account counted in pool summary: %+v", rows)
	}
}

// --- (5) ReadSnapshot rejects trailing JSON ---

func TestReadSnapshotRejectsTrailingJSONValues(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := WriteSnapshot(good, baseSnapshot()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(good); err != nil {
		t.Fatalf("control read: %v", err)
	}
	cases := map[string]string{
		"second object": string(raw) + string(raw),
		"scalar":        string(raw) + " 1",
		"garbage":       string(raw) + " garbage",
		"empty object":  string(raw) + "\n{}",
		"closing brace": string(raw) + "}",
	}
	for name, body := range cases {
		p := filepath.Join(dir, "t.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSnapshot(p); err == nil {
			t.Errorf("%s: trailing data accepted", name)
		}
	}
	// trailing whitespace alone stays valid
	p := filepath.Join(dir, "ws.json")
	if err := os.WriteFile(p, append(raw, " \n\t\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(p); err != nil {
		t.Fatalf("trailing whitespace must be accepted: %v", err)
	}
}
