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

## Build

```bash
scripts/new-plugin.sh  # not needed; this plugin already exists
scripts/build.sh tea-combos
scripts/check.sh
```
