package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"sync"
	"time"
)

type hostCallFunc func(string, any) ([]byte, error)

func nativeInventoryWith(call hostCallFunc) InventoryFunc {
	return func(ctx context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		a, e := call("host.auth.list", map[string]any{})
		if e != nil {
			return nil, nil, e
		}
		var listed struct {
			Files []pluginapi.HostAuthFileEntry `json:"files"`
		}
		if json.Unmarshal(a, &listed) != nil {
			return nil, nil, errors.New("invalid inventory")
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		b, e := call("host.auth.models", map[string]any{})
		if e != nil {
			return nil, nil, e
		}
		var models pluginapi.HostAuthModelsResponse
		if json.Unmarshal(b, &models) != nil {
			return nil, nil, errors.New("invalid model inventory")
		}
		return listed.Files, models.Credentials, nil
	}
}

type collectorControl struct {
	once    sync.Once
	cancel  context.CancelFunc
	done    <-chan struct{}
	refresh chan struct{}
	health  collectorHealth
}

func (p *runtimeState) configCopy() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	b, _ := json.Marshal(p.cfg)
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}

// collectorConfig is the configuration handed to the collector: overrides are
// stripped so it always collects the raw baseline. Operator overrides are
// applied to the result at publish time, under the state lock.
func (p *runtimeState) collectorConfig() Config {
	c := p.configCopy()
	c.AccountOverrides, c.Pools = nil, nil
	return c
}
func (p *runtimeState) startCollector(inv InventoryFunc) {
	p.control.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.mu.Lock()
		p.control.cancel = cancel
		p.control.refresh = make(chan struct{}, 1)
		p.mu.Unlock()
		if p.configCopy().FixtureSnapshotPath != "" {
			done := make(chan struct{})
			close(done)
			p.mu.Lock()
			p.control.done = done
			h := collectorHealth{Status: "fixture", LastRefreshAt: time.Now().UTC(), Setup: []setupItem{{Provider: "demo", Status: "fixture", Message: "Synthetic fixture; not live quota"}}}
			p.overlayExportHealthLocked(&h)
			p.control.health = h
			p.mu.Unlock()
			return
		}
		done := StartCollector(ctx, LoopHooks{Config: p.collectorConfig, Previous: p.baseCopy, Inventory: inv, Publish: p.publishHealth}, p.control.refresh)
		p.mu.Lock()
		p.control.done = done
		p.mu.Unlock()
	})
}

// publishHealth stores one collector pass result. A successful pass is
// committed (with current overrides applied) before its health is recorded;
// a failed pass keeps the committed state and only records health.
func (p *runtimeState) publishHealth(s quota.Snapshot, h collectorHealth, e error) {
	if e == nil {
		if err := p.publishCollected(s); err != nil && !errors.Is(err, errStaleCollection) {
			h.Status = "error"
			h.LastError = "snapshot persistence failed"
		}
	}
	p.mu.Lock()
	p.overlayExportHealthLocked(&h)
	p.control.health = h
	p.mu.Unlock()
}

func (p *runtimeState) stopCollector() {
	p.mu.RLock()
	cancel, done := p.control.cancel, p.control.done
	p.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}
func (p *runtimeState) triggerRefresh() {
	p.mu.RLock()
	ch := p.control.refresh
	p.mu.RUnlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
