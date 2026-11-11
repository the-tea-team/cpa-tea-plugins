# tea-metrics

Per-model and per-provider gateway health, collected in-process.

`HandleUsage` receives CPA's own usage records after every request — no queue, no polling,
no event theft — and stores per-request samples in a local SQLite file. The summary endpoint
turns them into decode tok/s percentiles, TTFT, prefill speed estimates, cache hit rate,
failure and 429 counts, plus hourly medians for the chart.

## Config

```yaml
plugins:
  configs:
    tea-metrics:
      enabled: true
      # dataDir: /var/lib/cliproxyapi/tea-metrics   # default: plugin data dir
      # retentionDays: 30
```

## Endpoints

- `GET /v0/management/plugins/tea-metrics/summary?window=24h|7d|30d` — JSON (management auth)
- `GET /v0/resource/plugins/tea-metrics/metrics` — the dashboard page (no auth on the page
  itself; it asks for the management key once and keeps it in browser storage)

CPA-Manager-Plus mounts the page automatically under its plugin-pages navigation.

## Notes

- Decode speed = (output+reasoning tokens) / (latency - TTFT), streamed successful requests only.
- Prefill t/s is an estimate: TTFT includes queue and network time, not just prefill.
- Samples older than `retentionDays` are pruned daily; the tap never blocks a request
  (records are dropped and counted if the writer falls behind).
