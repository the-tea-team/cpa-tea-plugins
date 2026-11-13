# Tea Combos (`tea-combos`)

Named model combos for CLIProxyAPI. A client asks for a virtual model such as `fast-fleet`; the
plugin picks a member, executes it through CPA's own host callback, and falls over to the next
member when the pick fails retryably. CPA stays stock: providers, credentials, schedulers and
cooldowns remain core-owned.

## Capabilities

- **ModelRouter** — routes a requested model to this plugin's executor when it names a configured,
  enabled combo with at least one member.
- **Executor** — walks the combo's members with `host.model.execute` (and `execute_stream` for
  streaming), rewriting only the request body's `model` field and passing `EntryProtocol` /
  `ExitProtocol` through unchanged. Token counting and HTTP bridging are not supported.
- **ManagementAPI** — authenticated combo CRUD under `/v0/management/` and a static page that
  CPA-Manager-Plus lists in its sidebar. See "Management UI and API".
- **ModelRegistrar** — publishes enabled combos in `/v1/models`. See "Model listing".

Session affinity keeps a conversation on one member; see "Session identity" and "Session affinity".

## Model listing

`GET /v1/models` lists every enabled combo with at least one member as a normal OpenAI model
object: `id` is the combo name, `object` is `model`, and `owned_by` is `tea-combos`. Disabled
combos and combos with no members are not listed, and upstream models are unaffected.

The catalog is rebuilt whenever CPA re-runs plugin model registration: at startup, on plugin
reconfigure, and on a CPA configuration reload (the config file is written or sent through the
management config API). A combo created, deleted, enabled or disabled through this plugin's
management API is therefore **not** reflected in `/v1/models` until one of those triggers; on
v8.0.51 a config file write reflects it in about 0.17s. Routing reads the store directly, so the
edit still takes effect on the next request immediately. CPA exposes no host callback that lets a
plugin re-run registration, so the plugin cannot refresh the catalog on its own.

**Name collision.** If a combo name equals an upstream model id, the upstream entry keeps the
`/v1/models` row: CPA does not republish a model already served by a native executor, so no
duplicate is shown. Routing is unaffected because `ModelRouter` runs before provider resolution, so
a request for that name still reaches the combo executor.

## Configuration

Lives under `plugins.configs.tea-combos` in CPA's `config.yaml`.

```yaml
plugins:
  enabled: true
  configs:
    tea-combos:
      enabled: true
      statePath: /var/lib/cli-proxy-api/tea-combos.state.json   # optional
      combos:
        fast-fleet:
          strategy: round-robin        # round-robin (default) or failover
          members:
            - model: gpt-4o
            - model: claude-sonnet
              provider: anthropic      # optional: pin this member's provider
          sessionAffinity: true        # default on; false disables affinity for this combo
          ttl: 1h                      # Go duration; affinity binding lifetime (default 1h)
          enabled: true
```

`combos` may also be a list of objects, each carrying its own `name`.

**State file.** Combos live in a plugin-owned JSON file, not in CPA's config, so edits apply
without a restart. The path is `statePath` when set, otherwise `tea-combos.state.json` beside the
loaded plugin library. CPA v8.0.51 loads plugins from a per-process shadow copy under
`$TMPDIR/cliproxy-pluginhost/pid-<pid>/`, so in a real deployment set `statePath`; by default
registration fails with an error naming `statePath` rather than writing state that a restart would
discard. Writes are atomic (temp file plus rename, mode `0600`, fsync) and a failed write is
returned, never swallowed.

**Seed merge rule.** Combos from `plugins.configs.tea-combos` are seeds only: on registration and
reconfiguration a seed is added only when no combo of that name already exists in the state file.
Runtime edits always win and are never overwritten by config.

## Strategies

- `round-robin` — default. A per-combo counter rotates the starting member on every request. On a
  retryable failure the executor continues around the ring, so every member is tried at most once.
- `failover` — ordered from the first member on every request.

An unknown strategy is a configuration error. Quota-aware routing is out of scope; the `Selector`
interface is the seam for it.

## Failure behaviour

Members are tried in selector order. The chain falls through on retryable failures — `408`,
`429`, any `5xx`, or an upstream error envelope carrying `retryable` — and also on **member-side
account failures**: a `401`/`403` whose message carries quota, billing, credit or credential
language (`insufficient_quota`, `RESOURCE_EXHAUSTED`, `credit balance`, `invalid api key`, …).
Those are the member's problem, not the client's, so the next member gets the request instead of
the whole combo erroring. A genuine client error (a `400` the payload deserves, an unrelated
`403`) stops the chain and is returned with its real HTTP status. When every member fails the
executor returns the last status, `retryable: true`, an attempt trace naming each member and
status, and a `cooling=[…]` snapshot. Request bodies and credentials are never logged.

## Cooldown

After a member-side failure that member is **cooled**: selection skips it for a while so a dead
lane stops eating requests and latency.

- Window: `cooldownSeconds` in the plugin config (default 30, `0` disables), overridable per combo
  with `cooldownSeconds` on the combo itself.
- A member that serves successfully clears its cooling immediately.
- A session-affinity binding to a cooled member is skipped; the binding rebinds to whoever serves.
- If EVERY member is cooling the filter fails open and the full ring is attempted anyway — a
  fleet-wide blip must not become a guaranteed 502.
- Cooling is process-local; a restart forgets it (the upstream may already have recovered).

## Streaming

`ExecuteStream` applies the same selector order and retryable/non-retryable rule, but its fallback
window is narrower: a combo falls back to the next member **only before the first payload byte
reaches the client**. A member counts as opened only once its stream produces a non-empty chunk (or
ends cleanly with no chunks, which is a successful empty response). If opening the stream or the
first read fails retryably before any payload, the member stream is closed and the next member is
tried. Once the first chunk has been forwarded, a later failure is final: the member stream is
closed, the error is reported as the stream's terminal error, and no other member is tried. This is
the same limit CPA's own alias-pool retry has, and it cannot be lifted without buffering.

Nothing is buffered beyond the single first chunk needed to make that decision. Every subsequent
chunk is forwarded as soon as it is read, so time-to-first-token is preserved; a test proves the
next read cannot start until the previous chunk has been emitted.

## Management UI and API

The plugin declares CPA's `ManagementAPI` capability, so it ships its own management page and
authenticated JSON API. There is no extra process, port or compose service.

**Page.** One embedded, self-contained HTML document (`ui/index.html`) is served as a CPA resource
route: `GET /v0/resource/plugins/tea-combos/combos`. It appears as the **Combos** sidebar entry in
CPAMP automatically, using the plugin's inline SVG logo. The document has no build step, no CDN, no
external font and no data: the resource route is the shell only.

**API.** Every data operation is a Management API route under the host-supplied `BasePath`
(`/v0/management`):

| Method | Path | Behaviour |
|---|---|---|
| GET | `/v0/management/plugins/tea-combos/combos` | list combos |
| POST | `/v0/management/plugins/tea-combos/combos` | create (409 on duplicate, 400 on validation) |
| GET | `/v0/management/plugins/tea-combos/combos?name=<name>` | one combo, 404 if absent |
| PUT | `/v0/management/plugins/tea-combos/combos?name=<name>` | replace, 404 if absent |
| DELETE | `/v0/management/plugins/tea-combos/combos?name=<name>` | delete, 404 if absent |

Bodies are JSON, limited to 1 MiB, and decoded with unknown fields rejected. Writes go through the
store the router reads, so an edit applies on the next request with no restart.

**Why `?name=` rather than `/combos/<name>`.** CPA v8.0.51 dispatches plugin management routes by an
exact method and path (`internal/pluginhost/management.go`), and rejects `:` and `*` in a declared
path. Per-name path patterns cannot be registered. The handler also understands
`/combos/<name>` when the host dispatches it, but on v8.0.51 it is not reachable, and a method with no
declared route (for example PATCH) reaches the host's 404 rather than the handler's 405.

**Resource path.** CPA v8.0.51 rejects `Path: "/"` for a `ResourceRoute` because it trims the path to
empty, so the page is registered at `/combos` under the resource base. CPAMP follows the menu path, so
the sidebar entry is unaffected.

**Known constraint.** CPA's `pluginResourceNoRoute` returns 404 for resource pages when
`home.enabled: true`. The plugin does not work around this; set home mode off to use the page.

**Security.** Resource routes are *not* management-authenticated: CPA's `pluginResourceNoRoute` skips
the management middleware. The page therefore never receives combo config, credentials or usage. The
operator pastes the management secret key once; it is kept in this origin's `localStorage` under
`tea-combos.management-key` and sent as `Authorization: Bearer <key>` on same-origin requests to the
management routes. The key is never embedded in the HTML, never logged and never echoed by the plugin;
the page computes the API base from its own URL rather than assuming a prefix. All management routes
are authenticated by CPA's own middleware. Use **Forget key** to remove the stored key.

The page also sets `Cache-Control: no-store`, `X-Content-Type-Options: nosniff` and a restrictive
`Content-Security-Policy` (no remote loads; `connect-src 'self'`; no `frame-ancestors` so CPAMP can
embed it).

## Session identity

Session affinity keys are derived from one conversation identity per request. The key is
`SHA-256("tea-combos-session-v1" + NUL + combo name + NUL + identity)`, so two combos never share a
binding and the state file holds only hex digests, never a header value, prompt or other raw text.

**Does the host give a plugin the canonical id?** No, not in CPA v8.0.51. The plugin-executor route
is `sdk/api/handlers/handlers_execution.go:198` `executeWithPluginExecutor`, which builds its request
with `handlers_execution.go:284` `pluginExecutorRequest`. That builder fills `Metadata` at
`handlers_execution.go:314` from `requestExecutionMetadata` (idempotency key, request path, pinned
auth) and never calls `cliproxysession.Enrich`. `Enrich` only runs on the core manager paths
(`sdk/cliproxy/auth/conductor_execution.go:125`, `187`, `242`), which a plugin executor bypasses:
`handlers_execution.go:226` passes the request straight to `host.ExecutePluginExecutor`. The
session helper used on the plugin path, `sdk/api/handlers/handlers.go:325`
`EnrichContextWithSessionHierarchy`, writes the id into the logging context only. Finally
`internal/pluginhost/adapters_executors.go:1329` copies request and option metadata verbatim in
`buildExecutorRequest`, so no canonical id appears from anywhere else.

The plugin still reads `Metadata["canonical_session_id"]` first, using the constant's value from
`sdk/cliproxy/executor/types.go:61`. The constant is mirrored as a string rather than imported
because that package imports `sdk/translator`, which drags the host's internal packages and a large
dependency tree into the plugin. If a future host version starts populating the key for plugin
executors, this path picks it up with no change.

When the key is absent, the plugin derives an identity in this order, matching
`cliproxysession.ExtractSessionInfo` for the sources covered:

1. `X-Claude-Code-Session-Id` (with `X-Claude-Code-Agent-Id` when it names a subagent).
2. Claude Code `metadata.user_id` in the payload: the `session_id` of the JSON object, or the legacy
   `_session_<id>` suffix.
3. Codex `Session-Id` / `Session_id`, then `Thread-Id` / `Thread_id`, then `X-Codex-Turn-Metadata`.
4. OpenCode and generic headers, in core's order: `X-Session-ID`, `X-Session-Affinity`,
   `X-Slot-Session-Id`, `X-Conversation-Id`, `X-Thread-Id`.
5. `prompt_cache_key` / `promptCacheKey` in the body (top level, then a nested `request` object).
6. The `conversation` object's `id` (or a string `conversation`), then `conversation_id`,
   `conversationId`, `chat_id`, `chatId`, `metadata.conversation_id`, `extra_body.conversation_id`.
7. A hash of the first user message (`messages`, Gemini `contents`, or Responses `input`).

If none of these yields an identity, the request has no affinity and behaves exactly as the plain
strategy; the plugin never invents a shared global key.

**Differences from core's derivation.** These are deliberate and documented because a divergent key
reduces cache hits:

- Core prefers `prompt_cache_key` over body `session_id`/`task_id`/`thread_id`/Gemini
  `cachedContent` only within its payload tier; this plugin skips those payload fields entirely and
  goes from headers to `prompt_cache_key`. A request that carries a body `session_id` but no header
  gets a `pck:`/`conv:`/message key here where core would use `session:<id>`. Add the body
  `session_id` tier if a real client is seen relying on it.
- Core treats Gemini `cachedContent` and Agy (`X-Http-Session-Id`) as identities; this plugin does
  not, because neither is a conversation session for affinity purposes here. Those requests fall
  through to `prompt_cache_key`, a conversation id, or the first-message hash.
- Core also recognises `X-Task-ID` and `X-Client-Request-Id` as identities in the same header tier.
  This plugin omits both so an internal task or per-request id cannot fragment a conversation's
  binding; such requests fall through to a body identity or the first-message hash.
- Core splits Claude and Codex subagents into distinct identities using parent/agent fields. The
  plugin keeps the Claude agent suffix when `X-Claude-Code-Agent-Id` supplies it, but does not fall
  back to `metadata.agent_id`/`metadata.subagent_id` or implement Codex fork/subagent decomposition,
  so those requests share the plain session or thread binding.
- Core's last-resort identity is `DeriveID`, a format-specific canonical root over instructions and
  the first user turn (`sdk/cliproxy/session/identity.go:492`). This plugin uses a simpler first
  user message hash, as the brief requires. A conversation whose only identity comes from this
  fallback therefore gets a different key from core's; it is stable per conversation but not
  byte-identical to core's.
- Core also consults `execution_session_id` and `lcp_affinity_session_id` metadata. The plugin uses
  only `canonical_session_id`, because the plugin route does not carry the former and the latter is
  explicitly LCP-routing-only.

## Session affinity

Affinity is on for both strategies by default; `sessionAffinity: false` disables it for one combo.
A session keeps one member for the affinity TTL (`ttl`, a Go duration, default `1h`). The TTL is
sliding: every successful use of the bound member refreshes the expiry, so an active conversation
stays warm for as long as it keeps succeeding.

The executor resolves the binding before it asks the selector, and the bound member is attempted
first while the rest of the selector's order is preserved. The binding is rewritten only when the
bound member is unavailable, which means it returned a retryable failure on this request (`408`,
`429`, any `5xx`, or an error envelope carrying `retryable`); the member that finally succeeds is
the one recorded. A non-retryable client error from the bound member stops the chain (existing
failure behaviour) and leaves the binding unchanged, as does a request in which every member fails
retryably. A request with no binding is bound to the first member that succeeds, and a binding is
recorded only on success.

Bindings live in memory in a fixed-size LRU (`10000` entries, least recently used evicted first) and
are mirrored to the reserved `bindings` section of the state file. Writes are write-behind: they are
debounced to at most one write every couple of seconds, and are flushed on shutdown and on
register/reconfigure, so request latency is not tied to disk. Bindings expired at load are dropped.
A failed bindings write never fails a request; the cache keeps the data dirty and retries on the next
flush. The state file stays mode `0600` and atomic, and writing bindings does not modify the combos
section. A combo edited at runtime invalidates a binding whose member at that index no longer has the
same model and provider.

## Build

```bash
scripts/build.sh tea-combos
scripts/check.sh
```

## Findings on CPA v8.0.51

Everything below was measured against the real `cli-proxy-api` binary at `/root/reference/CLIProxyAPI`
(v8.0.51), the plugin built as `c-shared`, the mock upstream in `testdata/mock-upstream.py`, and
CPAMP at commit `a67ed88`. The raw transcript, screenshots and JSON records are under
`/root/jethur/_work/tea-combos/slice-e/`.

### End-to-end proof

`testdata/run-e2e.sh [workdir] [cpa-binary] [plugin.so]` starts the mock, CPA with a scratch config
and a flat plugin directory, then prints the mock's own request log for every case. It generates a
throwaway client key and management key at run time and stops only the PIDs it started. Ports are
`183xx`. `testdata/run-stream-proof.sh` remains the streaming proof.

Against the 3-member `rr3` combo:

- **Round-robin.** 30 requests with no session headers and distinct prompts produced exactly
  `rr-a 10`, `rr-b 10`, `rr-c 10` in the mock log.
- **Stickiness.** Ten requests with `X-Session-Id: s-one` all hit `rr-a`; ten with
  `X-Session-Id: s-two` all hit `rr-b` (20 log lines, 10/10). `X-Claude-Code-Session-Id` binds the
  same way. Both headers demonstrably reach the plugin executor as `ExecutorRequest.Headers` on the
  real binary.
- **Affinity off.** Nine requests with a session header to a `sessionAffinity: false` combo produced
  `rr-a 3`, `rr-b 3`, `rr-c 3`.
- **Rebind on a retryable failure.** A session bound to `rr-a`; `rr-a` was then made to return 503.
  The next request logged `rr-a` followed by `rr-b` and returned `rr-b`'s body, so the binding moved
  to the member that succeeded.
- **Client error stops the chain.** A session bound to `rr-c`; `rr-c` then returned 400. The next
  request returned HTTP 400 and the mock log contains only `rr-c` — `bad400`, the next member, was
  never called. After restoring `rr-c`, the same session still hit `rr-c`, so a non-retryable error
  leaves the binding intact.
- **Restart persistence.** A bound session was served by `rr-c`; CPA was stopped with SIGTERM and
  restarted with the same state file. The state file held 39 bindings after shutdown and the same
  session was served by `rr-c` after restart. CPA's SIGTERM path does reach the plugin's shutdown
  hook and flushes the write-behind buffer. The loss window is therefore the write-behind interval
  (about two seconds) only for SIGKILL or a process crash; a graceful stop loses nothing.
- **Identity rungs.** With no headers, two requests carrying the same first user message stuck to
  one member (the first-user-message hash fired); different prompts rotated. The canonical-session
  metadata key remains absent on the plugin route, so derivation starts at the headers.

### Management page and CPAMP (`D1`: `/combos` plus `?name=`)

`GET /v0/management/plugins` on the real binary reports `effective_enabled: true` for `tea-combos`,
one menu `{"path":"/v0/resource/plugins/tea-combos/combos","menu":"Combos",...}` and an inline SVG
`logo` of 494 bytes (data URI). CPAMP at `a67ed88` (`collectPluginResourceEntries` in
`apps/web/src/features/plugins/pluginResources.ts`) consumes exactly `effectiveEnabled`,
`menus[].path/menu/description` and `logo`; the payload satisfies it.

Resource URLs on the real binary, unauthenticated:

| Request | Status | Body |
| --- | --- | --- |
| `GET /v0/resource/plugins/tea-combos/` | 404 | empty (the brief's URL; the host registers `/combos`, not `/`) |
| `GET /v0/resource/plugins/tea-combos` | 404 | empty |
| `GET /v0/resource/plugins/tea-combos/combos` | 200 | HTML shell, 21437 bytes, `text/html; charset=utf-8`, CSP `default-src 'none'; ...`, no `frame-ancestors` |
| `GET /v0/resource/plugins/tea-combos/combos/` | 404 | empty (the host dispatches exact paths) |

CPAMP ran locally: the web panel was built from source (`npm install && npm run build`), the Go
manager server was run pointed at the test CPA (`CPA_UPSTREAM_URL`, `CPA_MANAGEMENT_KEY`,
`PANEL_PATH`), and a headless Chromium logged in with the CPAMP admin key. The sidebar lists
**Combos** with the plugin logo and the page loads in the iframe at
`/v0/resource/plugins/tea-combos/combos`. A cross-origin parent page also framed the same resource
URL without being blocked.

**D1 verdict: works.** Through CPAMP's iframe the UI created a combo (`POST` collection -> 201),
edited it (`PUT .../combos?name=cpamp-combo` -> 200, affinity on and TTL applied) and deleted it
(`DELETE .../combos?name=cpamp-combo` -> 204). The same CRUD was repeated directly against CPA's
resource page. One caveat for CPAMP operators: the page asks for "the management secret key of this
CPA instance", but behind CPAMP's manager-server proxy that same-origin route is authenticated with
the **CPAMP Admin Key**; the CPA Management Key is rejected with 401 there. Served directly by CPA,
the CPA Management Key is the correct credential.

### home.enabled confirmation

`home.enabled` cannot be set from the v8.0.51 YAML config: `Home` is runtime-only and is turned on
by `-home-jwt`, which bootstraps mTLS certificates and a config from a reachable Home control plane.
With a syntactically valid JWT pointing at an unreachable control plane the binary exits with
`invalid -home-jwt: dial tcp ... connection refused`.

The 404 was then confirmed on the real binary by running a stand-in Home control plane (scratch-only,
not shipped with the plugin). It answered the JWT's mTLS certificate bootstrap (plaintext RESP
`CERTIFICATE REQUEST`, signing the CSR against the `ca_fingerprint` in the JWT) and the control-plane
RESP commands `GET config`, `GET plugin-sync` and `RPUSH plugin-status`, serving a config with
`plugins.enabled: true`, `plugins.dir` and the same `tea-combos.so`. The binary was started with
`-home-jwt <jwt> -home-disable-cluster-discovery`; its log shows `pluginhost: plugin loaded
plugin_id=tea-combos`, `plugin registered` and `Starting API server on 127.0.0.1:18391`.

- Home mode, unauthenticated `GET /v0/resource/plugins/tea-combos/combos` -> **HTTP 404**, empty body
  (`Content-Length: 0`).
- Control run, same binary and same `.so`, normal YAML config (Home off) -> **HTTP 200**, the 21,516-byte
  resource shell.
- Home mode also returns **HTTP 404** for `GET /v0/management/plugins` even with the correct management
  key; `managementAvailable` (`internal/api/server_management.go:219`) gates that route on Home too.
  The same request is 200 in the control run.

The code path matches the observation: `pluginResourceNoRoute`
(`internal/api/server_management.go:293`) aborts with 404 when `cfg.Home.Enabled` is true, before
dispatching to the plugin. The plugin does not work around this; set home mode off to use the page.
Logs, the stand-in source and the request/response transcripts are under
`/root/jethur/_work/tea-combos/slice-h/`.

### Open questions

**1. Does a combo appear in `/v1/models`?** Before the model-listing change on this branch
(`fb475d9`) it did not: with `rr3`, `sticky3`, `ui-combo` and the rest configured, `GET /v1/models`
listed only upstream aliases (`rr-a`, `rr-b`, `rr-c`, `bad400`, `bad503`, `stream-*`). The host
builds that list from `registry.GetAvailableModels`
(`sdk/api/handlers/openai/openai_handlers.go` `OpenAIModels`), and a plugin contributes entries only
through the separate `ModelRegistrar` capability (`Capabilities.ModelRegistrar`, wire flag
`model_registrar`, method `model.register`); routing a virtual model does not add it to the catalog.
That is why listing needed a plugin-side `ModelRegistrar`, not a core change.

With the model-listing slice now on this branch it does appear. Against the real binary, `GET
/v1/models` returned `bad400, bad503, noaff, rebind, rr-a, rr-b, rr-c, rr3, sticky3, stopchain,
stream-bad, stream-cut, stream-good`: every enabled combo is listed with `owned_by: tea-combos`
alongside the upstream aliases. Runtime edits are not reflected until CPA re-runs plugin model
registration. Measured on the real binary: creating `live-combo` through the management API
returned 201 but the catalog stayed `rr-a, seed-combo`; after a config-file write CPA reloaded and
it became `live-combo, rr-a, seed-combo`. `Host.RegisterModels` runs at startup, on reconfigure and
on config reload, and CPA exposes no host callback for a plugin to trigger it, which is why the
plugin's own README states the refresh trigger plainly.

**2. Usage attribution: the member model.** After draining the usage queue (CPAMP must not be
collecting), a combo request produced one usage record whose fields were
`"model":"rr-a"`, `"alias":"rr-a"`, `"provider":"openai-compatible-mock"`,
`"executor_type":"OpenAICompatExecutor"`, `"response_model":"rr-a"`. The combo name and the
`tea-combos` plugin id appear nowhere in the record. This matches the expectation that a combo is
billed against the member model. The nested host execution publishes the record; the outer
plugin-executor reporter is suppressed because the plugin made a nested call.

**3. State file writability.** By default the plugin looks for `tea-combos.state.json` beside the
loaded `.so`. That location is unusable on CPA v8.0.51: CPA shadow-copies every plugin library to
`$TMPDIR/cliproxy-pluginhost/pid-<pid>/` and loads it from there, so a restart would silently lose
every runtime edit. With `statePath` unset, registration fails loudly with
`cannot determine a durable combo state file path: set plugins.configs.tea-combos.statePath`, the
plugin is not registered and `effective_enabled` is false.

An operator must set one config line:

```yaml
plugins:
  configs:
    tea-combos:
      enabled: true
      statePath: /var/lib/cli-proxy-api/tea-combos.state.json
      combos: { ... }
```

Pointing `statePath` at an unwritable location fails at registration with the underlying write error
(for example `create combo state temp file in /proc/sys: ... no such file or directory`) and the
plugin stays unregistered. If the directory becomes unwritable after a successful start, a CRUD
write returns HTTP 500 with the same kind of message; it is never swallowed. The state file is mode
`0600`, written atomically, and bindings are written on a debounce plus on shutdown.

### Fix made during the real runs

The embedded editor treated an omitted `sessionAffinity` as off, while the plugin treats it as on:
opening and saving a combo silently disabled affinity, and the table showed `off`. `ui/index.html`
now defaults the checkbox and the table to on unless `sessionAffinity` is explicitly `false`;
`ui_test.go` guards it.
