# Brief — `tea-combos`

Hand this to the agent working in `/root/cpa-tea-plugins`. Read `AGENTS.md` first, then
`spikes/tea-combo-spike/FINDINGS.md`, then the spike's `main.go` / `abi.go`. This brief wins where it
disagrees with the spike.

## Mission

Ship `plugins/tea-combos`: named model combos for CLIProxyAPI. A client asks for a virtual model such
as `fast-fleet`; the plugin picks a member, executes it through the host, and fails over when the pick
is unhealthy. CPA stays stock — no fork, no patch.

## Start from this, because it is already true

- **CPA already does plain round-robin plus failover via alias pools.** Repeating the same `alias` in
  an upstream group's `models` builds a pool: the client sees one alias, requests round-robin across
  the listed upstream names, and a failing upstream falls through to the next one in the pool
  (`config.example.yaml`, v8.0.51, "internal model pool"). So do not rebuild single-provider RR and
  call it a feature.
- **What is missing, and what we are building:** combos that span providers and groups, are named as
  first-class objects rather than buried in one provider's model list, are editable at runtime, and
  carry **session affinity** so a conversation sticks to one member instead of round-robining every
  turn.
- **A router plus executor plugin can drive real models.** Proven in the spike: the router sends the
  combo name to the plugin's own executor, and each member is executed by CPA through
  `host.model.execute`, so providers, credentials, cooldowns and credential choice stay core-owned.

## v1 scope

**Strategies**

1. `round-robin` — default. Rotates members per request.
2. `failover` — ordered, first healthy member wins.
3. **Session affinity, on by default for both.** One session keeps one member for the affinity TTL
   (default `1h`, per-combo override). Affinity exists to hold the prompt cache warm; a per-turn
   rotation defeats the point of the combo.
   - Rebind only when the bound member is unavailable, and only on retryable failures
     (408/429/5xx/`retryable`), never on a client error the member already rejected.
   - Persist bindings in memory with an LRU cap, and mirror to the state file so a restart does not
     cold-start every conversation.
4. Quota-aware RR is **v2**. Leave a `selector` seam (an interface with `round-robin` and `failover`
   implementations); do not build quota logic, do not fetch quotas, do not guess.

**Session identity.** Reuse CPA's own derivation rather than inventing a second one: check whether
`ExecutorRequest.Metadata` carries the canonical session id (`cliproxyexecutor.CanonicalSessionIDMetadataKey`
in `sdk/cliproxy/executor`, produced by `cliproxysession.BoundSessionIdentity`). If the host does not
populate it for plugin executors, derive a key in this order — explicit Claude Code / Codex / OpenCode
session headers, `prompt_cache_key`, conversation IDs, then a hash of the first user message — and
write down in the README exactly how it differs from core's. A divergent key silently halves cache
hits, so treat this as a correctness requirement, not a detail.

**Management UI — required in v1.** The plugin must produce its own page inside CPA and appear in
**CPA-Manager-Plus** without anyone editing CPAMP. See the contract below; it is already supported.

**Not in v1:** quota awareness, weighted members, per-member timeouts beyond CPA's own, cost/routing
dashboards, usage-attribution changes.

## Management UI contract (verified against CPAMP `a67ed88`)

**Do not copy Keeper.** `cpa-usage-keeper` is a *standalone* service with its own port and dashboard;
the old panel's "Keeper" button existed because it had to link out to a separate process. We do better:
the page ships inside the plugin and is served by CPA itself, so there is no extra port, no second
deployment, and nothing for a panel to hard-code. Use `ResourceRoute` below. If you find yourself
opening a listener or adding a compose service, you have taken the wrong path.

CPAMP discovers plugin pages automatically. `apps/web/src/features/plugins/pluginResources.ts` calls
`pluginsApi.list()`, then for every plugin where `effectiveEnabled` is true it turns each entry of
`menus[]` into a sidebar item — label from `menu.menu`, description from `menu.description`, icon from
`plugin.logo || metadata.logo`, iframe `src` from `menu.path` resolved against the API base. Nothing
to contribute upstream. So:

1. Declare the `ManagementAPI` capability and return `Resources: []ResourceRoute{...}`.
2. `ResourceRoute{Path: "/", Menu: "Combos", Description: "Named model combos, strategies and session
   affinity", Handler: ...}` — a GET resource with a non-empty `Menu` is what becomes the sidebar entry
   and the iframe target under `/v0/resource/plugins/tea-combos/`.
3. Set `Metadata.Logo` to something browser-loadable (an absolute path on the CPA origin, or a data
   URI). Without it the sidebar item has no icon.
4. Serve **one self-contained HTML document**, no build step, no CDN, no external fonts. It runs inside
   an iframe that CPAMP injects its own stylesheet bridge into; use neutral markup and CSS variables so
   it does not fight the host theme.
5. Data operations go on authenticated `ManagementRoute`s (GET/POST/PUT/DELETE) under
   `/v0/management/`, not on the resource route.

**Security — do not get this wrong.** Resource routes are *not* management-authenticated:
`pluginResourceNoRoute` skips the management middleware entirely. Therefore the resource route serves
the static shell only. It must never return combo config, credentials, keys or usage. The page gets the
management key from the operator (paste once, `localStorage`, `Authorization: Bearer` on same-origin
`fetch`), and every read/write goes to the authenticated routes. Never log the key, never put it in
HTML, never ship a default in JS.

**Known constraint:** `pluginResourceNoRoute` returns 404 when `home.enabled: true`. Confirm this on the
real binary, document it in the README, and do not try to work around it in the plugin.

## Design

- **Capabilities:** `ModelRouter` + `Executor` + `ManagementAPI`.
- **Router:** if the requested model is a configured combo, return
  `TargetKind: ModelRouteTargetExecutor, Target: "tea-combos"`. Otherwise `Handled: false`.
- **Executor:** recover the combo name from `ExecutorRequest.Model` (route metadata does not survive),
  ask the selector for a member, execute it, apply affinity rules, return the response.
- **Config** (`plugins.configs.tea-combos`) seeds defaults only. Runtime edits from the UI must apply
  without restarting CPA, so combos live in a plugin-owned state file written atomically (temp file +
  rename, mode 0600), not in CPA's config. Keep `Reconfigure` able to merge seed config into state.
- **Combo shape:** `{ name, strategy, members: [{ model, provider? }], sessionAffinity? , ttl?, enabled }`.
  A member pins a provider only when `provider` is set; otherwise CPA resolves it, including its own
  credential choice and cooldowns.
- **AST/format:** pass `EntryProtocol` from `req.SourceFormat` and `ExitProtocol` from `req.Format`, and
  rewrite the body's `model` field per member. Do not rewrite anything else in the body.

## Streaming

Implement `ExecuteStream`. Fall back across members **only before the first byte reaches the client**:
open the member's stream, and if it fails with a retryable error before any payload, move to the next
member. Once bytes are sent, a failure is final — close the stream and surface the error. Put this
decision and its limit in the README, and test both branches. Do not fake it by buffering the whole
stream; that breaks TTFT.

## Traps already paid for (from the spike)

1. An executor that does not declare `executor_input_formats` and `executor_output_formats` (and
   `executor_model_scope`) is skipped **silently**; the client sees `unknown provider for model`. This
   cost six debugging rounds. Declare them.
2. Plugin config arrives as raw YAML bytes. Use `gopkg.in/yaml.v3`.
3. `host.model.execute` needs `host_callback_id` in a wrapper *beside* the request — take it from the
   executor request. That id is what sets `SkipRouterPluginID` and stops a combo re-entering itself.
4. Upstream failures arrive as error envelopes carrying `retryable` and `http_status`, not as non-2xx
   responses.
5. `ExecutorResponse` has no status field. To fail with a real code, return an error envelope with
   `http_status`; otherwise the client sees HTTP 200 with an error body.
6. `ModelRouteResponse` has no metadata — target only.
7. Loader discovery is flat: `<plugins.dir>/<name>.so`. Subdirectories are ignored.

## Open questions — verify, then document

- **Does a combo appear in `/v1/models`?** Routing a virtual model is proven; catalog listing is not.
  Check what the host does for plugin-routed models. If combos cannot be listed, say so plainly in the
  README and tell Kareem before considering any core-side change.
- **Usage attribution:** is a combo request billed against the member model or the combo name? Answer
  from a real run and write it down.
- **State file writability** next to `plugins.dir` on the deployed host; if it is not writable, use a
  configured path and fail loudly rather than silently dropping edits.

## Tests and definition of done

Unit tests: empty member list, all members failing, non-retryable stop, RR rotation order, affinity
stickiness across requests, affinity TTL expiry, affinity rebind on retryable failure, strategy parsing,
state file round-trip, and stream fallback before/after first byte.

End-to-end against a real CPA binary plus the spike's mock upstream (extend it to count round-robin
distribution and to prove stickiness). Evidence must be the mock's own request log, not plugin logs.

Also required:

- `scripts/check.sh` passes: gofmt, vet, tests, `c-shared` build, `registry.json` regenerated.
- `plugin.json` version matches `pluginVersion`.
- The README covers capabilities, config keys, strategy semantics, the UI, the security model and the
  streaming limit.
- **UI proof:** with a CPA instance and CPAMP pointed at it, the sidebar shows the Combos entry and the
  page loads in the iframe. Include the `curl` output of
  `/v0/management/plugins` filtered to `tea-combos` showing `effectiveEnabled`, `menus` and `logo`, plus
  an unauthenticated `GET /v0/resource/plugins/tea-combos/` returning the HTML shell. A screenshot of
  the sidebar is nice; the curl output is the acceptance criterion.

## Rules

- Never patch or fork CPA. If something genuinely needs core, stop and ask Kareem.
- Conventional Commits, human voice, signed, no AI or process narration.
- `gh` always with `-R the-tea-team/cpa-tea-plugins`. Never touch `router-for-me/*`.
- No generated `.so` / `.h` / `.dev/`, no real credentials, no management keys in the repo.
- Stop a test CPA with `kill $(pgrep -x cli-proxy-api)` — never `pkill -f` over ssh, it kills your own
  shell. Test instances use ports in the 183xx range.
- Go 1.27.3 at `/usr/local/go`; reference clone and binary at `/root/reference/CLIProxyAPI`.
