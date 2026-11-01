# Tea Combos (`tea-combos`)

Named model combos for CLIProxyAPI. A client asks for a virtual model such as `fast-fleet`; the
plugin picks a member, executes it through CPA's own host callback, and falls over to the next
member when the pick fails retryably. CPA stays stock: providers, credentials, schedulers and
cooldowns remain core-owned.

## Capabilities

- **ModelRouter** — routes a requested model to this plugin's executor when it names a configured,
  enabled combo with at least one member.
- **Executor** — walks the combo's members with `host.model.execute`, rewriting only the request
  body's `model` field and passing `EntryProtocol` / `ExitProtocol` through unchanged.

Streaming, session affinity and the management API/UI are later slices. `ExecuteStream` currently
returns a clear `not implemented yet` error; token counting and HTTP bridging are not supported.

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
          sessionAffinity: true        # carried for the affinity slice
          ttl: 1h                      # Go duration; validated here, used by the affinity slice
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

Members are tried in selector order. The chain falls through only on retryable failures: `408`,
`429`, any `5xx`, or an upstream error envelope carrying `retryable`. A non-retryable client error
stops the chain and is returned with its real HTTP status. When every member fails retryably the
executor returns the last status, `retryable: true`, and an attempt trace naming each member and
status. Request bodies and credentials are never logged.

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
scripts/new-plugin.sh  # not needed; this plugin already exists
scripts/build.sh tea-combos
scripts/check.sh
```
