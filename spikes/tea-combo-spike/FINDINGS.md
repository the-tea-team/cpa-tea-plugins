# Spike: cross-model combo fallback as a plugin (no core patch)

Status: **proven**, 2026-11-01. Question closed: a plugin can implement combo fallback across
different models without patching CPA.

## Result

A plugin that registers `ModelRouter` + `Executor` routes a virtual model name to its own executor,
which walks an ordered member list calling `host.model.execute` for each member. Verified against a
real CPA binary (built from `router-for-me/CLIProxyAPI`, plugin ABI v8.0.51) and a deterministic mock
OpenAI-compatible upstream whose own request log is the evidence.

| Scenario | Members | Upstream saw (mock log) | Client got |
|---|---|---|---|
| Fallback | `bad-model` (503), `good-model` | `bad-model`, `good-model` | 200, `served-by:good-model` |
| Exhausted | `bad-model`, `bad-model` | no further call (credential cooling) | 503 with the real upstream cause |
| Non-retryable | `bad400` (400), `good-model` | `bad400` only | 400, chain stops, `good-model` never called |
| Cooling | repeat of Fallback | `good-model` only | 200 |

CPA's own credential cooldown is respected: once `bad-model` is cooling, the next combo request skips
straight to `good-model`. The recursion guard works: zero re-entry into the router.

## What `tea-combos` must do (learned the hard way)

1. **Declare executor formats.** A plugin executor is silently skipped, and the client sees
   `unknown provider for model`, unless the registration declares `executor_input_formats` and
   `executor_output_formats` (and `executor_model_scope`). Nothing is logged. This cost the most time.
2. **Config arrives as raw YAML bytes** for `plugins.configs.<id>`. Use `gopkg.in/yaml.v3`.
   `ConfigField.Type` is advisory metadata for UIs, not a parser.
3. **Host callbacks need the callback id.** `host.model.execute` carries a `host_callback_id` in a
   wrapper next to the request; the executor request delivers it. CPA uses it to find the calling
   plugin and set `SkipRouterPluginID`, which is what prevents a combo re-entering itself.
4. **Failures are errors, not statuses.** The host reports upstream failure through the error
   envelope with `retryable` and `http_status`, not as a response with a non-2xx code. Fall through on
   `retryable` or 408/429/5xx; stop on anything else.
5. **`ExecutorResponse` has no status field.** Returning a failed chain as a response reaches the
   client as HTTP 200. Return an error through the envelope with `http_status` instead, so clients
   see the real code. The first run made exactly this mistake (200 with an error body).
6. **`ModelRouteResponse` has no metadata.** Use `TargetKind: executor` and `Target: <executor id>`;
   the executor recovers the combo name from `ExecutorRequest.Model`.
7. `ForcedProvider` pins a member to one provider; leave it empty to let CPA resolve the member
   normally, including its own credential selection and cooldowns.

## Not covered by the spike

- **Streaming.** `ExecuteStream` was not implemented. SSE fallback is only safe before the first
  byte is sent; once a member has started streaming, a mid-stream failure cannot fall back. The real
  plugin must decide and document this (CPA's own alias-pool retry has the same limit: "fails before
  producing output").
- Token counting (`CountTokens`), per-combo strategy (priority vs round robin), weights.
- Usage attribution: whether member usage is billed to the real model or to the combo name.
- Management API and UI for editing combos.
- Behaviour with real OAuth providers rather than an openai-compatible mock.

## Source

The throwaway plugin lives in `spikes/tea-combo-spike/`. It is a reference, not a product. Build the
real thing as `plugins/tea-combos`.
