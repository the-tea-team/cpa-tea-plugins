# Capacity and quota routing — acceptance record

## Scope and status

Implemented `tea-quota-router` 0.1.0 and tea-combos 0.3.0 in an isolated branch. No CPA/CPAMP fork, no production install, no live routing edits. The original full acceptance is **not complete**: independent per-combo account policy is blocked by the v8.0.69 native execution contract (see spike). This is a tested staged implementation, not a claim all original requirements shipped.

## Evidence

- `evidence/verification.json`: programmatically counted Go test results, vet and race runs.
- `evidence/native-e2e.json`: real released CPA 8.0.69, native shared plugins and synthetic upstream. Inventory, management authorization, 1x/5x weighting, 5:1 member allocation, streaming, 503 fallback, account attribution, shadow/active scheduling, override readback/restart and known-exhaustion rejection without an upstream request. Native session affinity is explicitly asserted.
- `evidence/collector-e2e.json`: real CPA/native plugin and synthetic OAuth credentials, mocked management api-call upstream; **no fixtureSnapshotPath**. Background usage/profile collection and normalized 1x/5x pools, API-key visibility without OAuth mis-probes, auth and refresh coalescing.
- Repository `scripts/check.sh`: formatting, vet, tests, all native c-shared builds and registry consistency passed.
- Browser: desktop light/dark, 390px mobile (document width=viewport width), 7-day period, refresh, override load→diff→save→readback. No browser storage keys. Screenshots visually inspected, not just captured.
- CPAMP 1.14.10 image, digest `sha256:0e803f21d51aceea7b752686417bf57f40678d8068fe102ba23cb2a3afb414e1`: isolated container proxy returned 200 for public static page, 401 for unauthenticated summary, and 200 for authorized summary. No inherited iframe cookie session exists; optional in-memory key prompt is used.
- Read-only existing CPA observations: Claude and Codex usage endpoints and Claude profile returned HTTP200 through CPA-owned token injection. Unknown plan identifiers remain unresolved; no 1x/5x guess was substituted. No raw credential/profile bodies were logged or saved.

## Review fixes exercised

Transactional single authoritative state envelope; stale in-flight collector cannot restore old overrides; removal restores raw baseline; fixture-to-live restart discards synthetic observations; no-follow/nonblocking secure state/snapshot readers; FIFO/writable-file rejection; scoped Cowork limits cannot exhaust unrelated models; glob-shaped scope IDs cannot broaden scope; above-100 utilization treated exhausted; stale pool references no longer block unrelated edits or broaden membership; full-group conflicting overrides rejected; native API config inventory retained; YAML pool fields preserved; usage retention and explicit-price estimates; caller-scoped bounded affinity.

## Remaining gates and limitations

1. **Blocked requirement:** combo-local accountStrategy other than inherit is explicitly rejected. AuthID pin bypassed normal priority selection in the actual spike. Member strategy and provider-default account strategy are independent; policies per combo sharing the same upstream model are not implemented.
2. Native end-to-end account affinity was proven for a direct chat request with prompt_cache_key; broader nested-combo/caller/model request coverage remains necessary before active rollout.
3. Installed background collection against real production accounts is not yet run. Separate native mock collection and live read-only endpoint probes are not equivalent to a production soak.
4. API billing adapters require authorized billing/admin integrations. OpenRouter key usage works at collector level; account-credit polling is not connected. No fictional all-provider remaining percentage or balance.
5. Plan multipliers are estimates/overrides when comparable absolute quotas are unavailable. Unrecognized plan IDs stay unknown and are excluded from capacity-weight denominators with coverage shown.
6. Concurrency-pressure penalties and forecasted waste are not included; recommendations show observed reset opportunities, not unsupported predictions.
7. `quotaPool` on a combo is an observation reference, **not** a credential allowlist or paid-account security boundary. Explicit paid fallback role is preserved, but mixed-provider native account inventory is not partitioned by this field.
8. Native binaries built/exercised on Linux arm64; the live CPA machine is amd64. Production-target artifacts and soak/rollback rehearsal are pending; do not deploy arm64 binaries there.
9. The final independent regression review passed for the specifically fixed production-code findings (secure state reads, fixture clearing, override conflicts, pool membership, scope parsing and exhaustion). It was a limited static review, not whole-repository approval; remaining limitations above keep the work a draft, not production approval.

## Reproduce

```sh
GOTOOLCHAIN=auto GOPROXY=https://proxy.golang.org,direct GOMAXPROCS=2 scripts/check.sh
python3 scripts/quota-native-e2e.py --cpa-binary /path/to/8.0.69/cli-proxy-api
CPA_BINARY=/path/to/8.0.69/cli-proxy-api python3 scripts/quota-collector-e2e.py
```

Python3 + PyYAML, Go1.27 and a C compiler are required. Test gateways bind only loopback and use synthetic credentials. The native harness leaves its run directory for debugging; test servers are stopped in `finally` unless `--serve` is explicitly requested. The UI proxy is synthetic-only and must never be exposed publicly.

**Secret exposure audit:** no real secret values were printed. Synthetic test credentials are clearly labeled in test scripts. Private live quota values and host inventory are not included in this public record.
