# Quota routing activation contract

User authorized build, test and deploy. Numeric account priority is intentionally irrelevant inside quota-aware strategies. No core fork, no upstream write. Parent integrates, reviews, versions and deploys.

## Provider scheduler
- quotaAcrossPriorities opt-in exposes all core-eligible tiers and bypasses plugin max-priority filter; default false preserves old behavior.
- Claude/Codex subscription quota selection excludes API/unidentified candidates whenever positive OAuth candidates exist. No paying silently to escape an exhausted subscription pool. API-only explicit routes inherit core. Known empty windows reject; missing capacity remains neutral, not invented.
- Existing inherited routes and non-selected providers unchanged. Disabled/model/caller eligibility and cooldowns remain CPA core's authority. Core pins continue restricting candidate sets.

## Combo account control (tea-combos)
- Combo accountStrategy: empty/inherit (core behavior), round-robin, quota-aware-round-robin.
- Member accountStrategy: empty means combo policy; explicit inherit means unpinned core, other choices override combo. This enables meta/local/paid explicit fallback members to use normal core behavior.
- Member accountPool is a NEW explicit eligibility boundary; quotaPool remains observation-only. Never silently repurpose quotaPool. Explicit strategy requires accountPool for every applicable member. Pool must resolve provider and membership from validated snapshot and match member.Provider if given. Pin native AuthID on every explicit attempt. NEVER unpin to recover from missing/stale/empty/exhausted pool.
- Read shared snapshot via existing weightedSelector mutex/cache or a safe independent reader. Snapshot must be fresh <=quotaMaxAgeSeconds; credentials must be matching provider, route model, not disabled/unavailable/nextRetry. Core remains final gate. Unknown quotas are neutral within explicit membership, never add credentials.
- Ignore numeric priority. Deduplicate quota accounts for weighting so two credentials sharing a quota do not receive twice allocation; enumerate credentials within chosen account for fallback. RR equally per quota-account; quota RR uses AccountScore.
- Attempts bounded (max eight account attempts per member, one per listed credential) and context cancellation honored. Retry only member-side errors. Caller/model denial and malformed requests stop; no blanket 401/403 retries. An attempt may include core's bounded retries; no infinite loops. After account attempts exhausted, allow configured member fallback, without expanding that member's account pool.
- Per-combo account state key includes combo, member model/provider/pool/strategy. Shared synchronized counters/bindings only; request lists, callback IDs, caller info are invocation-local. Original CallerScope, headers, query preserved. Never synthesize caller scope to pass policy context. Nonempty native host callback ID is required for explicit account strategies.
- Account affinity when combo.SessionAffinity on: hash original trusted caller scope + session + combo/member/pool, bounded TTL/cache. Only bind after a successful completion or successful stream first payload. Never a global/missing caller binding. Remove failed/unavailable binding.
- Streaming account retries only before first emitted payload. Close failed host streams, then try next permitted pin. After first payload, use existing handle/Pump; no replay. Member cooldown only after all its permitted accounts fail.
- Self/known combo references rejected at execution for explicit strategies; no arbitrary nested expansion. Home mode not supported. Do not skip tea-keys. Native deny test must verify actual managed key denies disallowed member.
- Deployment default: preserve fast member failover order (paid priority explicitly role fallback). Enable quota-aware model/member strategy for general/advisor, account quota for Claude/Codex pool members, explicit inherit for other providers/fallbacks. No model roster substitutions. Direct Claude/Codex provider scheduling may activate with across priorities after native checks.

## Acceptance
- Unit RED/GREEN and race/vet/check; native tests with CPA8.0.69 + synthetic credentials.
- Native cross-priority low account chosen because quota; normal inherit remains high priority.
- Same-model concurrent combos with independent account policies; pool isolation; restricted caller denial; disabled/cooldown/stale/exhausted; bounded account failover; stream pre-first fallback and no post-first replay. Do not count unit mocks as native coverage.
- amd64 CI artifact checksums verified, main merged signed/verified commit.
- Production backups config and both plugin binaries outside discovery, backup runtime combo state after stopped service, auto rollback on startup/inference checks. Preserve model roster and keys. Read-back API verifies mode, policies, active decision entries; test direct OAuth and real combo completions and auth boundaries. No secrets in output.
