---
title: Runbook — sdex-orderbook-maintain-failing
last_verified: 2026-07-29
status: draft
severity: P3
---

# Runbook — `stellarindex_sdex_orderbook_maintain_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_sdex_orderbook_maintain_failing` (ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 5–30 min (ClickHouse reachability, or the initial load exceeding its 30-min cap) |
| Impact | Two distinct modes — check WHICH outcome is failing. `load_error`: `/v1/sdex/orderbook` serves a 503 warming problem (user-visible outage of the endpoint). `advance_error`: the endpoint answers with increasingly stale depth, honestly timestamped (`as_of_ledger` stops advancing). |

## Symptoms

- `stellarindex_sdex_orderbook_maintain_total{outcome=~"load_error|advance_error"}`
  increasing for 30+ min.
- `journalctl -u stellarindex-api | grep "sdex order book"` shows the
  underlying lake error per attempt (load retries ride the 60s
  advance ticker).
- `curl localhost:3000/v1/sdex/orderbook?selling=native&buying=USDC-G...`
  returns 503 (load never landed) or a stale `as_of_ledger`
  (advance failing).

## Quick diagnosis (≤ 5 min)

```sh
# Which mode? load_error vs advance_error:
curl -s localhost:9464/metrics | grep sdex_orderbook_maintain_total

# Lake health — both paths read ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"

# If load_error: how long are attempts running before dying?
curl -s localhost:9464/metrics | grep 'sdex_orderbook_maintain_duration_seconds.*load_error' | tail -3
```

## Mitigation (≤ 15 min)

1. `advance_error` with ClickHouse healthy: transient — the next 60s
   tick retries from the same cursor (changes are applied by version,
   idempotent). Nothing to do if it recovers; investigate the
   specific error if sustained.
2. `load_error` repeating: the initial full-slice FINAL load (minutes
   of streaming IO, 30-min hard cap) keeps dying. Check whether a
   heavy one-shot job is saturating ClickHouse (one-heavy-job rule);
   the load retries automatically every 60s tick, so once the lake
   frees up it self-heals.
3. If the load consistently hits the 30-min cap on a healthy lake,
   the live-offer slice has outgrown the load's work shape — that is
   a code/schema issue, not an ops issue. File it; do not raise the
   cap ad hoc (the launch plan tracks initial-load wall-time as an
   acceptance item).
4. A process restart re-runs the full load from scratch — only
   worthwhile if the maintainer goroutine itself is wedged (no
   load/advance observations at all for several minutes).

## Root cause analysis

The book is loaded once per process start from the lake's live-offer
slice and advanced with partition-pruned incremental reads keyed by
`version` (see `internal/storage/clickhouse/sdex_offer_book_reader.go`
for the trade-off note). Failures are lake reads failing — there is
no served-tier or Redis dependency. Chart the `load_ok` duration
observations across deploys: a creeping load wall-time predicts
hitting the cap before it happens.

## Known false-positive patterns

- A deploy restarts the API while ClickHouse is mid-merge: the first
  load attempt can fail, the 60s retry lands, and the 30-min `for:`
  absorbs it. If the alert fired across a deploy window, confirm
  `load_ok` was observed after and close.

## Related

- Metric docs: `docs/reference/metrics/README.md` —
  `stellarindex_sdex_orderbook_maintain_total` /
  `stellarindex_sdex_orderbook_maintain_duration_seconds`.
- Sibling worker alert:
  [`dex-tvl-refresh-failing`](dex-tvl-refresh-failing.md) (same
  binary, same lake dependency).
- Endpoint + book design: `internal/api/v1/sdex_orderbook.go`.

## Changelog

- 2026-07-29: created with the v0.21.4 background-worker metrics.
