# CPA 8.0.69 native spike — P0 gates FAILED / incomplete

**Gate B FAILED:** real unpinned request chose synthetic priority 10; an exact AuthID pin chose synthetic priority 1 for the same model while the priority-10 account remained available. A selector must enforce priority before pinning; host pinning does not preserve it automatically. This is a decision gate, not an implementation approval.

**Initial Gate A incomplete; subsequently closed by `scripts/quota-collector-e2e.py` (14/14 native checks):** `host.auth.list` returned zero entries for two configured OpenAI-compatible API keys, while `host.auth.models` returned two credential entries. This fixture does not prove OAuth inventory, quota collection, management transport, or CPAMP authentication. Source `internal/pluginhost/auth_callbacks.go:695` excludes non-runtime records with no path, a plausible explanation requiring independent verification. No quota or UI feature is approved by this spike.

## Executed proof

- `python3 harness.py > run-result.json`: exit 0, actual CPA 8.0.69 commit f7ae8fee loaded and registered a Go c-shared plugin. Synthetic upstream only; two OpenAI-compatible accounts share model `shared-model`, with group priorities 10 and 1. Normal/high/low routes all returned 200; mock observed high/high/low. `host.auth.models` reported 2; `host.auth.list` reported 0. Caller scope metadata was present and forwarded, but authorization preservation was NOT demonstrated.
- `python3 -m unittest -v test_spike > test-result.txt 2>&1`: exit **1**, 3 passing tests, 1 intentional failing gate (inventory). Initial RED before implementation: missing harness assertion failed.
- `GOTOOLCHAIN=auto GOMAXPROCS=2 go test -p=2 ./...`: exit 0, no Go test files. Real behavior tests are Python above.
- Executable SHA-256 `782ec929a094596376b772d1d98630762488713bd5d9ee206c3cddc2836e0d46`, checked every run. Release archive SHA-256 `19f13e62643ea9f6275553622ad13f945e551397166f099a78c32a2e0f85d4c3`, matches provided 8.0.69 checksums.txt. Archive-to-binary extraction comparison is assumed from existing verified runtime, not repeated here.

## Boundaries and cautions

This is a diagnostic spike only, not safe production code. `host.auth.models` fallback selects the two entries by returned order, not priority metadata; the observed mock lanes establish which was selected for this fixture, not a reliable general identity mapping. Scope presence is not proof that nested model allowlists or account restrictions are enforced. No frontend-auth restriction test, unauthorized-account execution test, OAuth-versus-paid isolation, concurrency, affinity, cooldown/race, stream first-byte, cancellation, budget, secure management, or exactly-once accounting was performed. Two combo policies were exercised sequentially only. Test emits sanitized synthetic lane names and counts, no bearer values.

The adapted ABI retains the older spike's global callback ID and is not concurrency-safe; do not reuse it in production or claim simultaneous isolation. The streaming executor is deliberately unimplemented. The priority bypass alone fails the Gate B requirement unless the proposed selector explicitly enforces and verifies core priority in all eligibility/race cases. Never turn an exhausted pinned pool into an unpinned mixed-pool request.

Run from this directory: `python3 -m unittest -v test_spike`; its failure is expected and represents the open gate, not an environmental success. The harness uses only ephemeral loopback ports and TMPDIR temporary files and terminates its own process. Go module is pinned to v8.0.69 rather than combo spike's v8.0.51; exact complete SDK delta and metrics v8.0.62 comparison remain unperformed.
