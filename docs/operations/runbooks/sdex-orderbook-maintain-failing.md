---
title: Runbook — sdex-orderbook-maintain-failing
last_verified: 2026-07-29
status: draft
severity: P3
---

# Runbook — `stellarindex_sdex_orderbook_maintain_failing` / `stellarindex_sdex_orderbook_crossed_book`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_sdex_orderbook_maintain_failing` (ticket), `stellarindex_sdex_orderbook_crossed_book` (ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 5–30 min (ClickHouse reachability, or the initial load exceeding its 30-min cap) |
| Impact | Four distinct modes — check WHICH outcome or alert is firing. `load_error`: `/v1/sdex/orderbook` serves a 503 warming problem (user-visible outage of the endpoint). `advance_error`: the endpoint answers with increasingly stale depth, honestly timestamped (`as_of_ledger` stops advancing). `verify_error`: the version-tie quarantine stops draining, so its offers stay out of every served book — visible per side as `ask_offers_withheld` / `bid_offers_withheld`. `crossed_book`: a served pair has best bid > best ask — phantom offers on that market. |

## Symptoms

- `stellarindex_sdex_orderbook_maintain_total{outcome=~"load_error|advance_error|verify_error"}`
  increasing for 30+ min.
- `stellarindex_sdex_orderbook_pending_offers` flat and non-zero while
  `verify_error` rises (quarantine wedged), or
  `stellarindex_sdex_orderbook_crossed_pairs` > 0 for 30+ min.
- `journalctl -u stellarindex-api | grep "sdex order book"` shows the
  underlying lake error per attempt (load retries ride the 60s
  advance ticker).
- `curl localhost:3000/v1/sdex/orderbook?selling=native&buying=USDC-G...`
  returns 503 (load never landed), a stale `as_of_ledger`
  (advance failing), or non-zero `ask_offers_withheld` /
  `bid_offers_withheld` (quarantined offers not yet verified).

## Quick diagnosis (≤ 5 min)

```sh
# Which mode? load_error / advance_error / verify_error:
curl -s localhost:9464/metrics | grep sdex_orderbook_maintain_total

# Quarantine size and crossed pairs:
curl -s localhost:9464/metrics | grep -E 'sdex_orderbook_(pending_offers|crossed_pairs) '

# Which pairs are crossed (logged on every change of the count):
journalctl -u stellarindex-api --since -2h | grep "crossed-pair count changed" | tail -3

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
   load/advance observations at all for several minutes). A restart
   also re-quarantines every `intra_ledger_seq == 0` offer, so expect
   `pending_offers` to jump and the `*_offers_withheld` counts to be
   non-zero for hours afterwards.
5. `verify_error` sustained: the removal probe (`OfferRemovedAt`, an
   `IN (...)` point read on `ledger_entry_changes`) is failing. The
   book stays honest — suspects are withheld, never served — but it is
   thinner than the chain for as long as this lasts. Treat as a
   ClickHouse read failure; the journal line `sdex order book verify`
   carries the error.
6. `crossed_book`: take the pair ids from the journal line above and
   check the served book (`/v1/sdex/orderbook?selling=A&buying=B`). A
   crossed resting book is impossible on-chain, so one side carries an
   offer whose removal the lake never ingested. Verification cannot
   disprove it and the daily re-load reloads it — chase the missing
   `ledger_entry_changes` removal with the completeness tooling. There
   is no ops-side mitigation short of healing the lake.

## Root cause analysis

The book is loaded once per process start from the lake's live-offer
slice and advanced with partition-pruned incremental reads keyed by
`version` (see `internal/storage/clickhouse/sdex_offer_book_reader.go`
for the trade-off note). Failures are lake reads failing — there is
no served-tier or Redis dependency. Chart the `load_ok` duration
observations across deploys: a creeping load wall-time predicts
hitting the cap before it happens.

## Known false-positive patterns

- `crossed_book` does NOT count a touching book (best bid == best
  ask): a PASSIVE offer legally rests against an opposite offer at
  exactly the inverse price. Anything it does count is strictly
  crossed and is not a false positive.

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
- 2026-09-23: `verify_error` joined the maintain-failing alert; added
  `stellarindex_sdex_orderbook_crossed_book` and the per-side
  `*_offers_withheld` counts on `/v1/sdex/orderbook`.
