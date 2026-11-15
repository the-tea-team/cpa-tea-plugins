package main

// Transactional state. DataDir/state-v1.json is the ONLY authoritative
// commit. It carries the raw collected baseline, the operator overrides and
// the materialized view (baseline + overrides) in one atomically replaced
// file. snapshot-v1.json (shared export) and overrides.json (legacy
// compatibility copy) are derived from it: they are rewritten best-effort
// after each commit and repaired on every start.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

const autoPoolPrefix = "auto-"

const (
	stateFileName       = "state-v1.json"
	stateSchemaVersion  = 1
	exportSnapshotName  = "snapshot-v1.json"
	exportOverridesName = "overrides.json"
	maxStateBytes       = 32 << 20

	exportDegradedMessage = "shared snapshot export degraded"
)

// StateEnvelope is the single authoritative on-disk document.
type StateEnvelope struct {
	SchemaVersion int              `json:"schemaVersion"`
	Generation    uint64           `json:"generation"`
	BaseSnapshot  quota.Snapshot   `json:"baseSnapshot"`
	Overrides     overrideDocument `json:"overrides"`
	Snapshot      quota.Snapshot   `json:"snapshot"`
	// FixtureMode records that BaseSnapshot/Snapshot came from a synthetic
	// fixture, so a later restart in live mode can discard them.
	FixtureMode bool `json:"fixtureMode"`
}

// exportStatus describes the derived exports. Degraded means the committed
// state is newer than (or differs from) what the shared export file holds.
type exportStatus struct {
	Degraded bool   `json:"degraded"`
	Error    string `json:"error,omitempty"`

	statusChanged bool
	savedStatus   string
}

// commitResult is what a successful authoritative commit reports. A commit
// can succeed while the export is degraded; that is not a failed request.
type commitResult struct {
	Generation     uint64 `json:"generation"`
	Revision       uint64 `json:"revision"`
	ExportDegraded bool   `json:"exportDegraded"`
	ExportError    string `json:"exportError,omitempty"`
}

// stateWriteFunc and exportWriteFunc are failure-injection seams; production
// uses writeStateFile and writeDerivedExports.
type stateWriteFunc func(dir string, env StateEnvelope) error
type exportWriteFunc func(dir string, view quota.Snapshot, ov overrideDocument) error

func emptySnapshot() quota.Snapshot {
	return quota.Snapshot{SchemaVersion: quota.SchemaVersion, Accounts: []quota.Account{}, Credentials: []quota.Credential{}, Pools: []quota.Pool{}}
}

func validateEnvelope(env StateEnvelope) (StateEnvelope, error) {
	if env.SchemaVersion != stateSchemaVersion {
		return env, errors.New("unsupported state schema")
	}
	if env.Generation == 0 {
		return env, errors.New("invalid generation")
	}
	if err := quota.Validate(env.BaseSnapshot); err != nil {
		return env, errors.New("invalid base snapshot")
	}
	if err := quota.Validate(env.Snapshot); err != nil {
		return env, errors.New("invalid snapshot")
	}
	ov, err := normalizeOverrideDocument(env.Overrides)
	if err != nil {
		return env, errors.New("invalid overrides")
	}
	env.Overrides = ov
	return env, nil
}

// ---- secure file access -------------------------------------------------
//
// Linux only. Every file the plugin trusts from DataDir (state-v1.json and the
// legacy overrides.json) is opened exactly like shared snapshots: without
// following symlinks and without blocking (a FIFO must never hang setup),
// then validated on the opened descriptor (regular file, root or current
// owner, not group/world writable, bounded size) and read through a limit.

func checkSecureFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("must be a regular file that is not group/world writable")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) {
		return errors.New("owner must be root or the current user")
	}
	return nil
}

// readSecureFile returns the file's bytes. A missing file yields an error for
// which errors.Is(err, os.ErrNotExist) holds; every other refusal does not.
func readSecureFile(path string, max int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := checkSecureFileInfo(info); err != nil {
		return nil, &os.PathError{Op: "read", Path: path, Err: err}
	}
	if info.Size() > max {
		return nil, &os.PathError{Op: "read", Path: path, Err: errors.New("file too large")}
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, &os.PathError{Op: "read", Path: path, Err: errors.New("file too large")}
	}
	return data, nil
}

// prepareDataDir creates DataDir (0700) when absent and otherwise requires an
// existing real directory (never a symlink) owned by the effective user with
// no group/world write bit. A read-only directory is allowed.
func prepareDataDir(dir string) error {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.IsDir():
		return errors.New("data dir is not a directory")
	case !ok || st.Uid != uint32(os.Geteuid()):
		return errors.New("data dir must be owned by the current user")
	case info.Mode().Perm()&0o022 != 0:
		return errors.New("data dir must not be group/world writable")
	}
	return nil
}

func readStateFile(dir string) (StateEnvelope, bool, error) {
	raw, err := readSecureFile(filepath.Join(dir, stateFileName), maxStateBytes)
	if errors.Is(err, os.ErrNotExist) {
		return StateEnvelope{}, false, nil
	}
	if err != nil {
		return StateEnvelope{}, false, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var env StateEnvelope
	if err := dec.Decode(&env); err != nil {
		return StateEnvelope{}, false, errors.New("decode state")
	}
	if _, err := dec.Token(); err != io.EOF {
		return StateEnvelope{}, false, errors.New("trailing data in state")
	}
	env, err = validateEnvelope(env)
	if err != nil {
		return StateEnvelope{}, false, err
	}
	return env, true, nil
}

// writeStateFile validates and atomically replaces the authoritative state.
// On any error the previous file is left untouched.
func writeStateFile(dir string, env StateEnvelope) error {
	env, err := validateEnvelope(env)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(raw) > maxStateBytes {
		return errors.New("state too large")
	}
	return writeJSONAtomic(dir, stateFileName, raw)
}

// writeDerivedExports rewrites the shared snapshot export and the legacy
// overrides.json copy. Both are attempted; the first error is returned.
func writeDerivedExports(dir string, view quota.Snapshot, ov overrideDocument) error {
	err := quota.WriteSnapshot(filepath.Join(dir, exportSnapshotName), view)
	raw, merr := json.Marshal(ov)
	if merr == nil {
		merr = writeJSONAtomic(dir, exportOverridesName, raw)
	}
	if err == nil {
		err = merr
	}
	return err
}

func writeJSONAtomic(dir, name string, raw []byte) error {
	f, e := os.CreateTemp(dir, ".tmp-"+name+"-")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(raw)
	}
	if e == nil {
		e = f.Sync()
	}
	if e2 := f.Close(); e == nil {
		e = e2
	}
	if e != nil {
		return e
	}
	if e = os.Rename(tmp, filepath.Join(dir, name)); e != nil {
		return e
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func cleanStateTemps(dir string) {
	for _, pat := range []string{".tmp-" + stateFileName + "-*", ".tmp-" + exportOverridesName + "-*", ".snapshot-*.tmp"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// commitLocked is the only mutation path for authoritative state. The caller
// holds p.mu for writing. The candidate envelope is written first; memory is
// replaced only after the write succeeded, so a failed write changes neither.
// The derived export is then refreshed best-effort.
func (p *runtimeState) commitLocked(base quota.Snapshot, ov overrideDocument, view quota.Snapshot) error {
	base, view, ov = cloneSnapshot(base), cloneSnapshot(view), cloneOverrideDocument(ov)
	env := StateEnvelope{SchemaVersion: stateSchemaVersion, Generation: p.generation + 1, BaseSnapshot: base, Overrides: ov, Snapshot: view,
		FixtureMode: p.cfg.FixtureSnapshotPath != ""}
	if err := p.stateWriter(p.cfg.DataDir, env); err != nil {
		return fmt.Errorf("%w: %v", errStateCommit, err)
	}
	p.generation = env.Generation
	p.base, p.overrides, p.snapshot = base, ov, view
	p.syncConfigLocked()
	p.exportLocked()
	return nil
}

// syncConfigLocked mirrors committed overrides into cfg for readers of
// cfg.AccountOverrides/Pools. The committed state stays the source of truth.
func (p *runtimeState) syncConfigLocked() {
	c := cloneOverrideDocument(p.overrides)
	p.cfg.AccountOverrides, p.cfg.Pools = c.Credentials, c.Pools
}

// exportLocked refreshes derived exports from the committed state and keeps
// the degraded flag (and its collector-health overlay) accurate.
func (p *runtimeState) exportLocked() {
	err := p.exportWriter(p.cfg.DataDir, p.snapshot, p.overrides)
	was := p.export
	if err != nil {
		if !was.Degraded {
			p.export = exportStatus{Degraded: true, Error: exportDegradedMessage}
			p.overlayExportHealthLocked(&p.control.health)
		}
		return
	}
	if was.Degraded {
		// Undo only what the overlay did: the status it replaced (also when a
		// collector error is present, which must stay visible) and its own
		// placeholder message.
		if was.statusChanged && p.control.health.Status == "error" {
			p.control.health.Status = was.savedStatus
		}
		if p.control.health.LastError == exportDegradedMessage {
			p.control.health.LastError = ""
		}
	}
	p.export = exportStatus{}
}

// overlayExportHealthLocked makes a stale export visible in collector health.
func (p *runtimeState) overlayExportHealthLocked(h *collectorHealth) {
	if !p.export.Degraded {
		return
	}
	p.export.statusChanged, p.export.savedStatus = false, ""
	if h.Status == "" || h.Status == healthOK || h.Status == healthPartial {
		p.export.statusChanged, p.export.savedStatus = true, h.Status
		h.Status = "error"
	}
	if h.LastError == "" {
		h.LastError = exportDegradedMessage
	}
}

func (p *runtimeState) exportState() exportStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return exportStatus{Degraded: p.export.Degraded, Error: p.export.Error}
}

func (p *runtimeState) resultLocked() commitResult {
	return commitResult{Generation: p.generation, Revision: p.snapshot.Revision, ExportDegraded: p.export.Degraded, ExportError: p.export.Error}
}

// nextViewLocked materializes base+overrides and assigns the view revision:
// the base revision, bumped above the current view when needed so consumers
// always see a strictly newer revision after a change.
func (p *runtimeState) nextViewLocked(base quota.Snapshot, ov overrideDocument, strict bool) (quota.Snapshot, []string, error) {
	view, warns, err := materializeView(base, ov, strict)
	if err != nil {
		return view, warns, err
	}
	rev := base.Revision
	if p.generation > 0 && rev <= p.snapshot.Revision {
		rev = p.snapshot.Revision + 1
	}
	view.Revision = rev
	return view, warns, nil
}

func (p *runtimeState) baseCopy() quota.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneSnapshot(p.base)
}

func newRuntimeState(c Config, st *usageStore) *runtimeState {
	return &runtimeState{cfg: c, store: st, scheduler: newScheduler(), snapshot: emptySnapshot(), base: emptySnapshot(),
		overrides:   overrideDocument{Credentials: map[string]AccountOverride{}, Pools: []quota.Pool{}},
		queue:       make(chan usageSample, 1024),
		done:        make(chan struct{}),
		started:     time.Now().UTC(),
		stateWriter: writeStateFile, exportWriter: writeDerivedExports}
}

// readLegacyOverrides loads the pre-envelope overrides.json, if any.
func readLegacyOverrides(dir string) (overrideDocument, bool, error) {
	raw, err := osReadOverride(dir)
	if errors.Is(err, os.ErrNotExist) {
		return overrideDocument{}, false, nil
	}
	if err != nil {
		return overrideDocument{}, false, errors.New("override read failed")
	}
	doc, err := parseOverrideDocument(raw)
	if err != nil {
		return overrideDocument{}, false, errors.New("invalid persisted overrides")
	}
	return doc, true, nil
}

// recoverRuntimeState builds the runtime from disk. If state-v1.json exists
// it is the only input (corruption fails closed). Otherwise the legacy files
// (or configured overrides) are migrated exactly once into a first envelope.
// Fixture mode always takes the fixture as the baseline. The derived export
// is repaired from the committed state on every start.
func recoverRuntimeState(c Config, st *usageStore) (*runtimeState, error) {
	fail := func(msg string) (*runtimeState, error) {
		_ = st.close()
		return nil, errors.New(msg)
	}
	p := newRuntimeState(c, st)
	cleanStateTemps(c.DataDir)
	env, exists, err := readStateFile(c.DataDir)
	if err != nil {
		return fail("invalid persisted state")
	}
	persist := false
	fixtureToLive := false
	if exists {
		p.generation, p.base, p.overrides, p.snapshot = env.Generation, env.BaseSnapshot, env.Overrides, env.Snapshot
		if env.FixtureMode && c.FixtureSnapshotPath == "" {
			// Synthetic fixture data must never be served as live quota: drop
			// the authoritative base (operator overrides are kept) and clear
			// the derived exports before any collector runs.
			p.base, fixtureToLive, persist = emptySnapshot(), true, true
		}
	} else {
		doc := overrideDocument{Credentials: c.AccountOverrides, Pools: c.Pools}
		if legacy, ok, lerr := readLegacyOverrides(c.DataDir); lerr != nil {
			return fail(lerr.Error())
		} else if ok {
			doc = legacy
		}
		if doc.Credentials == nil {
			doc.Credentials = map[string]AccountOverride{}
		}
		if doc.Pools == nil {
			doc.Pools = []quota.Pool{}
		}
		if doc, err = normalizeOverrideDocument(doc); err != nil {
			return fail("invalid configured overrides")
		}
		p.overrides = doc
		switch legacySnap, lerr := quota.ReadSnapshot(filepath.Join(c.DataDir, exportSnapshotName)); {
		case lerr == nil:
			p.base = restoreLegacyBase(legacySnap, doc)
		case !errors.Is(lerr, os.ErrNotExist):
			return fail("invalid persisted snapshot")
		}
		persist = true
	}
	if c.FixtureSnapshotPath != "" {
		fx, ferr := quota.ReadSnapshot(c.FixtureSnapshotPath)
		if ferr != nil {
			return fail("invalid configured fixture")
		}
		p.base = fx
		persist = true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncConfigLocked()
	if !persist {
		p.exportLocked()
		return p, nil
	}
	view, _, verr := p.nextViewLocked(p.base, p.overrides, false)
	if verr != nil {
		return fail("invalid persisted state")
	}
	if fixtureToLive {
		// Clear the shared export first and fail closed if that is impossible;
		// the state still says fixture until the commit below, so a crash in
		// between simply repeats this step on the next start.
		if eerr := p.exportWriter(c.DataDir, view, p.overrides); eerr != nil {
			return fail("cannot clear fixture export")
		}
	}
	if cerr := p.commitLocked(p.base, p.overrides, view); cerr != nil {
		return fail("cannot persist state")
	}
	return p, nil
}

// restoreLegacyBase undoes, as far as the data allows, what a pre-envelope
// build baked into snapshot-v1.json from operator overrides, so that removing
// an override later restores the original values. Raw collection names an
// account after its credential, which makes account merging reversible.
//
// An override applies to the account its credential currently sits in
// (cr.AccountID): that is the override's accountId when set, and the
// credential's prior account when accountId is empty (no regrouping happened,
// but plan, billing, label and capacity were still baked in). Operator plan,
// billing, label and capacity values are removed from those accounts; where
// the provider's original cannot be known the field is marked unknown instead
// of keeping a false override. Pools the old build derived from overrides
// (auto pools and operator pool IDs) are dropped because the view recomputes
// them. On any inconsistency the legacy snapshot is returned unchanged.
func restoreLegacyBase(s quota.Snapshot, ov overrideDocument) quota.Snapshot {
	out := cloneSnapshot(s)
	idx := map[string]int{}
	for i, a := range out.Accounts {
		idx[a.ID] = i
	}
	type baked struct{ plan, billing, label string }
	// Operator values per account the credentials' overrides were baked into.
	byAcct := map[string]baked{}
	merged := map[string]bool{} // accounts that received a regrouping override
	applies := func(cr quota.Credential) (AccountOverride, bool) {
		o, ok := ov.Credentials[cr.ID]
		t := strings.TrimSpace(o.AccountID)
		if !ok || cr.AccountID == "" || (t != "" && cr.AccountID != t) {
			return o, false
		}
		return o, true
	}
	for _, cr := range out.Credentials {
		o, ok := applies(cr)
		if !ok {
			continue
		}
		b := byAcct[cr.AccountID]
		if b.plan == "" {
			b.plan = strings.TrimSpace(o.Plan)
		}
		if b.billing == "" {
			b.billing = strings.TrimSpace(o.BillingType)
		}
		if b.label == "" {
			b.label = cleanLabel(o.Label)
		}
		byAcct[cr.AccountID] = b
		if t := strings.TrimSpace(o.AccountID); t != "" && t != cr.ID {
			merged[t] = true
		}
	}
	strip := map[string]baked{}
	for id, b := range byAcct {
		strip[id] = b
	}
	for i, cr := range out.Credentials {
		o, ok := applies(cr)
		if !ok {
			continue
		}
		if l := cleanLabel(o.Label); l != "" && cr.Label == l {
			out.Credentials[i].Label = unknownLabel
		}
		t := strings.TrimSpace(o.AccountID)
		if t == "" || t == cr.ID {
			continue
		}
		if _, exists := idx[cr.ID]; !exists {
			j, ok := idx[t]
			if !ok {
				continue
			}
			a := cloneAccount(out.Accounts[j])
			a.ID = cr.ID
			out.Accounts = append(out.Accounts, a)
			idx[cr.ID] = len(out.Accounts) - 1
		}
		out.Credentials[i].AccountID = cr.ID
		strip[cr.ID] = byAcct[t]
	}
	referenced := map[string]bool{}
	for _, cr := range out.Credentials {
		referenced[cr.AccountID] = true
	}
	accts := out.Accounts[:0]
	for _, a := range out.Accounts {
		if merged[a.ID] && !referenced[a.ID] {
			delete(strip, a.ID)
			continue
		}
		if b, ok := strip[a.ID]; ok {
			a = cloneAccount(a)
			if a.PlanSource == "operator_override" || (b.plan != "" && a.Plan == b.plan) {
				a.Plan, a.PlanSource = "", ""
			}
			if b.billing != "" && a.BillingType == b.billing {
				a.BillingType = "unknown"
			}
			if b.label != "" && a.Label == b.label {
				a.Label = unknownLabel
			}
			for k := range a.Windows {
				if a.Windows[k].CapacitySource == "operator_override" {
					a.Windows[k].Capacity, a.Windows[k].CapacitySource = nil, ""
				}
			}
		}
		accts = append(accts, a)
	}
	out.Accounts = accts
	opIDs := map[string]bool{}
	for _, pl := range ov.Pools {
		opIDs[pl.ID] = true
	}
	pools := []quota.Pool{}
	for _, pl := range out.Pools {
		if !strings.HasPrefix(pl.ID, autoPoolPrefix) && !opIDs[pl.ID] {
			pools = append(pools, pl)
		}
	}
	out.Pools = pools
	if quota.Validate(out) != nil {
		return s
	}
	return out
}
