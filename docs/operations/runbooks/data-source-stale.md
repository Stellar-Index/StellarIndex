---
title: Runbook — data source stale
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_data_source_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_data_source_stale` |
| Severity | **P3** (ticket) |
| Detected by | `stellarindex_data_freshness_stale{domain,source} == 1` for > 1h |
| Emitted by | `data-freshness.sh` → node_exporter textfile (`data_freshness.prom`), every 15 min |
| Typical MTTR | 10–60 min (often an external key/quota or a wedged poller) |
| Impact | The named domain/source is no longer ingesting fresh rows. Reference/oracle feeds degrade cross-checks; a CEX/DEX trade source going stale degrades VWAP for its pairs. |

## Symptoms

`stellarindex_data_freshness_stale{domain="X",source="Y"} = 1`. The age is on
`stellarindex_data_freshness_age_seconds{domain="X",source="Y"}`. The gap
detector (`stellarindex_ingest_gap_detected`) covers on-chain trade/event source
*gaps*; this alert covers everything else going *stale*: reference oracles, FX,
supply, the issuer-metadata cron, the SEP-41 supply lake, and the completeness
verdict itself.

### Thresholds, per domain

The threshold is NOT uniform — each domain (and two sources within a domain)
carries its own, chosen as a generous multiple of that feed's real cadence.
Straight from `data-freshness.sh`; check this table before treating a firing
source as anomalous:

| `domain` | measured from | threshold | note |
| -------- | ------------- | --------- | ---- |
| `oracle` (`ecb`) | `oracle_updates.ingested_at` | **96 h** | ECB is a DAILY FX reference published ~16:00 CET on TARGET business days — a 3 h threshold read stale ~21 h of every day, and 96 h tolerates a weekend plus a holiday |
| `oracle` (everything else) | `oracle_updates.ingested_at` | 3 h | reflector / redstone / band / chainlink / coingecko update every few minutes |
| `fx` | `fx_quotes.bucket` | 48 h | daily-grain; measured off the bucket, so "today's bucket written" = the worker is alive |
| `trades` (`phoenix`, `comet`) | `source_volume_1h.bucket` | **24 h** | measured gap distribution on these sparse Soroban AMMs is max 8 h 28 m / p99 3 h 12 m; a genuine 12 h+ market lull false-fired the flat 4 h threshold twice (the CS-102 class — quiet, not stale) |
| `trades` (everything else) | `source_volume_1h.bucket` | 4 h | |
| `supply` | `asset_supply_history.time` | 30 h | whole-table max — see the false-positive note below |
| `verdict` | `completeness_snapshots.computed_at` | 36 h | the ADR-0033 verdict's own liveness |
| `sep1` | `issuers.sep1_resolved_at` | 48 h | issuer-metadata refresh cron |
| `sep41_supply` (`supply_flows`) | ClickHouse `stellar.supply_flows.ingested_at` | 1 h | the only non-Postgres probe; it backs `/v1/assets` SEP-41 supply, which sums `supply_flows` FINAL on demand, so its freshness IS the served supply's freshness |

## Quick diagnosis (≤ 5 min)

```sh
# Which domain/source + how stale (seconds):
curl -s localhost:9100/metrics | grep 'stellarindex_data_freshness_age_seconds' | sort -t' ' -k2 -n | tail
# The writer's recent logs (oracle/fx pollers run in the indexer or api):
journalctl -u stellarindex-indexer -u stellarindex-api --since '2 hours ago' | grep -iE "<source>|poller error|429|401|quota"
```

## Mitigation (≤ 15 min)

- **External API quota / auth (oracle `coingecko`, FX `massive`, `chainlink`):**
  a `429`/`401` in the poller logs means the key is exhausted/expired. Restore
  the paid key in `/etc/default/stellarindex` and restart the owning binary.
  (CoinGecko Pro purchase is tracked as launch-todo P0-3.)
- **`domain="verdict"` stale:** the `compute-completeness.timer` isn't running —
  see `systemctl status compute-completeness.service`.
- **`domain="sep1"` stale:** the `sep1-refresh.timer` isn't running.
- **`domain="trades"` (CEX/DEX) stale:** the venue connector/dispatcher stopped;
  check the indexer. For `phoenix`/`comet` confirm it is not just a quiet market
  (query the lake for swap events on any known pool) before chasing a decoder.
- **`domain="sep41_supply"` stale:** the indexer's live `supply_flows` write
  into ClickHouse has stalled. Check the lake is up (`curl -s
  http://localhost:8123/ --data-binary "SELECT 1"`), then the `ch-supply`
  gap-fill timer. **If the whole `data_freshness.prom` looks frozen rather than
  stale, read [data-freshness-watchdog-silent](data-freshness-watchdog-silent.md)
  first** — a pre-2026-08-29 build of the script aborted its entire run when
  this ClickHouse probe failed.

## Root cause analysis

The freshness threshold per domain lives in `data-freshness.sh` (a generous
multiple of each domain's natural cadence). A sustained breach means the writer
behind that domain stopped: an external quota, a wedged poller goroutine, a
crashed timer, or a connector outage.

## Known false-positive patterns

- **FX `massive`** is daily-grain; freshness is measured off `bucket` (today's
  bucket written), threshold 48h — a same-day late publish does not fire.
- **`domain="supply"` measures the WHOLE table's `max(time)`**, so it proves
  only that SOME asset is publishing. On 2026-07-28 it read green while 37 of 48
  watched assets had frozen. The per-asset shape it cannot express is the
  sibling `stellarindex_supply_assets_stale` (+
  `stellarindex_supply_asset_max_age_seconds`), CS-102 — see
  [supply-assets-stale](supply-assets-stale.md). A quiet `supply` domain here
  does NOT mean supply is healthy.
- A brand-new source with no rows yet won't emit a gauge (no false fire).
- `coingecko` will legitimately read stale until the CoinGecko Pro key lands
  (P0-3) — expected, not a regression.

## Related

- `stellarindex_completeness_incomplete` — a source that ingests but no longer reconciles to the lake ([completeness-incomplete](completeness-incomplete.md)).
- `stellarindex_supply_assets_stale` — the per-asset supply freeze this
  domain-level gauge cannot see ([supply-assets-stale](supply-assets-stale.md)).
- `stellarindex_data_freshness_watchdog_silent` — the meta-alert for this
  emitter going dark ([data-freshness-watchdog-silent](data-freshness-watchdog-silent.md)).
- `stellarindex_ingest_gap_detected` — contiguous on-chain ingest gaps (data-derived gap detector).
- `docs/operations/launch-todo.md` — P0-3 (CoinGecko), the freshness-watchdog design.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): added the
  per-domain threshold table — the page implied one uniform cadence while the
  script carries per-source exceptions (ECB 96 h, phoenix/comet 24 h) and the
  new `sep41_supply` ClickHouse domain (1 h) it never mentioned; added the
  CS-102 `stellarindex_supply_assets_stale` sibling and the reason the
  domain-level `supply` gauge cannot see a partial freeze.
- 2026-06-30: created with the data-freshness watchdog.
