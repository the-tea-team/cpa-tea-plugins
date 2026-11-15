package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"github.com/the-tea-team/cpa-tea-plugins/plugins/tea-quota-router/adapters"
)

// ---- fixtures ----------------------------------------------------------------

func testInventory() InventoryFunc {
	list := []pluginapi.HostAuthFileEntry{
		{ID: "claude-main.json", AuthIndex: "ai-claude-1", Provider: "claude", Label: "Claude Max", AccountType: "oauth", Email: "person@example.invalid", Path: "/secret/claude-main.json"},
		{ID: "codex-a.json", AuthIndex: "ai-codex-1", Provider: "codex", Label: "Codex A", AccountType: "oauth"},
		{ID: "codex-b.json", AuthIndex: "ai-codex-2", Provider: "codex", Label: "Codex B", AccountType: "oauth"},
		{ID: "claude-apikey.json", AuthIndex: "ai-claude-key", Provider: "claude", Label: "Claude API key", AccountType: "api_key"},
	}
	models := []pluginapi.HostAuthModels{
		{ID: "claude-main.json", AuthIndex: "ai-claude-1", Provider: "claude", Models: []string{"claude-sonnet-4"}},
		{ID: "openrouter-cfg", AuthIndex: "ai-or-1", Provider: "openrouter", Models: []string{"openrouter/auto"}}, // not in list
	}
	return func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return list, models, nil
	}
}

type harness struct {
	t   *testing.T
	clk *fakeClock
	m   *mockMgmt
	col *collector
	cfg Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := &fakeClock{t: t0}
	m := newMockMgmt(t, standardResponder(clk))
	col := newCollector(collectorDeps{now: clk.now, sleep: func(context.Context, time.Duration) error { return nil }})
	t.Cleanup(func() {
		if col.mc != nil {
			col.mc.close()
		}
	})
	return &harness{t: t, clk: clk, m: m, col: col, cfg: Config{
		DataDir: "/unused", ManagementURL: m.srv.URL, ManagementKeyEnv: testMgmtEnv,
		PollIntervalSeconds: 180, MaxAgeSeconds: 600,
	}}
}

func (h *harness) run(prev quota.Snapshot, inv InventoryFunc) (quota.Snapshot, collectorHealth, error) {
	h.t.Helper()
	return h.col.collect(context.Background(), h.cfg, prev, inv)
}

func acctByID(t *testing.T, s quota.Snapshot, id string) quota.Account {
	t.Helper()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q missing: %v", id, accountIDs(s))
	return quota.Account{}
}

func accountIDs(s quota.Snapshot) []string {
	var ids []string
	for _, a := range s.Accounts {
		ids = append(ids, a.ID)
	}
	return ids
}

func credByID(t *testing.T, s quota.Snapshot, id string) quota.Credential {
	t.Helper()
	for _, c := range s.Credentials {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("credential %q missing", id)
	return quota.Credential{}
}

func winByID(a quota.Account, id string) (quota.Window, bool) {
	for _, w := range a.Windows {
		if w.ID == id && len(w.Models) == 0 {
			return w, true
		}
	}
	return quota.Window{}, false
}

func setupFor(h collectorHealth, provider string) []setupItem {
	var out []setupItem
	for _, s := range h.Setup {
		if s.Provider == provider {
			out = append(out, s)
		}
	}
	return out
}

// ---- end-to-end pass ---------------------------------------------------------

func TestCollectMixedOAuthAndAPIEntries(t *testing.T) {
	h := newHarness(t)
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.Validate(snap); err != nil {
		t.Fatalf("collected snapshot invalid: %v", err)
	}
	if health.Status != healthOK || health.LastError != "" || !health.LastRefreshAt.Equal(t0) {
		t.Fatalf("health = %+v", health)
	}
	if snap.Revision != 1 || !snap.GeneratedAt.Equal(t0) {
		t.Fatalf("revision/generatedAt = %d %v", snap.Revision, snap.GeneratedAt)
	}

	// Claude OAuth: usage + profile through the fixed endpoints with the beta header.
	cl := acctByID(t, snap, "claude-main.json")
	if cl.Plan != "max5" || cl.PlanSource != "reported" || cl.BillingType != "subscription" || cl.RefreshStatus != refreshOK || cl.Label != "Claude Max" {
		t.Fatalf("claude = %+v", cl)
	}
	fh, ok := winByID(cl, "five-hour")
	if !ok || fh.UsedFraction == nil || *fh.UsedFraction != 0.4 || fh.Capacity == nil || *fh.Capacity != 5 || fh.CapacitySource != "plan_estimate" {
		t.Fatalf("claude five-hour = %+v", fh)
	}
	// Codex OAuth entries are separate accounts, no capacity invented.
	for _, id := range []string{"codex-a.json", "codex-b.json"} {
		a := acctByID(t, snap, id)
		if a.Plan != "plus" || a.RefreshStatus != refreshOK {
			t.Fatalf("%s = %+v", id, a)
		}
		w, ok := winByID(a, "five-hour")
		if !ok || w.UsedFraction == nil || w.Capacity != nil {
			t.Fatalf("%s five-hour = %+v", id, w)
		}
	}
	if w, _ := winByID(acctByID(t, snap, "codex-a.json"), "five-hour"); *w.UsedFraction != 0.31 {
		t.Fatalf("codex-a fraction = %v", *w.UsedFraction)
	}
	if w, _ := winByID(acctByID(t, snap, "codex-b.json"), "five-hour"); *w.UsedFraction != 0.2 {
		t.Fatalf("codex-b fraction = %v", *w.UsedFraction)
	}
	// OpenRouter exists only in host.auth.models (config key): kept, collected, unknown label.
	or := acctByID(t, snap, "openrouter-cfg")
	if or.Provider != "openrouter" || or.BillingType != "api" || or.Label != unknownLabel || or.RefreshStatus != refreshOK {
		t.Fatalf("openrouter = %+v", or)
	}
	kinds := map[string]float64{}
	for _, f := range or.Financial {
		kinds[f.Kind+"@"+f.ScopeID] = f.Amount
	}
	if kinds["key-budget-remaining@key"] != 74.5 || kinds["upstream-spend@key"] != 25.5 {
		t.Fatalf("openrouter financial = %+v", or.Financial)
	}
	// API-key Claude entry is not queried (OAuth-only endpoint) and not claimed healthy.
	ak := acctByID(t, snap, "claude-apikey.json")
	if ak.RefreshStatus != refreshUnsupported || len(ak.Windows) != 0 || ak.ObservedAt != (time.Time{}) {
		t.Fatalf("api-key claude = %+v", ak)
	}
	if h.m.countFor("ai-claude-key") != 0 {
		t.Fatal("API-key credential was sent to an OAuth-only endpoint")
	}
	if len(setupFor(health, "claude")) == 0 {
		t.Fatalf("api-key claude not reported in setup: %+v", health.Setup)
	}
	// credential mapping
	if c := credByID(t, snap, "codex-a.json"); c.AccountID != "codex-a.json" || c.AuthIndex != "ai-codex-1" {
		t.Fatalf("cred = %+v", c)
	}
	if c := credByID(t, snap, "openrouter-cfg"); c.AccountID != "openrouter-cfg" || len(c.Models) != 1 {
		t.Fatalf("models-only cred = %+v", c)
	}
	// no raw identity anywhere
	b, _ := json.Marshal(snap)
	for _, leak := range []string{"example.invalid", "/secret/", "synthetic-uuid", "synthetic-org", "sk-or-v1", "synthetic-user", "synthetic-account"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("snapshot leaks %q", leak)
		}
	}
	// exactly the fixed fetches happened
	if h.m.countFor("ai-claude-1") != 2 || h.m.countFor("ai-codex-1") != 1 || h.m.countFor("ai-or-1") != 1 {
		t.Fatalf("calls = %+v", h.m.snapshotCalls())
	}
}

func TestCollectSerializedSnapshotRoundTrips(t *testing.T) {
	h := newHarness(t)
	snap, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/snapshot.json"
	if err := quota.WriteSnapshot(path, snap); err != nil {
		t.Fatal(err)
	}
	back, err := quota.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Accounts) != len(snap.Accounts) || len(back.Credentials) != len(snap.Credentials) {
		t.Fatal("round trip changed shape")
	}
}

func TestCollectOverridesDedupAndPools(t *testing.T) {
	h := newHarness(t)
	h.cfg.AccountOverrides = map[string]AccountOverride{
		// both Codex entries are the same quota account (group), A carries label/capacity
		"codex-a.json":   {AccountID: "codex-team", Label: "Codex Team", Plan: "pro", CapacityByWindow: map[string]float64{"five-hour": 10}},
		"codex-b.json":   {AccountID: "codex-team"},
		"openrouter-cfg": {Label: "OR gateway", BillingType: "prepaid"},
	}
	h.cfg.Pools = []quota.Pool{
		{ID: "codex-explicit", Label: "Codex explicit", Provider: "codex", CredentialIDs: []string{"codex-a.json", "codex-b.json", "claude-main.json", "ghost"}},
		{ID: "ghosts", Provider: "codex", CredentialIDs: []string{"nobody"}},
		{ID: "by-billing", Label: "OR billing", Provider: "openrouter", BillingType: "prepaid"},
	}
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.Validate(snap); err != nil {
		t.Fatal(err)
	}
	team := acctByID(t, snap, "codex-team")
	if team.Label != "Codex Team" || team.Plan != "pro" || team.PlanSource != "operator_override" {
		t.Fatalf("override not applied to collected account: %+v", team)
	}
	w, _ := winByID(team, "five-hour")
	if w.Capacity == nil || *w.Capacity != 10 || w.CapacitySource != "operator_override" || w.UsedFraction == nil {
		t.Fatalf("capacity override = %+v", w)
	}
	if sd, _ := winByID(team, "seven-day"); sd.Capacity != nil {
		t.Fatalf("override leaked to another window: %+v", sd)
	}
	for _, a := range snap.Accounts {
		if a.ID == "codex-a.json" || a.ID == "codex-b.json" {
			t.Fatalf("grouped credential kept a separate account: %v", accountIDs(snap))
		}
	}
	if credByID(t, snap, "codex-a.json").AccountID != "codex-team" || credByID(t, snap, "codex-b.json").AccountID != "codex-team" {
		t.Fatal("grouped credentials not mapped to shared account")
	}
	// Only one upstream fetch for the grouped account (first usable credential succeeded).
	if h.m.countFor("ai-codex-1")+h.m.countFor("ai-codex-2") != 1 {
		t.Fatalf("grouped account fetched more than once: %+v", h.m.snapshotCalls())
	}
	if or := acctByID(t, snap, "openrouter-cfg"); or.BillingType != "prepaid" || or.Label != "OR gateway" {
		t.Fatalf("openrouter override = %+v", or)
	}

	pools := map[string]quota.Pool{}
	for _, p := range snap.Pools {
		pools[p.ID] = p
	}
	for _, id := range []string{"auto-claude-subscription", "auto-codex-subscription", "auto-openrouter-prepaid", "codex-explicit", "by-billing"} {
		if _, ok := pools[id]; !ok {
			t.Fatalf("pool %q missing: %+v", id, snap.Pools)
		}
	}
	if _, ok := pools["ghosts"]; ok {
		t.Fatal("pool with no resolvable members must be omitted")
	}
	// the explicit pool dropped the cross-provider and unknown members (fail closed)
	if got := strings.Join(pools["codex-explicit"].CredentialIDs, ","); got != "codex-a.json,codex-b.json" {
		t.Fatalf("explicit members = %s", got)
	}
	if len(setupFor(health, "pools")) < 2 {
		t.Fatalf("pool problems not reported: %+v", health.Setup)
	}
	// Summaries: grouped duplicate account counts once.
	for _, ps := range quota.Summarize(snap, t0, 10*time.Minute) {
		if ps.ID == "auto-codex-subscription" && ps.TotalAccounts != 1 {
			t.Fatalf("dedup failed in pool summary: %+v", ps)
		}
	}
}

func TestCollectOverrideAppliesToPreservedObservationAndDroppedOverrideClears(t *testing.T) {
	h := newHarness(t)
	h.cfg.AccountOverrides = map[string]AccountOverride{"codex-a.json": {CapacityByWindow: map[string]float64{"five-hour": 10}}}
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.setOverride("ai-codex-1", upstreamStatus(500))
	h.clk.advance(3 * time.Minute)
	// override changes while the source is failing: preserved window follows it
	h.cfg.AccountOverrides = map[string]AccountOverride{"codex-a.json": {CapacityByWindow: map[string]float64{"five-hour": 20}}}
	second, _, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	a := acctByID(t, second, "codex-a.json")
	w, _ := winByID(a, "five-hour")
	if a.RefreshStatus != refreshStale || w.Capacity == nil || *w.Capacity != 20 {
		t.Fatalf("override not applied to preserved account: %+v", a)
	}
	h.cfg.AccountOverrides = nil
	h.clk.advance(time.Minute)
	third, _, err := h.run(second, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if w, _ := winByID(acctByID(t, third, "codex-a.json"), "five-hour"); w.Capacity != nil {
		t.Fatalf("removed override lingered: %+v", w)
	}
}

// ---- preservation on failure --------------------------------------------------

func TestFailurePreservesPreviousObservationsWithStatus(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	prevA := acctByID(t, first, "codex-a.json")

	h.m.setOverride("ai-codex-1", upstreamStatus(503))
	h.clk.advance(4 * time.Minute) // fresh values would differ in ObservedAt/ResetAt
	second, health, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.Validate(second); err != nil {
		t.Fatal(err)
	}
	a := acctByID(t, second, "codex-a.json")
	if a.RefreshStatus != refreshStale {
		t.Fatalf("status = %q", a.RefreshStatus)
	}
	// Preserved verbatim: original observation time, fraction and reset — never refreshed.
	if !a.ObservedAt.Equal(prevA.ObservedAt) || !a.ObservedAt.Equal(t0) {
		t.Fatalf("observedAt refreshed: %v", a.ObservedAt)
	}
	w, pw := a.Windows[0], prevA.Windows[0]
	if !w.ObservedAt.Equal(t0) || *w.UsedFraction != *pw.UsedFraction || !w.ResetAt.Equal(pw.ResetAt) {
		t.Fatalf("window rewritten: %+v vs %+v", w, pw)
	}
	// Other accounts refreshed normally, so the pass is partial and says so generically.
	b := acctByID(t, second, "codex-b.json")
	if b.RefreshStatus != refreshOK || !b.ObservedAt.Equal(t0.Add(4*time.Minute)) {
		t.Fatalf("healthy account not refreshed: %+v", b)
	}
	if health.Status != healthPartial || !strings.Contains(health.LastError, "1 of 4 quota sources failed") {
		t.Fatalf("health = %+v", health)
	}
	if strings.Contains(health.LastError, "secret-body-marker") {
		t.Fatalf("upstream body leaked: %s", health.LastError)
	}
	// LastRefreshAt tracks the newest retained observation.
	if !health.LastRefreshAt.Equal(t0.Add(4 * time.Minute)) {
		t.Fatalf("lastRefreshAt = %v", health.LastRefreshAt)
	}
	// Preserved data still ages out by observation time, never looks fresh.
	sc := quota.AccountScore(a, "", t0.Add(4*time.Minute), 3*time.Minute)
	if sc.Known {
		t.Fatalf("expired preserved observation scored as known: %+v", sc)
	}
}

func TestAllSourcesFailingKeepsEverythingAndReportsFailed(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.respond = func(string, string) mgmtReply { return mgmtReply{status: 500, body: "secret-body-marker"} }
	h.clk.advance(time.Minute)
	second, health, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != healthFailed {
		t.Fatalf("status = %q", health.Status)
	}
	for _, id := range []string{"claude-main.json", "codex-a.json", "codex-b.json", "openrouter-cfg"} {
		a := acctByID(t, second, id)
		if a.RefreshStatus != refreshStale || !a.ObservedAt.Equal(t0) {
			t.Fatalf("%s = %+v", id, a)
		}
	}
	if !health.LastRefreshAt.Equal(t0) {
		t.Fatalf("lastRefreshAt = %v", health.LastRefreshAt)
	}
	// Failure without any prior observation is "error", not an invented value.
	h2 := newHarness(t)
	h2.m.respond = func(string, string) mgmtReply { return mgmtReply{status: 500, body: "secret-body-marker"} }
	snap, health2, err := h2.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	a := acctByID(t, snap, "codex-a.json")
	if a.RefreshStatus != refreshError || len(a.Windows) != 0 || !a.ObservedAt.IsZero() || health2.Status != healthFailed || !health2.LastRefreshAt.IsZero() {
		t.Fatalf("no-prior failure = %+v / %+v", a, health2)
	}
}

func TestNullFreshWindowKeepsPreviousMeasurementWithOriginalTimestamp(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.setOverride("ai-codex-1", func(string) mgmtReply {
		now := h.clk.now()
		return okReply(fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":null,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":15,"limit_window_seconds":604800,"reset_at":%d}}}`,
			now.Add(time.Hour).Unix(), now.Add(48*time.Hour).Unix()))
	})
	h.clk.advance(2 * time.Minute)
	second, _, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	a := acctByID(t, second, "codex-a.json")
	fh, _ := winByID(a, "five-hour")
	if fh.UsedFraction == nil || *fh.UsedFraction != 0.31 || !fh.ObservedAt.Equal(t0) {
		t.Fatalf("null window did not keep the original measurement: %+v", fh)
	}
	sd, _ := winByID(a, "seven-day")
	if sd.UsedFraction == nil || *sd.UsedFraction != 0.15 || !sd.ObservedAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("fresh window = %+v", sd)
	}
}

func TestClaudeProfileFailureKeepsReportedPlanEstimate(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.setOverride("ai-claude-1", func(url string) mgmtReply {
		if strings.HasSuffix(url, "/profile") {
			return mgmtReply{status: 500, body: "x"}
		}
		return okReply(claudeUsageBody(h.clk.now(), 55, 20))
	})
	h.clk.advance(time.Minute)
	second, _, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	cl := acctByID(t, second, "claude-main.json")
	fh, _ := winByID(cl, "five-hour")
	if cl.Plan != "max5" || cl.RefreshStatus != refreshOK || *fh.UsedFraction != 0.55 || fh.Capacity == nil || *fh.Capacity != 5 {
		t.Fatalf("plan not carried: %+v %+v", cl, fh)
	}
	// Never-seen profile: plan stays unknown, capacity unknown (nothing invented).
	h2 := newHarness(t)
	h2.m.setOverride("ai-claude-1", func(url string) mgmtReply {
		if strings.HasSuffix(url, "/profile") {
			return mgmtReply{status: 500, body: "x"}
		}
		return okReply(claudeUsageBody(h2.clk.now(), 55, 20))
	})
	s2, _, err := h2.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	cl2 := acctByID(t, s2, "claude-main.json")
	if w, _ := winByID(cl2, "five-hour"); cl2.Plan != "" || w.Capacity != nil {
		t.Fatalf("invented plan: %+v %+v", cl2, w)
	}
}

func TestMalformedAdapterBodyIsGenericParseFailure(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-codex-1", func(string) mgmtReply { return okReply(`{"error":{"message":"secret-body-marker"}}`) })
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if a := acctByID(t, snap, "codex-a.json"); a.RefreshStatus != refreshError {
		t.Fatalf("%+v", a)
	}
	if health.Status != healthPartial || strings.Contains(health.LastError, "secret-body-marker") || !strings.Contains(health.LastError, failMessages[failParse]) {
		t.Fatalf("health = %+v", health)
	}
}

// ---- retry / backoff / Retry-After -------------------------------------------

func TestTransientRetriesWithinPassThenSucceeds(t *testing.T) {
	h := newHarness(t)
	var slept []time.Duration
	var mu sync.Mutex
	h.col.deps.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return nil
	}
	h.col.deps.retryDelay = time.Second
	var n atomic.Int32
	h.m.setOverride("ai-codex-1", func(string) mgmtReply {
		if n.Add(1) < 3 {
			return mgmtReply{status: 502, body: "x"}
		}
		return okReply(codexBody(h.clk.now(), 31, 12))
	})
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if a := acctByID(t, snap, "codex-a.json"); a.RefreshStatus != refreshOK || health.Status != healthOK {
		t.Fatalf("%+v %+v", a, health)
	}
	mu.Lock()
	defer mu.Unlock()
	if h.m.countFor("ai-codex-1") != 3 || len(slept) != 2 || slept[0] != time.Second || slept[1] != 2*time.Second {
		t.Fatalf("attempts=%d slept=%v", h.m.countFor("ai-codex-1"), slept)
	}
}

func TestRetryBoundedAndNonTransientNotRetried(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-codex-1", upstreamStatus(500))
	h.m.setOverride("ai-codex-2", upstreamStatus(401))
	if _, _, err := h.run(quota.Snapshot{}, testInventory()); err != nil {
		t.Fatal(err)
	}
	if got := h.m.countFor("ai-codex-1"); got != maxAttempts {
		t.Fatalf("transient attempts = %d, want %d", got, maxAttempts)
	}
	if got := h.m.countFor("ai-codex-2"); got != 1 {
		t.Fatalf("auth failure retried: %d", got)
	}
}

func TestSourceBackoffSkipsRequestsThenRetries(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-codex-1", upstreamStatus(500))
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	before := h.m.countFor("ai-codex-1")
	// within backoff (30s base): no request at all for that source, previous data kept
	h.clk.advance(10 * time.Second)
	second, health, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if h.m.countFor("ai-codex-1") != before {
		t.Fatal("source in backoff was queried")
	}
	if health.Status != healthPartial {
		t.Fatalf("skipped source must still count as failed: %+v", health)
	}
	if a := acctByID(t, second, "codex-a.json"); a.RefreshStatus != refreshError {
		t.Fatalf("%+v", a)
	}
	// other sources keep refreshing independently
	if h.m.countFor("ai-codex-2") != 2 {
		t.Fatalf("healthy source stopped refreshing: %d", h.m.countFor("ai-codex-2"))
	}
	// after backoff the source is retried and recovery clears state
	h.m.setOverride("ai-codex-1", nil)
	h.clk.advance(31 * time.Second)
	third, health, err := h.run(second, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if a := acctByID(t, third, "codex-a.json"); a.RefreshStatus != refreshOK || health.Status != healthOK {
		t.Fatalf("no recovery: %+v %+v", a, health)
	}
	if _, ok := h.col.states.get("codex\x00codex-a.json"); ok {
		t.Fatal("state not cleared after success")
	}
}

func TestRetryAfterHonoredAcrossPasses(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-codex-1", func(string) mgmtReply {
		return mgmtReply{status: 429, header: map[string][]string{"Retry-After": {"600"}}, body: "secret-body-marker"}
	})
	first, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if got := h.m.countFor("ai-codex-1"); got != 1 {
		t.Fatalf("429 retried in-pass: %d", got)
	}
	if !strings.Contains(health.LastError, failMessages[failRateLimited]) {
		t.Fatalf("health = %+v", health)
	}
	h.m.setOverride("ai-codex-1", nil)
	h.clk.advance(9 * time.Minute) // far beyond the 30s base backoff but inside Retry-After
	second, _, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if got := h.m.countFor("ai-codex-1"); got != 1 {
		t.Fatalf("Retry-After ignored, source queried %d times", got)
	}
	h.clk.advance(90 * time.Second)
	third, _, err := h.run(second, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if a := acctByID(t, third, "codex-a.json"); a.RefreshStatus != refreshOK {
		t.Fatalf("%+v", a)
	}
}

func TestManagementKeyRejectedDoesNotBackoffSources(t *testing.T) {
	h := newHarness(t)
	t.Setenv(testMgmtEnv, "wrong-key")
	_, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != healthFailed || !strings.Contains(health.LastError, failMessages[failKeyRejected]) {
		t.Fatalf("health = %+v", health)
	}
	t.Setenv(testMgmtEnv, testMgmtKey)
	_, health, err = h.run(quota.Snapshot{}, testInventory())
	if err != nil || health.Status != healthOK {
		t.Fatalf("operator fix not picked up immediately: %+v %v", health, err)
	}
}

// ---- concurrency and cancellation --------------------------------------------

func manyCodexInventory(n int) InventoryFunc {
	var list []pluginapi.HostAuthFileEntry
	for i := 0; i < n; i++ {
		list = append(list, pluginapi.HostAuthFileEntry{ID: fmt.Sprintf("codex-%02d.json", i), AuthIndex: fmt.Sprintf("ai-many-%02d", i), Provider: "codex", AccountType: "oauth"})
	}
	return func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return list, nil, nil
	}
}

func TestAtMostTwoConcurrentSources(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.delay = 40 * time.Millisecond
	h.m.respond = func(string, string) mgmtReply { return okReply(codexBody(h.clk.now(), 10, 5)) }
	h.m.mu.Unlock()
	snap, health, err := h.run(quota.Snapshot{}, manyCodexInventory(8))
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Accounts) != 8 || health.Status != healthOK {
		t.Fatalf("%d accounts, %+v", len(snap.Accounts), health)
	}
	if peak := h.m.peakInflight(); peak > maxConcurrentSources || peak < 2 {
		t.Fatalf("peak concurrency = %d, want 2", peak)
	}
	// the pool may be narrowed but never widened
	c := newCollector(collectorDeps{concurrency: 50})
	if c.deps.concurrency != maxConcurrentSources {
		t.Fatalf("concurrency cap = %d", c.deps.concurrency)
	}
}

func TestCancellationReturnsPreviousUnchangedAndPromptly(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.mu.Lock()
	h.m.respond = func(string, string) mgmtReply { return mgmtReply{block: true} }
	h.m.mu.Unlock()
	h.clk.advance(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(80 * time.Millisecond); cancel() }()
	start := time.Now()
	snap, health, err := h.col.collect(ctx, h.cfg, first, testInventory())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation not prompt")
	}
	if health.Status != healthCanceled {
		t.Fatalf("health = %+v", health)
	}
	if snap.Revision != first.Revision || !snap.GeneratedAt.Equal(first.GeneratedAt) || len(snap.Accounts) != len(first.Accounts) {
		t.Fatalf("canceled pass changed snapshot: rev %d", snap.Revision)
	}
	if a := acctByID(t, snap, "codex-a.json"); a.RefreshStatus != refreshOK {
		t.Fatalf("canceled pass rewrote statuses: %+v", a)
	}
	// already-canceled context: no inventory call, no requests
	var invCalled bool
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, _, err = h.col.collect(ctx2, h.cfg, first, func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		invCalled = true
		return nil, nil, nil
	})
	if !errors.Is(err, context.Canceled) || invCalled {
		t.Fatalf("pre-canceled: err=%v inv=%v", err, invCalled)
	}
}

func TestRetrySleepIsCancelable(t *testing.T) {
	h := newHarness(t)
	h.col.deps.sleep = ctxSleep
	h.col.deps.retryDelay = 30 * time.Second
	h.m.setOverride("ai-many-00", upstreamStatus(500))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, _, err := h.col.collect(ctx, h.cfg, quota.Snapshot{}, manyCodexInventory(1))
	if !errors.Is(err, context.Canceled) || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(start))
	}
}

// ---- inventory failure / setup states ----------------------------------------

func TestInventoryFailureKeepsPreviousAndIsGeneric(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.reset()
	snap, health, err := h.run(first, func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return nil, nil, errors.New("rpc failed: secret-body-marker /secret/path")
	})
	if !errors.Is(err, errInventoryUnavailable) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	if health.Status != healthInventoryUnavailable || strings.Contains(health.LastError, "secret") {
		t.Fatalf("health = %+v", health)
	}
	if len(snap.Accounts) != len(first.Accounts) || snap.Revision != first.Revision || len(h.m.snapshotCalls()) != 0 {
		t.Fatal("inventory failure altered snapshot or fetched")
	}
	if _, _, err := h.run(first, nil); !errors.Is(err, errInventoryUnavailable) {
		t.Fatalf("nil inventory err = %v", err)
	}
}

func TestPartialInventoryEmptyListDoesNotDropAccounts(t *testing.T) {
	h := newHarness(t)
	// a pass that lost its list but still has models must keep config keys
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return nil, []pluginapi.HostAuthModels{{ID: "openrouter-cfg", AuthIndex: "ai-or-1", Provider: "openrouter", Models: []string{"m"}}}, nil
	}
	snap, _, err := h.run(quota.Snapshot{}, inv)
	if err != nil || len(snap.Credentials) != 1 || snap.Credentials[0].ID != "openrouter-cfg" {
		t.Fatalf("%+v %v", snap.Credentials, err)
	}
}

func TestNoManagementURLGivesInventoryOnlySnapshot(t *testing.T) {
	h := newHarness(t)
	h.cfg.ManagementURL, h.cfg.ManagementKeyEnv = "", ""
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != healthNotConfigured || len(h.m.snapshotCalls()) != 0 {
		t.Fatalf("health = %+v", health)
	}
	if len(snap.Credentials) != 5 || acctByID(t, snap, "codex-a.json").RefreshStatus != refreshError {
		t.Fatalf("unexpected: %+v", snap.Accounts)
	}
}

func TestMissingManagementKeyIsSetupRequiredNotHealthy(t *testing.T) {
	h := newHarness(t)
	t.Setenv(testMgmtEnv, "")
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != healthSetupRequired || len(setupFor(health, "management")) != 1 {
		t.Fatalf("health = %+v", health)
	}
	if setupFor(health, "management")[0].Status != "setup_required" || len(h.m.snapshotCalls()) != 0 {
		t.Fatal("management setup item wrong or requests were sent")
	}
	if len(snap.Credentials) != 5 {
		t.Fatalf("inventory lost: %d", len(snap.Credentials))
	}
}

func TestUnsupportedProviderStaysUnknown(t *testing.T) {
	h := newHarness(t)
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "gem.json", AuthIndex: "ai-gem", Provider: "gemini"}}, nil, nil
	}
	snap, health, err := h.run(quota.Snapshot{}, inv)
	if err != nil {
		t.Fatal(err)
	}
	a := acctByID(t, snap, "gem.json")
	if a.RefreshStatus != refreshUnsupported || a.BillingType != "unknown" || len(h.m.snapshotCalls()) != 0 {
		t.Fatalf("%+v", a)
	}
	if len(setupFor(health, "gemini")) != 1 {
		t.Fatalf("setup = %+v", health.Setup)
	}
	for _, p := range snap.Pools {
		if strings.Contains(p.ID, "gemini") {
			t.Fatalf("auto pool for unknown billing type: %+v", p)
		}
	}
}

func TestNeverReturnsInvalidSnapshotAndSurvivesBadPreviousState(t *testing.T) {
	h := newHarness(t)
	bad := 2.5
	nan := -1.0
	prev := quota.Snapshot{SchemaVersion: 1, Revision: 7, Accounts: []quota.Account{{
		ID: "codex-a.json", Provider: "codex", ObservedAt: t0.Add(-time.Minute), BillingType: "subscription",
		Windows: []quota.Window{
			{ID: "five-hour", UsedFraction: &bad, ObservedAt: t0.Add(-time.Minute)},
			{ID: "", UsedFraction: &nan},
			{ID: "seven-day", UsedFraction: ptr(0.3), ObservedAt: t0.Add(-time.Minute)},
			{ID: "seven-day", UsedFraction: ptr(0.4), ObservedAt: t0.Add(-time.Minute)},
		},
		Financial: []quota.Financial{{Kind: "nonsense", Amount: 1}},
	}}}
	h.m.setOverride("ai-codex-1", upstreamStatus(500))
	snap, _, err := h.run(prev, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.Validate(snap); err != nil {
		t.Fatalf("invalid snapshot escaped: %v", err)
	}
	a := acctByID(t, snap, "codex-a.json")
	if len(a.Windows) != 1 || *a.Windows[0].UsedFraction != 0.3 || len(a.Financial) != 0 {
		t.Fatalf("bad previous entries not sanitized: %+v", a)
	}
	if snap.Revision != 8 {
		t.Fatalf("revision = %d", snap.Revision)
	}
}

func ptr(f float64) *float64 { return &f }

func TestRemovedCredentialDisappearsButGroupedKeepsPreviousUnderOverride(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	inv := func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "codex-b.json", AuthIndex: "ai-codex-2", Provider: "codex", AccountType: "oauth"}}, nil, nil
	}
	second, _, err := h.run(first, inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Accounts) != 1 || second.Accounts[0].ID != "codex-b.json" || len(second.Credentials) != 1 {
		t.Fatalf("stale credential/accounts kept: %v", accountIDs(second))
	}
}

// ---- panic containment ---------------------------------------------------------

func TestInventoryPanicIsContained(t *testing.T) {
	h := newHarness(t)
	first, _, _ := h.run(quota.Snapshot{}, testInventory())
	snap, health, err := h.run(first, func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		panic("boom secret-body-marker")
	})
	if !errors.Is(err, errCollectorPanic) || health.Status != healthFailed || strings.Contains(health.LastError, "secret") {
		t.Fatalf("err=%v health=%+v", err, health)
	}
	if len(snap.Accounts) != len(first.Accounts) {
		t.Fatal("panic altered snapshot")
	}
}

// ---- admin reports -------------------------------------------------------------

type adminMock struct {
	t    *testing.T
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	hook func(r *http.Request) (int, map[string]string, string)
}

func newAdminMock(t *testing.T, hook func(r *http.Request) (int, map[string]string, string)) *adminMock {
	t.Helper()
	m := &adminMock{t: t, hook: hook}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.reqs = append(m.reqs, r.Clone(r.Context()))
		m.mu.Unlock()
		code, hdr, body := m.hook(r)
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *adminMock) requests() []*http.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*http.Request(nil), m.reqs...)
}

func adminHarness(t *testing.T, a *adminMock, providers ...string) *harness {
	t.Helper()
	h := newHarness(t)
	client := a.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	bases := map[string]string{}
	for _, p := range providers {
		bases[p] = a.srv.URL
	}
	h.col.deps.adminBases = bases
	h.col.deps.adminClient = client
	h.cfg.ManagementURL, h.cfg.ManagementKeyEnv = "", ""
	return h
}

func emptyInventory() InventoryFunc {
	return func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		return nil, nil, nil
	}
}

func TestAdminOpenAIPaginatesAndSumsOnce(t *testing.T) {
	t.Setenv("OPENAI_ADMIN_KEY", "synthetic-openai-admin")
	pages := map[string]string{
		"":   `{"object":"page","data":[{"object":"bucket","results":[{"object":"organization.costs.result","amount":{"value":1.25,"currency":"usd"}}]}],"has_more":true,"next_page":"p2"}`,
		"p2": `{"object":"page","data":[{"object":"bucket","results":[{"object":"organization.costs.result","amount":{"value":2.5,"currency":"usd"}},{"object":"organization.costs.result","amount":{"value":9,"currency":"eur"}}]}],"has_more":true,"next_page":"p3"}`,
		"p3": `{"object":"page","data":[{"object":"bucket","results":[{"object":"organization.costs.result","amount":{"value":0.25,"currency":"usd"}}]}],"has_more":false,"next_page":null}`,
	}
	a := newAdminMock(t, func(r *http.Request) (int, map[string]string, string) {
		if r.URL.Path != "/v1/organization/costs" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		return 200, nil, pages[r.URL.Query().Get("page")]
	})
	h := adminHarness(t, a, "openai")
	h.cfg.AdminSources = []AdminSource{{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"}}
	snap, health, err := h.run(quota.Snapshot{}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := quota.Validate(snap); err != nil {
		t.Fatal(err)
	}
	acct := acctByID(t, snap, "admin:openai:organization-costs")
	if len(acct.Financial) != 1 {
		t.Fatalf("financial = %+v", acct.Financial)
	}
	f := acct.Financial[0]
	if f.Kind != "upstream-spend" || f.ScopeID != adminScope || f.Currency != "USD" || f.Amount != 4.0 || f.Source != "openai-costs" {
		t.Fatalf("financial = %+v", f)
	}
	if health.Status != healthOK || acct.RefreshStatus != refreshOK {
		t.Fatalf("health = %+v acct = %+v", health, acct)
	}
	reqs := a.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	for i, r := range reqs {
		if r.Header.Get("Authorization") != "Bearer synthetic-openai-admin" || r.Method != "GET" {
			t.Fatalf("request %d headers wrong", i)
		}
		q := r.URL.Query()
		if q.Get("start_time") == "" || q.Get("end_time") == "" || q.Get("limit") != "31" || q.Get("bucket_width") != "1d" {
			t.Fatalf("query = %v", q)
		}
	}
	if reqs[1].URL.Query().Get("page") != "p2" || reqs[2].URL.Query().Get("page") != "p3" || reqs[0].URL.Query().Get("page") != "" {
		t.Fatal("page tokens not threaded")
	}
	// admin spend is an organization scope, never attached to a credential/pool
	if len(snap.Credentials) != 0 {
		t.Fatalf("admin report attached to credentials: %+v", snap.Credentials)
	}
	b, _ := json.Marshal(snap)
	if strings.Contains(string(b), "synthetic-openai-admin") {
		t.Fatal("admin key in snapshot")
	}
}

func TestAdminAnthropicPaginationAndHeaders(t *testing.T) {
	t.Setenv("ANTHROPIC_ADMIN_KEY", "synthetic-anthropic-admin")
	a := newAdminMock(t, func(r *http.Request) (int, map[string]string, string) {
		if r.URL.Path != "/v1/organizations/cost_report" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "" {
			return 200, nil, `{"data":[{"starting_at":"2026-11-01T00:00:00Z","ending_at":"2026-11-02T00:00:00Z","results":[{"amount":"123.45","currency":"USD"}]}],"has_more":true,"next_page":"page_b"}`
		}
		return 200, nil, `{"data":[{"starting_at":"2026-11-02T00:00:00Z","ending_at":"2026-11-03T00:00:00Z","results":[{"amount":"76.55","currency":"USD"}]}],"has_more":false,"next_page":null}`
	})
	h := adminHarness(t, a, "anthropic")
	h.cfg.AdminSources = []AdminSource{{Provider: "anthropic", KeyEnv: "ANTHROPIC_ADMIN_KEY"}}
	snap, _, err := h.run(quota.Snapshot{}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	f := acctByID(t, snap, "admin:anthropic:organization-costs").Financial[0]
	if f.Amount != 2.0 || f.Source != "anthropic-cost-report" {
		t.Fatalf("financial = %+v", f)
	}
	for _, r := range a.requests() {
		if r.Header.Get("x-api-key") != "synthetic-anthropic-admin" || r.Header.Get("anthropic-version") == "" || r.Header.Get("Authorization") != "" {
			t.Fatalf("anthropic headers wrong: %v", r.Header)
		}
		if r.URL.Query().Get("starting_at") == "" || r.URL.Query().Get("ending_at") == "" {
			t.Fatalf("query = %v", r.URL.Query())
		}
	}
}

func TestAdminPaginationFailuresNeverEmitPartialTotals(t *testing.T) {
	t.Setenv("OPENAI_ADMIN_KEY", "synthetic-openai-admin")
	good := `{"data":[{"results":[{"amount":{"value":5,"currency":"usd"}}]}],"has_more":true,"next_page":"%s"}`
	cases := map[string]func(*http.Request) (int, map[string]string, string){
		"repeating cursor": func(r *http.Request) (int, map[string]string, string) {
			return 200, nil, fmt.Sprintf(good, "same")
		},
		"empty cursor with has_more": func(r *http.Request) (int, map[string]string, string) {
			return 200, nil, `{"data":[],"has_more":true,"next_page":""}`
		},
		"endless distinct cursors": func(r *http.Request) (int, map[string]string, string) {
			return 200, nil, fmt.Sprintf(good, "c"+r.URL.Query().Get("page")+"x")
		},
		"second page fails": func(r *http.Request) (int, map[string]string, string) {
			if r.URL.Query().Get("page") == "" {
				return 200, nil, fmt.Sprintf(good, "p2")
			}
			return 500, nil, `secret-body-marker`
		},
		"second page malformed": func(r *http.Request) (int, map[string]string, string) {
			if r.URL.Query().Get("page") == "" {
				return 200, nil, fmt.Sprintf(good, "p2")
			}
			return 200, nil, `{"data":[{"results":[{"amount":{"value":"lots","currency":"usd"}}]}]}`
		},
	}
	for name, hook := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAdminMock(t, hook)
			h := adminHarness(t, a, "openai")
			h.col.deps.sleep = func(context.Context, time.Duration) error { return nil }
			h.cfg.AdminSources = []AdminSource{{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"}}
			snap, health, err := h.run(quota.Snapshot{}, emptyInventory())
			if err != nil {
				t.Fatal(err)
			}
			acct := acctByID(t, snap, "admin:openai:organization-costs")
			if len(acct.Financial) != 0 || acct.RefreshStatus != refreshError || health.Status != healthFailed {
				t.Fatalf("partial total emitted or not failed: %+v / %+v", acct, health)
			}
			if strings.Contains(health.LastError, "secret-body-marker") {
				t.Fatal("body leaked")
			}
			if n := len(a.requests()); n > maxAdminPages*maxAttempts {
				t.Fatalf("unbounded pagination: %d requests", n)
			}
		})
	}
}

func TestAdminFailureKeepsPreviousSpendAsStale(t *testing.T) {
	t.Setenv("OPENAI_ADMIN_KEY", "synthetic-openai-admin")
	var fail atomic.Bool
	a := newAdminMock(t, func(r *http.Request) (int, map[string]string, string) {
		if fail.Load() {
			return 429, map[string]string{"Retry-After": "300"}, "secret-body-marker"
		}
		return 200, nil, `{"data":[{"results":[{"amount":{"value":7.5,"currency":"usd"}}]}],"has_more":false,"next_page":null}`
	})
	h := adminHarness(t, a, "openai")
	h.cfg.AdminSources = []AdminSource{{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"}}
	first, _, err := h.run(quota.Snapshot{}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	h.clk.advance(6 * time.Minute)
	second, _, err := h.run(first, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	acct := acctByID(t, second, "admin:openai:organization-costs")
	if acct.RefreshStatus != refreshStale || len(acct.Financial) != 1 || acct.Financial[0].Amount != 7.5 || !acct.Financial[0].ObservedAt.Equal(t0) {
		t.Fatalf("%+v", acct)
	}
	// Retry-After 300s: a pass inside it makes no admin request.
	n := len(a.requests())
	h.clk.advance(2 * time.Minute)
	if _, _, err := h.run(second, emptyInventory()); err != nil {
		t.Fatal(err)
	}
	if len(a.requests()) != n {
		t.Fatal("admin source queried inside Retry-After")
	}
}

func TestAdminSourcesAreSetupRequiredWhenNotFullySafe(t *testing.T) {
	h := newHarness(t)
	h.cfg.ManagementURL, h.cfg.ManagementKeyEnv = "", ""
	t.Setenv("OPENAI_ADMIN_KEY", "")
	t.Setenv("ANTHROPIC_ADMIN_KEY", "synthetic")
	t.Setenv("ARBITRARY_ENV", "synthetic")
	h.cfg.AdminSources = []AdminSource{
		{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"},     // key unset
		{Provider: "anthropic", KeyEnv: "ARBITRARY_ENV"},     // non-fixed env var
		{Provider: "evil", KeyEnv: "ANTHROPIC_ADMIN_KEY"},    // unknown provider
		{Provider: "openrouter", KeyEnv: "OPENAI_ADMIN_KEY"}, // no admin collector
	}
	called := false
	h.col.deps.adminClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})}
	snap, health, err := h.run(quota.Snapshot{}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("admin client used for a source that is not setup-complete")
	}
	if len(snap.Accounts) != 0 {
		t.Fatalf("accounts invented: %v", accountIDs(snap))
	}
	for _, s := range health.Setup {
		if s.Status != "setup_required" {
			t.Fatalf("unexpected setup status %+v", s)
		}
	}
	if len(health.Setup) != 4 {
		t.Fatalf("setup = %+v", health.Setup)
	}
	for _, s := range health.Setup {
		if strings.Contains(s.Message, "synthetic") {
			t.Fatal("secret in setup message")
		}
	}
	if health.Status == healthOK {
		t.Fatalf("setup-required admin sources must not report ok: %+v", health)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAdminClientRefusesRedirectsAndNonHTTPSBase(t *testing.T) {
	t.Setenv("OPENAI_ADMIN_KEY", "synthetic-openai-admin")
	var landed atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { landed.Add(1) }))
	defer target.Close()
	a := newAdminMock(t, func(r *http.Request) (int, map[string]string, string) {
		return http.StatusTemporaryRedirect, map[string]string{"Location": target.URL + "/steal"}, ""
	})
	h := adminHarness(t, a, "openai")
	h.cfg.AdminSources = []AdminSource{{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"}}
	snap, health, err := h.run(quota.Snapshot{}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if landed.Load() != 0 || health.Status != healthFailed || len(acctByID(t, snap, "admin:openai:organization-costs").Financial) != 0 {
		t.Fatalf("redirect followed or accepted: landed=%d %+v", landed.Load(), health)
	}
	// production client has the same no-redirect rule and TLS floor
	pc := newAdminHTTPClient()
	if pc.CheckRedirect == nil {
		t.Fatal("production admin client follows redirects")
	}
	if tr, ok := pc.Transport.(*http.Transport); !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("production admin client TLS floor missing")
	}
	// a plain-HTTP base can never be used
	h2 := adminHarness(t, a, "openai")
	h2.col.deps.adminBases = map[string]string{"openai": "http://127.0.0.1:1"}
	h2.cfg.AdminSources = []AdminSource{{Provider: "openai", KeyEnv: "OPENAI_ADMIN_KEY"}}
	if _, health, _ := h2.run(quota.Snapshot{}, emptyInventory()); health.Status != healthFailed {
		t.Fatalf("http base accepted: %+v", health)
	}
}

func TestProductionAdminBasesAreFixedHTTPS(t *testing.T) {
	if defaultAdminBases["openai"] != "https://api.openai.com" || defaultAdminBases["anthropic"] != "https://api.anthropic.com" || len(defaultAdminBases) != 2 {
		t.Fatalf("admin bases = %v", defaultAdminBases)
	}
	if k, ok := adminKeyEnvFor("openai"); !ok || k != "OPENAI_ADMIN_KEY" {
		t.Fatal("openai key env")
	}
	if _, ok := adminKeyEnvFor("openrouter"); ok {
		t.Fatal("unexpected admin provider")
	}
	c := newCollector(collectorDeps{})
	if c.deps.adminBases["openai"] != "https://api.openai.com" {
		t.Fatal("default collector must use fixed production origins")
	}
}

// ---- background loop ----------------------------------------------------------

type loopRig struct {
	mu       sync.Mutex
	snap     quota.Snapshot
	health   []collectorHealth
	passes   chan struct{}
	invCalls atomic.Int32
}

func newLoopRig() *loopRig { return &loopRig{passes: make(chan struct{}, 64)} }

func (r *loopRig) hooks(cfg Config, col *collector, inv InventoryFunc) LoopHooks {
	return LoopHooks{
		Config:   func() Config { return cfg },
		Previous: func() quota.Snapshot { r.mu.Lock(); defer r.mu.Unlock(); return r.snap },
		Inventory: func(ctx context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
			r.invCalls.Add(1)
			return inv(ctx)
		},
		Publish: func(s quota.Snapshot, h collectorHealth, err error) {
			r.mu.Lock()
			if err == nil {
				r.snap = s
			}
			r.health = append(r.health, h)
			r.mu.Unlock()
			r.passes <- struct{}{}
		},
		MinInterval: time.Millisecond,
		Collector:   col,
	}
}

func waitPass(t *testing.T, r *loopRig) {
	t.Helper()
	select {
	case <-r.passes:
	case <-time.After(5 * time.Second):
		t.Fatal("collector pass did not complete")
	}
}

func TestLoopRunsImmediatelyOnTriggerAndStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	h.cfg.PollIntervalSeconds = 3600
	rig := newLoopRig()
	refresh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := StartCollector(ctx, rig.hooks(h.cfg, h.col, testInventory()), refresh)

	waitPass(t, rig) // immediate first pass
	rig.mu.Lock()
	if rig.snap.Revision != 1 || len(rig.snap.Accounts) != 5 {
		t.Fatalf("first pass snapshot = rev %d accounts %d", rig.snap.Revision, len(rig.snap.Accounts))
	}
	rig.mu.Unlock()

	refresh <- struct{}{}
	waitPass(t, rig)
	rig.mu.Lock()
	if rig.snap.Revision != 2 {
		t.Fatalf("previous snapshot not fed back: rev %d", rig.snap.Revision)
	}
	rig.mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop")
	}
	select {
	case <-rig.passes:
		t.Fatal("pass published after cancel")
	default:
	}
}

func TestLoopCancelInterruptsInFlightPass(t *testing.T) {
	h := newHarness(t)
	h.m.mu.Lock()
	h.m.respond = func(string, string) mgmtReply { return mgmtReply{block: true} }
	h.m.mu.Unlock()
	rig := newLoopRig()
	ctx, cancel := context.WithCancel(context.Background())
	done := StartCollector(ctx, rig.hooks(h.cfg, h.col, testInventory()), nil)
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight pass not canceled")
	}
	select {
	case <-rig.passes:
		t.Fatal("canceled pass published")
	default:
	}
}

func TestLoopPeriodicPasses(t *testing.T) {
	h := newHarness(t)
	h.cfg.PollIntervalSeconds = 1
	rig := newLoopRig()
	hooks := rig.hooks(h.cfg, h.col, testInventory())
	hooks.MinInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := StartCollector(ctx, hooks, nil)
	waitPass(t, rig)
	waitPass(t, rig) // second pass arrives from the 1s ticker
	cancel()
	<-done
}

func TestLoopPublishesFailureHealthWithoutReplacingSnapshot(t *testing.T) {
	h := newHarness(t)
	rig := newLoopRig()
	var fail atomic.Bool
	inv := func(ctx context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		if fail.Load() {
			return nil, nil, errors.New("host unavailable")
		}
		return testInventory()(ctx)
	}
	refresh := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := StartCollector(ctx, rig.hooks(h.cfg, h.col, inv), refresh)
	waitPass(t, rig)
	fail.Store(true)
	refresh <- struct{}{}
	waitPass(t, rig)
	cancel()
	<-done
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if rig.snap.Revision != 1 || len(rig.snap.Accounts) != 5 {
		t.Fatalf("failed pass replaced the good snapshot: rev %d", rig.snap.Revision)
	}
	if last := rig.health[len(rig.health)-1]; last.Status != healthInventoryUnavailable || last.LastRefreshAt.IsZero() {
		t.Fatalf("last health = %+v", last)
	}
}

// ---- race: concurrent passes sharing one collector ----------------------------

func TestConcurrentCollectPassesAreRaceFree(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-codex-2", upstreamStatus(500))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.clk.advance(time.Duration(i) * time.Second)
			snap, _, err := h.run(quota.Snapshot{}, testInventory())
			if err != nil {
				t.Error(err)
				return
			}
			if err := quota.Validate(snap); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

func TestCollectDoesNotMutatePrevious(t *testing.T) {
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(first)
	h.m.respond = func(string, string) mgmtReply { return mgmtReply{status: 500} }
	h.clk.advance(time.Minute)
	if _, _, err := h.run(first, testInventory()); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(first)
	if string(before) != string(after) {
		t.Fatal("collect mutated the previous snapshot")
	}
}

func TestExportedSeamAliasesMatch(t *testing.T) {
	var _ CollectorStatus = collectorHealth{}
	var _ HostInventory = InventoryFunc(nil)
	var _ func(context.Context, Config, quota.Snapshot, InventoryFunc) (quota.Snapshot, collectorHealth, error) = collectSnapshot
	var _ func(context.Context, Config, quota.Snapshot, HostInventory) (quota.Snapshot, CollectorStatus, error) = CollectOnce
	b, _ := json.Marshal(collectorHealth{Status: "ok", Setup: []setupItem{{Provider: "p", Status: "s", Message: "m"}}})
	for _, k := range []string{`"status"`, `"lastError"`, `"setup"`, `"provider"`, `"message"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("missing lowerCamel key %s in %s", k, b)
		}
	}
}

func TestLabelOnlyScopeSurfacesAsUnmatchedWithSetupExplanation(t *testing.T) {
	h := newHarness(t)
	h.m.setOverride("ai-claude-1", func(url string) mgmtReply {
		if strings.HasSuffix(url, "/profile") {
			return okReply(claudeProfileMax5Body)
		}
		return okReply(`{"limits":[{"kind":"weekly_scoped","group":"weekly","percent":100,"resets_at":"` + h.clk.now().Add(48*time.Hour).Format(time.RFC3339) + `","is_active":true,"scope":{"model":{"display_name":"Fable Preview"}}}]}`)
	})
	snap, health, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	cl := acctByID(t, snap, "claude-main.json")
	if len(cl.Windows) != 1 || !adapters.IsUnmatchedScope(cl.Windows[0].Models) {
		t.Fatalf("windows = %+v", cl.Windows)
	}
	if sc := quota.AccountScore(cl, "claude-sonnet-4", t0, time.Hour); sc.Exhausted {
		t.Fatalf("label-only scope exhausted all models: %+v", sc)
	}
	var found bool
	for _, s := range setupFor(health, "claude") {
		if strings.Contains(s.Message, "matches no model") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no explanation in setup: %+v", health.Setup)
	}
}

func profileCalls(h *harness, authIndex string) int {
	n := 0
	for _, c := range h.m.snapshotCalls() {
		if c.AuthIndex == authIndex && strings.HasSuffix(c.URL, "/profile") {
			n++
		}
	}
	return n
}

func TestClaudeProfileCachedForAnHourKeyedByAuthIndex(t *testing.T) {
	h := newHarness(t)
	prev := quota.Snapshot{}
	var snap quota.Snapshot
	for i := 0; i < 3; i++ { // passes at +0, +3m, +6m
		var err error
		if snap, _, err = h.run(prev, testInventory()); err != nil {
			t.Fatal(err)
		}
		prev = snap
		h.clk.advance(3 * time.Minute)
	}
	if n := profileCalls(h, "ai-claude-1"); n != 1 {
		t.Fatalf("profile fetched %d times in 3 passes, want 1", n)
	}
	if n := h.m.countFor("ai-claude-1"); n != 4 { // 3 usage + 1 profile
		t.Fatalf("calls = %d", n)
	}
	cl := acctByID(t, snap, "claude-main.json")
	fh, _ := winByID(cl, "five-hour")
	if cl.Plan != "max5" || cl.PlanSource != "reported" || fh.Capacity == nil || *fh.Capacity != 5 || fh.CapacitySource != "plan_estimate" {
		t.Fatalf("cached plan provenance lost: %+v %+v", cl, fh)
	}
	if !cl.ObservedAt.Equal(h.clk.now().Add(-3 * time.Minute)) {
		t.Fatalf("usage must stay fresh each pass: observedAt %v", cl.ObservedAt)
	}
	// Still cached just under the hour, refetched at the hour.
	h.clk.advance(53*time.Minute + 59*time.Second - 3*time.Minute) // now = +59m59s
	if _, _, err := h.run(prev, testInventory()); err != nil {
		t.Fatal(err)
	}
	if n := profileCalls(h, "ai-claude-1"); n != 1 {
		t.Fatalf("refetched before 1h: %d", n)
	}
	h.clk.advance(time.Second)
	if _, _, err := h.run(prev, testInventory()); err != nil {
		t.Fatal(err)
	}
	if n := profileCalls(h, "ai-claude-1"); n != 2 {
		t.Fatalf("not refetched after 1h: %d", n)
	}
}

func TestClaudeProfileCacheKeyedAndBoundedAndSurvivesRefetchFailure(t *testing.T) {
	c := newCollector(collectorDeps{})
	acct := quota.Account{Plan: "max5", PlanSource: "reported"}
	c.storeProfile("a", acct, t0)
	if _, fresh := c.cachedProfile("b", t0); fresh {
		t.Fatal("cache leaked across auth indexes")
	}
	if got, fresh := c.cachedProfile("a", t0.Add(time.Hour-time.Second)); !fresh || got.Plan != "max5" {
		t.Fatalf("got %+v fresh=%v", got, fresh)
	}
	if got, fresh := c.cachedProfile("a", t0.Add(time.Hour)); fresh || got.Plan != "max5" {
		t.Fatalf("expired entry must be stale but still available: %+v fresh=%v", got, fresh)
	}
	for i := 0; i < maxProfileCacheEntries*2; i++ {
		c.storeProfile(fmt.Sprintf("idx-%d", i), acct, t0.Add(time.Duration(i)*time.Second))
	}
	if len(c.profiles) > maxProfileCacheEntries {
		t.Fatalf("cache unbounded: %d", len(c.profiles))
	}
	c.pruneProfiles(map[string]bool{"idx-511": true})
	if len(c.profiles) != 1 {
		t.Fatalf("prune kept %d", len(c.profiles))
	}

	// Expired cache + failed refetch keeps the reported plan.
	h := newHarness(t)
	first, _, err := h.run(quota.Snapshot{}, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	h.m.setOverride("ai-claude-1", func(url string) mgmtReply {
		if strings.HasSuffix(url, "/profile") {
			return mgmtReply{status: 500, body: "x"}
		}
		return okReply(claudeUsageBody(h.clk.now(), 55, 20))
	})
	h.clk.advance(2 * time.Hour)
	second, _, err := h.run(first, testInventory())
	if err != nil {
		t.Fatal(err)
	}
	if cl := acctByID(t, second, "claude-main.json"); cl.Plan != "max5" || cl.PlanSource != "reported" {
		t.Fatalf("plan lost on refetch failure: %+v", cl)
	}
}
