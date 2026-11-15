package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// setupWithin runs setup and fails the test (instead of hanging the suite)
// when it does not return in time. On timeout the FIFO at unblock, if any, is
// opened read-write so the stuck reader can finish and release runtimeMu.
func setupWithin(t *testing.T, cfg []byte, unblock string) error {
	t.Helper()
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() {
		_, err := setup(cfg)
		ch <- res{err}
	}()
	select {
	case r := <-ch:
		return r.err
	case <-time.After(3 * time.Second):
		if unblock != "" {
			if f, err := os.OpenFile(unblock, os.O_RDWR, 0); err == nil {
				_ = f.Close()
			}
			<-ch
		}
		t.Fatal("setup blocked on a non-regular file")
		return nil
	}
}

func hazardDir(t *testing.T) string {
	t.Helper()
	shutdown()
	t.Cleanup(shutdown)
	return t.TempDir()
}

// ---- 1. secure state/override readers ------------------------------------

func TestStateFileFIFORejectedQuickly(t *testing.T) {
	dir := hazardDir(t)
	fifo := filepath.Join(dir, stateFileName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setupWithin(t, []byte("dataDir: "+dir+"\n"), fifo); err == nil {
		t.Fatal("FIFO state accepted")
	}
}

func TestLegacyOverridesFIFORejectedQuickly(t *testing.T) {
	dir := hazardDir(t)
	fifo := filepath.Join(dir, exportOverridesName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setupWithin(t, []byte("dataDir: "+dir+"\n"), fifo); err == nil {
		t.Fatal("FIFO overrides accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); err == nil {
		t.Fatal("state written after rejected legacy input")
	}
}

func TestSecureReadersRejectSymlinkModeAndSize(t *testing.T) {
	dir := t.TempDir()
	valid := `{"credentials":{},"pools":[]}`
	cases := []struct {
		name string
		read func(dir string) error
		file string
		body string
		max  int64
	}{
		{"state", func(d string) error { _, _, err := readStateFile(d); return err }, stateFileName, "{}", maxStateBytes},
		{"legacy", func(d string) error { _, _, err := readLegacyOverrides(d); return err }, exportOverridesName, valid, maxOverrideBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := filepath.Join(dir, c.name)
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(d, c.file)
			// symlink to a perfectly good file is refused, never followed
			target := filepath.Join(d, "target")
			if err := os.WriteFile(target, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			err := c.read(d)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("symlink: err = %v", err)
			}
			_ = os.Remove(path)
			// group/world writable
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, m := range []os.FileMode{0o620, 0o602, 0o666} {
				if err := os.Chmod(path, m); err != nil {
					t.Fatal(err)
				}
				if err := c.read(d); err == nil {
					t.Fatalf("mode %o accepted", m)
				}
			}
			// oversized sparse file rejected from fstat size
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, c.max+1); err != nil {
				t.Fatal(err)
			}
			if err := c.read(d); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("oversized: err = %v", err)
			}
			// a directory in place of the file is not regular
			_ = os.Remove(path)
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := c.read(d); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("directory: err = %v", err)
			}
			_ = os.Remove(path)
			// missing stays "not exist" (not an error for the caller)
			if err := c.read(d); err != nil {
				t.Fatalf("missing: %v", err)
			}
		})
	}
}

type fakeInfo struct {
	mode os.FileMode
	uid  uint32
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 1 }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Uid: f.uid} }

func TestSecureFileOwnerCheck(t *testing.T) {
	euid := uint32(os.Geteuid())
	if err := checkSecureFileInfo(fakeInfo{mode: 0o600, uid: euid}); err != nil {
		t.Fatalf("own file rejected: %v", err)
	}
	if err := checkSecureFileInfo(fakeInfo{mode: 0o600, uid: 0}); err != nil {
		t.Fatalf("root-owned file rejected: %v", err)
	}
	if err := checkSecureFileInfo(fakeInfo{mode: 0o600, uid: euid + 12345}); err == nil {
		t.Fatal("foreign owner accepted")
	}
	if err := checkSecureFileInfo(fakeInfo{mode: os.ModeNamedPipe | 0o600, uid: euid}); err == nil {
		t.Fatal("FIFO mode accepted")
	}
}

// ---- DataDir -------------------------------------------------------------

func TestDataDirPolicy(t *testing.T) {
	base := t.TempDir()

	// Created with 0700 (even below a permissive umask), nested parents too.
	old := syscall.Umask(0)
	nested := filepath.Join(base, "a", "b")
	err := prepareDataDir(nested)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(nested); st.Mode().Perm() != 0o700 {
		t.Fatalf("created mode %o", st.Mode().Perm())
	}

	// Read-only own directory is allowed.
	ro := filepath.Join(base, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := prepareDataDir(ro); err != nil {
		t.Fatalf("read-only dir rejected: %v", err)
	}

	// Group/world writable refused.
	for _, m := range []os.FileMode{0o770, 0o707, 0o777, 0o720} {
		d := filepath.Join(base, "w"+m.String())
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, m); err != nil {
			t.Fatal(err)
		}
		if err := prepareDataDir(d); err == nil {
			t.Fatalf("mode %o accepted", m)
		}
	}

	// Symlinked directory refused (even to a good directory).
	link := filepath.Join(base, "link")
	if err := os.Symlink(nested, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareDataDir(link); err == nil {
		t.Fatal("symlinked data dir accepted")
	}

	// A file in place of the directory.
	f := filepath.Join(base, "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDataDir(f); err == nil {
		t.Fatal("regular file accepted as data dir")
	}

	// Existing directory owned by someone else.
	if os.Geteuid() != 0 {
		if err := prepareDataDir("/"); err == nil {
			t.Fatal("foreign-owned directory accepted")
		}
	}
}

func TestSetupRejectsUnsafeDataDir(t *testing.T) {
	base := hazardDir(t)
	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := setup([]byte("dataDir: " + open + "\n")); err == nil {
		t.Fatal("world-writable data dir accepted by setup")
	}
	if _, err := os.Stat(filepath.Join(open, "usage.db")); err == nil {
		t.Fatal("usage store opened inside rejected dir")
	}
	good := filepath.Join(base, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := setup([]byte("dataDir: " + link + "\n")); err == nil {
		t.Fatal("symlinked data dir accepted by setup")
	}
}

// ---- 2. fixture origin -----------------------------------------------------

func fixtureCfg(t *testing.T, dir string) (data, fx string) {
	t.Helper()
	fx = filepath.Join(dir, "fixture.json")
	if err := quota.WriteSnapshot(fx, stateBase()); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "data"), fx
}

func TestFixtureModeRecordedInEnvelope(t *testing.T) {
	dir := hazardDir(t)
	data, fx := fixtureCfg(t, dir)
	if _, err := setup([]byte("dataDir: " + data + "\nfixtureSnapshotPath: " + fx + "\n")); err != nil {
		t.Fatal(err)
	}
	if !readState(t, data).FixtureMode {
		t.Fatal("fixture origin not recorded")
	}
	raw := fileBytes(t, filepath.Join(data, stateFileName))
	if !strings.Contains(string(raw), `"fixtureMode":true`) {
		t.Fatalf("envelope lacks fixtureMode: %.200s", raw)
	}
	shutdown()
	if _, err := setup([]byte("dataDir: " + data + "\n")); err != nil {
		t.Fatal(err)
	}
	if readState(t, data).FixtureMode {
		t.Fatal("live restart still marked fixture")
	}
}

func TestFixtureToLiveRestartClearsSyntheticData(t *testing.T) {
	dir := hazardDir(t)
	data, fx := fixtureCfg(t, dir)
	fcfg := []byte("dataDir: " + data + "\nfixtureSnapshotPath: " + fx + "\n")
	p, err := setup(fcfg)
	if err != nil {
		t.Fatal(err)
	}
	if r := put(t, p, groupedPut); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	revBefore := p.snapshotCopy().Revision
	shutdown()

	p, err = setup([]byte("dataDir: " + data + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.baseCopy().Accounts) + len(p.baseCopy().Credentials); n != 0 {
		t.Fatalf("synthetic base survived: %+v", p.baseCopy())
	}
	if v := p.snapshotCopy(); len(v.Accounts)+len(v.Credentials)+len(v.Pools) != 0 {
		t.Fatalf("synthetic view survived: %v", accountIDs(v))
	}
	if p.snapshotCopy().Revision <= revBefore {
		t.Fatal("view revision did not advance")
	}
	env := readState(t, data)
	if env.FixtureMode || len(env.BaseSnapshot.Accounts) != 0 || len(env.Snapshot.Accounts) != 0 {
		t.Fatalf("state still synthetic: %+v", env)
	}
	// Operator overrides are operator data and stay.
	if env.Overrides.Credentials["c1"].AccountID != "shared" {
		t.Fatalf("overrides lost: %+v", env.Overrides)
	}
	exp, err := quota.ReadSnapshot(filepath.Join(data, exportSnapshotName))
	if err != nil || len(exp.Accounts) != 0 || len(exp.Credentials) != 0 {
		t.Fatalf("export still synthetic: %v %+v", err, exp)
	}
}

func TestFixtureToLiveInventoryFailureLeavesNoSyntheticData(t *testing.T) {
	dir := hazardDir(t)
	data, fx := fixtureCfg(t, dir)
	if _, err := setup([]byte("dataDir: " + data + "\nfixtureSnapshotPath: " + fx + "\n")); err != nil {
		t.Fatal(err)
	}
	shutdown()
	p, err := setup([]byte("dataDir: " + data + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	p.startCollector(failingInventory)
	waitFor(t, func() bool {
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.control.health.Status == healthInventoryUnavailable
	})
	if v := p.snapshotCopy(); len(v.Accounts) != 0 || len(v.Credentials) != 0 {
		t.Fatalf("synthetic data served after failed inventory: %v", accountIDs(v))
	}
	exp, err := quota.ReadSnapshot(filepath.Join(data, exportSnapshotName))
	if err != nil || len(exp.Accounts) != 0 {
		t.Fatalf("export synthetic after failed inventory: %v", err)
	}
}

func TestFixtureToLiveFailsClosedWhenExportCannotBeCleared(t *testing.T) {
	dir := hazardDir(t)
	data, fx := fixtureCfg(t, dir)
	if _, err := setup([]byte("dataDir: " + data + "\nfixtureSnapshotPath: " + fx + "\n")); err != nil {
		t.Fatal(err)
	}
	shutdown()
	exp := filepath.Join(data, exportSnapshotName)
	if err := os.Remove(exp); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(exp, "blocker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := setup([]byte("dataDir: " + data + "\n")); err == nil {
		t.Fatal("setup succeeded though the synthetic export could not be cleared")
	}
}

// ---- 3. legacy restore ---------------------------------------------------

func bakedWindow(used, capv float64, src string) quota.Window {
	return quota.Window{ID: "five-hour", DurationSeconds: 18000, UsedFraction: &used, Capacity: &capv, CapacitySource: src, ObservedAt: stateT0}
}

func writeLegacy(t *testing.T, dir string, snap quota.Snapshot, overrides string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := quota.WriteSnapshot(filepath.Join(dir, exportSnapshotName), snap); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, exportOverridesName), []byte(overrides), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRestoreStripsOverridesWithEmptyAccountID(t *testing.T) {
	dir := hazardDir(t)
	snap := quota.Snapshot{SchemaVersion: 1, Revision: 4, GeneratedAt: stateT0,
		Accounts: []quota.Account{{ID: "c1", Provider: "claude", Label: "Baked", BillingType: "api", Plan: "max", PlanSource: "operator_override", RefreshStatus: "ok", ObservedAt: stateT0,
			Windows: []quota.Window{bakedWindow(.3, 5, "operator_override")}, Financial: []quota.Financial{}},
			{ID: "c2", Provider: "claude", Label: "Orig Two", BillingType: "subscription", Plan: "free", PlanSource: "provider", RefreshStatus: "ok", ObservedAt: stateT0,
				Windows: []quota.Window{bakedWindow(.3, 9, "reported")}, Financial: []quota.Financial{}}},
		Credentials: []quota.Credential{{ID: "c1", AccountID: "c1", Provider: "claude", Label: "Baked", Models: []string{"m"}}, {ID: "c2", AccountID: "c2", Provider: "claude", Label: "Orig Two", Models: []string{"m"}}},
		Pools:       []quota.Pool{}}
	writeLegacy(t, dir, snap, `{"credentials":{"c1":{"label":"Baked","plan":"max","billingType":"api","capacityByWindow":{"five-hour":5}}},"pools":[]}`)
	p, err := setup([]byte("dataDir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	base := p.baseCopy()
	a := accountOf(t, base, "c1")
	if a.Plan != "" || a.PlanSource != "" || a.BillingType != "unknown" || a.Label != unknownLabel {
		t.Fatalf("operator plan/billing/label survived in base: %+v", a)
	}
	if w := a.Windows[0]; w.Capacity != nil || w.CapacitySource != "" {
		t.Fatalf("operator weight survived in base: %+v", w)
	}
	if base.Credentials[0].Label != unknownLabel {
		t.Fatalf("credential label kept: %+v", base.Credentials[0])
	}
	// Untouched neighbour keeps real provider data.
	if b := accountOf(t, base, "c2"); b.Plan != "free" || b.Windows[0].Capacity == nil || b.Label != "Orig Two" {
		t.Fatalf("unrelated account altered: %+v", b)
	}
	// Removing the override must leave no false override behind either.
	if r := put(t, p, `{"credentials":{},"pools":[]}`); r.StatusCode != 200 {
		t.Fatal(string(r.Body))
	}
	v := accountOf(t, p.snapshotCopy(), "c1")
	if v.Plan != "" || v.PlanSource == "operator_override" || v.BillingType != "unknown" || v.Label != unknownLabel || v.Windows[0].Capacity != nil {
		t.Fatalf("false override preserved after removal: %+v", v)
	}
}

func TestLegacyRestoreKeepsLabelThatDiffersFromOverride(t *testing.T) {
	dir := hazardDir(t)
	snap := quota.Snapshot{SchemaVersion: 1, Revision: 4, GeneratedAt: stateT0,
		Accounts: []quota.Account{{ID: "c1", Provider: "claude", Label: "Real", BillingType: "subscription", Plan: "pro", PlanSource: "provider", RefreshStatus: "ok", ObservedAt: stateT0,
			Windows: []quota.Window{bakedWindow(.3, 9, "reported")}, Financial: []quota.Financial{}}},
		Credentials: []quota.Credential{{ID: "c1", AccountID: "c1", Provider: "claude", Label: "Real", Models: []string{"m"}}},
		Pools:       []quota.Pool{}}
	writeLegacy(t, dir, snap, `{"credentials":{"c1":{"label":"Operator Name"}},"pools":[]}`)
	p, err := setup([]byte("dataDir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	a := accountOf(t, p.baseCopy(), "c1")
	if a.Label != "Real" || a.Plan != "pro" || a.PlanSource != "provider" || a.BillingType != "subscription" || a.Windows[0].Capacity == nil {
		t.Fatalf("provider data stripped without an override match: %+v", a)
	}
}

// The override has no accountId but the credential belongs to a prior account
// other than itself: strip that account, never invent or split one.
func TestLegacyRestoreIDFallbackUsesCredentialPriorAccount(t *testing.T) {
	dir := hazardDir(t)
	snap := quota.Snapshot{SchemaVersion: 1, Revision: 4, GeneratedAt: stateT0,
		Accounts: []quota.Account{{ID: "prior", Provider: "claude", Label: "Baked", BillingType: "api", Plan: "max", PlanSource: "operator_override", RefreshStatus: "ok", ObservedAt: stateT0,
			Windows: []quota.Window{bakedWindow(.3, 5, "operator_override")}, Financial: []quota.Financial{}}},
		Credentials: []quota.Credential{{ID: "c1", AccountID: "prior", Provider: "claude", Label: "Baked", Models: []string{"m"}}, {ID: "c2", AccountID: "prior", Provider: "claude", Label: "Orig Two", Models: []string{"m"}}},
		Pools:       []quota.Pool{}}
	writeLegacy(t, dir, snap, `{"credentials":{"c1":{"label":"Baked","plan":"max","billingType":"api"}},"pools":[]}`)
	p, err := setup([]byte("dataDir: " + dir + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	base := p.baseCopy()
	if len(base.Accounts) != 1 || base.Credentials[0].AccountID != "prior" || base.Credentials[1].AccountID != "prior" {
		t.Fatalf("account structure changed: %v %+v", accountIDs(base), base.Credentials)
	}
	a := accountOf(t, base, "prior")
	if a.Plan != "" || a.PlanSource != "" || a.BillingType != "unknown" || a.Label != unknownLabel || a.Windows[0].Capacity != nil {
		t.Fatalf("prior account still carries override: %+v", a)
	}
}

// ---- 4. export recovery restores the collector status --------------------

func TestExportRecoveryRestoresPartialStatusKeepingCollectorError(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	const collectorErr = "1 of 2 accounts failed"
	p.mu.Lock()
	p.control.health = collectorHealth{Status: healthPartial, LastError: collectorErr, Setup: []setupItem{}}
	p.mu.Unlock()

	exportErr := errors.New("disk gone")
	setSeams(p, nil, func(string, quota.Snapshot, overrideDocument) error { return exportErr })
	if _, err := p.saveOverridesResult([]byte(groupedPut)); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	h := p.control.health
	p.mu.RUnlock()
	if h.Status != "error" || h.LastError != collectorErr {
		t.Fatalf("degraded overlay: %+v", h)
	}

	setSeams(p, nil, writeDerivedExports)
	if _, err := p.saveOverridesResult([]byte(groupedPut)); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	h = p.control.health
	p.mu.RUnlock()
	if h.Status != healthPartial || h.LastError != collectorErr {
		t.Fatalf("status sticky after export recovery: %+v", h)
	}
	if p.exportState().Degraded {
		t.Fatal("still degraded")
	}
}

func TestExportRecoveryViaCollectorPublishRestoresStatus(t *testing.T) {
	p, _, _ := newStateRuntime(t, "")
	seedBase(t, p)
	setSeams(p, nil, func(string, quota.Snapshot, overrideDocument) error { return errors.New("disk gone") })
	next := stateBase()
	next.Revision = 50
	p.publishHealth(next, collectorHealth{Status: healthPartial, LastError: "x failed", Setup: []setupItem{}}, nil)
	p.mu.RLock()
	h := p.control.health
	p.mu.RUnlock()
	if h.Status != "error" {
		t.Fatalf("overlay missing: %+v", h)
	}
	setSeams(p, nil, writeDerivedExports)
	next2 := stateBase()
	next2.Revision = 51
	p.publishHealth(next2, collectorHealth{Status: healthPartial, LastError: "x failed", Setup: []setupItem{}}, nil)
	p.mu.RLock()
	h = p.control.health
	p.mu.RUnlock()
	if h.Status != healthPartial || h.LastError != "x failed" {
		t.Fatalf("collector status not preserved: %+v", h)
	}
}

func failingInventory(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
	return nil, nil, errors.New("inventory down")
}

var _ = json.Marshal
