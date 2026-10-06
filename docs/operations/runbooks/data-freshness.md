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

- External API quota/auth (oracle `coingecko`, FX `massive`, `chainlink`): `429`/`401` in the poller logs = key exhausted/expired. Restore the paid key in `/etc/default/stellarindex` and restart the owning binary (CoinGecko Pro purchase: launch-todo P0-3).
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

## Related

- [Alerts catalogue](../alerts-catalog.md)
