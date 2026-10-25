---
title: Runbook — data freshness alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — data freshness

All alerts here are `severity: ticket` (P3), identical in `configs/prometheus/rules.r1/data-freshness.yml` and `deploy/monitoring/rules/data-freshness.yml`; the two serving alerts are identical in both `serving-freshness.yml` copies.

Most gauges come from `data-freshness.sh` (`configs/ansible/roles/archival-node/files/data-freshness.sh` -> `/usr/local/sbin/data-freshness.sh`), run by `data-freshness.timer` every 15 min, writing `/var/lib/node_exporter/textfile_collector/data_freshness.prom` atomically. Served-value gauges come from `verify-served-values`. If several alerts fire together, check the emitter first ([watchdog_silent](#stellarindex_data_freshness_watchdog_silent)).

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

Trips: `stellarindex_data_freshness_stale == 1`, `for: 1h`. The `trades` domain is suppressed while the source's `stellarindex_projector_lag_ledgers` is under 1000: a projected source whose writer sits at the tip is quiet, not stale. Age: `stellarindex_data_freshness_age_seconds{domain,source}`. Covers everything except on-chain trade/event gaps (`stellarindex_ingest_gap_detected` owns those). MTTR 10-60 min. The universe is every `{domain,source}` the table has EVER held, aged against its own newest row, so a dead source never leaves the alert and a source retired on purpose alarms forever ([Retiring a source](#retiring-a-source)). A new source with no rows emits no gauge; a domain-level probe (`supply`, `sep1`) with no rows reads stale.

Thresholds (from `data-freshness.sh`):

| `domain` | measured from | threshold | note |
| --- | --- | --- | --- |
| `oracle` (`ecb`, `tiingo`) | `oracle_updates.ingested_at` | **96 h** | daily feeds (ECB ~16:00 CET TARGET days); tolerates a weekend plus a holiday |
| `oracle` (`chainlink-stellar`) | `oracle_updates.ingested_at` | **26 h** | deviation or 24 h heartbeat |
| `oracle` (others) | `oracle_updates.ingested_at` | 3 h | reflector / redstone / band / coingecko; the retired `chainlink` is excluded |
| `fx` | `fx_quotes.bucket` | **76 h** | daily grain; mirrors `aggregate.composite_reference.fx_max_age_hours` (`scripts/ci/data-freshness-test.sh` pins the two together, change both) |
| `trades` (`phoenix`, `comet`) | `source_volume_1h.bucket` | **24 h** | sparse AMMs: max gap 8 h 28 m, p99 3 h 12 m |
| `trades` (others) | `source_volume_1h.bucket` | 4 h | |
| `supply` | `asset_supply_history.time` | 30 h | whole-table max; blind to a partial freeze, see [supply_assets_stale](#stellarindex_supply_assets_stale) |
| `verdict` | `completeness_snapshots.computed_at` | 36 h | ADR-0033 verdict liveness |
| `sep1` | `issuers.sep1_payload_fetched_at` | 48 h | issuer-metadata refresh |
| `sep41_supply` (`supply_flows`) | ClickHouse `stellar.supply_flows.ingested_at` | 1 h | only non-Postgres probe; its freshness IS the served SEP-41 supply's |

FX `frankfurter-historical` is not watched (one-off backfill label). `coingecko` reads stale until the CoinGecko Pro key lands.

Check:

```sh
# Which domain/source + how stale (seconds):
curl -s localhost:9100/metrics | grep 'stellarindex_data_freshness_age_seconds' | sort -t' ' -k2 -n | tail
# The writer's recent logs (oracle/fx pollers run in the indexer or api):
journalctl -u stellarindex-indexer -u stellarindex-api --since '2 hours ago' | grep -iE "<source>|poller error|429|401|quota"
```

Fix, by domain:

1. External API (`coingecko`, FX `massive`): `429`/`401` in the poller logs = key exhausted or expired; restore the key in `/etc/default/stellarindex`, restart the owning binary.
2. `verdict`: `systemctl status compute-completeness.service`.
3. `sep1`: probe reads `max(sep1_payload_fetched_at)` (success only), so it fires if `sep1-refresh.timer` is not running OR every fetch fails (our DNS/egress). `systemctl status sep1-refresh.service`; `journalctl -u sep1-refresh --since -24h | grep -E 'SYSTEMIC|FAIL'` (the job exits non-zero at a 90% failure rate over 50+ domains, tripping `stellarindex_systemd_unit_failed`).
4. `trades`: connector/dispatcher stopped, check the indexer. For `phoenix`/`comet` first confirm it is not a quiet market (query the lake for swap events on a known pool).
5. `sep41_supply`: live `supply_flows` write into ClickHouse stalled. `curl -s http://localhost:8123/ --data-binary "SELECT 1"`, then the `ch-supply` gap-fill timer. If the whole `data_freshness.prom` looks frozen, go to [probe_frozen](#stellarindex_data_freshness_probe_frozen).

## stellarindex_supply_assets_stale

Trips: `stellarindex_supply_assets_stale > 0`, `for: 2h`. Value = watched assets with no supply snapshot for over 30 h (worst age: `stellarindex_supply_asset_max_age_seconds`); the watched set is assets with rows in the 30 days before the newest supply row. Those assets serve a FROZEN supply (and market cap/FDV); the whole-table `supply` gauge usually stays green. MTTR 30 min-hours.

Check:

```sql
SELECT asset_key, now()-max(time) AS age
FROM asset_supply_history
GROUP BY asset_key ORDER BY age DESC LIMIT 20;
```

```sh
curl -s http://localhost:9465/metrics | grep supply_refresh_duration_seconds_count
```

(`stellarindex_aggregator_supply_refresh_duration_seconds`, outcomes as deltas.) `stale_component` dominant: the freshness GATE refuses, compare producer progress below. `no_ledger` / `compute_error`: [supply-refresh-error-dominant](supply.md#stellarindex_aggregator_supply_refresh_error_dominant). `ok` / `dormant` advancing: rows ARE landing, re-check the scope query. `missing_baseline` on a classic asset's SAC wrapper: opening balance never seeded, supply withheld.

```sql
-- producer watermarks (should track tip)
SELECT max(ledger) FROM trustline_observations;
SELECT max(ledger) FROM claimable_observations;
SELECT max(ledger) FROM sac_balance_observations;
SELECT max(ledger) FROM lp_reserve_observations;
SELECT max(ledger) FROM sep41_supply_events;
SELECT max(ledger) FROM account_observations;
```

Likely causes:

- Watermarks track tip but the assets are quiet in one component: the data is FINE and a gate misreads quiet as stale. Do NOT loosen the dormancy horizon. Keep per-contract probes index-bounded (an unbounded `GROUP BY` over `sep41_supply_events` ran 11 min).
- A watermark genuinely behind: real stall in the indexer/projector; check for a pending projector tail rebuild.
- An unseeded SAC wrapper (`missing_baseline`).

Fix:

1. Gate fault: the anchor must be the producer's watermark, not per-entity last activity (classic `e21fa3d0`, SEP-41 `3f26b8db`, XLM `aa0d08c2`); a regression likely means a new supply path with the old shape.
2. `missing_baseline`: the aggregator auto-seeds on first refresh (retry 10 min; failures log `genesis baseline auto-seed failed`, usually an unreachable lake). Otherwise preview, then seed by hand (idempotent; nothing written without `-write`); on r1 run as the service user (`systemd-run --wait --pipe -p User=stellarindex -p EnvironmentFile=/etc/default/stellarindex …`):

   ```sh
   stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml
   stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -write
   ```

3. Stalled producer: repair the writer, then catch up (`projector-replay` for projected sources).
4. Verify with `scripts/ops/reconcile-supply-vs-horizon.sh` (every classic asset against Horizon's full component sum); count returns to 0.

## stellarindex_completeness_incomplete

Trips: `stellarindex_completeness_incomplete == 1`, `for: 1h`, from the latest `completeness_snapshots` row per source (ADR-0033 `complete=false`; the `recognition` row is excluded). The served tier no longer reconciles to the certified ClickHouse lake for that source; the verdict self-seeds factory children, so this is a real served<>lake gap. Read `detail` first: it names the per-target delta and window, or says PENDING.

Check:

```sh
# The exact delta + window the verdict recorded:
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, complete, watermark_ledger, detail FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s WHERE NOT complete;"
# Re-run that source to confirm it persists (off the serving DB, -ch):
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -source <X> -from <recent> -write
```

Causes: dropped rows (a decoder bug fixed forward-only, e.g. SEP-41 CAP-67 loss), a missed projection window, a retention/PK artifact.

### Fix: real gap

Re-derive from the lake, then re-verify.

- Soroban projected sources; `-config`, `-source`, `-from` REQUIRED, `-write` to actually rewind (dry run is default); the source name is the projector registry's:

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
- Re-run `compute-completeness -ch -source <X> -write`; the gauge clears on `complete=true`. Run chunked and off-peak (SDEX re-derives blow ClickHouse's per-query memory over large windows).

The manual re-verify only clears the gauge sooner: the nightly `-pass` publishes `complete=true` itself once the repair earns it. A from-genesis re-verify can outlast the pass (`-timeout 170m`; sdex from 61249957 is ~4.8 h), so the pass runs those sources LAST. For a one-off larger budget set `PASS_TIMEOUT` (e.g. `PASS_TIMEOUT=300m`) in `/etc/default/compute-completeness` AND raise `TimeoutStartSec` in `compute-completeness.service` (54000 s = 43200 s lock wait + 180 min) by as much, or systemd kills the pass first.

### Pending: deferred dirty window (sdex, sep41_transfers)

`detail` reading `dirty window [F,T] PENDING this source's dedicated weekly compute-completeness timer` = nothing found wrong, PENDING RE-VERIFICATION: a `backfill -write`, `ch-rebuild` or `projector-replay` rewrote rows from `F`, and re-checking `F` to tip is too wide for the nightly pass, which withholds `complete`. Do NOT re-derive. Re-prove with the weekly budget:

```sh
sudo systemctl start compute-completeness-sdex.service   # or compute-completeness-sep41.service
# equivalent: run-compute-completeness.sh -source <X> -timeout 360m
```

A window within a day of ledgers is cleared by the next nightly run. Exception: after `backfill -write [F,T]` on a projected source (not sdex, band or soroswap-router), raw events landed behind the projector cursor and nothing re-projects them; run `stellarindex-ops projector-replay -config PATH -source <X> -from F`.

### Stale: projection evidence older than 10 d 6 h, or unknown

`/v1/coverage` sets `flags.stale` when a `projection_ok` source has `projection_evidenced_at` older than `MaxProjectionCarryAge` (7 d) plus three 26 h audit periods, or `null` (`computed_at` cannot show this; the carry detail names the proof time).

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, projection_evidenced_at, computed_at FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s \
  WHERE projection_ok ORDER BY projection_evidenced_at NULLS FIRST;"
```

- Event sources clear on their own: each `-pass` re-proves at most three expired sources from genesis per night (log `re-proving expired projection evidence from genesis this pass: …`); after migration 0201 expect ~`ceil(green sources / 3)` nights. Past that, check the pass's error for a deadline cut.
- `sdex` is re-proved by `compute-completeness-sdex.timer` (Sunday 18:47 UTC; ~4.8 h exceeds the pass's 170 min). It runs `-source sdex -timeout 360m` under the same `run-heavy-job.sh` job name (the nightly pass runs 05:30 UTC). If it failed or missed a week (`systemctl status compute-completeness-sdex`, `journalctl -u compute-completeness-sdex`), re-run off-peak, outside the 05:30 UTC window:

  ```sh
  sudo systemctl start --no-block compute-completeness-sdex.service
  ```

- `sep41_transfers` and `sep41_supply` via `compute-completeness-sep41.timer` (Wednesday 18:47 UTC, `-source sep41_transfers -timeout 360m` then `-source sep41_supply -timeout 300m`; the KALE SAC overruns the pass's 45 min `-source-timeout`): `sudo systemctl start --no-block compute-completeness-sep41.service`.

Prefer the unit (it applies the driver's tip-100 margin). Do not add `-from`: a run starting above the served floor stamps no evidence.

Known non-issues: the `recognition` row is not a source (`stellarindex_recognition_unattributed_shapes`, not alerted); `complete=true` covers only the range walked (`stellarindex_completeness_watermark_lag_ledgers` is the companion).

Model: [ADR-0033](../../adr/0033-completeness-verification-model.md).

## stellarindex_source_recognition_failing

Trips: `stellarindex_recognition_ok == 0`, `for: 1h` (label `source`; `recognition_ok` column of `completeness_snapshots`, daily `compute-completeness`). A source we INDEX emitted a `(contract_id, topic_0_sym)` shape on a contract it OWNS that none of its decoders claim: events are silently dropped from the served table (they remain in the lake; replay recovers them once fixed) and its verdict is capped (`complete=false`, `recognition_ok: false` on public `/v1/coverage`). MTTR hours-days. Not `stellarindex_recognition_unattributed_shapes` (foreign protocols, no action).

Check:

```sh
# 1. Which source, and what does the verdict say?
curl -s https://api.stellarindex.io/v1/coverage \
  | python3 -c "import sys,json;[print(r['source'], r['recognition_ok'], r['detail'][:200]) for r in json.load(sys.stdin)['data']['sources'] if not r['recognition_ok']]"

# 2. Which shapes are unclaimed? Read-only; run under the wrapper.
/usr/local/sbin/run-heavy-job.sh ch-recognition \
  stellarindex-ops ch-recognition -config /etc/stellarindex.toml -top 60
```

Match the reported `contract_id`s against the source's registry (`protocol_contracts`, or its in-code curated set); a shape on the alerting source's contract is the one to fix.

Causes: the contract upgraded in place (live ingest sees only the current WASM; [contract-schema-evolution](../../architecture/ingest-pipeline.md#contract-schema-evolution)), or a new event kind with no decoder arm.

Fix (no quick mitigation; never silence it, `/v1/coverage` is public):

1. Size the blast radius (events, ledger range, contract).
2. Add the decoder arm; gate any backfill behind a per-WASM-hash audit if the range predates the current WASM ([wasm-audits](../wasm-audits/README.md)).
3. `projector-replay` the range ([adr-0033-data-recovery](../adr-0033-data-recovery.md); for gated sources `backfill` writes nothing and exits 0).

A contract seeded between the recognition scan and the deploy of its decoder is briefly owned and unclaimed; it clears on the next daily run.

## stellarindex_twap_history_missing

Trips (`for: 2h`; label `view`, a `twap_*` or `prices_*` view):

```
stellarindex_twap_history_missing == 1
  or
stellarindex_cagg_history_missing == 1
```

The detector compares each view's oldest bar with the oldest `trades` row (or an armed retention floor); a deployment whose `prices_1m` has < 2 days of history is not judged. `trades` holds back-history but the view's oldest bar is far newer (or it is empty): it carries only the trailing window its refresh policy auto-fills (`twap_1h` 4h, `twap_1d` 7d), so the API serves NO bars for older ranges while recent bars read green. Cause: a migration changing a TWAP CAGG's SELECT must `DROP` + `CREATE` it `WITH NO DATA` (the `0081 -> 0115 -> 0126 -> 0147` pattern), or a replay rewrote base rows, and the manual re-materialization was skipped. The TWAP CAGGs are roll-ups over `prices_1m`. MTTR minutes per windowed refresh.

NEVER refresh a TWAP view with a NULL start. Migration 0156 puts a retention policy on `prices_1m`; every chunk it drops invalidates both TWAP views, and a NULL-start refresh processes those against a `prices_1m` whose old chunks are gone and DELETES the TWAP history for each dropped range. Quoted only so you recognise it (`scripts/ci/lint-migration-commands.sh` fails the tree if it appears unmarked in a runbook, rule or ops script):

```sql
-- DO NOT RUN: CALL refresh_continuous_aggregate('twap_1h', NULL, now());
```

Check:

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

Fix:

1. TWAP views: settle the `prices_1m` window first; a TWAP view can only be rebuilt over what `prices_1m` still holds. If retention is armed and the history you need is older than `prices_1m`'s `min(bucket)`, stop and follow the Recovery section of `migrations/0156_prices_1m_retention.up.sql` in order (DISARM and confirm, force-rebuild `prices_1m` in slices), then continue with the disarm still in force.
2. Re-materialize the empty range, windowed. `<start>` = the source's `min(bucket)` (TWAP) or `min(ts)` (`prices_*`); `<end>` = the view's current `min(bucket)`, or `now()` if empty. The window must reach the view's minimum width or TimescaleDB rejects it with `22023 refresh window too small`: 3 h for `twap_1h`/`prices_1h`, 3 days for `twap_1d`/`prices_1d`, 21 days for `prices_1w`, 93 days for `prices_1mo` (`MinWindow` in `TradesCAGGs`, `internal/storage/timescale/diagnostics.go`). If shorter, move `<end>` later toward `now()`, never `<start>` earlier:

   ```sql
   CALL refresh_continuous_aggregate(
          'twap_1h',
          '<start>'::timestamptz, '<end>'::timestamptz,
          force => true);
   ```

   Repeat for `twap_1d` (or the named `prices_*` view). `force => true` is required (over a range retention dropped a plain refresh reports `already up-to-date` and writes nothing). A `prices_*` view over years of `trades` takes hours: run slices of a few months.
3. The gauge clears on the next `data-freshness.sh` tick (<= 15 min) once the oldest bar reaches its floor; with `prices_1m` retention armed a TWAP floor is still the oldest trade, so it keeps firing until step 1's rebuild ran.

## stellarindex_data_freshness_watchdog_silent

Trips: `absent_over_time(stellarindex_data_freshness_stale[45m])`, `for: 15m`. The `stellarindex_data_freshness_*` series are gone: `data-freshness.timer` failed to fire or the script errors, so the watchdog is blind. MTTR 5-15 min. It sees ABSENCE only; a present-but-frozen file is [probe_frozen](#stellarindex_data_freshness_probe_frozen).

Check:

```sh
systemctl status data-freshness.service data-freshness.timer
journalctl -u data-freshness.service --since '1 hour ago' | tail -30
ls -la --time-style=+%H:%M /var/lib/node_exporter/textfile_collector/data_freshness.prom
# Run it by hand to see the error:
/usr/local/sbin/data-freshness.sh
```

Likely causes: timer not enabled after a rebuild; a DB outage failing a query under `set -euo pipefail`; a 0600 textfile (node_exporter is unprivileged; the script chmods 0644 before the swap, `chmod 0644` a stale one).

Fix:

1. Timer disabled: `systemctl enable --now data-freshness.timer`.
2. psql/DSN errors: the script reads `/etc/default/stellarindex` VERBATIM for `STELLARINDEX_POSTGRES_DSN` and never sources it (systemd env values are unquoted; sourcing would expand `$`, split on `;`; pinned by `scripts/ci/envfile-loader-test.sh`). Do NOT "fix" it by sourcing. Fix the DSN/DB, then `systemctl start data-freshness.service`.
3. `psql` not found: the script calls `/usr/lib/postgresql/${PG_VERSION:-15}/bin/psql` directly (bypassing Debian's `pg_wrapper`); after a Postgres MAJOR upgrade set `PG_VERSION=<major>` in `/etc/default/data-freshness` and restart the unit.
4. The last producer is a ClickHouse probe on `:8123`. Current builds log `data-freshness: ClickHouse supply_flows probe failed — sep41_supply gauges skipped this tick` and omit only the two `sep41_supply` gauges; a host not redeployed since may still abort the whole run on a ClickHouse outage (check the script on disk).

## stellarindex_sep41_supply_freshness_absent

Trips (`for: 1h`):

```
group by (instance) (stellarindex_data_freshness_stale)
  unless on (instance)
stellarindex_data_freshness_stale{domain="sep41_supply",source="supply_flows"}
```

`data_freshness.prom` publishes but the ClickHouse-probed `sep41_supply` pair has been missing for an hour (the script skips it when its `supply_flows` probe fails), so neither `data_source_stale` nor `watchdog_silent` can fire: served SEP-41 supply staleness is unmonitored.

```sh
journalctl -u data-freshness.service --since '2 hours ago' | grep 'supply_flows probe failed'
curl -sS -f --max-time 15 http://localhost:8123/ --data-binary 'SELECT 1'
```

Restore ClickHouse HTTP on `:8123`; the pair returns on the next 15-min tick.

## stellarindex_data_freshness_probe_frozen

Trips: `time() - node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/data_freshness.prom"} > 2700`, `for: 15m`. The file is over 45 m old but its series are still PRESENT (a run that dies before the atomic swap leaves the last good file, which `absent_over_time` cannot see; same pattern as `stellarindex_config_assertions_stale`, [config-assertion-failed](config-assertion-failed.md)). Do not trust any `stellarindex_data_freshness_stale` reading until cleared.

```sh
ls -la --time-style=full-iso /var/lib/node_exporter/textfile_collector/data_freshness.prom
curl -s localhost:9100/metrics | grep -E 'node_textfile_(mtime_seconds|scrape_error)'
systemctl status data-freshness.service data-freshness.timer
```

Then fix as in [watchdog_silent](#stellarindex_data_freshness_watchdog_silent).

## Served-value truth (verify-served-values)

`stellarindex-ops verify-served-values` reconciles served values against independent sources (SDF lumen API, Stellar Expert) and writes `stellarindex_served_value_{ok,rel_err,skipped,last_run_unix}` plus `stellarindex_sdf_reserve_list_drift{kind}`, daily 06:20 UTC from `verify-served-values.timer`.

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

Then `docs/architecture/supply-pipeline.md` for which algorithm (1 XLM / 2 classic / 3 SEP-41) derives the failing value. A series ABSENT rather than old: `systemctl status verify-served-values.timer`. `verify-served-values.service` is in `scripts/ci/unit-failed-dedicated.baseline` (its exit code is a verdict; the catch-all failed-unit rule skips it). Standing drift on XLM/USDC that resists a day: raise with the maintainer. See [adr-0041](../../adr/0041-ingest-durability-semantics.md).

## stellarindex_served_value_drift

Trips: `stellarindex_served_value_ok == 0`, `for: 26h` (two daily runs). Label `check`. A served value is outside tolerance vs its independent source; a drifted check is re-read once 6 minutes later, so a mint/burn between our snapshot and the truth read does not count. The unit log note says which: `gap persisted after the served snapshot advanced` is real drift; `served supply snapshot stale` means `supply_as_of_ledger` did not move (look at the supply writer).

Causes: a supply basis wrong or partly populated (standing: `xlm_circulating_supply` reads `xlm_total_only` until `sdf_reserve_accounts` is set; `usdc_total_supply` under-reads vs Stellar Expert); an observer gap; the GROUND TRUTH changed methodology (verify bases match before changing ours).

Fix: no cache to flush. Fix the derivation or its config, or state the differing basis on the wire (`supply_basis`). Never widen a tolerance without a written methodology justification in the check's `note`.

## stellarindex_served_value_check_stale

Trips (`for: 1h`):

```
(time() - stellarindex_served_value_last_run_unix) > 172800
or
absent_over_time(stellarindex_served_value_last_run_unix[2d])
```

No textfile in 48 h: timer dead or never installed (hence the `absent_over_time` arm), the run crashing, or every run skipping every check (`last_run_unix` only advances on a verdict). Data-truth reconciliation is dark. Start with `systemctl status verify-served-values.timer` on the archival node.

## stellarindex_served_value_persistently_skipped

Trips: `stellarindex_served_value_skipped == 1`, `for: 26h`. Label `check`. Skipped on every run for 2 days because its truth source (SDF lumen API / Stellar Expert / the stellar/dashboard source behind `sdf_reserve_list`) is unreachable. `drift` and `check_stale` stay quiet and a skipped check emits no `served_value_ok`, so without this the value stays UNVERIFIED behind a green gate. Check the source's reachability and response shape (URL/schema change); the check resumes when truth returns.

## stellarindex_sdf_reserve_list_drift

Trips: `stellarindex_sdf_reserve_list_drift > 0`, `for: 26h`. Label `kind`. `supply.sdf_reserve_accounts` differs from SDF's published reserve list on two daily runs. `kind="missing"`: SDF publishes it, we do not exclude it (circulating OVER-stated). `kind="extra"`: we exclude it, SDF no longer lists it (UNDER-stated). The `xlm_circulating_supply` check (2% of ~700M XLM) can stay green because one account is under 2%.

Source: not the dashboard API (sums only) but the `accounts` table plus `networkUpgradeReserveAccount` in <https://raw.githubusercontent.com/stellar/dashboard/master/common/lumens.js>. The burn address (`voidAccount`) is subtracted from TOTAL and is not in the set. SDF retires a row by commenting it out, which surfaces as `extra`. A dark or reshaped source (one table row outside the `key: "G…"` grammar fails the parse whole; a diff retiring over three configured accounts at once is refused as a likely moved table) emits `stellarindex_served_value_skipped{check="sdf_reserve_list"}=1` and NO drift gauge (ticketed by `_persistently_skipped`). An unreadable config file fails the run (`_unit_failed` after two runs).

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

Fix (config, not code, only for an account the upstream diff actually changed):

1. Find the stellar/dashboard commit that edited `accounts`; confirm every `extra` was commented out or deleted and every `missing` added. An account still a live upstream row is a parser defect: file it, leave config alone (deleting a reserve account adds its balance to circulating supply).
2. Update BOTH lists in `configs/ansible/roles/archival-node/defaults/main.yml` (`stellarindex_sdf_reserve_accounts` and the paired `stellarindex_reserve_balances_stroops`; the supply writer refuses to start with an account missing from the balance map), re-render `/etc/stellarindex.toml`, restart the supply writer. A genuine edit is a methodology change worth a CHANGELOG line.
3. The next daily run clears the gauge. Never widen the value check's tolerance.

## stellarindex_served_value_unit_failed

Trips (`for: 1h`):

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

The unit failed on two consecutive daily runs. It exits non-zero on a DRIFTED check (`served_value_drift` / `sdf_reserve_list_drift` name it), an all-skipped run (`served_value_persistently_skipped` names it), or a crash before the textfile is written; only the crash is unique to this ticket. It is deliberately not the catch-all `stellarindex_systemd_unit_failed`. With neither companion firing, treat it as a crash:

```sh
systemctl status verify-served-values
journalctl -u verify-served-values --since -50h
```

## stellarindex_serving_insert_frozen

Rule file `serving-freshness.yml` (both trees). Trips: `(time() - max(stellarindex_source_last_insert_unix)) > 1800`, `for: 10m` (gauge set in `internal/storage/timescale/trades.go` on each successful insert). NOT ONE source landed a row in 30 min: the served tier is frozen and `/v1` goes stale across the board, even while source EVENTS flow (`all_sources_stopped` stays quiet: a stuck cursor or replay loop where every insert ON-CONFLICT-noops or fails). Check Timescale write health and `stellarindex_trade_insert_retries_total` / `stellarindex_ingestion_trade_insert_backpressure` first.

## stellarindex_serving_insert_absent

Rule file `serving-freshness.yml` (both trees). Trips: `absent(stellarindex_source_last_insert_unix)`, `for: 15m`. No scrape for 15+ min: indexer down, metric dropped, or no source has landed a row since boot. Every `time() - stellarindex_source_last_insert_unix` alert is a no-op while it is absent; this is the backstop.

## Retiring a source

A retired source keeps its history and alarms forever on [data_source_stale](#stellarindex_data_source_stale) (the script cannot tell retired from dead; there is no exclusion list). In the SAME change that retires it, pick one:

1. Delete its rows from the table the domain reads (`oracle_updates` for `oracle`, `fx_quotes` for `fx`, `asset_supply_history` for `supply`). Irreversible; right when the integration is gone for good.
2. Exclude it in the domain's `WHERE` clause in `data-freshness.sh`. Keeps history; reverting that line silently re-arms the alarm.

Never silence the alert in Alertmanager: a blanket silence hides every OTHER source's real outage. A declared retired-source list is not built yet (#1347).

## stellarindex_assets_popular_priceless

Covers `stellarindex_assets_popular_priceless` (a real coverage gap) and `stellarindex_priceless_coverage_check_stale` (the tripwire stopped sweeping, so a gap would go unseen). P3, rule `deploy/monitoring/rules/pricing-coverage.yml`, emitted by `internal/pricelesscoverage` (aggregator sweep, 10 min). MTTR 30-120 min, usually a missing USD quote path. A genuinely traded asset renders priceless on `/v1/assets`; no wrong price is served (fail-closed).

The gauge counts assets (`internal/pricelesscoverage.popularPriceless`) that are ALL of: priceless (no non-null `prices_1m` VWAP against USDC / its SAC / `fiat:USD` / `native` / the XLM SAC in 24 h, and no `asset_price_snapshot` row younger than `assetPriceSnapshotMaxAge`, 15 min); not withheld (the `pricingguard.AssetSubstanceVerdict` gate allows it; an unmeasurable asset DOES count); not wash (busiest `(maker, taker)` pair owns < 90% of 7 d priced volume); popular (7 d priced volume > $10k OR 7 d trades > 5,000). `> 0` for 1 h pages.

Check:

```sh
# 1) How many assets, and is the sweep fresh?
curl -fs http://localhost:9465/metrics \
  | grep -E '^stellarindex_(assets_popular_priceless|priceless_coverage_check_)'

# 2) WHICH assets — the worker logs each firing asset every sweep.
journalctl -u stellarindex-aggregator -n 500 \
  | grep 'priceless-popular coverage gap'
#   -> asset_id, volume_7d_usd, trades_7d, top_account_pair_share
```

Pick one asset and reproduce:

```sh
# Does /v1/assets/{asset_id} really serve no price_usd?
curl -fs "https://api.stellarindex.io/v1/assets/<asset_id>" | jq '.data.price_usd'
# What quotes does it actually trade against? (the missing-path clue)
# Count BOTH stored directions — a pair is written as (A,B) AND (B,A), each
# holding only PART of the market; grouping on base_asset alone under-reports
# buckets by roughly half and sends you after a thin-market theory that is not real.
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT CASE WHEN base_asset = '<asset_id>' THEN quote_asset ELSE base_asset END AS counterparty,
          count(*) AS buckets, sum(trade_count) AS trades, max(bucket) AS last_seen
     FROM prices_1m
    WHERE (base_asset = '<asset_id>' OR quote_asset = '<asset_id>')
      AND bucket >= now() - INTERVAL '24 hours'
    GROUP BY 1 ORDER BY 2 DESC;"

# Is the asset in a catalogue spine? Classic assets via classic_assets;
# Soroban-native contracts via discovered_assets + an asset_volume_24h row.
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT (SELECT count(*) FROM classic_assets   WHERE asset_id = '<asset_id>') AS classic,
          (SELECT count(*) FROM discovered_assets WHERE contract_id = '<asset_id>') AS discovered,
          (SELECT count(*) FROM asset_volume_24h  WHERE asset_id = '<asset_id>') AS vol_rollup;"

# Which DIRECTION is the XLM leg stored in? Only (CAS3J…/native, <id>)
# rows == the SAC-as-base class (see the table below).
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT base_asset, quote_asset, count(*) AS buckets, max(bucket) AS last_seen
     FROM prices_1m
    WHERE (base_asset = '<asset_id>' OR quote_asset = '<asset_id>')
      AND bucket >= now() - INTERVAL '24 hours'
    GROUP BY 1, 2 ORDER BY 3 DESC;"
```

Decision table:

| Finding | Likely cause | Mitigation |
| ------- | ------------ | ---------- |
| Trades only against a quote NOT in the USD-proxy set | Missing USD-proxy bridge (AUDD/EURC class) | Extend the proxy set in `listAssetsBaseSelect` + `coverageQuoteProxies` (lockstep) and re-derive |
| Trades only against another classic asset with no USD path | No triangulation route | Confirm the intermediate has a USD price; add the pair to the chain |
| SAC form of a priced classic | Alias fold gap | Confirm `[supply].sac_wrappers` maps the SAC |
| A scam we should not price | Should be labelled/withheld | Add it to the scam directory / withhold path |
| Soroban-native (`C…`, no classic twin) with no `classic_assets` row | Both catalogue spines UNION `discovered_assets` (bounded by `asset_volume_24h`); if none, it has no 24h rollup row (check `assetvolrollup`). The gate is not the blocker: it allows the asset, so `price_usd` is null with NO withheld reason | Confirm the rollup row, then the direction row. Do NOT record a synthetic withheld verdict (it turns a real gap into a silent one). A native asset reachable only via another native one needs the transitive hop `Store.TransitiveUSDPrice` |
| XLM (native or SAC `CAS3J…`) stored as BASE: only `(CAS3J…, <id>)` rows | Swap-direction sources (aquarius: base = `token_in`) store `(XLM-SAC, token)`; every XLM-leg read path needs an inverted arm | Each path has one (base-side preferred). If it fires again one of the lists/arms drifted: run `TestProxyQuoteLists_Lockstep` and `TestXLMSacAsBase_PriceableThroughEveryPath` |

Clear a false-positive directory scam flag (a scam tag withholds the issuer's price; override durably, since `directory-sync` would re-apply an edited row; `-delete` hands the address back; dry run until `-write`):

```sh
stellarindex-ops directory-override -config /etc/stellarindex.toml -address <G... or C...> -clear-scam-flag -reason "<why>" -write
```

For `stellarindex_priceless_coverage_check_stale`: `candidate read failed` in the aggregator log = Postgres unreachable; no `priceless-popular coverage tripwire: wired` at startup = worker did not start (`worker.Recover(logger, "priceless-coverage")`).

Fix: identify the asset(s) from the warn logs, reproduce, apply the table row (usually a missing USD-proxy quote path; keep `coverageQuoteProxies` in `internal/storage/timescale/priceless_coverage.go` in lockstep with the catalogue's `direct_usd` / `asset_vs_xlm` set), and verify the gauge returns to 0 on the next sweep (auto-resolves after 1 h). A just-listed asset is masked by `for: 1h`, a restart by the staleness `for: 30m`.

## curated-rwa-sync

Alerts: `stellarindex_curated_rwa_sync_stale`, `stellarindex_curated_rwa_sync_refused`, `stellarindex_curated_rwa_published_stale`. All P3, routed by `configs/alertmanager/alertmanager.r1.yml`; rules `deploy/monitoring/rules/curated-rwa-sync.yml` and `configs/prometheus/rules.r1/curated-rwa-sync.yml` (byte-identical; group `stellarindex.curated_rwa_sync`).

- Impact: a comparison panel degrades; the verified RWA surface is untouched. The reader is fail-closed: a row with `synced_at` older than 48 h is not served, so `/v1/rwa/assets` reports `curated.status: unavailable`.
Check (shared):

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

The absent arm waits for 30 h of scrapes so a rule load or Prometheus restart does not fire it. No run has stamped `last_run_unix` in 30 h (at 48 h the reader empties the arm). Triage in order:

1. Timer not listed or `NEXT` `n/a`: `systemctl enable --now curated-rwa-sync.timer`; the ansible role is the source of record.
2. Journal `GET /api/v1/query/…/results: HTTP 4xx`: `401`/`403` key rotated or revoked (see `_refused`); `402`/`429` credit allowance exhausted, almost always another consumer of the key (reads are metered by datapoint; wait for the monthly reset or lower the cadence in `curated-rwa-sync.timer.j2`); `404` the curator deleted or privatised the query, find its successor at dune.com/stellar/rwas (ids are constants in `internal/ops/ingest/curated_rwa_sync.go`).
3. `latest execution is QUERY_STATE_…, not completed`: the curator's run failed; the previous rows stay served until 48 h. Persisting past a day: say so on the page's issue, not in code.
4. `printed no usable row` / `printed N of the M rows it declared`: the curator changed a column or paging. Rows decode STRICTLY (`duneMonthlyTotalRow`, `duneMonthlyBySubclassRow`); a layout change is a code change with a test.
5. `Read N rows; kept …` then a Postgres error: DSN, pool, constraint, or `-timeout` mid-commit; a failed write stamps nothing. Fix the database, re-run the unit.
6. Textfile written but `last_run_unix` frozen: node_exporter not scraping the directory (permission, or missing `--collector.textfile.directory`).
7. All healthy and still firing: the r1 node_exporter scrape target is down.

### stellarindex_curated_rwa_sync_refused

Trips: `stellarindex_curated_rwa_sync_refused == 1`, `for: 2h`. The run exited clean without reading the curator because `DUNE_API_KEY` is empty in `/etc/default/curated-rwa-sync`; it still stamps `last_run_unix`. Expected on a fresh install until a key is set (check 4 prints `0`).

Fix: the file is `root:root` mode `0600`. Set `vault_dune_api_key` in `inventory/r1.secrets.yml` (workstation, `configs/ansible/`), then:

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
```

The role renders `DUNE_API_KEY={{ vault_dune_api_key }}` for a non-empty vault value and otherwise installs the empty placeholder once without overwriting a pasted value; a hand-set key is replaced by the vault's on the next apply. Then `systemctl start curated-rwa-sync.service` and re-read the textfile; the reader recognises the arm within its 10-minute cache TTL.

### stellarindex_curated_rwa_published_stale

Trips (`for: 1h`):

```
(time() - stellarindex_curated_rwa_sync_executed_at_unix) > (72 * 3600)
and
stellarindex_curated_rwa_sync_executed_at_unix > 0
```

The sync is healthy but the CURATOR stopped re-executing its public query (`executed_at_unix` 0 means nothing was read; `_refused`/`_stale` own that). Worst healthy case is ~48 h; the API serves `stale: true` past 48 h and drops `curated.published` past 7 days (`by_subclass_executed_at` is the split's own stamp; a split older than 7 days is withheld). Nothing to fix here: confirm on dune.com that the curator's schedule is paused or broken.

## fx-history-missing

Customer report "FX history is empty" / `history_1y: 0` and `history_all: 0` on `/v1/assets/<fiat>` while `history_7d` populates. No Prometheus alert; P3, MTTR 5-15 min. Cause: migration 0028 (`fx_quotes` hypertable) shipped but was never applied. The stablecoin-fiat proxy is unaffected. A normal `gh workflow run deploy.yml` syncs and applies migrations before the binary swap (unless `migrations_skip`), so this is the out-of-band fallback. See also [disk_full](postgres.md#stellarindex_timescale_disk_full), [writes_blocked](cache.md#stellarindex_redis_writes_blocked).

Check:

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

Fix:

1. Copy the migration to the canonical dir (`/usr/local/share/stellarindex/migrations/` on r1, host `136.243.90.96`; NOT the stale `/var/lib/stellarindex/migrations/`):

   ```sh
   scp migrations/0028_create_fx_quotes.{up,down}.sql \
       root@<host>:/usr/local/share/stellarindex/migrations/
   ```

2. Apply (forward-only, additive, safe live; the forex worker picks the table up on its next hourly tick):

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

   Expected: `1/u create_fx_quotes`, exit 0.
3. Confirm persistence (may take up to 1 h; a zero count only proves absence of failure once a tick fired since the migration):

   ```sh
   journalctl -u stellarindex-api --since "2 hours ago" -o cat \
     | grep -c "fx_quotes persist failed"
   # → 0 once the next refresh tick fires (hourly cadence)

   sudo -u postgres psql -d stellarindex -tA -c "SELECT count(*) FROM fx_quotes"
   # → > 0 within ~1 h
   ```

4. Backfill depth: the live worker (Massive `MASSIVE_API_KEY`, keyless ECB fallback) writes only the latest snapshot. The one-shot `fx-history-backfill` fetches ECB-backed Frankfurter (free, ~32 currencies, daily back to 1999-01-04) in 5-year chunks, upserting on `(ticker, bucket)` (safe to resume). From the operator's workstation; it exits non-zero if any chunk failed, so re-run the `--from`/`--to` of each `chunk failed` line until it exits 0:

   ```sh
   # On the operator's workstation:
   export DATABASE_URL=postgres://...:5432/stellarindex
   go run ./scripts/ops/fx-history-backfill --years=25
   ```

## Related

- [Alerts catalogue](../alerts-catalog.md)
- `internal/pricelesscoverage/`, `internal/storage/timescale/priceless_coverage.go`, `internal/pricingguard/substance.go`.
