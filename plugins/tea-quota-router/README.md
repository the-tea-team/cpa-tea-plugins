# Tea Capacity & Resets

Native CPA plugin `tea-quota-router` v0.2.0. CPA SDK pinned to v8.0.69. Linux is the supported snapshot-sharing target.

## Included

- Provider/window-specific capacity-weighted subscription pools with freshness, unknown capacity and coverage; account deduplication and explicit overrides.
- Reset timestamps, scoped-model limits and observed opportunities (not token forecasts).
- Safe runtime inventory from the union of `host.auth.list` and `host.auth.models`, including config API-key credentials omitted by the former.
- Background Claude OAuth usage/profile, Codex usage and OpenRouter key observation through CPA-owned token injection. Profile polling is cached for an hour; snapshots retain their actual observation timestamps on fetch failure.
- Optional OpenAI/Anthropic organization cost collection with separate admin access. These are spend reports, not prepaid balance. OpenRouter account-credit parser exists; account-credit polling is not wired in this release.
- In-process credential-attributed gateway usage collection into SQLite (not a second usage-queue consumer), 30-day retention, and nullable cost estimates only from explicitly configured per-million prices.
- Plugin-owned account affinity uses a hashed caller/model/session tuple when CPA supplies the required metadata; 60-minute idle TTL and bounded entries. Missing caller/session metadata does not create a shared binding. Core affinity lookup is unavailable while any plugin scheduler is installed. Native direct-chat prompt_cache_key affinity is proven; broader nested-combo/caller coverage remains a rollout gate.
- Shadow-default, provider-opt-in quota-aware credential scheduler. Core-provided eligibility remains authoritative. `quotaAcrossPriorities: true` intentionally ignores numeric account priority (default false preserves tiers). Selection weights are request-share heuristics, not guaranteed token allocation.
- Embedded `/v0/resource/plugins/tea-quota-router/capacity` dashboard; all data requires management authentication.
- Transactional `state-v1.json` owns observations, overrides and materialized state. The shared `snapshot-v1.json` and legacy `overrides.json` are derived exports; export failure is reported and repaired on restart.
- The first collection pass retries while the host's inventory is still unreadable (up to ten attempts, 250 ms apart). CPA registers plugins before it can list credentials, and reveals which models each credential serves even later, so an early pass publishes either no pools or pools whose members answer no model; either one freezes quota-aware routing until the next poll tick. A pass counts as publishable when every pool it built has at least one member with a known model inventory.

## Configure

```yaml
plugins:
  configs:
    tea-quota-router:
      enabled: true
      dataDir: /var/lib/cliproxyapi/tea-quota-router
      schedulerMode: off
      quotaAcrossPriorities: false
      providerStrategies:
        claude: quota-aware-round-robin
      managementURL: http://127.0.0.1:8317
      managementKeyEnv: CPA_QUOTA_MANAGEMENT_KEY
      pollIntervalSeconds: 180
      maxAgeSeconds: 600
      resetBoost:            # optional; omit to disable
        horizonHours: 24
        maxMultiplier: 3
        ramp: false          # default: flat multiplier for the whole horizon
      drainToZero: false     # optional; true drops the headroom guard
```

`drainToZero: true` removes the headroom guard, which normally scales an account down once any window
has less than 10% left. With it, an account keeps full weight until a window is actually exhausted.
Expect a few 429s right at exhaustion: the snapshot only refreshes every `pollIntervalSeconds`, so the
scheduler learns an account is empty up to one poll late.

`resetBoost` spends quota that is about to expire. When an account's weekly (or any window longer
than `horizonHours`) resets within `horizonHours`, its score is multiplied by `maxMultiplier` for the
whole horizon. Keep it soft (2-3x): a flat multiplier spreads the extra traffic across the last day,
while a big or ramped one concentrates it at the end, where session windows and rate limits block it
anyway. `ramp: true` grows the multiplier linearly from 1 at the horizon to `maxMultiplier` at the
reset instead. Several accounts can be boosted at once. Session windows never trigger it.
It applies after the headroom guard, so a nearly empty account stays suppressed, and it never revives
an exhausted, stale or unknown account. Decisions show `reset boost xN` in their reasons. tea-combos
reads its own `resetBoost` block for combo account and member weighting; set both to the same value.

`CPA_QUOTA_MANAGEMENT_KEY` is an environment-variable reference, not a key value. Supply through your protected service environment. The collector uses only the literal loopback management origin and fixed provider endpoint allowlist. It never duplicates OAuth refresh tokens. With no management client, inventory and gateway usage still work; upstream collection reports setup-required.

`accountOverrides` is keyed by native auth ID, with optional `accountId`, `label`, `billingType`, `plan` and `capacityByWindow`. Explicit `pools` contain provider plus billing type or credential IDs. Shared account IDs must not span providers. Overrides supplied through the UI take precedence and are committed to state. Removal recomputes from the raw observation baseline.

`fixtureSnapshotPath` is for explicit synthetic testing only. Switching fixture/live mode requires restart; it is never enabled implicitly. No example account is presented as live data.

`adminSources` supports OpenAI or Anthropic cost reports. Read `adapters/README.md` for source references and limitations. Normal inference keys do not imply organization billing permission. Missing setup does not suppress an account's gateway usage.

## Dashboard authentication

CPAMP v1.14.10 authenticates management proxy calls by header, not a reusable iframe cookie session. The page first tries same-origin requests, then offers an in-memory management/panel-key prompt on 401/403. The key is cleared from the form after submission and is not placed in localStorage or sessionStorage. The public resource serves only the static shell. Use HTTPS outside loopback.

## Routing boundary and explicit limitations

- `schedulerMode`: `off`, `shadow` (default), or `active`. In v0.1.1, **off does not register the scheduler capability**: collection and usage work without occupying the host scheduler slot. Use off for observer-only rollout. Shadow and active register a scheduler, so check existing plugin schedulers before opting in. Provider strategy defaults to inherit. Restart CPA when changing modes; active routing remains an explicit operator decision.
- `tea-combos` v0.3.0 adds **member/model** `weighted-round-robin` and `quota-aware-round-robin`, positive `weight`, explicit `role: fallback`, and `quotaPool` (an observation-pool reference).
- **`quotaPool` does not restrict credential eligibility.** It is not a paid/subscription security boundary. Only the configured core account policies restrict actual accounts.
- `tea-combos` v0.4.0 supports combo/member `accountStrategy` with explicit `accountPool`, separately from member weighting. Native same-model concurrent RR/quota policies, pool isolation and caller denial are exercised in `scripts/quota-routing-e2e.py`. Numeric priority is intentionally superseded for explicit account policies.
- Claude/Codex quota routes stay on positively classified OAuth subscriptions when available or present for that model; if core filters those subscriptions out, the plugin rejects rather than silently spending on paid keys. A known explicit API pin or positively API-only model still inherits core. `scripts/quota-subscription-e2e.py` exercises exhaustion and core-filtered subscription cases.
- Production observer collection was verified separately. Native tests are synthetic and do not replace post-deploy live routing verification or a multi-day soak. No concurrency-pressure scoring or account-credit polling is included.
- Unknown API billing type stays unknown. Some API keys are subscriptions; authentication mechanism alone is not evidence of billing type.
- Some host IDs contain filenames/labels and are NOT guaranteed opaque or PII-free. They are exposed only to management-authorized viewers, never public routes. Tokens/keys and raw upstream bodies are not persisted.
- Secure snapshot reads fail closed on non-Linux platforms in this release.

## Verify

```sh
GOTOOLCHAIN=auto GOMAXPROCS=2 go test -race ./...
GOTOOLCHAIN=auto go vet ./...
# repository root:
GOTOOLCHAIN=auto scripts/check.sh
python3 scripts/quota-native-e2e.py --cpa-binary /path/to/CPA-v8.0.69/cli-proxy-api
```

The native E2E harness runs the released CPA v8.0.69 binary with real c-shared plugins and a synthetic upstream. It checks inventory, auth boundaries, pooling, member ratio, streaming, fallback, usage, scheduler invocation and persistence. `--serve` retains the synthetic loopback preview until stopped. Do not expose the synthetic proxy script publicly.

## Deployment/rollback

No production deployment is included in this branch's test run. Back up service/config/state before an approved deployment. For store-managed plugins, update the version pin before placing the new `.so`; checksum-verify artifacts. Keep prior binaries outside CPA's plugin discovery directory. Roll back config and binary together; preserve the new data directory rather than opening it with an incompatible older schema. Observer-only (`schedulerMode: off`) is the initial deployment mode. Enable shadow only after checking scheduler coexistence.
