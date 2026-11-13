# tea-combos release notes

## v0.2.0 — cooldown-aware fallback

- Account/quota failures (`401`/`403` with quota, billing, credit or credential wording) now fall
  through to the next member instead of erroring the whole combo. A `403 insufficient_quota` on a
  single-member combo is what motivated this release.
- Cooled members: after any member-side failure the member is skipped by selection for
  `cooldownSeconds` (plugin default 30, per-combo override, `0` disables). Success clears cooling.
- Session affinity skips a cooled binding and rebinds to the member that served.
- Fail-open when every member is cooling: the full ring is attempted rather than a guaranteed 502.
- Exhausted-chain messages now carry a `cooling=[…]` snapshot next to the attempt trace.
- Streaming applies the same classification, cooling and fail-open rules, still only before the
  first payload byte reaches the client.

# tea-combos release notes (draft, v0.1.0)

## Assumptions
- A1 (Design) `ttl` is a Go duration string; default `1h`.
- A2 (Design) `sessionAffinity` is nullable; unset = on.
- A3 (Management) route paths decided in slice D; recorded in the README.
- A4 (Delivery) Nothing pushed and no PR opened; work is on local `feat/tea-combos*` branches.
- A5 (Streaming) A read error before the first payload carries no status from CPA; treated as retryable (502). A non-retryable host error with a status still stops the chain.
- A6 (Streaming) First payload chunk is emitted before `execute_stream` returns, so response headers are the serving member's.
- A7 (UI/API) Page at `/v0/resource/plugins/tea-combos/combos`; per-combo operations at `/combos?name=<name>` (see D1).
- A8 (Affinity) TTL is sliding: refreshed on each successful use.
- A10 (State) With `statePath` unset the plugin refuses CPA's per-process shadow-copy dir and fails registration naming `plugins.configs.tea-combos.statePath`.
- A11 (Fallthrough) Every 5xx plus 408/429 is retryable (the spike listed 500/502/503/504 only).
- A12 (UI) Affinity defaults to ON in the editor, matching the plugin default (fixed in slice E; the first UI build defaulted it off).
- A9 (Session identity) The host does not populate the canonical session id for plugin executors (CPA v8.0.51, `sdk/api/handlers/handlers_execution.go:198-314`), so the documented fallback derivation is used. It differs from core's; differences listed in the README.

## Out-of-scope (not in the brief)
- none recorded

## Signed deferrals
- none

## Signed deviations (host-forced, accepted by Kareem)
- D1 (C15/C17, UI contract steps 2 and 5), signed 2026-11-01 ("ok D1 but test it"). The brief names a resource route at `/` and per-combo routes at `/combos/<name>`. CPA v8.0.51 rejects an empty resource path (`internal/pluginhost/management.go:190-215`) and `:`/`*` in management routes (`:121-153`), so the page is at `/v0/resource/plugins/tea-combos/combos` and per-combo operations are `/combos?name=<name>`. Kareem's condition (test it) was met: the sidebar entry and iframe load on `/combos` in CPAMP, create/edit/delete work through `?name=` via CPAMP and directly, and the brief's literal `/v0/resource/plugins/tea-combos/` returns 404 on the real binary. This also covers the brief's UI-proof URL for C20: the HTML shell is returned at `/combos`, not at `/`.

## SHOULD dispositions
- C23 BUILD (Kareem, 2026-11-01 "ok c23"): third-implementation test for the `selector` seam. Delivered at d9efb83 (`selector_seam_test.go`).

## Verified answers to the brief's open questions (CPA v8.0.51, real runs)
- `/v1/models`: combos ARE listed (C24, ModelRegistrar). Refresh limit: see C24-R.
- Usage attribution: recorded against the MEMBER model (`model`/`alias`/`response_model` = member, provider = the member's provider, executor `OpenAICompatExecutor`); the combo name and plugin id never appear. Matches Kareem's expectation.
- State file: default location is CPA's per-process shadow copy, so `statePath` is required; unset -> register fails naming the key; unwritable at start -> register fails with the write error; unwritable later -> CRUD returns HTTP 500 with the message.
- D1 tested: sidebar entry + iframe load on `/v0/resource/plugins/tea-combos/combos`; create/edit/delete work through `?name=` via CPAMP and directly. The brief's literal `/v0/resource/plugins/tea-combos/` returns 404 on the real binary (CPA rejects `/`).

## UNSIGNED (awaiting Kareem)
- C24-R (refresh limit): combos are listed in `/v1/models` at startup, plugin reconfigure and config reload (measured 0.17 s after a config write), but NOT immediately after a UI/management edit. CPA v8.0.51 has no host callback that re-runs model registration (host methods in `internal/pluginhost/host_callbacks.go`; only callers are `sdk/cliproxy/service_lifecycle.go:141,230`, `service_plugins.go:86,147`, `service_config.go:249`). Routing is unaffected: an edit routes immediately. Options: accept, or authorise a small additive `host.model.register` callback in CPA (core change; brief says ask first).
- (D1 moved to Signed deviations below.)

## INVENTED UI (accepted into release)
- Combos table, create/edit form, member up/down reorder, key prompt / forget key, error/loading/empty states, inline-SVG logo, CSP policy. Each is brief-required; the repo has no prior UI system to reuse.

## Known non-blocking issues
- C24 listing lags UI edits until the next config reload (see C24-R).
- Behind CPA-Manager-Plus the editor page needs the CPAMP Admin Key, not the CPA management key (CPAMP proxies and gates the route). The page text now says so.
- `home.enabled: true` makes CPA return 404 for plugin resource pages; see the next item for the real-binary confirmation.
- A CPA 503 cooldown on a member can hide that member from routing before the request reaches the upstream; this is core behaviour.
- Binding persistence is write-behind (~2 s). A graceful CPA stop flushes it; a SIGKILL can lose up to that window.
- N1 (UI, minor, from QA): a 128-character combo name widens the combos table past its container, so Strategy, Members and Actions are clipped. Names up to 128 characters are valid; the table does not wrap them.
- Noted (core, not this change): a few wrong management keys from one IP trigger CPA's own IP ban (403) that clears after about a minute.
- A combo with an empty member list can be saved (201); requests for it fail with `unknown provider for model` because the router does not claim it. Not a listed requirement.
- After a member returns 503, CPA's own cooldown can steer new round-robin conversations away from it for about two minutes (observed by the G5 audit; core behaviour).
- In CPA home mode (`-home-jwt`) the plugin page AND `/v0/management/plugins` return 404 (`internal/api/server_management.go:219,293`). Confirmed on the real v8.0.51 binary with a stand-in home control plane (README "home.enabled confirmation"); home off returns 200 for the same plugin. Not worked around.
