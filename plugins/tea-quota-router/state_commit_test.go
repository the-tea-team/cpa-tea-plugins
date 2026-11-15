package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

const overridesPath = "/v0/management/plugins/tea-quota-router/overrides"

var stateT0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// stateBase is a raw collected baseline: two claude credentials with their
// own accounts, one codex credential, and the auto pools raw collection builds.
func stateBase() quota.Snapshot {
	used := .2
	win := func() []quota.Window {
		return []quota.Window{{ID: "five-hour", DurationSeconds: 18000, UsedFraction: &used, ObservedAt: stateT0, ResetAt: stateT0.Add(time.Hour)}}
	}
	s := quota.Snapshot{SchemaVersion: quota.SchemaVersion, Revision: 1, GeneratedAt: stateT0,
		Accounts: []quota.Account{
			{ID: "c1", Provider: "claude", Label: "Orig One", BillingType: "subscription", Plan: "pro", PlanSource: "provider", RefreshStatus: "ok", ObservedAt: stateT0, Windows: win(), Financial: []quota.Financial{}},
			{ID: "c2", Provider: "claude", Label: "Orig Two", BillingType: "subscription", Plan: "free", PlanSource: "provider", RefreshStatus: "ok", ObservedAt: stateT0, Windows: win(), Financial: []quota.Financial{}},
			{ID: "x1", Provider: "codex", Label: "Orig Codex", BillingType: "subscription", Plan: "plus", PlanSource: "provider", RefreshStatus: "ok", ObservedAt: stateT0, Windows: win(), Financial: []quota.Financial{}},
		},
		Credentials: []quota.Credential{
			{ID: "c1", AccountID: "c1", Provider: "claude", Label: "Orig One", Models: []string{"m"}},
			{ID: "c2", AccountID: "c2", Provider: "claude", Label: "Orig Two", Models: []string{"m"}},
			{ID: "x1", AccountID: "x1", Provider: "codex", Label: "Orig Codex", Models: []string{"m"}},
		},
		Pools: []quota.Pool{}}
	var setup []setupItem
	s.Pools = buildPools(s, nil, &setup)
	return cloneSnapshot(s)
}

func newStateRuntime(t *testing.T, extraYAML string) (*runtimeState, string, []byte) {
	t.Helper()
	shutdown()
	t.Cleanup(shutdown)
	dir := t.TempDir()
	cfg := []byte("dataDir: " + dir + "\n" + extraYAML)
	p, err := setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir, cfg
}

func restart(t *testing.T, cfg []byte) *runtimeState {
	t.Helper()
	shutdown()
	p, err := setup(cfg)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	return p
}

func seedBase(t *testing.T, p *runtimeState) {
	t.Helper()
	if err := p.applySnapshot(stateBase()); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, p *runtimeState, body string) pluginapi.ManagementResponse {
	t.Helper()
	r, err := p.management(pluginapi.ManagementRequest{Method: http.MethodPut, Path: overridesPath, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// putCode is the status the PUT handler contract (overridesPutResponse) yields
// for a body. management.go is owned elsewhere and must call
// overridesPutResponse to expose 500 vs 400; until it does, the route itself
// reports every failure as 400 and these assertions go through this helper.
func putCode(t *testing.T, p *runtimeState, body string) int {
	t.Helper()
	code, _ := overridesPutResponse(p.saveOverridesResult([]byte(body)))
	return code
}

func getOverrides(t *testing.T, p *runtimeState) overrideDocument {
	t.Helper()
	r, err := p.management(pluginapi.ManagementRequest{Method: http.MethodGet, Path: overridesPath})
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("GET overrides: %d %v", r.StatusCode, err)
	}
	var d overrideDocument
	if err := json.Unmarshal(r.Body, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func failWith(msg string) stateWriteFunc {
	return func(string, StateEnvelope) error { return errors.New(msg) }
}

func setSeams(p *runtimeState, sw stateWriteFunc, ew exportWriteFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sw != nil {
		p.stateWriter = sw
	}
	if ew != nil {
		p.exportWriter = ew
	}
}

func readState(t *testing.T, dir string) StateEnvelope {
	t.Helper()
	env, ok, err := readStateFile(dir)
	if err != nil || !ok {
		t.Fatalf("read state: ok=%v err=%v", ok, err)
	}
	return env
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	s, _ := filepath.Glob(filepath.Join(dir, ".snapshot-*"))
	if len(m)+len(s) != 0 {
		t.Fatalf("temp files left behind: %v %v", m, s)
	}
}

const groupedPut = `{"credentials":{"c1":{"accountId":"shared","label":"Shared","plan":"max","billingType":"api","capacityByWindow":{"five-hour":5}},"c2":{"accountId":"shared"}},"pools":[]}`

func accountOf(t *testing.T, s quota.Snapshot, id string) quota.Account {
	t.Helper()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q missing: %v", id, accountIDs(s))
	return quota.Account{}
}

// 1. A failed authoritative write changes neither memory nor committed state.
func TestFailedAuthoritativeWriteChangesNothing(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	seedBase(t, p)
	if r := put(t, p, `{"credentials":{"c1":{"label":"Before"}},"pools":[]}`); r.StatusCode != 200 {
		t.Fatalf("seed PUT: %d %s", r.StatusCode, r.Body)
	}
	stateBefore := fileBytes(t, filepath.Join(dir, stateFileName))
	exportBefore := fileBytes(t, filepath.Join(dir, exportSnapshotName))
	legacyBefore := fileBytes(t, filepath.Join(dir, exportOverridesName))
	viewBefore, baseBefore, docBefore := jsonOf(t, p.snapshotCopy()), jsonOf(t, p.baseCopy()), jsonOf(t, p.overrideDoc())
	p.mu.RLock()
	genBefore := p.generation
	p.mu.RUnlock()

	same := func(what string) {
		t.Helper()
		if got := jsonOf(t, p.snapshotCopy()); got != viewBefore {
			t.Fatalf("%s changed memory view", what)
		}
		if got := jsonOf(t, p.baseCopy()); got != baseBefore {
			t.Fatalf("%s changed memory base", what)
		}
		if got := jsonOf(t, p.overrideDoc()); got != docBefore {
			t.Fatalf("%s changed memory overrides", what)
		}
		p.mu.RLock()
		g, cfgOv := p.generation, p.cfg.AccountOverrides["c1"].Label
		p.mu.RUnlock()
		if g != genBefore || cfgOv != "Before" {
			t.Fatalf("%s changed generation/cfg: %d %q", what, g, cfgOv)
		}
		for name, want := range map[string][]byte{stateFileName: stateBefore, exportSnapshotName: exportBefore, exportOverridesName: legacyBefore} {
			if got := fileBytes(t, filepath.Join(dir, name)); string(got) != string(want) {
				t.Fatalf("%s changed %s on disk", what, name)
			}
		}
		noTempFiles(t, dir)
	}

	t.Run("injected writer, PUT", func(t *testing.T) {
		setSeams(p, failWith("disk full"), nil)
		if r := put(t, p, groupedPut); r.StatusCode == 200 {
			t.Fatal("failed commit reported as success")
		}
		if c := putCode(t, p, groupedPut); c != 500 {
			t.Fatalf("PUT status %d, want 500 (commit failure, not a client error)", c)
		}
		if _, err := p.saveOverridesResult([]byte(groupedPut)); !errors.Is(err, errStateCommit) {
			t.Fatalf("err = %v", err)
		}
		same("failed PUT")
	})
	t.Run("injected writer, collector publish", func(t *testing.T) {
		raw := stateBase()
		raw.Revision = 50
		if err := p.publishCollected(raw); !errors.Is(err, errStateCommit) {
			t.Fatalf("publish err = %v", err)
		}
		if err := p.applySnapshot(raw); !errors.Is(err, errStateCommit) {
			t.Fatalf("apply err = %v", err)
		}
		same("failed publish")
	})
	t.Run("real filesystem failure: state path is a non-empty directory", func(t *testing.T) {
		setSeams(p, writeStateFile, nil)
		statePath := filepath.Join(dir, stateFileName)
		if err := os.Remove(statePath); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(statePath, "blocker"), 0o700); err != nil {
			t.Fatal(err)
		}
		if c := putCode(t, p, groupedPut); c != 500 {
			t.Fatalf("PUT status %d, want 500", c)
		}
		if got := jsonOf(t, p.snapshotCopy()); got != viewBefore {
			t.Fatal("memory changed after real write failure")
		}
		if got := jsonOf(t, p.overrideDoc()); got != docBefore {
			t.Fatal("overrides changed after real write failure")
		}
		if got := fileBytes(t, filepath.Join(dir, exportSnapshotName)); string(got) != string(exportBefore) {
			t.Fatal("export written even though the authoritative commit failed")
		}
		noTempFiles(t, dir)
	})
}

func TestInvalidWriteRejectedBeforeAnyCommit(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	seedBase(t, p)
	before := fileBytes(t, filepath.Join(dir, stateFileName))
	for name, body := range map[string]string{
		"unknown field":    `{"credentials":{},"pools":[],"x":1}`,
		"missing pools":    `{"credentials":{}}`,
		"trailing data":    `{"credentials":{},"pools":[]} {}`,
		"negative cap":     `{"credentials":{"c1":{"capacityByWindow":{"five-hour":-1}}},"pools":[]}`,
		"duplicate pool":   `{"credentials":{},"pools":[{"id":"p","provider":"claude","credentialIds":["c1"]},{"id":"p","provider":"claude","credentialIds":["c2"]}]}`,
		"not json":         `nope`,
		"empty credential": `{"credentials":{" ":{"label":"x"}},"pools":[]}`,
	} {
		if r := put(t, p, body); r.StatusCode != 400 {
			t.Fatalf("%s: status %d", name, r.StatusCode)
		}
	}
	if string(fileBytes(t, filepath.Join(dir, stateFileName))) != string(before) {
		t.Fatal("rejected request touched state")
	}
}

// 2. Export failure after a successful commit: committed config + degraded export.
func TestSharedExportFailureReportsCommittedConfigDegraded(t *testing.T) {
	p, dir, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	oldExport := fileBytes(t, filepath.Join(dir, exportSnapshotName))
	exportErr := errors.New("export disk gone")
	setSeams(p, nil, func(string, quota.Snapshot, overrideDocument) error { return exportErr })

	res, err := p.saveOverridesResult([]byte(groupedPut))
	if err != nil {
		t.Fatalf("committed write reported as failed: %v", err)
	}
	if !res.ExportDegraded || res.ExportError == "" || res.Generation == 0 {
		t.Fatalf("commit result = %+v", res)
	}
	code, body := overridesPutResponse(res, nil)
	if code != 200 || body["status"] != "saved_export_degraded" {
		t.Fatalf("response = %d %v", code, body)
	}
	if strings.Contains(jsonOf(t, body), "export disk gone") {
		t.Fatal("raw export error leaked into the response")
	}
	// The management route must not turn this into a failed request.
	r := put(t, p, `{"credentials":{"c1":{"accountId":"shared","label":"Shared","plan":"max","billingType":"api","capacityByWindow":{"five-hour":5}},"c2":{"accountId":"shared"}},"pools":[]}`)
	if r.StatusCode != 200 {
		t.Fatalf("PUT with degraded export returned %d %s", r.StatusCode, r.Body)
	}

	// Authoritative state carries the new config; the shared export is stale.
	env := readState(t, dir)
	if env.Overrides.Credentials["c1"].AccountID != "shared" || accountOf(t, env.Snapshot, "shared").Plan != "max" {
		t.Fatalf("committed state missing override: %+v", env.Overrides)
	}
	if string(fileBytes(t, filepath.Join(dir, exportSnapshotName))) != string(oldExport) {
		t.Fatal("export unexpectedly changed")
	}
	if got := getOverrides(t, p); got.Credentials["c1"].AccountID != "shared" {
		t.Fatalf("GET readback = %+v", got)
	}
	if st := p.exportState(); !st.Degraded || st.Error == "" {
		t.Fatalf("export status = %+v", st)
	}

	// Degradation is visible through the summary health.
	sr, err := p.management(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/tea-quota-router/summary"})
	if err != nil || sr.StatusCode != 200 {
		t.Fatalf("summary %d %v", sr.StatusCode, err)
	}
	var sum struct {
		Health struct {
			CollectorStatus string `json:"collectorStatus"`
			LastError       string `json:"lastError"`
		} `json:"health"`
	}
	if err := json.Unmarshal(sr.Body, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Health.CollectorStatus != "error" || sum.Health.LastError != exportDegradedMessage {
		t.Fatalf("degraded export not visible in health: %+v", sum.Health)
	}

	// A later successful export clears the degradation and the health overlay.
	setSeams(p, nil, writeDerivedExports)
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if st := p.exportState(); st.Degraded {
		t.Fatalf("still degraded: %+v", st)
	}
	p.mu.RLock()
	h := p.control.health
	p.mu.RUnlock()
	if h.LastError != "" || h.Status == "error" {
		t.Fatalf("health overlay not cleared: %+v", h)
	}

	// Degrade again, then restart: the derived export is repaired from state.
	setSeams(p, nil, func(string, quota.Snapshot, overrideDocument) error { return exportErr })
	if _, err := p.saveOverridesResult([]byte(`{"credentials":{"c1":{"label":"Final"}},"pools":[]}`)); err != nil {
		t.Fatal(err)
	}
	stale, _ := quota.ReadSnapshot(filepath.Join(dir, exportSnapshotName))
	if strings.Contains(jsonOf(t, stale), "Final") {
		t.Fatal("export should be stale before restart")
	}
	p = restart(t, cfg)
	fixed, err := quota.ReadSnapshot(filepath.Join(dir, exportSnapshotName))
	if err != nil || accountOf(t, fixed, "c1").Label != "Final" {
		t.Fatalf("restart did not repair export: %v", err)
	}
	if jsonOf(t, fixed) != jsonOf(t, p.snapshotCopy()) {
		t.Fatal("repaired export differs from committed view")
	}
	if !strings.Contains(string(fileBytes(t, filepath.Join(dir, exportOverridesName))), `"Final"`) {
		t.Fatal("legacy overrides.json copy not repaired")
	}
}

func TestExportFailureRealFilesystem(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	seedBase(t, p)
	exp := filepath.Join(dir, exportSnapshotName)
	if err := os.Remove(exp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(exp, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := put(t, p, groupedPut)
	if r.StatusCode != 200 {
		t.Fatalf("status %d %s", r.StatusCode, r.Body)
	}
	if !p.exportState().Degraded {
		t.Fatal("export not marked degraded")
	}
	if readState(t, dir).Overrides.Credentials["c1"].AccountID != "shared" {
		t.Fatal("state not committed")
	}
	noTempFiles(t, dir)
}

// 3. Restart repairs the derived export no matter how it was damaged.
func TestRestartRepairsDerivedExport(t *testing.T) {
	for name, damage := range map[string]func(string){
		"missing":   func(f string) { _ = os.Remove(f) },
		"corrupt":   func(f string) { _ = os.WriteFile(f, []byte("{not json"), 0o600) },
		"old valid": func(f string) { _ = quota.WriteSnapshot(f, stateBase()) },
	} {
		t.Run(name, func(t *testing.T) {
			p, dir, cfg := newStateRuntime(t, "")
			seedBase(t, p)
			if r := put(t, p, groupedPut); r.StatusCode != 200 {
				t.Fatal(string(r.Body))
			}
			want := jsonOf(t, p.snapshotCopy())
			damage(filepath.Join(dir, exportSnapshotName))
			_ = os.Remove(filepath.Join(dir, exportOverridesName))
			p = restart(t, cfg)
			got, err := quota.ReadSnapshot(filepath.Join(dir, exportSnapshotName))
			if err != nil || jsonOf(t, got) != want || jsonOf(t, p.snapshotCopy()) != want {
				t.Fatalf("not repaired: %v", err)
			}
			if _, err := osReadOverride(dir); err != nil {
				t.Fatalf("overrides.json not repaired: %v", err)
			}
		})
	}
}

func TestCorruptStateFailsClosed(t *testing.T) {
	p, dir, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	shutdown()
	statePath := filepath.Join(dir, stateFileName)
	good := fileBytes(t, statePath)
	for name, bad := range map[string][]byte{
		"garbage":       []byte("{"),
		"wrong schema":  []byte(strings.Replace(string(good), `"schemaVersion":1,"generation"`, `"schemaVersion":9,"generation"`, 1)),
		"unknown field": []byte(strings.Replace(string(good), `{"schemaVersion"`, `{"extra":1,"schemaVersion"`, 1)),
	} {
		if err := os.WriteFile(statePath, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := setup(cfg); err == nil {
			shutdown()
			t.Fatalf("%s: corrupt state accepted", name)
		}
	}
	// A valid, intact state file still loads (and nothing was clobbered).
	if err := os.WriteFile(statePath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setup(cfg); err != nil {
		t.Fatal(err)
	}
}

// 4. A collector pass that started before an override PUT cannot overwrite it.
func TestOldCollectorPassCannotOverwriteNewOverrides(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	inv := func(ctx context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		if calls.Add(1) == 2 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return nil, []pluginapi.HostAuthModels{{ID: "openai-compatibility:demo:hash", AuthIndex: "idx", Provider: "openai-compatible-demo", Models: []string{"m"}}}, nil
	}
	p.startCollector(inv)
	waitFor(t, func() bool { return len(p.snapshotCopy().Credentials) == 1 })
	cred := p.snapshotCopy().Credentials[0]
	origAccount := cred.AccountID

	p.triggerRefresh()
	<-started // pass 2 is in flight with a configuration captured before the PUT

	body := `{"credentials":{"` + cred.ID + `":{"accountId":"operator-acct","label":"Operator"}},"pools":[]}`
	if r := put(t, p, body); r.StatusCode != 200 {
		t.Fatalf("PUT %d %s", r.StatusCode, r.Body)
	}
	baseRevAtPut := p.baseCopy().Revision
	close(release)
	waitFor(t, func() bool { return p.baseCopy().Revision > baseRevAtPut })

	view := p.snapshotCopy()
	if len(view.Credentials) != 1 || view.Credentials[0].AccountID != "operator-acct" || view.Credentials[0].Label != "Operator" {
		t.Fatalf("stale pass rolled back overrides: %+v", view.Credentials)
	}
	if accountOf(t, view, "operator-acct").Label != "Operator" {
		t.Fatalf("account lost override: %+v", view.Accounts)
	}
	env := readState(t, dir)
	if jsonOf(t, env.Snapshot) != jsonOf(t, view) || env.Overrides.Credentials[cred.ID].AccountID != "operator-acct" {
		t.Fatal("committed state does not carry current overrides")
	}
	// The raw base never contains operator identity.
	if b := p.baseCopy(); b.Credentials[0].AccountID != origAccount {
		t.Fatalf("raw base polluted by override: %+v", b.Credentials[0])
	}
	// And removal restores the original identity after the collector merged.
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if got := p.snapshotCopy().Credentials[0]; got.AccountID != origAccount || got.Label == "Operator" {
		t.Fatalf("removal did not restore: %+v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestCollectorHooksUseRawBaseline(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	cc := p.collectorConfig()
	if len(cc.AccountOverrides) != 0 || len(cc.Pools) != 0 {
		t.Fatalf("collector config carries overrides: %+v %+v", cc.AccountOverrides, cc.Pools)
	}
	if p.configCopy().AccountOverrides["c1"].AccountID != "shared" {
		t.Fatal("committed overrides should still be mirrored in cfg")
	}
	raw := p.baseCopy()
	if accountOf(t, raw, "c1").Plan != "pro" || len(raw.Credentials) != 3 || raw.Credentials[0].AccountID != "c1" {
		t.Fatalf("Previous is not the raw base: %+v", raw.Accounts)
	}
	if view := p.snapshotCopy(); len(view.Accounts) != 2 {
		t.Fatalf("view not merged: %v", accountIDs(view))
	}
}

func TestStalePublishRejectedAndOldRawGetsCurrentOverrides(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	seedBase(t, p)
	oldRaw := p.baseCopy()
	oldRaw.Revision = p.baseCopy().Revision + 1
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if err := p.publishCollected(oldRaw); err != nil {
		t.Fatal(err)
	}
	if got := accountOf(t, p.snapshotCopy(), "shared"); got.Plan != "max" || got.Label != "Shared" {
		t.Fatalf("publish dropped current overrides: %+v", got)
	}
	gen := readState(t, dir).Generation
	if err := p.publishCollected(oldRaw); !errors.Is(err, errStaleCollection) {
		t.Fatalf("same-revision publish accepted: %v", err)
	}
	if readState(t, dir).Generation != gen {
		t.Fatal("stale publish committed")
	}
}

// 5. Removing an override restores the original identities.
func TestRemovingOverrideRestoresOriginalIdentities(t *testing.T) {
	p, _, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	base := p.baseCopy()

	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	view := p.snapshotCopy()
	sh := accountOf(t, view, "shared")
	if len(view.Accounts) != 2 || sh.Plan != "max" || sh.PlanSource != "operator_override" || sh.BillingType != "api" || sh.Label != "Shared" ||
		sh.Windows[0].Capacity == nil || *sh.Windows[0].Capacity != 5 {
		t.Fatalf("override not applied: %+v", sh)
	}
	foundAuto := false
	for _, pl := range view.Pools {
		if pl.ID == "auto-claude-api" {
			foundAuto = true
		}
		if pl.ID == "auto-claude-subscription" {
			t.Fatal("auto pool not recomputed after billing override")
		}
	}
	if !foundAuto {
		t.Fatalf("pools = %+v", view.Pools)
	}

	check := func(p *runtimeState, what string) {
		t.Helper()
		got := p.snapshotCopy()
		if jsonOf(t, got.Accounts) != jsonOf(t, base.Accounts) || jsonOf(t, got.Credentials) != jsonOf(t, base.Credentials) {
			t.Fatalf("%s: not restored\n got %s\nwant %s", what, jsonOf(t, got.Accounts), jsonOf(t, base.Accounts))
		}
		if jsonOf(t, got.Pools) != jsonOf(t, base.Pools) {
			t.Fatalf("%s: pools %s != %s", what, jsonOf(t, got.Pools), jsonOf(t, base.Pools))
		}
	}
	// Restart in the overridden state, then remove: identities still restore.
	p = restart(t, cfg)
	if len(p.snapshotCopy().Accounts) != 2 {
		t.Fatal("override lost on restart")
	}
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	check(p, "after restart + removal")

	// Partial removal: drop only c2's grouping.
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if r := put(t, p, `{"credentials":{"c1":{"label":"Only Label"}},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	v := p.snapshotCopy()
	if accountOf(t, v, "c1").Label != "Only Label" || accountOf(t, v, "c1").Plan != "pro" || accountOf(t, v, "c2").Label != "Orig Two" || accountOf(t, v, "c1").BillingType != "subscription" {
		t.Fatalf("partial removal wrong: %+v", v.Accounts)
	}
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	check(p, "after second removal")
}

func TestOperatorPoolsAppliedAndRemoved(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	base := p.baseCopy()
	if r := put(t, p, `{"credentials":{},"pools":[{"id":"mine","label":"Mine","provider":"claude","credentialIds":["c1","c2"],"billingType":""}]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	found := false
	for _, pl := range p.snapshotCopy().Pools {
		if pl.ID == "mine" && len(pl.CredentialIDs) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("pool missing: %+v", p.snapshotCopy().Pools)
	}
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if jsonOf(t, p.snapshotCopy().Pools) != jsonOf(t, base.Pools) {
		t.Fatalf("pools not restored: %+v", p.snapshotCopy().Pools)
	}
}

// 6. Cross-provider grouping and conflicting overrides are rejected.
func TestCrossProviderAndConflictingOverridesRejected(t *testing.T) {
	p, dir, _ := newStateRuntime(t, "")
	seedBase(t, p)
	if r := put(t, p, `{"credentials":{"c1":{"label":"Keep"}},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	stateBefore := fileBytes(t, filepath.Join(dir, stateFileName))
	viewBefore := jsonOf(t, p.snapshotCopy())
	for name, body := range map[string]string{
		"cross-provider group":     `{"credentials":{"c1":{"accountId":"mix"},"x1":{"accountId":"mix"}},"pools":[]}`,
		"cross-provider onto base": `{"credentials":{"x1":{"accountId":"c1"}},"pools":[]}`,
		"conflicting plan":         `{"credentials":{"c1":{"accountId":"s","plan":"a"},"c2":{"accountId":"s","plan":"b"}},"pools":[]}`,
		"conflicting label":        `{"credentials":{"c1":{"accountId":"s","label":"A"},"c2":{"accountId":"s","label":"B"}},"pools":[]}`,
		"conflicting billing":      `{"credentials":{"c1":{"accountId":"s","billingType":"api"},"c2":{"accountId":"s","billingType":"subscription"}},"pools":[]}`,
		"conflicting capacity":     `{"credentials":{"c1":{"accountId":"s","capacityByWindow":{"five-hour":1}},"c2":{"accountId":"s","capacityByWindow":{"five-hour":2}}},"pools":[]}`,
		"pool foreign provider":    `{"credentials":{},"pools":[{"id":"bad","provider":"claude","credentialIds":["x1"]}]}`,
	} {
		r := put(t, p, body)
		if r.StatusCode != 400 {
			t.Fatalf("%s: status %d %s", name, r.StatusCode, r.Body)
		}
		if _, err := p.saveOverridesResult([]byte(body)); !errors.Is(err, errInvalidOverrides) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if string(fileBytes(t, filepath.Join(dir, stateFileName))) != string(stateBefore) || jsonOf(t, p.snapshotCopy()) != viewBefore {
		t.Fatal("rejected override changed state")
	}
	// Equal (non-conflicting) values for one account are accepted.
	ok := `{"credentials":{"c1":{"accountId":"s","plan":"a","capacityByWindow":{"five-hour":3}},"c2":{"accountId":"s","plan":"a","capacityByWindow":{"five-hour":3}}},"pools":[]}`
	if r := put(t, p, ok); r.StatusCode != 200 {
		t.Fatalf("compatible overrides rejected: %s", r.Body)
	}
}

// Stale persisted overrides must not wedge the collector: non-strict apply
// keeps the credential in its own provider-suffixed account.
func TestNonStrictMaterializeNeverWedgesOnStaleOverrides(t *testing.T) {
	ov := overrideDocument{Credentials: map[string]AccountOverride{"c1": {AccountID: "mix"}, "x1": {AccountID: "mix"}}, Pools: []quota.Pool{{ID: "gone", Provider: "claude", CredentialIDs: []string{"ghost"}}}}
	view, warns, err := materializeView(stateBase(), ov, false)
	if err != nil || len(warns) == 0 {
		t.Fatalf("err=%v warns=%v", err, warns)
	}
	if quota.Validate(view) != nil {
		t.Fatal("invalid view")
	}
	if _, _, err := materializeView(stateBase(), ov, true); !errors.Is(err, errInvalidOverrides) {
		t.Fatalf("strict err = %v", err)
	}
}

// 7. GET readback survives restart; state-v1.json is the sole truth.
func TestGETReadbackSurvivesRestartStateIsSoleTruth(t *testing.T) {
	p, dir, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	want := jsonOf(t, getOverrides(t, p))
	if !strings.Contains(want, `"shared"`) {
		t.Fatal(want)
	}
	// Legacy derived file is wiped or poisoned: state still wins.
	for _, poison := range []func(){
		func() { _ = os.Remove(filepath.Join(dir, exportOverridesName)) },
		func() {
			_ = os.WriteFile(filepath.Join(dir, exportOverridesName), []byte(`{"credentials":{"c1":{"label":"EVIL"}},"pools":[]}`), 0o600)
		},
		func() { _ = os.WriteFile(filepath.Join(dir, exportOverridesName), []byte("garbage"), 0o600) },
	} {
		poison()
		p = restart(t, cfg)
		if got := jsonOf(t, getOverrides(t, p)); got != want {
			t.Fatalf("readback after restart\n got %s\nwant %s", got, want)
		}
		if len(p.snapshotCopy().Accounts) != 2 {
			t.Fatal("view lost after restart")
		}
		// restart repaired the derived copy from state
		b, err := osReadOverride(dir)
		if err != nil || strings.Contains(string(b), "EVIL") || strings.Contains(string(b), "garbage") {
			t.Fatalf("legacy copy not repaired: %s %v", b, err)
		}
	}
	// YAML-supplied overrides cannot displace committed state.
	p = restart(t, append(cfg, []byte("accountOverrides:\n  c1:\n    label: FromYAML\n")...))
	if got := jsonOf(t, getOverrides(t, p)); got != want {
		t.Fatalf("yaml displaced committed state: %s", got)
	}
	// Generation survives restarts and keeps increasing.
	g := readState(t, dir).Generation
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if readState(t, dir).Generation != g+1 {
		t.Fatal("generation not monotonic across restart")
	}
}

func TestRevisionMonotonicAcrossOverrideAndRestart(t *testing.T) {
	p, _, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	last := p.snapshotCopy().Revision
	for _, body := range []string{groupedPut, `{"credentials":{},"pools":[]}`, groupedPut} {
		if r := put(t, p, body); r.StatusCode != 200 {
			t.Fatal(string(r.Body))
		}
		if rev := p.snapshotCopy().Revision; rev <= last {
			t.Fatalf("revision %d not above %d", rev, last)
		} else {
			last = rev
		}
	}
	p = restart(t, cfg)
	if p.snapshotCopy().Revision != last {
		t.Fatalf("revision changed across restart: %d != %d", p.snapshotCopy().Revision, last)
	}
}

// Legacy files migrate exactly once and removal still restores identities.
func TestLegacyFilesMigrateOnce(t *testing.T) {
	shutdown()
	t.Cleanup(shutdown)
	dir := t.TempDir()
	cfg := []byte("dataDir: " + dir + "\n")
	used := .3
	cp := 5.0
	merged := quota.Snapshot{SchemaVersion: 1, Revision: 7, GeneratedAt: stateT0,
		Accounts: []quota.Account{{ID: "shared", Provider: "claude", Label: "Shared", BillingType: "api", Plan: "max", PlanSource: "operator_override", RefreshStatus: "ok", ObservedAt: stateT0,
			Windows: []quota.Window{{ID: "five-hour", DurationSeconds: 18000, UsedFraction: &used, Capacity: &cp, CapacitySource: "operator_override", ObservedAt: stateT0}}, Financial: []quota.Financial{}},
			{ID: "x1", Provider: "codex", Label: "Codex", BillingType: "subscription", RefreshStatus: "ok", ObservedAt: stateT0, Windows: []quota.Window{}, Financial: []quota.Financial{}}},
		Credentials: []quota.Credential{{ID: "c1", AccountID: "shared", Provider: "claude", Label: "Shared", Models: []string{"m"}}, {ID: "c2", AccountID: "shared", Provider: "claude", Label: "Orig Two", Models: []string{"m"}}, {ID: "x1", AccountID: "x1", Provider: "codex", Label: "Codex", Models: []string{"m"}}},
		Pools:       []quota.Pool{}}
	if err := quota.WriteSnapshot(filepath.Join(dir, exportSnapshotName), merged); err != nil {
		t.Fatal(err)
	}
	legacy := `{"credentials":{"c1":{"accountId":"shared","label":"Shared","plan":"max","billingType":"api","capacityByWindow":{"five-hour":5}},"c2":{"accountId":"shared"}},"pools":[]}`
	if err := os.WriteFile(filepath.Join(dir, exportOverridesName), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	env := readState(t, dir)
	if env.Generation == 0 || env.Overrides.Credentials["c1"].AccountID != "shared" {
		t.Fatalf("not migrated: %+v", env.Overrides)
	}
	if len(p.snapshotCopy().Accounts) != 2 || accountOf(t, p.snapshotCopy(), "shared").Plan != "max" {
		t.Fatalf("migrated view wrong: %v", accountIDs(p.snapshotCopy()))
	}
	// Later edits to the legacy file are ignored: state is sole truth.
	shutdown()
	if err := os.WriteFile(filepath.Join(dir, exportOverridesName), []byte(`{"credentials":{},"pools":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.snapshotCopy().Accounts) != 2 {
		t.Fatal("legacy file re-imported after migration")
	}
	// Removal restores per-credential identities; baked plan/capacity cleared.
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	v := p.snapshotCopy()
	if len(v.Accounts) != 3 {
		t.Fatalf("identities not restored: %v", accountIDs(v))
	}
	for _, id := range []string{"c1", "c2"} {
		a := accountOf(t, v, id)
		if a.Plan != "" || a.PlanSource != "" || a.Windows[0].Capacity != nil || a.BillingType != "unknown" || a.Label == "Shared" {
			t.Fatalf("baked override survived removal: %+v", a)
		}
	}
	if v.Credentials[0].AccountID != "c1" || v.Credentials[1].AccountID != "c2" {
		t.Fatalf("credential accounts: %+v", v.Credentials)
	}
}

func TestLegacyInvalidOverridesFailSetup(t *testing.T) {
	shutdown()
	t.Cleanup(shutdown)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exportOverridesName), []byte(`{"credentials":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := setup([]byte("dataDir: " + dir + "\n")); err == nil {
		t.Fatal("invalid legacy overrides accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); err == nil {
		t.Fatal("state written from invalid legacy input")
	}
}

// Fixture mode: base is the fixture; mode/path changes need a restart.
func TestFixtureModeBaseOverridesAndRestartOnlyModeChange(t *testing.T) {
	shutdown()
	t.Cleanup(shutdown)
	dir := t.TempDir()
	fx := filepath.Join(dir, "fixture.json")
	if err := quota.WriteSnapshot(fx, stateBase()); err != nil {
		t.Fatal(err)
	}
	cfg := []byte("dataDir: " + filepath.Join(dir, "data") + "\nfixtureSnapshotPath: " + fx + "\n")
	p, err := setup(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if jsonOf(t, p.baseCopy()) != jsonOf(t, stateBase()) {
		t.Fatal("fixture is not the base")
	}
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if len(p.snapshotCopy().Accounts) != 2 {
		t.Fatal("override not applied over fixture")
	}
	// Fixture refresh re-reads the base but keeps operator overrides.
	if err := p.loadFixture(); err != nil {
		t.Fatal(err)
	}
	if len(p.snapshotCopy().Accounts) != 2 {
		t.Fatal("fixture refresh dropped overrides")
	}
	// Explicit mode/path changes are rejected until restart.
	other := filepath.Join(dir, "other.json")
	if err := quota.WriteSnapshot(other, stateBase()); err != nil {
		t.Fatal(err)
	}
	for name, y := range map[string]string{
		"other fixture":    "fixtureSnapshotPath: " + other + "\n",
		"fixture disabled": "",
	} {
		if _, err := setup([]byte("dataDir: " + filepath.Join(dir, "data") + "\n" + y)); err == nil || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// Same path reconfigure is fine and keeps committed overrides.
	if p2, err := setup(append(cfg, []byte("maxAgeSeconds: 900\n")...)); err != nil || p2 != p || len(p2.snapshotCopy().Accounts) != 2 || p2.configCopy().MaxAgeSeconds != 900 {
		t.Fatalf("reconfigure: %v", err)
	}
	// Restart: overrides persist, base follows the (edited) fixture again.
	edited := stateBase()
	edited.Accounts[0].Label = "Fixture Edited"
	edited.Credentials[0].Label = "Fixture Edited"
	if err := quota.WriteSnapshot(fx, edited); err != nil {
		t.Fatal(err)
	}
	shutdown()
	if p, err = setup(cfg); err != nil {
		t.Fatal(err)
	}
	if p.baseCopy().Accounts[0].Label != "Fixture Edited" || len(p.snapshotCopy().Accounts) != 2 {
		t.Fatalf("fixture restart: %+v", p.baseCopy().Accounts[0])
	}
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	if got := accountOf(t, p.snapshotCopy(), "c1"); got.Label != "Fixture Edited" || got.Plan != "pro" {
		t.Fatalf("removal in fixture mode: %+v", got)
	}
}

func TestFixtureModeInvalidFixtureFailsSetup(t *testing.T) {
	shutdown()
	t.Cleanup(shutdown)
	dir := t.TempDir()
	if _, err := setup([]byte("dataDir: " + dir + "\nfixtureSnapshotPath: " + filepath.Join(dir, "missing.json") + "\n")); err == nil {
		t.Fatal("missing fixture accepted")
	}
}

// Collector publish failure keeps committed state and surfaces in health.
func TestCollectorPublishFailureKeepsStateAndReportsHealth(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	view := jsonOf(t, p.snapshotCopy())
	setSeams(p, failWith("boom"), nil)
	raw := stateBase()
	raw.Revision = 99
	p.publishHealth(raw, collectorHealth{Status: healthOK}, nil)
	if jsonOf(t, p.snapshotCopy()) != view {
		t.Fatal("failed publish changed view")
	}
	p.mu.RLock()
	h := p.control.health
	p.mu.RUnlock()
	if h.Status != "error" || h.LastError != "snapshot persistence failed" {
		t.Fatalf("health = %+v", h)
	}
	// A stale pass is dropped silently without flagging an error.
	setSeams(p, writeStateFile, nil)
	stale := stateBase()
	p.publishHealth(stale, collectorHealth{Status: healthOK}, nil)
	p.mu.RLock()
	h = p.control.health
	p.mu.RUnlock()
	if h.Status != healthOK {
		t.Fatalf("stale pass flagged: %+v", h)
	}
}

func TestOverridesPutResponseMapping(t *testing.T) {
	if c, _ := overridesPutResponse(commitResult{}, errors.Join(errInvalidOverrides, errors.New("x"))); c != 400 {
		t.Fatal(c)
	}
	if c, b := overridesPutResponse(commitResult{}, errors.New("wrapped: "+errStateCommit.Error())); c != 400 || b["error"] == nil {
		t.Fatalf("%d %v", c, b)
	}
	if c, _ := overridesPutResponse(commitResult{}, errStateCommit); c != 500 {
		t.Fatal(c)
	}
	if c, b := overridesPutResponse(commitResult{Generation: 3}, nil); c != 200 || b["status"] != "saved" {
		t.Fatalf("%d %v", c, b)
	}
}

// Concurrent PUTs and collector publishes keep memory and disk identical.
func TestConcurrentOverridesAndPublishesStayConsistent(t *testing.T) {
	p, dir, cfg := newStateRuntime(t, "")
	seedBase(t, p)
	var wg sync.WaitGroup
	bodies := []string{groupedPut, `{"credentials":{},"pools":[]}`, `{"credentials":{"c1":{"label":"L"}},"pools":[]}`}
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				_ = p.saveOverrides([]byte(bodies[(g+i)%len(bodies)]))
				_ = p.snapshotCopy()
				_ = p.overrideDoc()
			}
		}(g)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				raw := p.baseCopy()
				raw.Revision++
				_ = p.publishCollected(raw)
			}
		}()
	}
	wg.Wait()
	env := readState(t, dir)
	p.mu.RLock()
	gen := p.generation
	p.mu.RUnlock()
	if env.Generation != gen || jsonOf(t, env.Snapshot) != jsonOf(t, p.snapshotCopy()) || jsonOf(t, env.BaseSnapshot) != jsonOf(t, p.baseCopy()) || !reflect.DeepEqual(env.Overrides, p.overrideDoc()) {
		t.Fatal("memory and committed state diverged")
	}
	want := jsonOf(t, p.snapshotCopy())
	p = restart(t, cfg)
	if jsonOf(t, p.snapshotCopy()) != want {
		t.Fatal("restart changed the view")
	}
	noTempFiles(t, dir)
}
