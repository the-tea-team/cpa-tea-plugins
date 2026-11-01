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

## Build

```bash
scripts/new-plugin.sh  # not needed; this plugin already exists
scripts/build.sh tea-combos
scripts/check.sh
```
