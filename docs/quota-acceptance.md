# Capacity and quota routing — acceptance record

## Scope

Native plugins, stock CPA 8.0.69 and CPAMP: tea-quota-router 0.2.0 and tea-combos 0.4.0. Numeric priority preservation is deliberately not required in quota-aware mode. See ROUTING-CONTRACT.md. Deployment requires amd64 CI artifacts, checksums, protected config/binary/state backups and post-deploy read-back. This source record does not itself assert a production deployment.

## Reproducible evidence

- scripts/check.sh: formatting, vet, all module tests, native c-shared builds and registry consistency. Race tests for both modified plugin modules.
- evidence/native-e2e.json: released CPA and synthetic credentials, 19 checks for observer/core affinity, weighted pools and members, stream/fallback, usage attribution, scheduler shadow/active/affinity, overrides and exhaustion.
- evidence/collector-e2e.json: 14 checks without fixtureSnapshotPath; native inventory/background collection and parser, authentication, coalescing, weighted pools.
- evidence/quota-routing-e2e.json: 17 checks. Native all-priority account scheduling, concurrent same-model independent RR/quota policies, concurrent pool isolation, actual tea-keys managed caller denial, account affinity, bounded 503 retry, no unpin on failure, stream-before-first fallback/no post-output replay, core cooldown and disabled rejection with deliberately stale eligibility, fully exhausted pool and stale-snapshot no-upstream rejection. Synthetic RR 15/30 low lane, quota 30/30 low lane.
- evidence/quota-subscription-e2e.json: 20 checks. Actual dummy Claude OAuth/model inventory plus mock paid API lane. Ordinary exhausted and core-disabled-subscription routes reject without paid calls; explicit API pin and API-only model succeed. Tripwire proves no dummy OAuth completion sent to Anthropic.
- Independent limited regression review: account_exec.go, account_router.go, scheduler.go diff; no blocker found after interleaved credential retries, empty-stream handling, status-only errors and filtered-subscription paid-spill fixes. Not whole-repository approval.

## Limits

- Unknown plan capacity remains unknown; routing uses neutral account weights where not measurable. No invented 1x/5x mapping.
- accountPool is the explicit pin boundary; quotaPool is observation only. Native pins still pass core eligibility. Inherit includes installed provider policy; choose explicit RR to override that policy per combo.
- Explicit-account nested combos are rejected. Broader provider/client protocol combinations and a multi-day production soak are not proven by these harnesses.
- Affinity is success-only and caller-scoped; explicit account bindings are in-memory with TTL, not restart-persisted.
- Paid fallback remains an explicit operator role; API-only and explicit API-pin routes remain available.
- No concurrency-pressure penalty, expiry forecast or OpenRouter account-credit polling. Pricing requires configured rates.
- Historical browser dashboard/CPAMP proxy checks are separate from new combo-control browser checks and live verification.

## Reproduce

```sh
GOTOOLCHAIN=auto GOMAXPROCS=2 scripts/check.sh
export CPA_BINARY=/path/to/CPA-v8.0.69/cli-proxy-api
python3 scripts/quota-native-e2e.py
python3 scripts/quota-collector-e2e.py
python3 scripts/quota-routing-e2e.py
python3 scripts/quota-subscription-e2e.py
```

Python3 + PyYAML, Go1.27 and a C compiler required. Loopback test gateways use synthetic credentials; processes close in finally. No real secret values are included in public evidence.
