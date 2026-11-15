package main

import (
	"encoding/json"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"sync"
	"sync/atomic"
	"time"
)

type runtimeState struct {
	control     collectorControl
	writeErrors atomic.Int64
	cfg         Config
	store       *usageStore
	scheduler   *scheduler
	mu          sync.RWMutex
	queue       chan usageSample
	done        chan struct{}
	dropped     atomic.Int64
	started     time.Time

	// Authoritative state, guarded by mu and replaced only through
	// commitLocked: generation counts commits, base is the raw collected
	// snapshot, overrides the operator document and snapshot the served view
	// (base + overrides).
	generation uint64
	base       quota.Snapshot
	overrides  overrideDocument
	snapshot   quota.Snapshot
	export     exportStatus

	// Failure-injection seams; production values are set by newRuntimeState.
	stateWriter  stateWriteFunc
	exportWriter exportWriteFunc
}

var runtimeMu sync.Mutex
var current *runtimeState

func setup(raw []byte) (*runtimeState, error) {
	c, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if current != nil {
		if current.cfg.DataDir != c.DataDir {
			return nil, errors.New("dataDir cannot change without restart")
		}
		if current.cfg.FixtureSnapshotPath != c.FixtureSnapshotPath {
			return nil, errors.New("fixtureSnapshotPath cannot change without restart")
		}
		current.mu.Lock()
		// Committed overrides stay authoritative; configuration only
		// changes the remaining settings.
		c.AccountOverrides, c.Pools = nil, nil
		current.cfg = c
		current.syncConfigLocked()
		current.mu.Unlock()
		return current, nil
	}
	if err := prepareDataDir(c.DataDir); err != nil {
		return nil, errors.New("unsafe data dir")
	}
	st, err := openUsageStore(c.DataDir)
	if err != nil {
		return nil, errors.New("cannot open usage store")
	}
	p, err := recoverRuntimeState(c, st)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(p.done)
		prune := func() {
			if err := p.store.prune(time.Now().Add(-30 * 24 * time.Hour)); err != nil {
				p.writeErrors.Add(1)
			}
		}
		prune()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case v, ok := <-p.queue:
				if !ok {
					if err := p.store.close(); err != nil {
						p.writeErrors.Add(1)
					}
					return
				}
				if err := p.store.insert(v); err != nil {
					p.writeErrors.Add(1)
				}
			case <-ticker.C:
				prune()
			}
		}
	}()
	current = p
	return p, nil
}
func getRuntime() *runtimeState { runtimeMu.Lock(); defer runtimeMu.Unlock(); return current }
func shutdown() {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if current != nil {
		current.stopCollector()
		close(current.queue)
		<-current.done
		current = nil
	}
}
func (p *runtimeState) submit(r pluginapi.UsageRecord) {
	v := usageSample{At: r.RequestedAt, AuthID: r.AuthID, Provider: r.Provider, Model: r.Model, Failed: r.Failed, InputTokens: r.Detail.InputTokens, OutputTokens: r.Detail.OutputTokens, CacheReadTokens: r.Detail.CacheReadTokens}
	if v.At.IsZero() {
		v.At = time.Now().UTC()
	}
	p.mu.RLock()
	for _, c := range p.snapshot.Credentials {
		if c.ID == v.AuthID {
			v.AccountID = c.AccountID
			break
		}
	}
	p.mu.RUnlock()
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if current != p {
		return
	}
	select {
	case p.queue <- v:
	default:
		p.dropped.Add(1)
	}
}
func (p *runtimeState) snapshotCopy() quota.Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	b, _ := json.Marshal(p.snapshot)
	var s quota.Snapshot
	_ = json.Unmarshal(b, &s)
	return s
}

// applySnapshot installs s as the raw baseline and commits it together with
// the current overrides. Invalid snapshots and failed writes change nothing.
func (p *runtimeState) applySnapshot(s quota.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applyBaseLocked(s)
}

func (p *runtimeState) applyBaseLocked(s quota.Snapshot) error {
	if err := quota.Validate(s); err != nil {
		return err
	}
	view, _, err := p.nextViewLocked(s, p.overrides, false)
	if err != nil {
		return err
	}
	return p.commitLocked(s, p.overrides, view)
}

// errStaleCollection reports a collector pass that raced with a newer base.
var errStaleCollection = errors.New("stale collection pass")

// publishCollected commits one raw collector result. The CURRENT overrides
// are applied under the lock, so a pass that started before an override PUT
// cannot roll the overrides back.
func (p *runtimeState) publishCollected(raw quota.Snapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if raw.Revision <= p.base.Revision {
		return errStaleCollection
	}
	return p.applyBaseLocked(raw)
}

func (p *runtimeState) loadFixture() error {
	p.mu.RLock()
	path := p.cfg.FixtureSnapshotPath
	p.mu.RUnlock()
	if path == "" {
		return errors.New("collector not configured")
	}
	s, err := quota.ReadSnapshot(path)
	if err != nil {
		return errors.New("invalid fixture")
	}
	return p.applySnapshot(s)
}
