package quota

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)

func ptr(f float64) *float64 { return &f }

func baseSnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: SchemaVersion,
		Revision:      1,
		GeneratedAt:   t0,
		Accounts: []Account{{
			ID: "acct-1", Provider: "claude", Label: "One", BillingType: "subscription",
			Plan: "1x", PlanSource: "reported", RefreshStatus: "ok", ObservedAt: t0,
			Windows: []Window{{
				ID: "session", DurationSeconds: 18000, UsedFraction: ptr(0.2),
				ResetAt: t0.Add(5 * time.Hour), ObservedAt: t0,
				Capacity: ptr(1), CapacitySource: "reported",
			}},
		}},
		Credentials: []Credential{{
			ID: "cred-1", AccountID: "acct-1", Provider: "claude",
			Models: []string{"claude-sonnet"}, Priority: 10,
		}},
		Pools: []Pool{{
			ID: "claude-sub", Provider: "claude", BillingType: "subscription",
			CredentialIDs: []string{"cred-1"},
		}},
	}
}

func TestValidateAcceptsValidSnapshot(t *testing.T) {
	if err := Validate(baseSnapshot()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejectsWrongSchema(t *testing.T) {
	s := baseSnapshot()
	s.SchemaVersion = 99
	if err := Validate(s); err == nil {
		t.Fatal("expected schema error")
	}
}

func TestValidateRejectsDuplicateAccountID(t *testing.T) {
	s := baseSnapshot()
	s.Accounts = append(s.Accounts, s.Accounts[0])
	if err := Validate(s); err == nil {
		t.Fatal("expected duplicate account error")
	}
}

func TestValidateRejectsDuplicateCredentialID(t *testing.T) {
	s := baseSnapshot()
	s.Credentials = append(s.Credentials, s.Credentials[0])
	if err := Validate(s); err == nil {
		t.Fatal("expected duplicate credential error")
	}
}

func TestValidateRejectsDuplicatePoolID(t *testing.T) {
	s := baseSnapshot()
	s.Pools = append(s.Pools, s.Pools[0])
	if err := Validate(s); err == nil {
		t.Fatal("expected duplicate pool error")
	}
}

func TestValidateRejectsUnknownCredentialReference(t *testing.T) {
	s := baseSnapshot()
	s.Pools[0].CredentialIDs = []string{"missing"}
	if err := Validate(s); err == nil {
		t.Fatal("expected unknown credential reference error (fail-closed membership depends on it)")
	}
}

func TestValidateRejectsUnknownAccountReference(t *testing.T) {
	s := baseSnapshot()
	s.Credentials[0].AccountID = "missing"
	if err := Validate(s); err == nil {
		t.Fatal("expected unknown account reference error")
	}
}

func TestValidateRejectsEmptyProvider(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Provider = ""
	if err := Validate(s); err == nil {
		t.Fatal("expected empty provider error")
	}
}

func TestValidateRejectsNonFiniteUsedFraction(t *testing.T) {
	s := baseSnapshot()
	nan := math.NaN()
	s.Accounts[0].Windows[0].UsedFraction = &nan
	if err := Validate(s); err == nil {
		t.Fatal("expected non-finite error")
	}
}

func TestValidateRejectsFractionOutOfRange(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows[0].UsedFraction = ptr(1.5)
	if err := Validate(s); err == nil {
		t.Fatal("expected range error")
	}
}

func TestValidateRejectsNegativeDuration(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows[0].DurationSeconds = -1
	if err := Validate(s); err == nil {
		t.Fatal("expected duration error")
	}
}

func TestValidateRejectsBadFinancialKindAndNonFiniteAmount(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Financial = []Financial{{Kind: "made-up", Amount: 1}}
	if err := Validate(s); err == nil {
		t.Fatal("expected financial kind error")
	}
	s.Accounts[0].Financial = []Financial{{Kind: "prepaid-balance", Amount: math.Inf(1)}}
	if err := Validate(s); err == nil {
		t.Fatal("expected non-finite amount error")
	}
}

func TestValidateRejectsNegativeCapacityAndDuplicateWindowID(t *testing.T) {
	s := baseSnapshot()
	s.Accounts[0].Windows[0].Capacity = ptr(-1)
	if err := Validate(s); err == nil {
		t.Fatal("expected capacity error")
	}
	s = baseSnapshot()
	s.Accounts[0].Windows = append(s.Accounts[0].Windows, s.Accounts[0].Windows[0])
	if err := Validate(s); err == nil {
		t.Fatal("expected duplicate window error")
	}
}

func TestUnknownPlanRemainsUnknown(t *testing.T) {
	s := baseSnapshot()
	// Generic Max must NOT become a capacity weighting.
	s.Accounts[0].Plan = "plan_max"
	s.Accounts[0].PlanSource = "reported"
	s.Accounts[0].Windows[0].Capacity = nil
	sums := Summarize(s, t0, 10*time.Minute)
	if len(sums) == 0 {
		t.Fatal("expected pool summary row even when measurement missing")
	}
	row := sums[0]
	if row.RemainingFraction != nil {
		t.Fatalf("unknown capacity must not fabricate remaining fraction, got %v", *row.RemainingFraction)
	}
	if row.Estimated {
		t.Fatal("unknown plan must not be marked estimated capacity")
	}
	if row.TotalAccounts != 1 || row.MeasuredAccounts != 0 || row.UnknownAccounts != 1 {
		t.Fatalf("coverage counts wrong: %+v", row)
	}
	if row.TotalCapacity != 0 || row.MeasuredCapacity != 0 {
		t.Fatalf("unknown capacity must stay 0/unknown, got %+v", row)
	}
}
