package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"github.com/the-tea-team/cpa-tea-plugins/plugins/tea-quota-router/adapters"
)

// ---- seam types --------------------------------------------------------------

type setupItem struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Message  string `json:"message"`
}

// collectorHealth is the per-pass collector report. LastRefreshAt is the
// newest observation time that the returned snapshot still carries (fresh or
// preserved), so it is honest about freshness; it is omitted from JSON when
// nothing was ever observed.
type collectorHealth struct {
	Status        string      `json:"status"`
	LastRefreshAt time.Time   `json:"lastRefreshAt,omitzero"`
	LastError     string      `json:"lastError"`
	Setup         []setupItem `json:"setup"`
}

// CollectorStatus is the exported name of collectorHealth for CollectOnce.
type CollectorStatus = collectorHealth

// Health status values.
const (
	healthOK                   = "ok"
	healthPartial              = "partial"
	healthFailed               = "failed"
	healthIdle                 = "idle"
	healthNotConfigured        = "not_configured"
	healthSetupRequired        = "setup_required"
	healthInventoryUnavailable = "inventory_unavailable"
	healthInvalidSnapshot      = "invalid_snapshot"
	healthCanceled             = "canceled"
)

// Account refresh status values.
const (
	refreshOK          = "ok"
	refreshStale       = "stale" // refresh failed, previous observations retained with their original timestamps
	refreshError       = "error" // refresh failed, nothing observed yet
	refreshUnsupported = "unsupported"
	refreshDisabled    = "disabled"
)

const (
	maxConcurrentSources = 2
	maxAttempts          = 3
	passTimeout          = 150 * time.Second
	adminKeyEnvOpenAI    = "OPENAI_ADMIN_KEY"
	adminKeyEnvAnthropic = "ANTHROPIC_ADMIN_KEY"
)

var (
	errInventoryUnavailable = errors.New("host inventory unavailable")
	errInvalidSnapshot      = errors.New("collected snapshot invalid")
	errCollectorPanic       = errors.New("collector pass failed")
)

// adminKeyEnvFor is the fixed allowlist of admin report providers and the one
// environment variable each may read. Config selects whether a provider is
// enabled; it can never choose another variable or endpoint.
func adminKeyEnvFor(provider string) (string, bool) {
	switch provider {
	case "openai":
		return adminKeyEnvOpenAI, true
	case "anthropic":
		return adminKeyEnvAnthropic, true
	}
	return "", false
}

// ---- collector ---------------------------------------------------------------

type collectorDeps struct {
	now         func() time.Time
	sleep       func(context.Context, time.Duration) error
	retryDelay  time.Duration
	adminBases  map[string]string // test hook only; never reachable from Config
	adminClient *http.Client
	concurrency int
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type collector struct {
	deps   collectorDeps
	states *sourceStates

	mu    sync.Mutex
	mc    *managementClient
	mcSig string
	admin *http.Client

	profMu   sync.Mutex
	profiles map[string]profileEntry // auth index -> last successful Claude profile
}

func newCollector(d collectorDeps) *collector {
	if d.now == nil {
		d.now = time.Now
	}
	if d.sleep == nil {
		d.sleep = ctxSleep
	}
	if d.retryDelay <= 0 {
		d.retryDelay = time.Second
	}
	if d.concurrency <= 0 || d.concurrency > maxConcurrentSources {
		d.concurrency = maxConcurrentSources
	}
	if d.adminBases == nil {
		d.adminBases = defaultAdminBases
	}
	return &collector{deps: d, states: newSourceStates(), profiles: map[string]profileEntry{}}
}

// ---- Claude profile cache ----------------------------------------------------

const (
	profileCacheTTL        = time.Hour // minimum reuse of a successful profile
	maxProfileCacheEntries = 256
)

// profileEntry keeps only the derived plan (name, "reported" provenance and
// plan-estimate capacities), never the raw profile body or any identity field.
// at is when the profile was observed; it is the age basis for the TTL.
type profileEntry struct {
	at   time.Time
	plan quota.Account
}

// cachedProfile returns the stored plan for authIndex and whether it is still
// fresh at now. A non-fresh result with a non-empty PlanSource is an expired
// entry usable only as a fallback when a refetch fails.
func (c *collector) cachedProfile(authIndex string, now time.Time) (quota.Account, bool) {
	c.profMu.Lock()
	defer c.profMu.Unlock()
	e, ok := c.profiles[authIndex]
	if !ok {
		return quota.Account{}, false
	}
	age := now.Sub(e.at)
	return cloneAccount(e.plan), age >= 0 && age < profileCacheTTL
}

// storeProfile records the plan derived from a successfully fetched profile.
// An unrecognized plan is cached too (as unknown) so it is not refetched every
// pass. The map is bounded: the oldest entry is evicted when full.
func (c *collector) storeProfile(authIndex string, acct quota.Account, now time.Time) {
	if authIndex == "" {
		return
	}
	snap := quota.Account{Plan: acct.Plan, PlanSource: acct.PlanSource, ObservedAt: now}
	for _, w := range acct.Windows {
		if len(w.Models) == 0 && w.Capacity != nil && w.CapacitySource == "plan_estimate" {
			cp := *w.Capacity
			snap.Windows = append(snap.Windows, quota.Window{ID: w.ID, Capacity: &cp, CapacitySource: w.CapacitySource})
		}
	}
	c.profMu.Lock()
	defer c.profMu.Unlock()
	if _, exists := c.profiles[authIndex]; !exists && len(c.profiles) >= maxProfileCacheEntries {
		oldest, first := "", true
		var oldestAt time.Time
		for k, e := range c.profiles {
			if first || e.at.Before(oldestAt) || (e.at.Equal(oldestAt) && k < oldest) {
				oldest, oldestAt, first = k, e.at, false
			}
		}
		delete(c.profiles, oldest)
	}
	c.profiles[authIndex] = profileEntry{at: now, plan: snap}
}

// pruneProfiles drops cache entries for credentials that no longer exist.
func (c *collector) pruneProfiles(live map[string]bool) {
	c.profMu.Lock()
	defer c.profMu.Unlock()
	for k := range c.profiles {
		if !live[k] {
			delete(c.profiles, k)
		}
	}
}

var defaultCollector = newCollector(collectorDeps{})

// collectSnapshot is the background-callable seam. It never returns a
// snapshot that failed quota.Validate: on a non-nil error the returned
// snapshot is an unchanged copy of previous and the health value describes
// what happened. It must not be used when FixtureSnapshotPath is configured.
func collectSnapshot(ctx context.Context, cfg Config, previous quota.Snapshot, inventory InventoryFunc) (quota.Snapshot, collectorHealth, error) {
	return defaultCollector.collect(ctx, cfg, previous, inventory)
}

// CollectOnce is the exported alias of collectSnapshot.
func CollectOnce(ctx context.Context, cfg Config, previous quota.Snapshot, inventory HostInventory) (quota.Snapshot, CollectorStatus, error) {
	return collectSnapshot(ctx, cfg, previous, inventory)
}

func (c *collector) managementClientFor(cfg Config) (*managementClient, error) {
	if cfg.ManagementURL == "" {
		return nil, nil
	}
	sig := cfg.ManagementURL + "\x00" + cfg.ManagementKeyEnv
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mc != nil && c.mcSig == sig {
		return c.mc, nil
	}
	mc, err := newManagementClient(cfg.ManagementURL, cfg.ManagementKeyEnv)
	if err != nil {
		return nil, err
	}
	if c.mc != nil {
		c.mc.close()
	}
	c.mc, c.mcSig = mc, sig
	return mc, nil
}

func (c *collector) adminClient() *http.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deps.adminClient != nil {
		return c.deps.adminClient
	}
	if c.admin == nil {
		c.admin = newAdminHTTPClient()
	}
	return c.admin
}

// ---- grouping ----------------------------------------------------------------

type mergedOverride struct {
	label, billing, plan string
	caps                 map[string]float64
}

type acctGroup struct {
	accountID string
	provider  string
	creds     []quota.Credential // usable first, then by ID
	ov        mergedOverride
}

func credUsable(c quota.Credential) bool { return !c.Disabled && !c.Unavailable }

func mergeOverrides(creds []quota.Credential, all map[string]AccountOverride) mergedOverride {
	var m mergedOverride
	for _, cr := range creds {
		o, ok := all[cr.ID]
		if !ok {
			continue
		}
		if m.label == "" {
			m.label = cleanLabel(o.Label)
		}
		if m.billing == "" {
			m.billing = strings.TrimSpace(o.BillingType)
		}
		if m.plan == "" {
			m.plan = strings.TrimSpace(o.Plan)
		}
		for k, v := range o.CapacityByWindow {
			if m.caps == nil {
				m.caps = map[string]float64{}
			}
			if _, set := m.caps[k]; !set && v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) {
				m.caps[k] = v
			}
		}
	}
	return m
}

// buildGroups assigns each credential to a quota account. The account ID is
// the override accountId when set, else the credential ID. Credentials that
// share an account ID collapse into one account (dedup), but only within one
// provider: a cross-provider collision is split and reported.
func buildGroups(creds []quota.Credential, overrides map[string]AccountOverride, setup *[]setupItem) []*acctGroup {
	byAccount := map[string]*acctGroup{}
	var order []string
	assign := func(cr quota.Credential, id string) {
		g := byAccount[id]
		if g == nil {
			g = &acctGroup{accountID: id, provider: cr.Provider}
			byAccount[id] = g
			order = append(order, id)
		}
		g.creds = append(g.creds, cr)
	}
	for _, cr := range creds {
		id := cr.ID
		if o, ok := overrides[cr.ID]; ok && strings.TrimSpace(o.AccountID) != "" {
			id = strings.TrimSpace(o.AccountID)
		}
		if g := byAccount[id]; g != nil && g.provider != cr.Provider {
			orig := id
			for n := 1; ; n++ {
				id = orig + "@" + cr.Provider
				if n > 1 {
					id += "-" + strconv.Itoa(n)
				}
				if byAccount[id] == nil {
					break
				}
			}
			*setup = append(*setup, setupItem{Provider: cr.Provider, Status: "warning", Message: "account override groups credentials of different providers; credential kept in its own account"})
		}
		assign(cr, id)
	}
	sort.Strings(order)
	out := make([]*acctGroup, 0, len(order))
	for _, id := range order {
		g := byAccount[id]
		sort.SliceStable(g.creds, func(i, j int) bool {
			ui, uj := credUsable(g.creds[i]), credUsable(g.creds[j])
			if ui != uj {
				return ui
			}
			return g.creds[i].ID < g.creds[j].ID
		})
		g.ov = mergeOverrides(g.creds, overrides)
		out = append(out, g)
	}
	return out
}

// ---- fetch -------------------------------------------------------------------

type fetchOutcome struct {
	acct    quota.Account // adapter output; ID/label filled later
	ok      bool
	err     error
	skipped bool // in backoff, no request made
	kind    failKind
}

// quotaKind maps a runtime provider name to the quota adapter family that
// serves it, or "" when there is none. The host's provider key for a
// config-defined OpenAI-compatible provider named "openrouter" is
// "openai-compatible-openrouter"; it is served by the OpenRouter adapter but
// Credential.Provider and Account.Provider keep the original name so that
// scheduler provider joins still match.
func quotaKind(provider string) string {
	switch provider {
	case "claude", "codex", "openrouter":
		return provider
	case "openai-compatible-openrouter":
		return "openrouter"
	}
	return ""
}

func collectibleProvider(p string) bool { return quotaKind(p) != "" }

// fetchEligible reports whether a credential can be queried for provider
// quota: it needs an auth index, must not be disabled, and the credential type
// must match the endpoint (Claude and Codex usage are OAuth-only, OpenRouter's
// key endpoint needs an API key).
func fetchEligible(c quota.Credential) bool {
	if c.AuthIndex == "" || c.Disabled {
		return false
	}
	switch quotaKind(c.Provider) {
	case "claude", "codex":
		return c.AuthType == "oauth"
	case "openrouter":
		return c.AuthType != "oauth"
	}
	return false
}

func (c *collector) withRetry(ctx context.Context, fn func() ([]byte, error)) ([]byte, error) {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var b []byte
		b, err = fn()
		if err == nil {
			return b, nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if kindOf(err) != failTransient || attempt == maxAttempts-1 {
			return nil, err
		}
		if serr := c.deps.sleep(ctx, c.deps.retryDelay<<attempt); serr != nil {
			return nil, serr
		}
	}
	return nil, err
}

func (c *collector) fetchOne(ctx context.Context, mc *managementClient, cr quota.Credential, prev *quota.Account, now time.Time) (quota.Account, error) {
	call := func(ep string) ([]byte, error) {
		return c.withRetry(ctx, func() ([]byte, error) { return mc.call(ctx, ep, cr.AuthIndex) })
	}
	switch quotaKind(cr.Provider) {
	case "claude":
		usage, err := call(epClaudeUsage)
		if err != nil {
			return quota.Account{}, err
		}
		// The plan tier changes rarely: a successful profile is reused for
		// profileCacheTTL (keyed by auth index) instead of every pass.
		cachedPlan, fresh := c.cachedProfile(cr.AuthIndex, now)
		var profile []byte
		var perr error
		if !fresh {
			profile, perr = call(epClaudeProfile)
			if perr != nil {
				if ctx.Err() != nil {
					return quota.Account{}, ctx.Err()
				}
				profile = nil
			}
		}
		acct, aerr := adapters.Claude(usage, profile, now)
		if aerr != nil {
			return quota.Account{}, &sourceError{kind: failParse}
		}
		switch {
		case fresh:
			carryPlan(&acct, &cachedPlan)
		case perr == nil && acct.Plan == "" && cachedPlan.PlanSource != "":
			carryPlan(&acct, &cachedPlan) // unreadable profile must not erase a known plan
		case perr == nil:
			c.storeProfile(cr.AuthIndex, acct, now)
		case cachedPlan.PlanSource != "":
			carryPlan(&acct, &cachedPlan) // expired entry beats no data
		default:
			carryPlan(&acct, prev)
		}
		return acct, nil
	case "codex":
		body, err := call(epCodexUsage)
		if err != nil {
			return quota.Account{}, err
		}
		acct, aerr := adapters.Codex(body, now)
		if aerr != nil {
			return quota.Account{}, &sourceError{kind: failParse}
		}
		return acct, nil
	case "openrouter":
		body, err := call(epOpenRouterKey)
		if err != nil {
			return quota.Account{}, err
		}
		acct, aerr := adapters.OpenRouterKey(body, now)
		if aerr != nil {
			return quota.Account{}, &sourceError{kind: failParse}
		}
		return acct, nil
	}
	return quota.Account{}, &sourceError{kind: failRejected}
}

// fetchGroup tries the group's eligible credentials in order until one
// succeeds. Rate limiting and management-key failures stop the sequence
// (another credential of the same account would not help).
func (c *collector) fetchGroup(ctx context.Context, mc *managementClient, g *acctGroup, prev *quota.Account, now time.Time) fetchOutcome {
	key := g.provider + "\x00" + g.accountID
	if kind, blocked := c.states.blocked(key, now); blocked {
		return fetchOutcome{skipped: true, kind: kind, err: &sourceError{kind: kind}}
	}
	var last error
	for _, cr := range g.creds {
		if !fetchEligible(cr) {
			continue
		}
		acct, err := c.fetchOne(ctx, mc, cr, prev, now)
		if err == nil {
			c.states.succeed(key)
			return fetchOutcome{acct: acct, ok: true}
		}
		if ctx.Err() != nil {
			return fetchOutcome{err: ctx.Err()}
		}
		last = err
		if k := kindOf(err); k == failRateLimited || k == failKeyMissing || k == failKeyRejected {
			break // another credential of the same account would not help
		}
	}
	if last == nil {
		last = &sourceError{kind: failRejected}
	}
	if k := kindOf(last); k != failKeyMissing && k != failKeyRejected {
		c.states.fail(key, last, now)
	}
	return fetchOutcome{err: last, kind: kindOf(last)}
}

// ---- admin cost reports ------------------------------------------------------

func (c *collector) collectAdminCost(ctx context.Context, provider, keyEnv string, now time.Time) (quota.Account, error) {
	key := os.Getenv(keyEnv)
	if key == "" {
		return quota.Account{}, &sourceError{kind: failKeyMissing}
	}
	base, ok := c.deps.adminBases[provider]
	if !ok || !strings.HasPrefix(base, "https://") {
		return quota.Account{}, &sourceError{kind: failRejected}
	}
	hc := c.adminClient()
	var path string
	query := func(page string) url.Values {
		q := url.Values{}
		q.Set("bucket_width", "1d")
		q.Set("limit", strconv.Itoa(adminPageLimit))
		start, end := now.Add(-adminCostWindow), now
		if provider == "openai" {
			q.Set("start_time", strconv.FormatInt(start.Unix(), 10))
			q.Set("end_time", strconv.FormatInt(end.Unix(), 10))
		} else {
			q.Set("starting_at", start.UTC().Format(time.RFC3339))
			q.Set("ending_at", end.UTC().Format(time.RFC3339))
		}
		if page != "" {
			q.Set("page", page)
		}
		return q
	}
	source := ""
	switch provider {
	case "openai":
		path, source = "/v1/organization/costs", "openai-costs"
	case "anthropic":
		path, source = "/v1/organizations/cost_report", "anthropic-cost-report"
	default:
		return quota.Account{}, &sourceError{kind: failRejected}
	}
	var total float64
	page := ""
	seen := map[string]bool{}
	complete := false
	for i := 0; i < maxAdminPages; i++ {
		body, err := c.withRetry(ctx, func() ([]byte, error) {
			return adminGet(ctx, hc, base, provider, path, query(page), key, now)
		})
		if err != nil {
			return quota.Account{}, err
		}
		var cp adapters.CostPage
		if provider == "openai" {
			cp, err = adapters.OpenAICost(body, now)
		} else {
			cp, err = adapters.AnthropicCost(body, now)
		}
		if err != nil {
			return quota.Account{}, &sourceError{kind: failParse}
		}
		total += cp.AmountUSD
		if math.IsNaN(total) || math.IsInf(total, 0) {
			return quota.Account{}, &sourceError{kind: failParse}
		}
		if !cp.HasMore {
			complete = true
			break
		}
		if cp.NextPage == "" || seen[cp.NextPage] || len(cp.NextPage) > 512 {
			return quota.Account{}, &sourceError{kind: failIncomplete}
		}
		seen[cp.NextPage] = true
		page = cp.NextPage
	}
	if !complete {
		return quota.Account{}, &sourceError{kind: failIncomplete}
	}
	return quota.Account{
		Provider: provider, BillingType: "api", ObservedAt: now, RefreshStatus: refreshOK,
		Financial: []quota.Financial{{
			Kind: "upstream-spend", ScopeID: adminScope, Currency: "USD",
			Source: source, Amount: total, ObservedAt: now,
		}},
	}, nil
}

func adminAccountID(provider string) string { return "admin:" + provider + ":organization-costs" }

// ---- pass --------------------------------------------------------------------

type task struct {
	key string
	run func(context.Context) fetchOutcome
}

func (c *collector) runTasks(ctx context.Context, tasks []task) []fetchOutcome {
	out := make([]fetchOutcome, len(tasks))
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < c.deps.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				func() {
					defer func() {
						if recover() != nil {
							out[i] = fetchOutcome{err: errCollectorPanic, kind: failParse}
						}
					}()
					out[i] = tasks[i].run(ctx)
				}()
			}
		}()
	}
loop:
	for i := range tasks {
		select {
		case idx <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(idx)
	wg.Wait()
	return out
}

func (c *collector) collect(ctx context.Context, cfg Config, previous quota.Snapshot, inv InventoryFunc) (snap quota.Snapshot, health collectorHealth, err error) {
	health.Setup = []setupItem{}
	keep := func(status, msg string, e error) (quota.Snapshot, collectorHealth, error) {
		health.Status, health.LastError = status, msg
		health.LastRefreshAt = latestObservation(previous)
		return cloneSnapshot(previous), health, e
	}
	defer func() {
		if r := recover(); r != nil {
			snap, health, err = keep(healthFailed, "collector pass failed", errCollectorPanic)
		}
	}()
	if cerr := ctx.Err(); cerr != nil {
		return keep(healthCanceled, "collection canceled", cerr)
	}
	if inv == nil {
		return keep(healthInventoryUnavailable, "host inventory unavailable", errInventoryUnavailable)
	}
	list, models, ierr := inv(ctx)
	if ierr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return keep(healthCanceled, "collection canceled", cerr)
		}
		return keep(healthInventoryUnavailable, "host inventory unavailable", errInventoryUnavailable)
	}
	creds, _, uerr := unionInventory(list, models)
	if uerr != nil {
		return keep(healthInventoryUnavailable, "host inventory unavailable", errInventoryUnavailable)
	}
	now := c.deps.now().UTC()

	prevAccts := map[string]quota.Account{}
	for _, a := range previous.Accounts {
		prevAccts[a.ID] = a
	}
	groups := buildGroups(creds, cfg.AccountOverrides, &health.Setup)

	// management availability
	mc, merr := c.managementClientFor(cfg)
	mgmtReady := false
	switch {
	case cfg.ManagementURL == "":
		// quota fetching is optional; inventory-only snapshot
	case merr != nil:
		health.Setup = append(health.Setup, setupItem{Provider: "management", Status: "setup_required", Message: "managementURL or managementKeyEnv is invalid"})
	case !mc.keyAvailable():
		health.Setup = append(health.Setup, setupItem{Provider: "management", Status: "setup_required", Message: "management key environment variable is not set"})
	default:
		mgmtReady = true
	}

	// tasks
	var tasks []task
	groupTask := map[string]int{}
	live := map[string]bool{}
	if mgmtReady {
		for _, g := range groups {
			if !collectibleProvider(g.provider) {
				continue
			}
			eligible := false
			for _, cr := range g.creds {
				if fetchEligible(cr) {
					eligible = true
					break
				}
			}
			if !eligible {
				continue
			}
			g := g
			var prev *quota.Account
			if p, ok := prevAccts[g.accountID]; ok {
				pp := cleanPrevAccount(p)
				prev = &pp
			}
			live[g.provider+"\x00"+g.accountID] = true
			groupTask[g.accountID] = len(tasks)
			tasks = append(tasks, task{key: g.accountID, run: func(ctx context.Context) fetchOutcome {
				return c.fetchGroup(ctx, mc, g, prev, now)
			}})
		}
	}
	// admin sources
	adminTask := map[string]int{}
	seenAdmin := map[string]bool{}
	for _, as := range cfg.AdminSources {
		p := strings.ToLower(strings.TrimSpace(as.Provider))
		want, allowed := adminKeyEnvFor(p)
		switch {
		case !allowed || strings.TrimSpace(as.KeyEnv) != want:
			name := p
			if name == "" {
				name = "admin"
			}
			health.Setup = append(health.Setup, setupItem{Provider: name, Status: "setup_required", Message: "admin report source must use a supported provider (openai, anthropic) and its fixed key environment variable"})
			continue
		case seenAdmin[p]:
			continue
		}
		seenAdmin[p] = true
		if os.Getenv(want) == "" {
			health.Setup = append(health.Setup, setupItem{Provider: p, Status: "setup_required", Message: "admin key environment variable is not set"})
			continue
		}
		skey := "admin\x00" + p
		live[skey] = true
		adminTask[p] = len(tasks)
		tasks = append(tasks, task{key: p, run: func(ctx context.Context) fetchOutcome {
			if kind, blocked := c.states.blocked(skey, now); blocked {
				return fetchOutcome{skipped: true, kind: kind, err: &sourceError{kind: kind}}
			}
			acct, err := c.collectAdminCost(ctx, p, want, now)
			if err != nil {
				if ctx.Err() != nil {
					return fetchOutcome{err: ctx.Err()}
				}
				if k := kindOf(err); k != failKeyMissing {
					c.states.fail(skey, err, now)
				}
				return fetchOutcome{err: err, kind: kindOf(err)}
			}
			c.states.succeed(skey)
			return fetchOutcome{acct: acct, ok: true}
		}})
	}
	c.states.prune(live)
	liveIdx := map[string]bool{}
	for _, cr := range creds {
		if cr.AuthIndex != "" {
			liveIdx[cr.AuthIndex] = true
		}
	}
	c.pruneProfiles(liveIdx)

	results := c.runTasks(ctx, tasks)
	if cerr := ctx.Err(); cerr != nil {
		return keep(healthCanceled, "collection canceled", cerr)
	}

	// assemble
	out := quota.Snapshot{
		SchemaVersion: quota.SchemaVersion,
		Revision:      previous.Revision + 1,
		GeneratedAt:   now,
		Accounts:      []quota.Account{},
		Credentials:   []quota.Credential{},
		Pools:         []quota.Pool{},
	}
	failCounts := map[failKind]int{}
	attempted, failed := len(tasks), 0
	for _, r := range results {
		if r.ok {
			continue
		}
		failed++
		failCounts[r.kind]++
	}

	unsupported := map[string]bool{}
	noEndpoint := map[string]bool{}
	unmatched := map[string]bool{}
	for _, g := range groups {
		acct := c.assembleAccount(g, prevAccts, results, groupTask, mgmtReady, now)
		if !collectibleProvider(g.provider) {
			unsupported[g.provider] = true
		} else if acct.RefreshStatus == refreshUnsupported {
			noEndpoint[g.provider] = true
		}
		for _, w := range acct.Windows {
			if adapters.IsUnmatchedScope(w.Models) {
				unmatched[g.provider] = true
				break
			}
		}
		out.Accounts = append(out.Accounts, acct)
		for _, cr := range g.creds {
			cr.AccountID = g.accountID
			out.Credentials = append(out.Credentials, cr)
		}
	}
	sort.Slice(out.Credentials, func(i, j int) bool { return out.Credentials[i].ID < out.Credentials[j].ID })

	for _, p := range sortedKeys(adminTask) {
		id := adminAccountID(p)
		r := results[adminTask[p]]
		acct := quota.Account{ID: id, Provider: p, Label: p + " organization costs (admin report)", BillingType: "api"}
		if r.ok {
			acct = r.acct
			acct.ID, acct.Label = id, p+" organization costs (admin report)"
		} else if prev, ok := prevAccts[id]; ok {
			pp := cleanPrevAccount(prev)
			acct.Financial, acct.ObservedAt = pp.Financial, pp.ObservedAt
			acct.RefreshStatus = refreshError
			if len(pp.Financial) > 0 {
				acct.RefreshStatus = refreshStale
			}
		} else {
			acct.RefreshStatus = refreshError
		}
		if acct.Windows == nil {
			acct.Windows = []quota.Window{}
		}
		if acct.Financial == nil {
			acct.Financial = []quota.Financial{}
		}
		out.Accounts = append(out.Accounts, acct)
	}
	// previously collected admin accounts whose source is not currently
	// collectible (key unset, setup-required) stay as stale observations.
	for _, p := range []string{"anthropic", "openai"} {
		id := adminAccountID(p)
		if _, active := adminTask[p]; active {
			continue
		}
		prev, ok := prevAccts[id]
		if !ok || !seenAdmin[p] {
			continue
		}
		pp := cleanPrevAccount(prev)
		pp.RefreshStatus = refreshStale
		if pp.Windows == nil {
			pp.Windows = []quota.Window{}
		}
		out.Accounts = append(out.Accounts, pp)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].ID < out.Accounts[j].ID })

	for _, p := range sortedKeys(unsupported) {
		health.Setup = append(health.Setup, setupItem{Provider: p, Status: "unsupported", Message: "no quota collector for this provider; capacity stays unknown"})
	}
	for _, p := range sortedKeys(unmatched) {
		health.Setup = append(health.Setup, setupItem{Provider: p, Status: "warning", Message: "a scoped weekly limit names only a model label, not a model ID; it matches no model and is not applied to routing"})
	}
	for _, p := range sortedKeys(noEndpoint) {
		health.Setup = append(health.Setup, setupItem{Provider: p, Status: "unsupported", Message: "credential type has no quota endpoint or is not positively classified as OAuth (for example an API-key or unclassified Claude/Codex entry, or an OAuth OpenRouter entry); capacity stays unknown"})
	}
	out.Pools = buildPools(out, cfg.Pools, &health.Setup)
	sort.SliceStable(health.Setup, func(i, j int) bool {
		if health.Setup[i].Provider != health.Setup[j].Provider {
			return health.Setup[i].Provider < health.Setup[j].Provider
		}
		return health.Setup[i].Message < health.Setup[j].Message
	})
	if len(health.Setup) > 64 {
		health.Setup = health.Setup[:64]
	}

	if verr := quota.Validate(out); verr != nil {
		return keep(healthInvalidSnapshot, "collected snapshot invalid", errInvalidSnapshot)
	}

	health.LastRefreshAt = latestObservation(out)
	switch {
	case cfg.ManagementURL == "" && attempted == 0:
		health.Status = healthNotConfigured
	case attempted == 0 && !mgmtReady && cfg.ManagementURL != "":
		health.Status = healthSetupRequired
	case attempted == 0:
		health.Status = healthIdle
	case failed == 0:
		health.Status = healthOK
	case failed == attempted:
		health.Status = healthFailed
	default:
		health.Status = healthPartial
	}
	if failed > 0 {
		health.LastError = failureSummary(failed, attempted, failCounts)
	}
	return out, health, nil
}

func failureSummary(failed, attempted int, counts map[failKind]int) string {
	kinds := make([]int, 0, len(counts))
	for k := range counts {
		kinds = append(kinds, int(k))
	}
	sort.Ints(kinds)
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		msg := failMessages[failKind(k)]
		if msg == "" {
			msg = "collection failed"
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", msg, counts[failKind(k)]))
	}
	return fmt.Sprintf("%d of %d quota sources failed: %s", failed, attempted, strings.Join(parts, ", "))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func latestObservation(s quota.Snapshot) time.Time {
	var t time.Time
	for _, a := range s.Accounts {
		if len(a.Windows) == 0 && len(a.Financial) == 0 {
			continue
		}
		if a.ObservedAt.After(t) {
			t = a.ObservedAt
		}
	}
	return t
}

// assembleAccount builds one account: fresh adapter output, or the previous
// observations preserved verbatim (original timestamps, never refreshed
// values) when the refresh failed or was skipped.
func (c *collector) assembleAccount(g *acctGroup, prevAccts map[string]quota.Account, results []fetchOutcome, groupTask map[string]int, mgmtReady bool, now time.Time) quota.Account {
	label := g.ov.label
	if label == "" {
		label = unknownLabel
		for _, cr := range g.creds {
			if cr.Label != "" && cr.Label != unknownLabel {
				label = cr.Label
				break
			}
		}
	}
	acct := quota.Account{ID: g.accountID, Provider: g.provider, Label: label}
	prev, hadPrev := prevAccts[g.accountID]
	if hadPrev && prev.Provider != g.provider {
		hadPrev = false
	}
	if hadPrev {
		prev = cleanPrevAccount(prev)
	}

	idx, fetched := groupTask[g.accountID]
	switch {
	case fetched && results[idx].ok:
		a := results[idx].acct
		acct.BillingType, acct.Plan, acct.PlanSource = a.BillingType, a.Plan, a.PlanSource
		acct.ObservedAt, acct.RefreshStatus = a.ObservedAt, refreshOK
		acct.Windows, acct.Financial = a.Windows, a.Financial
		if hadPrev {
			acct.Windows = carryForwardWindows(acct.Windows, prev.Windows)
		}
	case hadPrev:
		acct.BillingType, acct.Plan, acct.PlanSource = prev.BillingType, prev.Plan, prev.PlanSource
		acct.ObservedAt = prev.ObservedAt
		acct.Windows, acct.Financial = prev.Windows, prev.Financial
		if len(acct.Windows) > 0 || len(acct.Financial) > 0 {
			acct.RefreshStatus = refreshStale
		} else {
			acct.RefreshStatus = unobservedStatus(g, fetched)
		}
	default:
		acct.RefreshStatus = unobservedStatus(g, fetched)
	}
	if acct.BillingType == "" {
		acct.BillingType = "unknown"
	}

	// overrides apply to the collected (or preserved) account
	if g.ov.billing != "" {
		acct.BillingType = g.ov.billing
	}
	if g.ov.plan != "" {
		acct.Plan, acct.PlanSource = g.ov.plan, "operator_override"
	}
	acct.Windows = applyCapacityOverrides(acct.Windows, g.ov.caps)
	if acct.Windows == nil {
		acct.Windows = []quota.Window{}
	}
	if acct.Financial == nil {
		acct.Financial = []quota.Financial{}
	}
	return acct
}

func unobservedStatus(g *acctGroup, fetched bool) string {
	if !collectibleProvider(g.provider) {
		return refreshUnsupported
	}
	allDisabled := true
	eligible := false
	for _, cr := range g.creds {
		if !cr.Disabled {
			allDisabled = false
		}
		if fetchEligible(cr) {
			eligible = true
		}
	}
	switch {
	case allDisabled:
		return refreshDisabled
	case !eligible:
		return refreshUnsupported
	}
	return refreshError
}

// ---- observation hygiene -----------------------------------------------------

func windowKey(w quota.Window) string {
	m := append([]string(nil), w.Models...)
	sort.Strings(m)
	return w.ID + "\x00" + strings.Join(m, "\x00")
}

func finiteF(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func cloneWindow(w quota.Window) quota.Window {
	if w.UsedFraction != nil {
		v := *w.UsedFraction
		w.UsedFraction = &v
	}
	if w.Capacity != nil {
		v := *w.Capacity
		w.Capacity = &v
	}
	if w.Models != nil {
		w.Models = append([]string(nil), w.Models...)
	}
	return w
}

func cloneAccount(a quota.Account) quota.Account {
	ws := make([]quota.Window, len(a.Windows))
	for i, w := range a.Windows {
		ws[i] = cloneWindow(w)
	}
	a.Windows = ws
	a.Financial = append([]quota.Financial{}, a.Financial...)
	return a
}

func cloneSnapshot(s quota.Snapshot) quota.Snapshot {
	out := s
	out.Accounts = make([]quota.Account, len(s.Accounts))
	for i, a := range s.Accounts {
		out.Accounts[i] = cloneAccount(a)
	}
	out.Credentials = make([]quota.Credential, len(s.Credentials))
	for i, cr := range s.Credentials {
		cr.Models = append([]string{}, cr.Models...)
		out.Credentials[i] = cr
	}
	out.Pools = make([]quota.Pool, len(s.Pools))
	for i, p := range s.Pools {
		p.CredentialIDs = append([]string{}, p.CredentialIDs...)
		out.Pools[i] = p
	}
	return out
}

// cleanPrevAccount deep-copies a previous account and drops entries that
// would make the next snapshot invalid, so one bad stored value can never
// wedge collection.
func cleanPrevAccount(a quota.Account) quota.Account {
	a = cloneAccount(a)
	seen := map[string]bool{}
	ws := a.Windows[:0]
	for _, w := range a.Windows {
		if strings.TrimSpace(w.ID) == "" || w.DurationSeconds < 0 || seen[windowKey(w)] {
			continue
		}
		if w.UsedFraction != nil && (!finiteF(*w.UsedFraction) || *w.UsedFraction < 0 || *w.UsedFraction > 1) {
			continue
		}
		if w.Capacity != nil && (!finiteF(*w.Capacity) || *w.Capacity <= 0) {
			w.Capacity, w.CapacitySource = nil, ""
		}
		bad := false
		for _, m := range w.Models {
			if strings.TrimSpace(m) == "" {
				bad = true
			}
		}
		if bad {
			continue
		}
		seen[windowKey(w)] = true
		ws = append(ws, w)
	}
	a.Windows = ws
	fs := a.Financial[:0]
	for _, f := range a.Financial {
		switch f.Kind {
		case "upstream-spend", "prepaid-balance", "key-budget-remaining", "configured-budget-remaining", "gateway-estimated-cost":
			if finiteF(f.Amount) {
				fs = append(fs, f)
			}
		}
	}
	a.Financial = fs
	return a
}

// carryForwardWindows keeps a previous measured window when the fresh
// observation of the same window carries no usage value (null/invalid
// upstream field). The previous window keeps its own ObservedAt, so it ages
// out normally; no value is invented.
func carryForwardWindows(fresh, prev []quota.Window) []quota.Window {
	byKey := map[string]quota.Window{}
	for _, w := range prev {
		if w.UsedFraction != nil {
			byKey[windowKey(w)] = w
		}
	}
	out := make([]quota.Window, len(fresh))
	for i, w := range fresh {
		if w.UsedFraction == nil {
			if p, ok := byKey[windowKey(w)]; ok {
				if w.Capacity != nil {
					cp := *w.Capacity
					p.Capacity, p.CapacitySource = &cp, w.CapacitySource
				}
				w = p
			}
		}
		out[i] = w
	}
	return out
}

// carryPlan restores the previously reported plan (and its plan-estimate
// capacities) when this refresh could not read the profile.
func carryPlan(acct *quota.Account, prev *quota.Account) {
	if prev == nil || acct.Plan != "" || prev.Plan == "" || prev.PlanSource != "reported" {
		return
	}
	acct.Plan, acct.PlanSource = prev.Plan, prev.PlanSource
	pc := map[string]quota.Window{}
	for _, w := range prev.Windows {
		if len(w.Models) == 0 && w.Capacity != nil && w.CapacitySource == "plan_estimate" {
			pc[w.ID] = w
		}
	}
	for i, w := range acct.Windows {
		if p, ok := pc[w.ID]; ok && len(w.Models) == 0 && w.Capacity == nil {
			v := *p.Capacity
			acct.Windows[i].Capacity, acct.Windows[i].CapacitySource = &v, p.CapacitySource
		}
	}
}

// applyCapacityOverrides sets operator capacities by window ID (or
// "id|model,scope") and removes an operator capacity that no longer has an
// override, so a deleted override cannot linger in preserved observations.
func applyCapacityOverrides(ws []quota.Window, caps map[string]float64) []quota.Window {
	for i, w := range ws {
		scoped := w.ID
		if len(w.Models) > 0 {
			m := append([]string(nil), w.Models...)
			sort.Strings(m)
			scoped = w.ID + "|" + strings.Join(m, ",")
		}
		v, ok := caps[scoped]
		if !ok && len(w.Models) == 0 {
			v, ok = caps[w.ID]
		}
		switch {
		case ok && v > 0 && finiteF(v):
			c := v
			ws[i].Capacity, ws[i].CapacitySource = &c, "operator_override"
		case w.CapacitySource == "operator_override":
			ws[i].Capacity, ws[i].CapacitySource = nil, ""
		}
	}
	return ws
}

// ---- pools -------------------------------------------------------------------

// buildPools returns auto pools (one per provider+billingType with a known
// billing type) plus operator pools. Explicit pools replace an auto pool with
// the same ID. Unresolved or cross-provider explicit members are dropped
// (membership fails closed) and reported; an ID-only pool with no resolvable
// member is omitted and reported.
func buildPools(s quota.Snapshot, explicit []quota.Pool, setup *[]setupItem) []quota.Pool {
	credProvider := map[string]string{}
	for _, cr := range s.Credentials {
		credProvider[cr.ID] = cr.Provider
	}
	acctBilling := map[string]string{}
	for _, a := range s.Accounts {
		acctBilling[a.ID] = a.BillingType
	}
	pools := map[string]quota.Pool{}
	for _, cr := range s.Credentials {
		b := acctBilling[cr.AccountID]
		if b == "" || b == "unknown" {
			continue
		}
		id := "auto-" + cr.Provider + "-" + b
		if _, ok := pools[id]; ok {
			continue
		}
		pools[id] = quota.Pool{ID: id, Label: cr.Provider + " " + b, Provider: cr.Provider, CredentialIDs: []string{}, BillingType: b}
	}
	for _, p := range explicit {
		id := strings.TrimSpace(p.ID)
		prov := strings.TrimSpace(p.Provider)
		if id == "" || prov == "" {
			*setup = append(*setup, setupItem{Provider: "pools", Status: "warning", Message: "a configured pool without id or provider was ignored"})
			continue
		}
		np := quota.Pool{ID: id, Label: p.Label, Provider: prov, BillingType: strings.TrimSpace(p.BillingType), CredentialIDs: []string{}}
		if np.Label == "" {
			np.Label = id
		}
		seen := map[string]bool{}
		dropped := 0
		for _, cid := range p.CredentialIDs {
			if seen[cid] {
				continue
			}
			seen[cid] = true
			if cp, ok := credProvider[cid]; !ok || cp != prov {
				dropped++
				continue
			}
			np.CredentialIDs = append(np.CredentialIDs, cid)
		}
		if dropped > 0 {
			*setup = append(*setup, setupItem{Provider: "pools", Status: "warning", Message: fmt.Sprintf("pool %q: %d member(s) unknown or of another provider were excluded", id, dropped)})
		}
		if len(np.CredentialIDs) == 0 && np.BillingType == "" {
			*setup = append(*setup, setupItem{Provider: "pools", Status: "warning", Message: fmt.Sprintf("pool %q has no resolvable members and was omitted", id)})
			delete(pools, id)
			continue
		}
		pools[id] = np
	}
	out := make([]quota.Pool, 0, len(pools))
	for _, id := range sortedKeys(pools) {
		out = append(out, pools[id])
	}
	return out
}

// ---- background loop ---------------------------------------------------------

// startupInventoryAttempts and startupInventoryDelay shape the retry of the
// first collection pass. The host hands a plugin its auth inventory only after
// the plugin registers, so a first pass can see no credentials even though the
// host has plenty; retrying for a couple of seconds turns that race into a
// populated snapshot instead of an empty one that blanks every derived pool
// until the next poll tick.
const (
	startupInventoryAttempts = 10
	startupInventoryDelay    = 250 * time.Millisecond
)

// firstPassWithRetry runs the startup pass and repeats it while the result
// carries no credentials. A pass that errors is returned immediately: a host or
// transport failure is not the cold-start race this guards against, and
// retrying it would only delay the health report. wait receives the delay
// between attempts and reports whether it completed before the context ended.
func firstPassWithRetry(ctx context.Context, attempts int, delay time.Duration,
	pass func(context.Context) (quota.Snapshot, collectorHealth, error),
	wait func(context.Context, time.Duration) bool) (quota.Snapshot, collectorHealth, error) {
	snap, health, err := pass(ctx)
	for attempt := 1; attempt < attempts; attempt++ {
		if err != nil || len(snap.Credentials) > 0 {
			return snap, health, err
		}
		if !wait(ctx, delay) {
			return snap, health, err
		}
		snap, health, err = pass(ctx)
	}
	return snap, health, err
}

// waitContext sleeps for d unless the context ends first.
func waitContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// LoopHooks wires the collector loop to parent state. All hooks run on the
// loop goroutine only; Publish must be safe to call from it (the parent takes
// its own lock). On a pass error Publish receives the unchanged previous
// snapshot plus the health report; it should store health and keep serving
// the snapshot it already has.
type LoopHooks struct {
	Config      func() Config
	Previous    func() quota.Snapshot
	Inventory   InventoryFunc
	Publish     func(quota.Snapshot, collectorHealth, error)
	MinInterval time.Duration // zero means 10s
	Collector   *collector    // nil means the package default
}

// StartCollector runs one pass immediately and then one per PollIntervalSeconds
// (default 180s) until ctx is canceled. A value received on refresh triggers an
// extra pass; triggers that arrive during a pass coalesce into one. The
// returned channel closes after the loop and any in-flight pass have ended
// (passes honor ctx, so cancellation is prompt).
func StartCollector(ctx context.Context, h LoopHooks, refresh <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	col := h.Collector
	if col == nil {
		col = defaultCollector
	}
	minIv := h.MinInterval
	if minIv <= 0 {
		minIv = 10 * time.Second
	}
	go func() {
		defer close(done)
		first := true
		for {
			cfg := h.Config()
			pctx, cancel := context.WithTimeout(ctx, passTimeout)
			var (
				snap   quota.Snapshot
				health collectorHealth
				err    error
			)
			if first {
				// The host loads plugins before its auth inventory is complete, so the first
				// pass may legitimately see no credentials. Publishing that pass would blank
				// every derived pool until the next tick, so retry it first.
				first = false
				snap, health, err = firstPassWithRetry(pctx, startupInventoryAttempts, startupInventoryDelay,
					func(c context.Context) (quota.Snapshot, collectorHealth, error) {
						return col.collect(c, cfg, h.Previous(), h.Inventory)
					}, waitContext)
			} else {
				snap, health, err = col.collect(pctx, cfg, h.Previous(), h.Inventory)
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
			h.Publish(snap, health, err)

			iv := time.Duration(cfg.PollIntervalSeconds) * time.Second
			if iv <= 0 {
				iv = 180 * time.Second
			}
			if iv < minIv {
				iv = minIv
			}
			timer := time.NewTimer(iv)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-refresh:
				timer.Stop()
			}
		}
	}()
	return done
}
