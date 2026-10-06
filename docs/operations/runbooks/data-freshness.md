---
title: Runbook — data freshness alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — data freshness

All alerts here are `severity: ticket` (P3), identical in `configs/prometheus/rules.r1/data-freshness.yml` and `deploy/monitoring/rules/data-freshness.yml`; the two serving alerts are identical in both `serving-freshness.yml` copies.

Most gauges come from `data-freshness.sh` (`configs/ansible/roles/archival-node/files/data-freshness.sh` -> `/usr/local/sbin/data-freshness.sh`), run by `data-freshness.timer` every 15 min, writing `/var/lib/node_exporter/textfile_collector/data_freshness.prom` atomically (temp file + swap). Served-value gauges come from `verify-served-values`. If several alerts fire together, check the emitter first ([watchdog_silent](#stellarindex_data_freshness_watchdog_silent)).

## At a glance

- [`stellarindex_data_source_stale`](#stellarindex_data_source_stale)
- [`stellarindex_supply_assets_stale`](#stellarindex_supply_assets_stale)
- [`stellarindex_completeness_incomplete`](#stellarindex_completeness_incomplete)
- [`stellarindex_source_recognition_failing`](#stellarindex_source_recognition_failing)
- [`stellarindex_twap_history_missing`](#stellarindex_twap_history_missing)
- [`stellarindex_data_freshness_watchdog_silent`](#stellarindex_data_freshness_watchdog_silent)
- [`stellarindex_sep41_supply_freshness_absent`](#stellarindex_sep41_supply_freshness_absent)
- [`stellarindex_data_freshness_probe_frozen`](#stellarindex_data_freshness_probe_frozen)
- [`stellarindex_served_value_drift`](#stellarindex_served_value_drift)
- [`stellarindex_served_value_check_stale`](#stellarindex_served_value_check_stale)
- [`stellarindex_served_value_persistently_skipped`](#stellarindex_served_value_persistently_skipped)
- [`stellarindex_sdf_reserve_list_drift`](#stellarindex_sdf_reserve_list_drift)
- [`stellarindex_served_value_unit_failed`](#stellarindex_served_value_unit_failed)
- [`stellarindex_serving_insert_frozen`](#stellarindex_serving_insert_frozen)
- [`stellarindex_serving_insert_absent`](#stellarindex_serving_insert_absent)
- [`stellarindex_assets_popular_priceless`](#stellarindex_assets_popular_priceless)
- [`curated-rwa-sync`](#curated-rwa-sync)
- [`fx-history-missing`](#fx-history-missing)

## stellarindex_data_source_stale

Trips: `stellarindex_data_freshness_stale == 1`, `for: 1h`. Age: `stellarindex_data_freshness_age_seconds{domain,source}`. Covers everything except on-chain trade/event gaps (`stellarindex_ingest_gap_detected` owns those): reference oracles, FX, supply, issuer metadata, the SEP-41 supply lake, the completeness verdict.

Means: the feed died (e.g. coingecko API quota), a poller wedged, or a writer/timer stopped. Reference/oracle staleness degrades cross-checks; a CEX/DEX trade source going stale degrades VWAP for its pairs. MTTR 10-60 min.

The universe is every `{domain,source}` the table has EVER held, aged against its own newest row, so a dead source never leaves the alert; one retired on purpose alarms forever ([Retiring a source](#retiring-a-source)). A brand-new source with no rows emits no gauge. A domain-level probe (`supply`, `sep1`) with no rows at all reads stale (1).

Thresholds, from `data-freshness.sh` (check before treating a firing source as anomalous):

| `domain` | measured from | threshold | note |
| --- | --- | --- | --- |
| `oracle` (`ecb`, `tiingo`) | `oracle_updates.ingested_at` | **96 h** | daily/end-of-day feeds (ECB ~16:00 CET on TARGET business days; tiingo NAV bars, a re-poll of a stored bar does not move `ingested_at`); 96 h tolerates a weekend plus a holiday |
| `oracle` (everything else) | `oracle_updates.ingested_at` | 3 h | reflector / redstone / band / chainlink / coingecko update every few minutes |
| `fx` | `fx_quotes.bucket` | **76 h** | daily-grain; "today's bucket written" = worker alive. Mirrors `aggregate.composite_reference.fx_max_age_hours` (the serving path's FX budget; `scripts/ci/data-freshness-test.sh` pins the two together, change both) and spans a weekend market close |
| `trades` (`phoenix`, `comet`) | `source_volume_1h.bucket` | **24 h** | sparse Soroban AMMs: measured max gap 8 h 28 m / p99 3 h 12 m; a 12 h+ real lull false-fired a flat 4 h (quiet, not stale) |
| `trades` (everything else) | `source_volume_1h.bucket` | 4 h | |
| `supply` | `asset_supply_history.time` | 30 h | whole-table max; blind to a partial freeze, see [stellarindex_supply_assets_stale](#stellarindex_supply_assets_stale) |
| `verdict` | `completeness_snapshots.computed_at` | 36 h | the ADR-0033 verdict's own liveness |
| `sep1` | `issuers.sep1_payload_fetched_at` | 48 h | issuer-metadata refresh cron |
| `sep41_supply` (`supply_flows`) | ClickHouse `stellar.supply_flows.ingested_at` | 1 h | only non-Postgres probe; backs `/v1/assets` SEP-41 supply (sums `supply_flows` FINAL on demand), so its freshness IS the served supply's freshness |

FX `frankfurter-historical` is not watched (provenance label of the one-off `scripts/ops/fx-history-backfill`; its newest row never advances; excluded by exact name). `coingecko` legitimately reads stale until the CoinGecko Pro key lands.

Diagnose (<= 5 min):

```sh
# Which domain/source + how stale (seconds):
curl -s localhost:9100/metrics | grep 'stellarindex_data_freshness_age_seconds' | sort -t' ' -k2 -n | tail
# The writer's recent logs (oracle/fx pollers run in the indexer or api):
journalctl -u stellarindex-indexer -u stellarindex-api --since '2 hours ago' | grep -iE "<source>|poller error|429|401|quota"
```

Fix, by domain:

- External API quota/auth (oracle `coingecko`, FX `massive`, `chainlink`): `429`/`401` in the poller logs = key exhausted/expired. Restore the paid key in `/etc/default/stellarindex` and restart the owning binary.
- `verdict`: `compute-completeness.timer` not running; `systemctl status compute-completeness.service`.
- `sep1`: no issuer stellar.toml fetched successfully for 48 h. The probe reads `max(sep1_payload_fetched_at)`, stamped only on success, so it fires both when `sep1-refresh.timer` isn't running and when the refresh runs but every fetch fails (our DNS/egress). `systemctl status sep1-refresh.service`; `journalctl -u sep1-refresh --since -24h | grep -E 'SYSTEMIC|FAIL'`. The job also exits non-zero when its failure rate over previously-served domains crosses 90% over 50+ of them, tripping `stellarindex_systemd_unit_failed`.
- `trades` (CEX/DEX): venue connector/dispatcher stopped; check the indexer. For `phoenix`/`comet` first confirm it is not a quiet market (query the lake for swap events on any known pool) before chasing a decoder.
- `sep41_supply`: the indexer's live `supply_flows` write into ClickHouse stalled. Check the lake (`curl -s http://localhost:8123/ --data-binary "SELECT 1"`), then the `ch-supply` gap-fill timer. If the whole `data_freshness.prom` looks frozen rather than stale, go to [stellarindex_data_freshness_probe_frozen](#stellarindex_data_freshness_probe_frozen) first.

## stellarindex_supply_assets_stale

Trips: `stellarindex_supply_assets_stale > 0`, `for: 2h`. Value = count of watched assets with no supply snapshot for over 30 h; worst age on `stellarindex_supply_asset_max_age_seconds`. The watched set is assets with rows in the 30 days before the newest supply row (anchored to the table, so an all-stop stays visible).

Means: those assets serve a FROZEN supply (and market cap/FDV); the API keeps answering with old numbers. The `supply` domain gauge measures whole-table `max(time)` and reads green while other assets publish (once with 37 of 48 watched assets frozen), so usually no accompanying `data_source_stale{domain="supply"}`. MTTR 30 min-hours, usually a freshness gate or observer problem.

Diagnose:

1. Scope:

```sql
SELECT asset_key, now()-max(time) AS age
FROM asset_supply_history
GROUP BY asset_key ORDER BY age DESC LIMIT 20;
```

2. Refresher rejecting or not running? Read outcome counters as deltas, not since-boot totals:

```sh
curl -s http://localhost:9465/metrics | grep supply_refresh_duration_seconds_count
```

(metric `stellarindex_aggregator_supply_refresh_duration_seconds`, aggregator metrics port 9465)

- Dominated by `stale_component`: the freshness GATE is refusing, go to 3.
- Dominated by `no_ledger` / `compute_error`: a reader or the lake; see [supply-refresh-error-dominant](supply.md#stellarindex_aggregator_supply_refresh_error_dominant).
- `ok` / `dormant` advancing: refresher fine, rows ARE landing; re-check step 1.
- `missing_baseline` on a classic asset's SAC wrapper: its pre-Soroban opening balance was never seeded, supply is withheld, no snapshot lands (newly watched wrappers start here). See Fix.

3. Gate refusing: compare PRODUCER progress against the asset's own last activity (the likely cause):

```sql
-- producer watermarks (should track tip)
SELECT max(ledger) FROM trustline_observations;
SELECT max(ledger) FROM claimable_observations;
SELECT max(ledger) FROM sac_balance_observations;
SELECT max(ledger) FROM lp_reserve_observations;
SELECT max(ledger) FROM sep41_supply_events;
SELECT max(ledger) FROM account_observations;
```

If watermarks track tip but the affected assets are quiet in one component, the data is FINE and a gate is misreading quiet as stale. A quiet asset is not a stale asset; do NOT loosen the dormancy horizon (hides the defect, republishes unverified figures). Scope any per-contract/per-asset probe with an indexed predicate: an unbounded `GROUP BY` over `sep41_supply_events` ran 11 min with no output, the index-bounded per-contract form 96 ms.

4. A producer watermark genuinely behind = real stall, the gate is right. Find the stalled writer (indexer/projector); check whether a projector tail rebuild is pending.

Fix:

- Gate misreading quiet as stale: the anchor must be the producer's watermark, not per-entity last activity (all three supply algorithms were fixed this way: classic `e21fa3d0`, SEP-41 `3f26b8db`, XLM `aa0d08c2`). A regression most likely means a new supply path with the old per-entity shape.
- Unseeded SAC wrapper (`missing_baseline`): the aggregator seeds a newly watched wrapper itself on its first refresh (retries every 10 min; failures log `genesis baseline auto-seed failed`, usually an unreachable lake). If it persists, preview then seed by hand (idempotent; nothing written without `-write`):

  ```sh
  stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml
  stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -write
  ```

  On r1 run as the service user with the service environment (`systemd-run --wait --pipe -p User=stellarindex -p EnvironmentFile=/etc/default/stellarindex …`). The next refresh pass writes a snapshot; expect `outcome="ok"` per asset.
- Genuinely stalled producer: restart/repair the writer, then the relevant catch-up (`projector-replay` for projected sources).
- Verify: `scripts/ops/reconcile-supply-vs-horizon.sh` (checks every classic asset against Horizon's FULL component sum); confirm the count returns to 0.

## stellarindex_completeness_incomplete

Trips: `stellarindex_completeness_incomplete == 1`, `for: 1h`. From the latest `completeness_snapshots` row per source (ADR-0033 `complete=false`; the `recognition` row is excluded on purpose, see below). Means the served tier no longer reconciles to the certified ClickHouse lake for that source. The verdict (`compute-completeness`, nightly `-pass`) self-seeds factory children, so by default this is a real served<>lake gap, not a checker artifact. Read `detail` first: it names the per-target delta and window, or says PENDING (below).

Diagnose (<= 5 min):

```sh
# The exact delta + window the verdict recorded:
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, complete, watermark_ledger, detail FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s WHERE NOT complete;"
# Re-run that source to confirm it persists (off the serving DB, -ch):
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -source <X> -from <recent>
```

Causes: dropped rows (decoder bug fixed forward-only, e.g. SEP-41 CAP-67 loss), a missed projection window, a retention/PK artifact.

### Fix: real gap

Re-derive the source from the lake, then re-verify.

- Soroban projected sources (`trades`/protocol tables); `-config`, `-source`, `-from` all REQUIRED, `-write` required to actually rewind (dry run is default); the source name is the projector registry's, not the hyphenated table name:

  ```sh
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source <X> -from <F> -write
  ```

- Non-projected (`sdex`, soroban-events):

  ```sh
  stellarindex-ops backfill -write -config /etc/stellarindex.toml \
    -source <X> -from <F> -to <T>
  ```

  or the lake re-derive `ch-rebuild` (`-config`, `-from`, `-to` required; `-write` to apply; `-sdex` for SDEX, `-contract-calls` for band/soroswap-router).
- Re-run `compute-completeness -ch -source <X>`; the gauge clears on `complete=true`. Run chunked and off-peak (SDEX/heavy re-derives blow ClickHouse's per-query memory over large windows).

The manual re-verify only clears the gauge sooner: the nightly `-pass` floors a source whose prior projection verdict is failing at its genesis and publishes `complete=true` itself once the repair earns it. That re-verify can outlast the pass deadline (`-timeout`, default `120m`; sdex from 61249957 is ~3.44M ledgers at ~200 ledgers/s ~ 4.8 h), so the pass runs from-genesis sources LAST and publishes the `recognition` row first; only the re-verifying tail is left unevaluated (named in the pass's error). For a one-off larger budget set `PASS_TIMEOUT` (e.g. `PASS_TIMEOUT=300m`) in `/etc/default/compute-completeness` AND raise `TimeoutStartSec` in `compute-completeness.service` (54000 s = 43200 s lock wait + 180 min) by as much, or systemd kills the pass first; or clear the source by hand with the chunked `-source` re-run.

### Pending: deferred dirty window (sdex, sep41_transfers)

`detail` reading `dirty window [F,T] PENDING this source's dedicated weekly compute-completeness timer` = nothing found wrong, PENDING RE-VERIFICATION. A `backfill -write`, `ch-rebuild` or `projector-replay` rewrote served rows from `F`, and re-checking `F` to tip is more than one day of ledgers, too wide for the nightly `-pass`; the pass withholds `complete` and holds the watermark below `F` rather than carrying the old claim over the rewrite. Do NOT re-derive. Re-prove with the weekly budget (clears once it reconciles clean):

```sh
sudo systemctl start compute-completeness-sdex.service   # or compute-completeness-sep41.service
# equivalent: run-compute-completeness.sh -source <X> -timeout 360m
```

A window that fits the pass (re-check from `F` within a day of ledgers) is cleared by the next nightly run with no action.

The exception is a `backfill -write [F,T]` window on a projected source (not sdex, band or soroswap-router). A `backfill -source soroban-events` landed raw events behind that source's projector cursor while the projector read Postgres `soroban_events`, and nothing re-projects them on its own. The backfill logged the command; run it after the backfill finishes: `stellarindex-ops projector-replay -config PATH -source <X> -from F`.

### Stale: projection evidence older than 10 d 6 h, or unknown

`/v1/coverage` sets `flags.stale` when a source claiming `projection_ok` has `projection_evidenced_at` older than `MaxProjectionCarryAge` (7 d) plus three 26 h audit periods, or `null`. `computed_at` cannot show this (the nightly `-pass` restamps it while carrying the old claim). The carry detail names the proof time ("the carried prefix was last reconciled in full at …", or "has no full-range reconcile on record").

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, projection_evidenced_at, computed_at FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s \
  WHERE projection_ok ORDER BY projection_evidenced_at NULLS FIRST;"
```

- Event sources clear on their own: each `-pass` re-proves from genesis the expired (> 7 d, or `null`) sources, oldest first, at most three per night (logs `re-proving expired projection evidence from genesis this pass: …`). Right after migration 0201 every green source is `null`, so the flag holds ~`ceil(green sources / 3)` nights. If a source stays expired past that, check the pass's error for a deadline cut.
- `sdex` (the census) is re-proved weekly by `compute-completeness-sdex.timer` (Sunday 18:47 UTC): full re-proof ~4.8 h exceeds the pass's 120 min, so the pass never re-floors it. It runs the nightly driver as `-source sdex -timeout 360m` under the same `run-heavy-job.sh` job name (never overlaps the nightly pass; the nightly pass runs 05:30 UTC). After migration 0201 sdex stays `null` until the first Sunday run. If the timer failed or missed a week (`systemctl status compute-completeness-sdex`, `journalctl -u compute-completeness-sdex`), re-run off-peak and outside the 05:30 UTC pass window:

  ```sh
  sudo systemctl start --no-block compute-completeness-sdex.service
  ```

- `sep41_transfers` likewise via `compute-completeness-sep41.timer` (Wednesday 18:47 UTC, `-source sep41_transfers -timeout 360m`): the watched KALE SAC puts CAP-67 transfers in nearly every lake granule, so its from-genesis re-proof overruns the pass's 45 min `-source-timeout`. Re-run with `sudo systemctl start --no-block compute-completeness-sep41.service`.

Prefer the unit: it applies the driver's tip-100 margin (without it undrained ledgers read as sdex mismatches). Do not add `-from`: a run starting above the served floor carries the range below it and stamps no evidence, so chunked `-from` runs cannot clear this.

### Known non-issues

- If a NEW factory-gated source false-fires, confirm its creation events are reachable in `soroban_events` (factory childgate self-seeds).
- The `recognition` row is not a source. `data-freshness.sh` filters `source <> 'recognition'` from this gauge: that row counts event shapes on contracts no source owns (~23k, growing ~30/day), so `complete=false` there is the permanent state of a curated indexer. That census is exported as `stellarindex_recognition_unattributed_shapes` and deliberately not alerted on. The per-source defect signal is [stellarindex_source_recognition_failing](#stellarindex_source_recognition_failing).
- A green verdict with a lagging watermark: `complete=true` speaks only for the range walked; `stellarindex_completeness_watermark_lag_ledgers` is the companion gauge for "verified, but only up to an old ledger".

Model: [ADR-0033](../../adr/0033-completeness-verification-model.md).

## stellarindex_source_recognition_failing

Trips: `stellarindex_recognition_ok == 0`, `for: 1h` (labelled `source`; from the `recognition_ok` column of `completeness_snapshots`, written by the daily `compute-completeness` job).

Means: a source we INDEX emitted a `(contract_id, topic_0_sym)` shape on a contract it OWNS that none of its decoders claim: we silently drop events on a protocol we claim to cover, and its coverage verdict is capped (`complete=false`, shown by public `/v1/coverage` as `recognition_ok: false`). Events are still in the ClickHouse lake; what is lost is projection into the served table, recoverable by replay once the decoder is fixed. Served rows stop advancing, or advance with an event category missing. MTTR hours-days (decoder arm written, reviewed, released, then replay).

Not `stellarindex_recognition_unattributed_shapes` (unowned + unrecognised: foreign protocols, grows forever, no action). Only the owned bucket is a defect.

Diagnose (<= 5 min):

```sh
# 1. Which source, and what does the verdict say?
curl -s https://api.stellarindex.io/v1/coverage \
  | python3 -c "import sys,json;[print(r['source'], r['recognition_ok'], r['detail'][:200]) for r in json.load(sys.stdin)['data']['sources'] if not r['recognition_ok']]"

# 2. Which shapes are unclaimed? Read-only; run under the wrapper.
/usr/local/sbin/run-heavy-job.sh ch-recognition \
  stellarindex-ops ch-recognition -config /etc/stellarindex.toml -top 60
```

Cross-reference reported `contract_id`s against the source's registry (`protocol_contracts`, or its in-code curated set); a shape whose contract belongs to the alerting source is the one to fix.

Causes: (1) the contract upgraded in place (`update_contract` keeps the address; live ingest only sees the current WASM; [contract-schema-evolution](../../architecture/ingest-pipeline.md#contract-schema-evolution)); (2) a new event kind the decoder has no arm for. Same fix: add the decoder arm, gate the backfill behind a per-WASM-hash audit if the range predates the current WASM ([wasm-audits](../wasm-audits/README.md)), then `projector-replay` the affected range ([adr-0033-data-recovery](../adr-0033-data-recovery.md); trap: for gated sources `backfill` writes nothing and exits 0).

No quick mitigation. Immediately establish blast radius (events, ledger range, contract) to size the replay. Do NOT silence the alert: the verdict is capped for a real reason and `/v1/coverage` is public.

False positive: a contract seeded into a source's registry between the recognition scan and the decoder deploy that understands it is briefly owned and unclaimed; clears on the next daily run.

## stellarindex_twap_history_missing

Trips:

```
stellarindex_twap_history_missing == 1
  or
stellarindex_cagg_history_missing == 1
```

`for: 2h`. Label `view` (`twap_1h`/`twap_1d`, or a `prices_*` view via `stellarindex_cagg_history_missing`). The detector compares each view's oldest bar against the oldest `trades` row (or an armed retention policy's floor); a deployment whose `prices_1m` has < 2 days of history is deliberately not judged. MTTR minutes (one WINDOWED refresh per view) once the source is whole.

Means: `trades` holds back-history but the view's oldest materialized bar is far newer (or the view is empty), so it carries only the trailing window its refresh policy auto-fills (`twap_1h` `start_offset` 4h, `twap_1d` 7d). The API serves NO bars for older ranges while recent bars and newest-bar freshness checks read green. Cause: a migration changing a TWAP CAGG's SELECT must `DROP` + `CREATE` it `WITH NO DATA` (the `0081 -> 0115 -> 0126 -> 0147` recreate pattern), or a replay rewrote base rows, and the manual re-materialization was skipped. Nothing enforces it and ADR-0033 does not cover it (`twap_*` are derived CAGGs, not reconcile targets). The TWAP CAGGs are hierarchical roll-ups over `prices_1m`.

NEVER refresh a TWAP view with a NULL start. Migration 0156 attaches a retention policy to `prices_1m`; every chunk it drops writes an invalidation against both TWAP views, and a NULL-start refresh processes them against a `prices_1m` whose old chunks are gone and DELETES the TWAP history for every dropped range. Form quoted only so you recognise it:

```sql
-- DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
```

`scripts/ci/lint-migration-commands.sh` fails the tree if the NULL-start form appears unmarked in a runbook, rule or ops script.

Diagnose (<= 5 min):

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT 'trades' v, min(ts) FROM trades
  UNION ALL SELECT 'prices_1m', min(bucket) FROM prices_1m
  UNION ALL SELECT 'twap_1h', min(bucket) FROM twap_1h
  UNION ALL SELECT 'twap_1d', min(bucket) FROM twap_1d;"
sudo -u postgres psql -d stellarindex -c \
 "SELECT job_id, scheduled FROM timescaledb_information.jobs
   WHERE proc_name = 'policy_retention' AND hypertable_name = 'prices_1m';"
```

If the view's `min(bucket)` trails its source's by more than a day (or the view is empty), the follow-up did not run. Source is `prices_1m` for TWAP views, `trades` for `prices_*` views. The second query shows whether `prices_1m` retention is armed (`scheduled = t`).

Fix:

1. TWAP views: settle the `prices_1m` window first. A TWAP view can only be rebuilt over the range `prices_1m` still holds. If retention is armed and the TWAP history you need is older than `prices_1m`'s `min(bucket)`, stop and follow the Recovery section of `migrations/0156_prices_1m_retention.up.sql` in order (DISARM and confirm, force-rebuild `prices_1m` in slices for that window), then return to step 2 with the disarm still in force.
2. Re-materialize the empty range, windowed. `<start>` = the source's `min(bucket)` (TWAP) or `min(ts)` (`prices_*`); `<end>` = the view's current `min(bucket)`, or `now()` if the view is empty. The window must be at least the view's minimum refresh width or TimescaleDB rejects it with `22023 refresh window too small`: 3 h for `twap_1h`/`prices_1h`, 3 days for `twap_1d`/`prices_1d`, 21 days for `prices_1w`, 93 days for `prices_1mo` (`MinWindow` in `TradesCAGGs`, `internal/storage/timescale/diagnostics.go`). If shorter, move `<end>` later toward `now()`, never `<start>` earlier (for a TWAP view that reaches into the range retention dropped):

   ```sql
   CALL refresh_continuous_aggregate(
          'twap_1h',
          '<start>'::timestamptz, '<end>'::timestamptz,
          force => true);
   ```

   Repeat for `twap_1d` (or the named `prices_*` view). `force => true` is required: over a range retention dropped, a plain refresh reports `already up-to-date` and writes nothing. Minutes for TWAP views; a `prices_*` view over years of `trades` takes hours, run in slices of a few months.
3. The gauge clears on the next `data-freshness.sh` tick (<= 15 min) once the oldest bar reaches its floor. With `prices_1m` retention armed a TWAP view's floor is still the oldest trade, so the TWAP gauge keeps firing until step 1's rebuild has run.

During a legitimate recreate deploy, `for: 2h` leaves time to run the refresh; after it, wait one tick and confirm it clears rather than silencing. Related: `stellarindex_cagg_last_refresh_unix` / [cagg-stale](timescale.md#stellarindex_timescale_cagg_stale) is the refresh-POLICY health probe (policy not running, a different failure).

## stellarindex_data_freshness_watchdog_silent

Trips: `absent_over_time(stellarindex_data_freshness_stale[45m])`, `for: 15m`. The `stellarindex_data_freshness_*` series are gone: `data-freshness.timer` (every 15 min) failed to fire or the script errors, so the watchdog backstopping coingecko/sep1/verdict staleness is blind. MTTR 5-15 min. A node_exporter restart briefly drops textfile metrics; `for: 15m` absorbs it. It sees ABSENCE only; for a present-but-frozen file see [stellarindex_data_freshness_probe_frozen](#stellarindex_data_freshness_probe_frozen).

Diagnose (<= 5 min):

```sh
systemctl status data-freshness.service data-freshness.timer
journalctl -u data-freshness.service --since '1 hour ago' | tail -30
ls -la --time-style=+%H:%M /var/lib/node_exporter/textfile_collector/data_freshness.prom
# Run it by hand to see the error:
/usr/local/sbin/data-freshness.sh
```

Fix:

- Timer disabled/not loaded: `systemctl enable --now data-freshness.timer`.
- Script errors (psql/DSN): the script reads `/etc/default/stellarindex` VERBATIM for `STELLARINDEX_POSTGRES_DSN` and deliberately never `.`/`source`s it (a systemd EnvironmentFile has unquoted values; the shell parser would expand `$`, split on `;`, eat quotes inside the password; pinned by `scripts/ci/envfile-loader-test.sh`). Do NOT "fix" it by sourcing. A Postgres outage or DSN drift breaks the run: fix the DSN/DB, then `systemctl start data-freshness.service`.
- `psql` not found / wrong cluster: the script calls `/usr/lib/postgresql/${PG_VERSION:-15}/bin/psql` directly to bypass Debian's `pg_wrapper` (stats the cluster data dir, aborts for the unprivileged `stellarindex` user). After a Postgres MAJOR upgrade that path is gone: set `PG_VERSION=<major>` in `/etc/default/data-freshness` and restart the unit.
- Textfile unreadable (0600): node_exporter is unprivileged; the script chmods 0644 before the swap; if a stale 0600 file lingers, `chmod 0644` it.

Root cause patterns: timer not enabled after a rebuild, a DB outage failing a query under `set -euo pipefail`, a permissions regression on the textfile. The script's last producer is a ClickHouse HTTP probe on `:8123` for `stellar.supply_flows`; current builds run it as an `if` condition, log `data-freshness: ClickHouse supply_flows probe failed — sep41_supply gauges skipped this tick` and omit only the two `domain="sep41_supply"` gauges (`scripts/ci/data-freshness-test.sh` pins this). A host not redeployed since may still run the old bare `SF_AGE=$(curl … | tr …)` form, where a ClickHouse outage aborted the whole run before the atomic swap and froze every gauge: check the script on disk.

## stellarindex_sep41_supply_freshness_absent

Trips:

```
group by (instance) (stellarindex_data_freshness_stale)
  unless on (instance)
stellarindex_data_freshness_stale{domain="sep41_supply",source="supply_flows"}
```

`for: 1h`. `data_freshness.prom` is publishing but the ClickHouse-probed `sep41_supply` pair has been missing over an hour: `data-freshness.sh` skips it whenever its `supply_flows` probe fails, so `stellarindex_data_source_stale` cannot fire for served SEP-41 supply and neither can `watchdog_silent` (whole family). Served SEP-41 supply staleness is unmonitored until it clears.

```sh
journalctl -u data-freshness.service --since '2 hours ago' | grep 'supply_flows probe failed'
curl -sS -f --max-time 15 http://localhost:8123/ --data-binary 'SELECT 1'
```

Restore ClickHouse HTTP on `:8123`; the pair returns on the next 15-min tick and the alert clears.

## stellarindex_data_freshness_probe_frozen

Trips: `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/data_freshness.prom"} > 2700`, `for: 15m`. The file's mtime is > 45 m old (timer runs every 15 min) but the series are still PRESENT: a run that dies before its atomic swap leaves the last good file in place and node_exporter re-serves it verbatim, which `watchdog_silent`'s `absent_over_time` cannot see. Same pattern as `stellarindex_config_assertions_stale` ([config-assertion-failed](config-assertion-failed.md)). Do not trust any `stellarindex_data_freshness_stale` reading as current until cleared.

```sh
ls -la --time-style=full-iso /var/lib/node_exporter/textfile_collector/data_freshness.prom
curl -s localhost:9100/metrics | grep -E 'node_textfile_(mtime_seconds|scrape_error)'
systemctl status data-freshness.service data-freshness.timer
```

Then fix as in [watchdog_silent](#stellarindex_data_freshness_watchdog_silent).

## Served-value truth (verify-served-values)

Shared context for the five alerts below. `stellarindex-ops verify-served-values` reconciles a curated set of served values against INDEPENDENT sources (SDF lumen API for XLM supply, Stellar Expert for classic-asset supply) and writes textfile gauges `stellarindex_served_value_{ok,rel_err,skipped,last_run_unix}` plus `stellarindex_sdf_reserve_list_drift{kind}`. Runs from `verify-served-values.timer` (daily 06:20 UTC, after the supply chain refreshed); units are ansible-managed and installed only where ground truth is pubnet. These alerts mean a customer-visible NUMBER is wrong (or unverifiable) while availability is green: credibility, not uptime; investigation-bound. Scope: prices are NOT checked here (the divergence worker, [price-divergence](divergence.md)); lake<>served row counts are NOT checked here (compute-completeness).

Investigate:

```sh
# Reproduce with full detail (read-only, run anywhere):
stellarindex-ops verify-served-values -api https://api.stellarindex.io

# What do we serve, on what basis?
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data | {circulating_supply, total_supply, max_supply, supply_basis}'

# The independent sources:
curl -s https://dashboard.stellar.org/api/v3/lumens | jq .
curl -s 'https://api.stellar.expert/explorer/public/asset/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN' | jq '{supply}'

# Supply pipeline state (r1):
sudo -u postgres psql -d stellarindex -c "SELECT asset, basis, max(time) FROM asset_supply_history GROUP BY 1,2 ORDER BY 3 DESC LIMIT 10"
```

Then `docs/architecture/supply-pipeline.md` for which algorithm (1 XLM / 2 classic / 3 SEP-41) derives the failing value. If an alert fires with the series ABSENT rather than old, check `systemctl status verify-served-values.timer` first. `verify-served-values.service` is named in `scripts/ci/unit-failed-dedicated.baseline` and excluded from the catch-all failed-unit rule: its exit code is a VERDICT and the other rules deliberately wait 26 h on those verdicts. Standing drift on a flagship value (XLM/USDC) that resists a day of investigation: raise with the maintainer (may need upstream SDF/SE/Circle methodology confirmation). Related: [adr-0041](../../adr/0041-ingest-durability-semantics.md) (durability vs data-truth split).

## stellarindex_served_value_drift

Trips: `stellarindex_served_value_ok == 0`, `for: 26h` (two consecutive daily runs). Label `check`. A served value sits outside its tolerance vs its independent source.

Causes: a supply-derivation basis wrong or partially populated (standing cases: `xlm_circulating_supply` reads `xlm_total_only` until the operator sets `sdf_reserve_accounts`; `usdc_total_supply` under-reads vs Stellar Expert); a backfill/observer gap left supply hypertables incomplete; the GROUND TRUTH changed methodology (SE counts locked amounts, SDF changes basis), so verify windows/bases match before "fixing" our side.

Fix: no cache to flush. Fix the derivation (or its config), or document the differing basis honestly on the wire (`supply_basis`). Never widen a tolerance to silence the alert without a written methodology justification in the check's `note`.

## stellarindex_served_value_check_stale

Trips:

```
(time() - stellarindex_served_value_last_run_unix) > 172800
or
absent_over_time(stellarindex_served_value_last_run_unix[2d])
```

`for: 1h`. The harness has not written its textfile in 48 h: timer dead, run crashing, every run skipping every check (`last_run_unix` advances only on a run that reached a verdict), or the timer never installed (hence the `absent_over_time` arm: `time()` minus an absent vector is empty, not large). Data-truth reconciliation is dark. Start with `systemctl status verify-served-values.timer` on the archival node.

## stellarindex_served_value_persistently_skipped

Trips: `stellarindex_served_value_skipped == 1`, `for: 26h`. Label `check`. The check was SKIPPED on every run for 2 days because its independent truth source (SDF lumen API / Stellar Expert / the stellar/dashboard source behind `sdf_reserve_list`) is unreachable. Availability, not drift: `served_value_drift` (ok==0) and `check_stale` (run still completes) both stay quiet, and a skipped check emits no `served_value_ok`, so without this alert a persistent truth-source break (URL/schema change) leaves the value UNVERIFIED behind a green gate forever. Check the source is reachable and its response shape; the check resumes once truth returns.

## stellarindex_sdf_reserve_list_drift

Trips: `stellarindex_sdf_reserve_list_drift > 0`, `for: 26h`. Label `kind`. `supply.sdf_reserve_accounts` differs from the reserve list SDF publishes on two consecutive daily runs. `kind="missing"`: SDF publishes it, we do not exclude it (we OVER-state circulating). `kind="extra"`: we exclude it, SDF no longer publishes it (we UNDER-state). The `xlm_circulating_supply` value check (2% tolerance on ~700M XLM, to ride out residuals such as the fee pool) can stay green because one account is under 2%; this check exists for that. It diffs the configured set (reading the node's `/etc/stellarindex.toml` via `-config`) against the list.

Source: NOT the dashboard API (`/api/lumens`, `/api/v2/lumens`, `/api/v3/lumens`, `/api/v3/lumens/all` expose sums only). It is the `accounts` table plus the `networkUpgradeReserveAccount` constant in <https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js> (what `noncirculatingSupply()` subtracts, fee pool aside). The burn address (`voidAccount`) is subtracted from TOTAL, not circulating, and is not in the set. SDF retires a row by commenting it out; the parser skips comments, surfacing a retirement as `extra`.

Gauges are emitted only when both sides were read and diffed. A dark or reshaped source emits `stellarindex_served_value_skipped{check="sdf_reserve_list"}=1` and NO drift gauge (ticketed by `_persistently_skipped` after two runs). Reshaped includes ONE table row outside the `key: "G…"` grammar (nested object, spread, computed value): the parse fails whole. A diff that would retire more than three configured accounts at once is refused the same way (its journal line still names them), as likelier a moved table than a real retirement. An unreadable config file is OUR side and fails the run (`_unit_failed` after two runs).

```sh
# The verdict, with account ids (the journal keeps the last runs):
journalctl -u verify-served-values -n 20 | grep sdf_reserve_list

# Re-run by hand on r1 against the node's own config:
sudo -u stellarindex stellarindex-ops verify-served-values -api http://127.0.0.1:3000 -config /etc/stellarindex.toml

# The two sides, side by side (published = table rows not commented
# out, plus the upgrade reserve; configured = the one-line TOML array):
SRC=https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js
{ curl -s "$SRC" | sed -n '/const accounts = {/,/^};/p' | grep -v '^ *//' ;
  curl -s "$SRC" | grep -A1 'networkUpgradeReserveAccount =' ; } \
  | grep -o 'G[A-Z2-7]\{55\}' | sort -u > /tmp/published
grep '^sdf_reserve_accounts' /etc/stellarindex.toml | grep -o 'G[A-Z2-7]\{55\}' | sort -u > /tmp/configured
comm -3 /tmp/published /tmp/configured   # col 1 = missing, col 2 = extra
```

Fix is config, not code, but only for an account the upstream diff actually changed. First find the stellar/dashboard commit that edited `accounts` and confirm every `extra` account was commented out or deleted and every `missing` one added. An account still a live upstream row is a parser defect: file it and leave config alone (deleting a reserve account adds its whole balance to circulating supply). A genuine edit is a methodology change worth a CHANGELOG line. Then update BOTH lists in `configs/ansible/roles/archival-node/defaults/main.yml` (`stellarindex_sdf_reserve_accounts` and the paired `stellarindex_reserve_balances_stroops`: the supply writer refuses to start with an account missing from the balance map), re-render `/etc/stellarindex.toml`, restart the supply writer. The next daily run clears the gauge. Never widen the value check's tolerance: it is not what fired.

## stellarindex_served_value_unit_failed

Trips:

```
node_systemd_unit_state{
  name="verify-served-values.service",
  state="failed"
} == 1
and
node_systemd_unit_state{
  name="verify-served-values.service",
  state="failed"
} offset 25h == 1
```

`for: 1h`. The unit failed on two consecutive daily runs. The tool exits non-zero on three conditions: a DRIFTED check (value or list; `served_value_drift` names it, `sdf_reserve_list_drift` is the list check's own ticket), an all-skipped run where every served-VALUE truth source was dark (a verified reserve-list check does not count; `served_value_persistently_skipped` names it), and a crash before the textfile is written. Only the third is unique to this ticket (`check_stale` cannot report a crash until `last_run_unix` is 48 h old). It is deliberately not the catch-all `stellarindex_systemd_unit_failed`, which would open 15 min into a third-party outage. If neither companion alert is firing, treat it as the crash case:

```sh
systemctl status verify-served-values
journalctl -u verify-served-values --since -50h
```

## stellarindex_serving_insert_frozen

Rule file: `serving-freshness.yml` (both trees). Trips: `(time() - max(stellarindex_source_last_insert_unix)) > 1800`, `for: 10m`. NOT ONE source has landed a successful row (gauge set in `internal/storage/timescale/trades.go` on every successful insert) in 30 min: the served tier (trades, oracle updates, protocol flows) is frozen and `/v1` responses go stale across the board. Fires even while source EVENTS flow, where the events-side `stellarindex_ingestion_all_sources_stopped` stays quiet (stuck-cursor / replay-loop: events match but every insert ON-CONFLICT-noops or fails). Check Timescale write health and the trade-insert backpressure metric first (`stellarindex_trade_insert_retries_total`; alert `stellarindex_ingestion_trade_insert_backpressure`).

## stellarindex_serving_insert_absent

Rule file: `serving-freshness.yml` (both trees). Trips: `absent(stellarindex_source_last_insert_unix)`, `for: 15m`. The series has no scrape for 15+ min: indexer down, metric dropped, or no source has landed a row since boot. Every `time() - stellarindex_source_last_insert_unix` staleness alert is a no-op while it is absent; this is the backstop against freshness monitoring going blind.

## Retiring a source

A source retired on purpose keeps its history and so alarms forever on [stellarindex_data_source_stale](#stellarindex_data_source_stale) (fail-closed by design: the script cannot tell a retired feed from a dead one, so it never resolves on its own for either). There is no exclusion list or config flag. In the SAME change that retires the source, pick one:

1. Delete the source's rows from the table the domain reads (`oracle_updates` for `oracle`, `fx_quotes` for `fx`, `asset_supply_history` for `supply`), so it drops out of the universe the watchdog enumerates. Irreversible; right when the integration is gone for good and its history has no further use.
2. Exclude it in the SQL: add it to the domain's `WHERE` clause in `data-freshness.sh` (a script edit). Keeps the history; a revert of that one-line exclusion silently re-arms the alarm.

Never silence the alert at Alertmanager instead: a blanket silence on `stellarindex_data_source_stale` hides every OTHER source's real outage (the watchdog alerts per `{domain, source}` precisely so one dead feed never costs visibility into the rest). A declared retired-source list the script reads is the durable form; not built yet (#1347).

## stellarindex_assets_popular_priceless

_Source page `data-freshness.md#stellarindex_assets_popular_priceless`: status living, severity P3, last verified 2026-08-28._

Covers both tripwire alerts:

- `stellarindex_assets_popular_priceless` — a real coverage gap exists.
- `stellarindex_priceless_coverage_check_stale` — the tripwire itself
  stopped sweeping (it is blind, so a gap would go unseen).

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_assets_popular_priceless` / `stellarindex_priceless_coverage_check_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/pricing-coverage.yml` |
| Emitted by | `internal/pricelesscoverage` (aggregator sweep, 10 min) |
| Typical MTTR | 30–120 min (usually adding a missing USD quote-path) |
| Impact | A genuinely-traded asset renders priceless on `/v1/assets` and every downstream surface. No wrong price is served (fail-closed) — the gap is a MISSING price, not a bad one. |

### Background — what "popular + priceless + not withheld" means

Each sweep the aggregator asks, per asset, whether ALL of the following
hold (the classifier is `internal/pricelesscoverage.popularPriceless`):

1. **Priceless** — no servable USD/XLM-proxy price
   (`prices_1m` has no non-null VWAP against USDC / its SAC / `fiat:USD`
   / `native` / the XLM SAC in the last 24 h), AND no `asset_price_snapshot`
   row younger than the listing's staleness bound
   (`assetPriceSnapshotMaxAge`, 15 min). A fresh snapshot row is priced by
   definition: it is exactly what `/v1/assets` serves, whatever the quote
   floors above say.
2. **Not withheld** — the serving substance gate does not withhold its
   USD price. The tripwire asks the gate itself
   (`pricingguard.AssetSubstanceVerdict`, the verdict the `/v1/assets`
   listing applies): all three floors (volume, distinct minutes, span)
   over the alias union, against XLM, `fiat:USD` and each declared USD
   peg. A withheld market's pricelessness is expected and does NOT
   count; an asset the gate could not measure DOES count.
3. **Not wash** — the busiest single unordered `(maker, taker)` account
   pair owns **< 90 %** of its 7 d priced volume. A volume-painting wash
   farm (the reported scam AUD: ~108/109 of its trades one wallet pair)
   contributes NO market-character volume, so it can never be "popular".
4. **Popular by market-character volume** — 7 d priced volume **> $10k**
   OR 7 d trades **> 5,000**.

The gauge is the COUNT of assets meeting all four. `> 0` for 1 h+ pages.

> Why the market-character floor matters: a raw-volume floor would let
> every wash farm self-select into this alert. The concentration filter
> is what keeps the scam AUD (huge raw volume, one wallet pair) SILENT
> while still catching a genuinely-traded asset that lost its price path.

### Quick diagnosis (≤ 5 min)

```sh
# 1) How many assets, and is the sweep fresh?
curl -fs http://localhost:9465/metrics \
  | grep -E '^stellarindex_(assets_popular_priceless|priceless_coverage_check_)'

# 2) WHICH assets — the worker logs each firing asset every sweep.
journalctl -u stellarindex-aggregator -n 500 \
  | grep 'priceless-popular coverage gap'
#   -> asset_id, volume_7d_usd, trades_7d, top_account_pair_share
```

Each `priceless-popular coverage gap` warn line names the `asset_id` and
its signals. Pick one and reproduce:

```sh
# Does /v1/assets/{asset_id} really serve no price_usd?
curl -fs "https://api.stellarindex.io/v1/assets/<asset_id>" | jq '.data.price_usd'
# What quotes does it actually trade against? (the missing-path clue)
# BOTH stored directions — a pair is written as (A,B) AND (B,A), each
# holding only PART of the market. Grouping on base_asset alone
# under-reports the true bucket count by roughly half and will send you
# chasing a thin-market theory that is not real (2026-08-27: CBIJ/XLM
# reads 13 buckets one way, 14 the other, 27 unioned — floor is 20).
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT CASE WHEN base_asset = '<asset_id>' THEN quote_asset ELSE base_asset END AS counterparty,
          count(*) AS buckets, sum(trade_count) AS trades, max(bucket) AS last_seen
     FROM prices_1m
    WHERE (base_asset = '<asset_id>' OR quote_asset = '<asset_id>')
      AND bucket >= now() - INTERVAL '24 hours'
    GROUP BY 1 ORDER BY 2 DESC;"

# Is the asset in a catalogue spine? Classic assets via classic_assets;
# Soroban-native contracts via discovered_assets + an asset_volume_24h
# row (the listing AND detail spines share that bound since 2026-08-28).
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT (SELECT count(*) FROM classic_assets   WHERE asset_id = '<asset_id>') AS classic,
          (SELECT count(*) FROM discovered_assets WHERE contract_id = '<asset_id>') AS discovered,
          (SELECT count(*) FROM asset_volume_24h  WHERE asset_id = '<asset_id>') AS vol_rollup;"

# Which DIRECTION is the XLM leg stored in? Only (CAS3J…/native, <id>)
# rows == the SAC-as-base class (see the decision tree).
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT base_asset, quote_asset, count(*) AS buckets, max(bucket) AS last_seen
     FROM prices_1m
    WHERE (base_asset = '<asset_id>' OR quote_asset = '<asset_id>')
      AND bucket >= now() - INTERVAL '24 hours'
    GROUP BY 1, 2 ORDER BY 3 DESC;"
```

Note the gate itself is direction-safe — `Store.PairMarketSubstance`
already unions both directions and every alias spelling of each leg, and
de-dupes with `GROUP BY bucket`. It is
the *ad-hoc diagnosis query* that misleads, not the production measurement.

### Decision tree — `stellarindex_assets_popular_priceless`

| Finding | Likely cause | Mitigation |
| ------- | ------------ | ---------- |
| Asset trades only against a stablecoin/quote NOT in the USD-proxy set | Missing USD-proxy bridge (the AUDD/EURC class — PR #152 added USDC/SAC) | Extend the proxy set in `listAssetsBaseSelect` + `coverageQuoteProxies` (keep them in lockstep) and re-derive |
| Asset trades only against another classic asset with no USD path | No triangulation route to USD | Confirm the intermediate has a USD price; add the pair to the chain if warranted |
| Asset is a SAC form of a classic that IS priced | Alias fold gap | Confirm `[supply].sac_wrappers` maps the SAC; the alias registry should fold it (task #28 Part A) |
| Asset is genuinely a scam we should not price | It should be labelled/withheld, not surfaced here | Add it to the scam directory / withhold path so it stops counting |
| Asset is **Soroban-native** (56-char `C…` contract, no classic twin) and `classic_assets` has no row for it | Both catalogue spines now UNION `discovered_assets` (bounded by `asset_volume_24h`), so a TRADED contract asset has a catalogue row. If it still has none, it has no 24h volume rollup row — check `asset_volume_24h` and the `assetvolrollup` worker. The substance gate is NOT the blocker — it runs and *allows* the asset, which is why `price_usd` is null with **no withheld reason** | Confirm the rollup row exists; then work the direction row below. Do **not** paper over it by recording a synthetic withheld verdict — that converts a real coverage gap into a silent one, which is precisely what this alert exists to catch |
| Asset's XLM market is stored with **XLM (native or the SAC `CAS3J…`) as BASE** — `SELECT base_asset, quote_asset, count(*) FROM prices_1m WHERE (base_asset = '<id>' OR quote_asset = '<id>') AND bucket >= now() - INTERVAL '24 hours' GROUP BY 1,2` shows only `(CAS3J…, <id>)` rows | **Direction gap (fixed 2026-08-28).** Sources that write SWAP direction (aquarius: base = `token_in`, no `canonical.Orient`) store a token bought with XLM as `(XLM-SAC, token)`. Until 2026-08-28 every price path read the XLM leg base-side only (`base_asset = X AND quote_asset IN (native, SAC)`), so that market was invisible to the catalogue `asset_vs_xlm*` CTEs, to `TransitiveUSDPrice.hop_usd`, and to the tripwire's `priced_direct` — while the volume path read both directions, which is why the asset had $730k/7d and no price (r1, `CBIJ…`/`CAUP7…`) | Every read path now has an inverted arm (base-side preferred). If this fires again on a SAC-as-base asset, one of the four lists/arms has drifted — `TestProxyQuoteLists_Lockstep` and `TestXLMSacAsBase_PriceableThroughEveryPath` are the guards; run them first |

**Soroban assets: SAC wrapper vs Soroban-native.** These behave completely
differently and the distinction is the first thing to establish:

- A **SAC wrapper** of a classic asset (AQUA, SHX, EURC, BTC, XRP, PYUSD,
  sUSD, BLND, CETES, VELO…) is folded onto its classic form by the alias
  registry and prices normally. Nothing to do.
- A **Soroban-native** asset — a contract with no classic counterpart — has
  no `classic_assets` row and so no price path at all.

Measured 2026-08-27: of the 15 Soroban assets over $1k/24h, **13 were SAC
wrappers (all correctly priced)** and exactly **2 were Soroban-native and
unpriced** — `CBIJ…` ($19k/24h) and `CAUP7…` ($9.4k/24h). The gap is
narrow, but it is a genuine capability gap, not a tuning problem.

Note also that a Soroban-native asset may only be reachable through
*another* Soroban-native asset: `CAUP7` trades against nothing but `CBIJ`,
so pricing it needs a **transitive hop** (`CAUP7/CBIJ × CBIJ_usd`), which
`Store.TransitiveUSDPrice` provides (one hop, both legs substance-gated
by the API). The hop's own USD price is resolved: hop IS XLM (either
identity) → `xlm_usd`; else direct USD proxy; else base-side XLM; else
the INVERTED XLM market. The multi-hop graph router (`MaxHops=3`) only
operates over `cfg.Pairs`, a ~10-pair operator allow-list that does not
serve the long tail.

**Keep the proxy lists AND the direction arms in lockstep.** Four places
decide "what is a proxy": `coverageQuoteProxies` (tripwire — composed
from the resolver's `usdProxyQuotes` + `xlmQuotes`), and the literal
IN-lists in `listAssetsBaseSelect` + `getAssetBySlugSQL`. Each XLM-leg
CTE has a base-side arm and an inverted arm. `TestProxyQuoteLists_Lockstep`
(`internal/storage/timescale/proxy_lockstep_test.go`) fails if any of
them drift.

### Decision tree — `stellarindex_priceless_coverage_check_stale`

| Finding | Likely cause | Mitigation |
| ------- | ------------ | ---------- |
| `candidate read failed` warns in the aggregator log | Postgres unreachable / query error | Restore Postgres reachability; sweep resumes next tick |
| No `priceless-popular coverage tripwire: wired` at startup | Worker not started | Confirm the aggregator build + restart; check for a panic in `worker.Recover(logger, "priceless-coverage")` |
| Sweep slow (full-catalogue scan) | Trades hypertable pressure | Check DB load; the scan is 24 h/7 d windowed and should be seconds |

### Mitigation (≤ 120 min)

- [ ] Identify the firing asset(s) from the warn logs.
- [ ] Reproduce the missing `price_usd` and inspect the asset's quote mix.
- [ ] Apply the appropriate fix from the decision tree (usually a
      missing USD-proxy quote-path). Keep `coverageQuoteProxies`
      (`internal/storage/timescale/priceless_coverage.go`) in lockstep
      with the catalogue's `direct_usd` / `asset_vs_xlm` quote set.
- [ ] Verify `stellarindex_assets_popular_priceless` returns to 0 on the
      next sweep; the alert auto-resolves after 1 h.

### Known false-positive patterns

- **Just-listed asset mid-pricing**: an asset that crossed the
  popularity floor minutes ago, before its first price bucket
  materialised. The `for: 1h` gate masks this.
- **Process restart**: `last_success_unix` reads its pre-sweep 0 until
  the first sweep completes (seconds after start). The staleness
  `for: 30m` gate masks this.

### Changelog

- 2026-08-25 — initial draft alongside the priceless-popular tripwire
  (task #28 Part B).
- 2026-08-28 — root cause of the `CBIJ…`/`CAUP7…` firing found and fixed:
  the XLM leg was stored with the **XLM SAC as BASE** (aquarius writes
  swap direction) and every price path read it base-side only, while
  the volume path read both directions. Added the direction row, the
  direction query, and the lockstep note; the Soroban-native row now
  describes the shared `discovered_assets` spine rather than a
  structural impossibility.
- 2026-08-27 — added the **Soroban-native** decision-tree row after the
  `CAUP7…` firing was misdiagnosed three times (as a routing gap, then a
  thin-market/`MinBuckets` problem, then a pair-direction bug). None were
  correct: `classic_assets` holds no contract assets, so the price is
  never computed and the gate never withholds. Also corrected the quick-
  diagnosis quote-mix query to union both stored pair directions — the
  single-direction form under-reports buckets by ~half and is what
  produced the false thin-market diagnosis.


## curated-rwa-sync

**Runbook — curated-rwa-sync**

_Source page `data-freshness.md#curated-rwa-sync`: status ratified, severity P3, last verified 2026-10-06._

Alerts: `stellarindex_curated_rwa_sync_stale`, `stellarindex_curated_rwa_sync_refused`, `stellarindex_curated_rwa_published_stale`. All P3 (`severity: ticket`), routed by `configs/alertmanager/alertmanager.r1.yml`. Rules: `deploy/monitoring/rules/curated-rwa-sync.yml` and `configs/prometheus/rules.r1/curated-rwa-sync.yml` (byte-identical; group `stellarindex.curated_rwa_sync`).

- **Impact:** a comparison panel degrades; the verified RWA surface is untouched. The reader is fail-closed: a row whose `synced_at` is older than 48 h is not served, so `/v1/rwa/assets` reports `curated.status: unavailable` and the explorer panel says so. 30 h (one missed daily run plus jitter) fires with 18 h to spare.
- **Scope:** r1 / pubnet (Dune's Stellar datasets are pubnet). The unit is installed on every network; a test net with no key stays `unwired` by design and these alerts do not fire there, because the metric is stamped on a dry run too. A test net whose unit never stamps will fire `_stale` (evaluated per instance): silence it in Alertmanager with an `instance` matcher if that net carries no key.
- **Metric source:** `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom`, written by `stellarindex-ops curated-rwa-sync` at the end of every run (dry or wet, success or refusal) from `curated-rwa-sync.timer` (daily 04:12 UTC, `RandomizedDelaySec=600`). A failed read or write stamps nothing.
- **Steady state:** `stellarindex_curated_rwa_sync_last_run_unix` advances daily; `_rows` ≈ 220–230 (monthly total series ~13 points plus per-subclass split ~210 rows); `_datapoints_read` a small constant (Dune metering for two result reads; a run never executes a query); `_executed_at_unix` advances about daily on the curator's own schedule.
- **Companion:** [api-smoke-stale](sla-probe.md#stellarindex_api_smoke_stale) (same textfile-stamp pattern, same diagnosis order); `docs/methodology/rwa-coverage-reconciliation.md` § *The curated arm*.

### At a glance

- [`stellarindex_curated_rwa_sync_stale`](#stellarindex_curated_rwa_sync_stale)
- [`stellarindex_curated_rwa_sync_refused`](#stellarindex_curated_rwa_sync_refused)
- [`stellarindex_curated_rwa_published_stale`](#stellarindex_curated_rwa_published_stale)

### Quick diagnosis

```sh
# 1. Timer scheduled? When did the unit last run?
ssh r1 'systemctl list-timers curated-rwa-sync.timer'
ssh r1 'systemctl show curated-rwa-sync.service -p Result,InactiveEnterTimestamp,ExecMainStartTimestamp'
# Type=oneshot: read Result WITH its timestamp. Empty InactiveEnterTimestamp = no run for Result to describe.

# 2. What did the last run say?
ssh r1 'journalctl -u curated-rwa-sync -n 40 --no-pager'

# 3. Textfile present and moving?
ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'
ssh r1 'cat /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'

# 4. Key present? (never print it)
ssh r1 'grep -c "^DUNE_API_KEY=." /etc/default/curated-rwa-sync'   # 1 = set, 0 = empty placeholder

# 5. Arm state per the API
curl -s https://api.stellarindex.io/v1/rwa/assets | jq '.curated | {status, assets, census, published: (.published | {total_usd, as_of, executed_at, gap_vs_verified_usd})}'
```

### stellarindex_curated_rwa_sync_stale

Trips (`for: 10m`):

```
(time() - stellarindex_curated_rwa_sync_last_run_unix) > (30 * 3600)
or
(
  absent_over_time(stellarindex_curated_rwa_sync_last_run_unix[30h])
  and on() (count_over_time(up{job="node_exporter"}[30h]) > 1700)
)
```

The absent arm is gated on Prometheus having watched 30 h of scrapes (~1,700 samples at 60 s is 28 h+), so a fresh rule load or Prometheus restart does not fire it. If it fires on a host that never stamped, the timer has not fired in 30 h: go to step 1. The two rule trees are identical.

Means: no run has stamped `last_run_unix` in 30 h, or none ever has (timer not scheduled, unit failing before it writes, Dune read failing, textfile dir not writable). At 48 h the reader empties the arm.

Triage, in order:

1. **Timer not listed, or `NEXT` is `n/a`:** the timer was not enabled (deploy skipped task 14, or host bootstrapped before the unit existed). `systemctl enable --now curated-rwa-sync.timer`; the ansible role is the source of record, so re-run the archival-node playbook if it drifts again.
2. **Journal `GET /api/v1/query/…/results: HTTP 4xx`:** Dune refused the read. `401`/`403`: key rotated or revoked; replace it (see `_refused` fix). `402`/`429`: credit allowance exhausted. A run reads two public query results, metered by datapoint (`datapoints_read`), never by execution, so this is almost always another consumer of the same key. Wait for the monthly reset or lower the cadence in `curated-rwa-sync.timer.j2`; the 48 h bound tolerates one missed day, not more. `404`: the curator deleted or privatised the query; find its successor at dune.com/stellar/rwas. The ids are constants in `internal/ops/ingest/curated_rwa_sync.go`.
3. **Journal `latest execution is QUERY_STATE_…, not completed`:** the curator's own scheduled run failed. Nothing to do here: the previous day's rows stay served until the 48 h bound and the next successful curator run clears it. If it persists past a day the dashboard is broken: say so on the page's issue, not in code.
4. **Journal `printed no usable row` / `printed N of the M rows it declared`:** the curator changed a column name/layout, or its paging broke. Rows are decoded STRICTLY (`duneMonthlyTotalRow`, `duneMonthlyBySubclassRow` in `internal/ops/ingest/curated_rwa_sync.go`); compare with the query's current columns on dune.com. A layout change is a code change with a test, not an ops fix.
5. **Journal `Read N rows; kept …` then a Postgres error:** curator read, cache write failed (DSN, pool, constraint, or `-timeout` expiring mid-commit). A failed write stamps nothing, so `last_run_unix` keeps the last committed run's time. Fix the database side, re-run the unit.
6. **Textfile written but `last_run_unix` frozen:** node_exporter is not scraping the directory (file permission, or collector flag missing). `ls -la` the file; check node_exporter args for `--collector.textfile.directory`.
7. **All healthy and still firing:** the r1 scrape target (node_exporter) is down; the alert is a symptom.

Clears on its own `for: 10m` after the next scrape of a fresh `last_run_unix`.

### stellarindex_curated_rwa_sync_refused

Trips:

```
stellarindex_curated_rwa_sync_refused == 1
```

`for: 2h`. Identical in both trees.

Means: the run exited clean without reading the curator because `DUNE_API_KEY` is empty in `/etc/default/curated-rwa-sync`. It still stamps `last_run_unix` (so `_stale` measures the timer, not the key). Expected state of a fresh install until an operator sets the key. Confirm: step 4 above prints `0`. The arm reads `unwired`/`unavailable` meanwhile.

Fix: the file is `root:root` mode `0600` (only systemd reads it, as PID 1; no group read unlike sibling `/etc/default/*` files). Set the key the way the role does so the next apply agrees with the host: set `vault_dune_api_key` in `inventory/r1.secrets.yml` (workstation, `configs/ansible/`), then

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
```

The role renders `DUNE_API_KEY={{ vault_dune_api_key }}` whenever the vault defines a non-empty value; with the variable undefined it installs the empty placeholder once and never overwrites a pasted value. Pasting by hand works, but the vault is the source of record: a hand-set key is replaced by the vault's on the first apply after the variable is defined. Then `systemctl start curated-rwa-sync.service` and re-read the textfile; the gauge clears on that run. The reader recognises the arm within its 10-minute cache TTL.

### stellarindex_curated_rwa_published_stale

Trips:

```
(time() - stellarindex_curated_rwa_sync_executed_at_unix) > (72 * 3600)
and
stellarindex_curated_rwa_sync_executed_at_unix > 0
```

`for: 1h`. Identical in both trees. `executed_at_unix` is 0 when a run read nothing; the `_refused`/`_stale` rules own that case.

Means: the sync is healthy (`last_run_unix` advances, arm recognised) but the CURATOR has stopped re-executing its public query, so the figure is frozen. Healthy worst case is ~48 h (daily curator read by daily sync, held a day), so 72 h is a full missed curator day beyond that. The API's `curated.published.executed_at` shows the same stamp. Past 48 h the API serves the block with `stale: true`; past 7 days it drops `curated.published` entirely (`curated.status` reads `unavailable` when no per-asset row is readable either). The split is a separate query: `curated.published.by_subclass_executed_at` is its own execution time, and a split older than 7 days is withheld while a fresher total is still served.

Fix: nothing on this side. Confirm on dune.com that the curator's query schedule is paused or broken.


## fx-history-missing

**FX history empty / `fx_quotes` table missing**

_Source page `data-freshness.md#fx-history-missing`: status living procedure, last verified 2026-08-29._


### At a glance

| Field | Value |
| ----- | ----- |
| Trigger | Customer report: "FX history is empty" / operator-noticed `history_1y: 0` on `/v1/assets/<fiat>`. No specific Prometheus alert fires today — surfaces as a recurring WARN in the API log. |
| Severity | P3 (data-quality, not data-loss) |
| Detected by | API log: `forex: fx_quotes persist failed ... pq: relation "fx_quotes" does not exist` |
| Typical MTTR | 5–15 min (one-shot operator action: apply migration + restart) |
| Impact | FX history endpoints serve `history_1y: 0` and `history_all: 0` for every ticker. `history_7d` populates normally because it reads from a different surface. The aggregator's stablecoin-fiat proxy is unaffected (uses `[trades].usd_pegged_classic_assets`, not `fx_quotes`). |

Companion to [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full) and
[`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked).
Different shape: a database migration that ships in the repo
(0028) but hasn't been applied to the deployment, so a feature
that depends on the new table fails silently at runtime.

This runbook captures the 2026-05-10 finding on r1 + the recovery
sequence so future operators don't re-investigate from
"FX history is empty for EUR" backwards. (Original 2026-05-10
investigation was against `/v1/currencies/EUR`, retired in
rc.48 — same data now flows through `/v1/assets/eur`; the
underlying `fx_quotes` table is the same and the runbook below
applies unchanged.)

### Signal

- `/v1/assets/eur` (or any other fiat ticker) returns
  `history_1y: 0` and `history_all: 0` on the wire while
  `history_7d` populates normally.
- API log shows recurring WARN every forex refresh tick:
  ```
  {"level":"WARN","msg":"forex: fx_quotes persist failed",
   "rows":810,
   "err":"timescale: InsertFXQuoteBatch ticker=\"AED\":
          pq: relation \"fx_quotes\" does not exist
          at position 2:15 (42P01)"}
  ```
- `psql -tA -c "SELECT to_regclass('public.fx_quotes')"` returns
  empty.
- `psql -tA -c "SELECT version FROM schema_migrations
  ORDER BY version DESC LIMIT 1"` returns a version below 28 —
  the general signal. ("Returns 27" was the specific 2026-05-10
  state on r1; HEAD's migrations run far past it — 0150 at the
  2026-08-29 re-verification — so any `version < 28` means 0028
  was never applied.)

### Why this happens

The `fx_quotes` hypertable was added in task #104
("Persistent fx_quotes hypertable + 10y backfill") via migration
0028. The migration ships in the repo at
`migrations/0028_create_fx_quotes.up.sql`. Two operator-side
steps make it live on a deployment:

1. **Copy the migration file** to the deployment's canonical
   migrations directory (`/usr/local/share/stellarindex/migrations/`
   on r1 — the dir the deploy playbook syncs + applies from; NOT
   `/var/lib/stellarindex/migrations/`, which is a stale unmanaged
   leftover).
2. **Apply it** via `stellarindex-migrate up`.

> Note: as of the `migrations_skip | bool` fix, `deploy.yml` syncs +
> applies pending migrations automatically before swapping binaries,
> so a normal `gh workflow run deploy.yml` deploy already runs this.
> The manual steps below are the fallback for an out-of-band fix.

Once the table exists, the forex worker (running inside
`stellarindex-api`) starts persisting on its next refresh tick,
so live data backfills forward as it arrives. The live worker
polls Massive (paid feed, `MASSIVE_API_KEY`) with a keyless ECB
daily-reference-rates fallback; Frankfurter is used by the
backfill script only. Historical depth needs the one-shot
`fx-history-backfill` script — see step 3.

### Triage (1 min)

```sh
# 1. Confirm the table is missing
sudo -u postgres psql -d stellarindex -tA -c "SELECT to_regclass('public.fx_quotes')"
# → empty line means missing

# 2. Confirm migration version
sudo -u postgres psql -d stellarindex -tA -c "SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1"
# → 27|f means migration 0028 hasn't been applied yet

# 3. Confirm the API log shows the symptom. The forex worker's
#    refresh cadence is HOURLY (`time.Hour`, wired in
#    cmd/stellarindex-api/main.go), so expect ONE WARN per hourly
#    tick — grep a window wide enough to catch at least one:
journalctl -u stellarindex-api --since "2 hours ago" -o cat | grep "fx_quotes persist failed" | tail -1
```

### Recovery (5 min)

#### 1. Copy the migration file

From your local checkout:

```sh
scp migrations/0028_create_fx_quotes.{up,down}.sql \
    root@<host>:/usr/local/share/stellarindex/migrations/
```

(R1 host: `136.243.90.96`. `/usr/local/share/stellarindex/migrations`
is the canonical path the deploy playbook syncs to with `delete:true`
and that `stellarindex-migrate` should read. `/var/lib/stellarindex/migrations`
is a stale unmanaged dir — don't use it.)

#### 2. Apply the migration

```sh
ssh root@<host> '
  set -e
  set -a; . /etc/default/stellarindex; set +a
  /usr/local/bin/stellarindex-migrate \
    -migrations /usr/local/share/stellarindex/migrations \
    -dsn "$STELLARINDEX_POSTGRES_DSN" \
    up
'
```

Expected output: `1/u create_fx_quotes` then exit 0.

The migration is forward-only, additive, and idempotent on
re-runs (the `create_hypertable` call uses `if_not_exists =>
TRUE`; the table itself doesn't but won't be re-attempted because
`schema_migrations.version` advances). Safe to apply on a live
deployment — no service restart needed; the forex worker picks
up the new table on its next refresh tick (hourly cadence — up
to 1 h away).

#### 3. Confirm the worker started persisting

```sh
# The refresh cadence is hourly, so post-fix confirmation can take
# up to 1 h — a zero count here only proves absence-of-failure once
# a tick has actually fired since the migration:
journalctl -u stellarindex-api --since "2 hours ago" -o cat \
  | grep -c "fx_quotes persist failed"
# → 0 once the next refresh tick fires (hourly cadence)

sudo -u postgres psql -d stellarindex -tA -c "SELECT count(*) FROM fx_quotes"
# → > 0 within ~1 h
```

#### 4. Backfill historical depth (slow path — separate step)

The forward-flow worker only writes the LATEST snapshot per
refresh tick — it doesn't go back in time. The 1y / all-time
fiat charts need historical data that the one-shot
`fx-history-backfill` binary fetches from the ECB-backed
Frankfurter API (frankfurter.dev) — free, no API key, ~32
currencies, daily granularity back to 1999-01-04.

```sh
# On the operator's workstation:
export DATABASE_URL=postgres://...:5432/stellarindex
go run ./scripts/ops/fx-history-backfill --years=25
```

No cost — Frankfurter is free (ECB reference rates,
maintained as a public utility). The script walks the window in
5-year chunks (one HTTP request per chunk) so a 25-year backfill
is ~6 requests total. Safe to interrupt and resume — the writer
upserts on `(ticker, bucket)` so re-running on the same range
is a no-op.

The script logs one line per chunk to stderr; on completion it
writes a final summary (total chunks, failed chunks, total rows,
elapsed). It exits non-zero if any chunk failed or the run was
interrupted before covering the whole window. Re-run the `--from`/`--to`
range of each `chunk failed` line until the script exits 0.

### Prevention

The 2026-05-10 finding exposed a process gap: a release that
adds a migration ships the binary changes via the deploy
workflow, but the migration files + `stellarindex-migrate up`
were operator-side actions not automated by the same workflow.

**Path 1 is DONE (F-1220):** the deploy workflow now syncs the
migrations directory and runs `stellarindex-migrate up` before
any binary swap, unless the operator passes the
`migrations_skip` input (`.github/workflows/deploy.yml` +
`configs/ansible/playbooks/deploy-binary.yml`). A normal
`gh workflow run deploy.yml` deploy cannot reproduce this
incident class anymore; the manual steps above remain only as
the out-of-band fallback.

**Still open — the startup-gate idea:** `stellarindex-api`'s
ready check could compare the binary's expected schema version
(computed at build time from the embedded migrations) against
`schema_migrations.version`; readyz returns 503 with a
diagnostic if they diverge. Doesn't auto-apply but would catch
any remaining out-of-band drift (e.g. a hand-copied binary)
instead of letting it silently fail at runtime.
TODO(maintainer): decide whether the startup gate is still worth it
post-F-1220, or close it as superseded.

### Changelog

- 2026-08-29 — re-verified against HEAD (Wave I). The forex
  worker's refresh cadence corrected from "~5 min" to HOURLY
  (`time.Hour`) in the triage grep, the persist-pickup note, and
  the post-fix confirmation windows; migration-version signal
  generalised to `version < 28` (27 was the 2026-05-10 snapshot;
  HEAD runs to 0150); Prevention path 1 marked DONE via F-1220
  (deploy workflow syncs migrations + runs `stellarindex-migrate
  up` pre-swap unless `migrations_skip`), leaving only the
  startup-gate idea open; noted the live worker polls Massive
  (`MASSIVE_API_KEY`) with keyless ECB fallback — Frankfurter is
  backfill-only.

## Related

- [Alerts catalogue](../alerts-catalog.md)

**`stellarindex_assets_popular_priceless`**

- `internal/pricelesscoverage/` — the tripwire worker + classifier.
- `internal/storage/timescale/priceless_coverage.go` — the candidate SQL.
- `internal/pricingguard/substance.go` — `AssetSubstanceVerdict`, the
  withheld verdict the tripwire asks.
- PR #152 (`assets:` USDC/SAC stablecoin-proxy bridge) — the class of fix
  a firing alert usually needs.
- `feat/scam-labels-and-volume-character` (PR #161) — the volume-character
  design the market-character filter mirrors.

**`curated-rwa-sync`**

- `internal/ops/ingest/curated_rwa_sync.go`: sync command (query ids, paging loop, strict row shapes, textfile writer).
- `internal/storage/timescale/rwa_curated_published.go`: reader with the 48 h recognition bound; `internal/api/v1/rwa_curated.go` turns an empty set into `curated.status: unavailable` and a full one into `published_totals`. (`rwa_curated_directory.go` is the per-asset reader; the first curator's per-asset tables are private, so it stays empty.)
- `configs/ansible/roles/archival-node/templates/systemd/curated-rwa-sync.service.j2`: unit, `EnvironmentFile`, `ReadWritePaths` grant for the textfile directory.
- `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`: the two tasks (vault render / first-install placeholder) owning `/etc/default/curated-rwa-sync`.

**`fx-history-missing`**

- [`postgres.md#stellarindex_timescale_disk_full`](postgres.md#stellarindex_timescale_disk_full) — different shape; the
  postgres-side disk-pressure surface.
- [`cache.md#stellarindex_redis_writes_blocked`](cache.md#stellarindex_redis_writes_blocked) —
  another silent-runtime-failure shape (Redis writes blocked).
