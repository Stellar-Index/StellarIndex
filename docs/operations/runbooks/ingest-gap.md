---
title: Runbook — ingest-gap
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — ingest-gap alerts

Ledger-gap detector alerts. Merged from three former pages (ingest-gap-detected, ingest-gap-detector-silent, sdex-gap-detected).

## At a glance

- [`stellarindex_ingest_gap_detected`](#stellarindex_ingest_gap_detected)
- [`stellarindex_ingest_gap_detector_silent`](#stellarindex_ingest_gap_detector_silent)
- [`sdex-gap-detected`](#sdex-gap-detected)

## stellarindex_ingest_gap_detected

_Source page `ingest-gap.md#stellarindex_ingest_gap_detected`: status ratified, severity P1, last verified 2026-08-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingest_gap_detected` |
| Severity | P1 (page) |
| Detected by | `stellarindex_ingest_gap_max_size_ledgers{source} > 1000` for 15 min |
| Typical MTTR | 30 min — 4 h (depending on gap size and bucket reachability) |
| Impact | A contiguous block of soroban_events ingest is missing. Per-decoder coverage for every Soroban source is incomplete across the gap window; price-history queries spanning the gap return holes. |

### Symptoms

- Prometheus alert `stellarindex_ingest_gap_detected{source="soroban-events"}` is firing.
- `stellarindex_ingest_gap_max_size_ledgers{source="soroban-events"}` reports the size of the largest contiguous gap.
- The status page's per-source density may still show 100% — cursor-derived density measures process state, this alert measures data state.

### Triage — 5 minutes

1. **Get the exact gap list:**

   ```sh
   ssh root@<region-host>
   stellarindex-ops find-data-gaps --config /etc/stellarindex.toml --output text
   ```

   Output prints each `[from, to]` range + a ready-to-paste `stellarindex-ops backfill` command per gap.

   `find-data-gaps` is ledger-scoped and cannot see CEX/oracle sources (no ledger). For those, run `scripts/ops/find-external-gaps.sh [source...]` on the host (needs `STELLARINDEX_POSTGRES_DSN`; `MIN_GAP_DAYS` defaults to 2). It reports day-level holes in `kraken coinbase bitstamp binance`; a venue that genuinely did not trade is not a defect, so a human judges each hole.

2. **Classify the gap pattern:**
   - **One large contiguous gap (>50 K ledgers)** → cascade signature. Suspect ingest halt (Redis MISCONF, Postgres back-pressure, AsyncSink wedge). Cross-check F-0020-cluster alerts (`stellarindex_redis_writes_blocked`, `stellarindex_postgres_connections_high`).
   - **Many small gaps (each ~100-500 ledgers)** → flaky-write pattern. Suspect MinIO blip, transient ledgerstream reconnect, or a partial-batch sink failure.
   - **Single small gap at the trailing edge** → likely active backfill or a brief Postgres pause; re-check in 5 min before acting.

3. **Confirm the source is actively ingesting NOW:**

   ```sh
   ssh root@<region-host> 'sudo -iu postgres psql -d stellarindex -c "SELECT MAX(ledger_close_time), MAX(ledger) FROM soroban_events;"'
   ```

   If `MAX(ledger_close_time)` is fresh (within ~30 s) the writer is healthy and the gap is historic; if stale, the writer is still wedged and the gap is growing.

### Remediation

#### Healthy writer + historic gap

Run the targeted backfill commands the diagnostic emitted:

```sh
stellarindex-ops backfill -write --config /etc/stellarindex.toml \
  --from <gap.start> --to <gap.end> --source soroban-events
```

One invocation per gap. Each ~92 K ledger gap takes 15-30 min on r1. Confirm the gauge drops by re-running `find-data-gaps` or watching `stellarindex_ingest_gap_max_size_ledgers` decay.

#### Wedged writer + growing gap

This is the F-0020 cascade pattern. Pause heavy walks (any running `stellarindex-ops backfill -source soroban-events` invocation — `pkill -INT -f 'stellarindex-ops backfill'`; `verify-archive-tier-a.service`) per `docs/operations/backfill-with-live-ingest.md`, then:

1. Check Redis (`redis-cli info persistence` — `rdb_last_bgsave_status: ok`?).
2. Check Postgres (`SELECT count(*) FROM pg_stat_activity;` — saturated?).
3. Restart the indexer (`systemctl restart stellarindex-indexer`).
4. Watch the live cursor advance via `/v1/diagnostics/cursors`.
5. Once live ingest is recovered, schedule the historic-gap backfill above.

### Known false-positive patterns

- **First boot after rc.84+ deploy.** The detector's first cycle runs immediately on startup (light targets are scanned; the 6h-cadence `sdex`/`soroban-events` targets are scanned only if their cadence has elapsed since the persisted `gap-detector-scan` cursor, otherwise their last-known gauges are re-emitted from `source_coverage_snapshots`), so the gauge is non-empty before the first tick; if a historic gap is preserved from before deploy the alert fires within 15 min. Resolve via the standard targeted-backfill path.
- **Genuinely-empty mainnet window.** Soroban activity dipped briefly below the `min-gap-size=1000` threshold (~1.5 h of zero contracts). Vanishingly rare on mainnet post-2024 but possible during testnet experiments — lower the threshold flag if your network is quieter.

### Scaling landmine: detector targets with no leading-`ledger` index

Both detector queries (`FindPerSourceLedgerGaps`'s LAG-over-DISTINCT and
the generic `COUNT(DISTINCT ledger)`) predicate ONLY on `ledger BETWEEN
$1 AND $2`. Every per-source hypertable partitions on `ledger_close_time`,
so without a btree whose **first** column is `ledger` (or a `(source,
ledger)` btree matched by the target's `WhereFilter`) a ledger-only
predicate excludes no chunks and the query is a full scan of the table —
its cost grows with lifetime table size, not with the ~4,300-ledger
trailing window. `soroban_events` (257 GB) crossed the line on
2026-08-28 (556 s mean per count). The tables below are the same class
and will cross it as they grow; today they are small enough that the
scan completes in seconds. Adding the index is a deliberate migration
(`CREATE INDEX CONCURRENTLY`, sized per table), NOT something to do
during an incident. Census from `migrations/*.up.sql` on 2026-08-28:

| Target table | Ledger-prefixed btree? | Notes |
| ------------ | ---------------------- | ----- |
| `soroban_events` | **none** | count moved to `ledger_ingest_log` census (2026-08-28); LAG gap scan still full-scans, 13-min PG timeout |
| `cctp_events` | none | `(contract_id, ledger)` only |
| `rozo_events` | none | `(contract_id, ledger)` only |
| `comet_liquidity` | none | |
| `blend_emitter_events` | none | |
| `blend_emissions` | none | |
| `blend_admin` | none | `(contract_id, ledger, …)` PK only |
| `soroswap_skim_events` | none | |
| `soroswap_liquidity` | none | |
| `soroswap_router_swaps` | none | |
| `defindex_flows` | none | |
| `defindex_fees` | none | |
| `defindex_admin_events` | none | |
| `phoenix_liquidity` | none | |
| `phoenix_initialize` | none | |
| `phoenix_admin_events` | none | |
| `phoenix_stake_events` | none | |
| `aquarius_liquidity` | none | |
| `aquarius_reserves` | none | roughly as dense as aquarius trades — the next likely offender |
| `aquarius_reserves_sync` | none | |
| `aquarius_protocol_fee` | none | |
| `aquarius_kill_switches` | none | |
| `aquarius_rewards_events` | none | dense (`pool_state`) |
| `aquarius_admin` | none | |
| `credit_positions` | none | |
| `credit_statements` | none | |
| `credit_settlements` | none | |
| `credit_events` | none | |
| `trades` (sdex / aquarius / soroswap / phoenix / comet) | `(source, ledger DESC)` | served by `WhereFilter: source = '…'` |
| `oracle_updates` (band / redstone / reflector-*) | `(source, ledger DESC)` | served by `WhereFilter` |
| `sep41_transfers` | `(ledger DESC)` (0083) | |
| `sep41_supply_events` | `(ledger DESC)` | |
| `blend_auctions` | PK `(ledger, …)` + `(ledger DESC)` | |
| `blend_positions` | `(ledger DESC)` | |
| `blend_backstop_events` | `(ledger DESC)` | |
| `sdex_offer_events` | PK `(ledger, …)` | |

If a target from the "none" rows starts showing `elapsed_s` in the
hundreds in `gap-detector: scan failed` / duration histograms, that is
this class — open a migration for a `(ledger DESC)` index on that
table, or (for a table whose count has a census equivalent) route its
count through `GapDetectorTarget.DistinctLedgerCountSQL`.

### Changelog

- 2026-05-28 — initial draft alongside the gap detector worker ship.

## stellarindex_ingest_gap_detector_silent

_Source page `ingest-gap.md#stellarindex_ingest_gap_detector_silent`: status ratified, severity P2, last verified 2026-08-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingest_gap_detector_silent` |
| Severity | P2 (ticket) |
| Detected by | `(time() - stellarindex_ingest_gap_detector_last_success_unix) > 8h` (per source/table) OR the detector metric absent for 15 min (aggregator down) OR `runs_total{outcome="error"}` present now and 8h ago with no last-success stamp seen in 8h (a target that has never succeeded in this process life) |
| Typical MTTR | 15 min (restart) — 1 h (deeper Postgres issue) |
| Impact | The data-gap detector goroutine is wedged for a target. `stellarindex_ingest_gap_max_size_ledgers` gauges read stale value; the paging `ingest_gap_detected` alert can't fire even if a real gap forms. The system has lost its data-derived ingest-health signal for that target. |

### Symptoms

- `stellarindex_ingest_gap_detector_last_success_unix{source,table}` for one target is more than 8h old (or the whole detector metric is absent → aggregator down).
- **Or the stamp does not exist at all** for the target and the alert carries `outcome="error"`: the target has never scanned successfully in this process life (no `gap-detector-scan` cursor row, so nothing re-emits a stamp on boot) and has been erroring for 8h+. Before 2026-08-28 this case fired nothing — the error counter satisfied the "runs_total present" clause and there was no stamp to age. The 2026-08-28 r1 `soroban-events/soroban_events` statement_timeout loop (PR #258) is the canonical example.
- `stellarindex_ingest_gap_detector_runs_total{outcome="error"}` is climbing for that target (the scan is failing every cycle), and the aggregator log shows `gap-detector: scan failed` lines with a large `elapsed_s` (timeout) or a Postgres error string.
- Operators reading the dashboard see the gap-size gauge frozen on its last-known value.
- May coincide with `stellarindex_aggregator_silent` (aggregator binary is down) or `stellarindex_postgres_exporter_down` (Postgres is unreachable).

> **Why a timestamp gauge, not `rate(runs_total{outcome="ok"})`?**
> The heavy targets (`sdex`/`trades`, `soroban-events`/`soroban_events`)
> scan on a 6h `ScanCadence`, so their `ok` counter increments only once
> per 6h. When the aggregator restarts more often than that, each process
> life records exactly one `ok`, pinning the counter at `1`. Because the
> value is `1` both before and after the restart, Prometheus counter-reset
> detection never fires and `rate(...ok[7h])` reads a flat line → `0` → the
> alert false-fired for >7h on 2026-07-06 even though every scan succeeded.
> The wall-clock gauge is reset-proof: a healthy startup scan re-stamps it
> to `now()`, so the alert clears the moment a scan succeeds.

### Triage — 5 minutes

1. **Aggregator service healthy?**

   ```sh
   ssh root@<region-host> 'systemctl status stellarindex-aggregator | head'
   ```

   If inactive or crash-looping, that's the root cause — fix the aggregator first (`journalctl -u stellarindex-aggregator -n 200`).

2. **Postgres reachable?**

   ```sh
   ssh root@<region-host> 'sudo -iu postgres pg_isready'
   ```

   If not, the detector's per-target scan timeout (15 min Go-side / 13 min SQL `statement_timeout`) is firing every cycle and incrementing `outcome=error` instead — the last-success gauge stops advancing and staleness grows past 8h. Cross-check `stellarindex_postgres_exporter_down`.

3. **Connection pool saturated?**

   ```sh
   ssh root@<region-host> "sudo -iu postgres psql -d stellarindex -c 'SELECT count(*), state FROM pg_stat_activity GROUP BY state;'"
   ```

   `active` count near `max_connections` means the detector can't get a connection. Likely caused by concurrent fill walks per F-0020; see `docs/operations/backfill-with-live-ingest.md` for the recommended posture.

### Remediation

#### Aggregator down

```sh
ssh root@<region-host> 'systemctl restart stellarindex-aggregator'
ssh root@<region-host> 'journalctl -u stellarindex-aggregator -f'
```

The detector's first cycle runs immediately on aggregator boot, but since 2026-08-28 it honours each target's persisted schedule: the light targets scan within seconds; the heavy `sdex`/`soroban-events` targets (6h cadence) are scanned only if 6h have elapsed since their `gap-detector-scan` cursor's `last_updated`, otherwise they are skipped until their cadence is due and their `last_success_unix` + gap gauges are re-emitted from the persisted cursor / `source_coverage_snapshots` row. (Before that date every restart re-ran both heavy scans immediately — each a >10-min full scan of `soroban_events` — which is how a deploy loop turned into the 2026-08-28 IO incident.) A healthy restart therefore clears the alert within ~15 min for a light target; for a heavy target the re-emitted stamp is the previous process's last success, so the alert clears as soon as the next due scan succeeds (≤ 6h + ~13 min after that stamp — still under the 8h threshold). If you need a heavy target scanned NOW, delete its cursor row (`DELETE FROM ingestion_cursors WHERE source = 'gap-detector-scan' AND sub_source = 'soroban-events/soroban_events'`) before restarting — this also widens its next scan to the `GapDetectorFirstScanCap` window.

#### Postgres degraded

Defer the detector restart until Postgres is healthy. Once `pg_isready` returns clean, the detector recovers on its own cycle (no aggregator restart needed unless the goroutine has fully exited — check the aggregator log for `gap-detector` warnings).

#### Pool saturation

Reduce concurrent walk parallelism per `docs/operations/backfill-with-live-ingest.md`:

```sh
# Stop the running fill (a manual operator invocation, not a systemd unit) —
# find its PID and kill -INT it per backfill-with-live-ingest.md
# "Stop a running fill walk". Then wait for connection-count to drop and
# resume at -parallel 4 instead of -parallel 12.
```

### Known false-positive patterns

- **Fresh deploy / operator-triggered restart.** The startup cycle re-stamps the last-success gauge for the light targets (~3 s) and re-emits the PERSISTED stamp for heavy targets whose 6h cadence has not elapsed (they are not re-scanned on restart since 2026-08-28), so staleness never reads higher after a restart than before it — a restart clears or preserves the alert state, it does not cause it. This is the class the 2026-07-06 fix eliminated: the previous `rate(runs_total{outcome="ok"}[7h]) == 0` expr false-fired because the heavy targets' `ok` counter is pinned at `1` per process life and `1 → 1` across a restart is invisible to `rate()`.
- **Heavy target between 6h scans.** `sdex`/`soroban-events` scan every 6h, so their staleness sawtooths up to ~6h + scan duration (~11 min). That peak (~6.2h) is below the 8h threshold, so it does not fire. Only a genuinely missed cycle (no success in 8h) trips the alert.

### Changelog

- 2026-08-28 — third clause: fire when `runs_total{outcome="error"}` is present now and 8h ago and no `last_success_unix` stamp has been seen for the target in 8h. Closes the blind spot where a target that had never once succeeded (no stamp to age; error counter satisfying `absent_over_time(runs_total)`) fired nothing. promtool unit tests in `deploy/monitoring/rule-tests/ingestion_test.yml`.
- 2026-08-28 — restart no longer re-scans heavy targets ahead of their cadence (schedule seeded from the persisted `gap-detector-scan` cursor; last-success stamp + gap gauges re-emitted from persisted state). Density count for `soroban-events` now reads the `ledger_ingest_log` census instead of a full scan of `soroban_events`; PG `statement_timeout` for both detector queries is 13 min (the count had been 2h against a 15-min Go context, orphaning backends). r1 incident 2026-08-28 18:23Z.
- 2026-07-06 — re-keyed the alert off `stellarindex_ingest_gap_detector_last_success_unix` staleness (`> 8h`) instead of `rate(runs_total{outcome="ok"}[7h]) == 0`. The rate expr false-fired for >7h on the 6h-cadence heavy targets because their `ok` counter is pinned at `1` per process life and `1 → 1` across a restart defeats Prometheus counter-reset detection. A wall-clock gauge is reset-proof.
- 2026-05-28 — initial draft alongside the gap detector worker ship.

## sdex-gap-detected

**Runbook: SDEX data-coverage gap detected**

_Source page `ingest-gap.md#sdex-gap-detected`: status current, severity P1, last verified 2026-08-29._

### At a glance

- **Severity:** P1 — pages on-call (`severity: page`)
- **Trigger:** `max by (source) (stellarindex_ingest_gap_max_size_ledgers) > 1000` with `source="sdex"`, sustained 15 min (the generic `stellarindex_ingest_gap_detected` alert firing with the sdex label — there is no sdex-specific rule)
- **Detected by:** `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `severity: page`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`
- **Time to act:** within 30 min
- **Owner:** stellarindex on-call
- **TL;DR fix:** confirm writer health → `stellarindex-ops backfill -config /etc/stellarindex.toml -from $GAP_START -to $GAP_END -source sdex -dry-run`, then re-run with `-write -resume` (on r1, under `/usr/local/sbin/run-heavy-job.sh`)

This is the SDEX-specific surface of [ingest-gap-detected](ingest-gap.md#stellarindex_ingest_gap_detected). SDEX is classic-DEX and does NOT flow through `soroban_events`; its rows land in the unified `trades` hypertable filtered by `source = 'sdex'`. Symmetric to the Soroban path, an SDEX-side cascade (Postgres back-pressure halting the SDEX writer goroutine while the rest of ingest stays healthy) used to be invisible at the data layer. This alert closes that gap.

**No offer-events coverage:** `sdex_offer_events` (OfferCreated/OfferUpdated/OfferRemoved) has NO gap-detector target and never has — migration 0026 created the table but no writer has ever existed, so a target here would scan a permanently-empty table and page on a phantom source (`per_source_gaps.go`, #358, 2026-09-02). This alert covers only the `trades`-table SDEX gauge; an offer-events writer halt (if one ever ships) will not page until a target is added alongside its writer.

**Scan-window caveat (applies to both the gauge and this alert):** the detector scans only a trailing window below tip — `GapDetectorSafetyLookback` = 200,000 ledgers steady-state, `GapDetectorFirstScanCap` = 2,000,000 on a target's first-ever scan (`internal/storage/timescale/gap_detector.go`). A gap deeper in history than the scan window never appears in the gauge; deep-history assurance belongs to the ADR-0033 completeness verdict, and full-range diagnosis to `find-data-gaps -from/-to`.

### Triage (5 min)

1. **Confirm the signal.** Two quick checks:
   ```
   ssh root@136.243.90.96
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap.*sdex'
   stellarindex-ops find-data-gaps -config /etc/stellarindex.toml -source sdex
   ```
   Both should agree on the gap inventory WITHIN the detector's trailing scan window (see caveat above): the gauge covers only that window, while `find-data-gaps -from N -to M` is the full-range tool.

2. **Is the SDEX writer alive?**
   ```
   ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "30 min ago" | grep -E "sdex|source=sdex" | tail -50'
   ```
   Healthy steady-state writes are SILENT — the sink logs only failures and recovery, so an absence of sdex lines is normal. Liveness comes from the metrics instead:
   ```
   curl -s localhost:9464/metrics | grep 'stellarindex_source_last_insert_unix{source="sdex"}'
   curl -s localhost:9464/metrics | grep 'stellarindex_source_events_total{source="sdex"'
   ```
   `last_insert_unix` climbing + `events_total` rising = writer healthy. Both frozen = goroutine wedged or the source is halted.

3. **Is the gap forming or static?**
   ```
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
   sleep 60
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
   ```
   Same value = static (incident already over; backfill needed). Growing = active outage (the writer goroutine is still down). Note the gauge only refreshes on the detector's scan cadence — 6h for sdex (see Remediation) — so "same value" over one minute is expected either way; use the `last_insert_unix` liveness signal from step 2 as the live discriminator.

### Common shapes

- **Active writer halt (cascade-style).** The classic SDEX writer is paused; `stellarindex_source_last_insert_unix{source="sdex"}` is stale. Investigate the cascade root cause first (Redis MISCONF, Postgres pool exhaustion, disk pressure). Restarting the indexer without addressing the root cause will deadlock again within minutes.
- **Historic quiet windows do NOT fire this alert.** The sdex target carries a per-target `MinGapSizeOverride` of **1,000,000 ledgers** (`internal/storage/timescale/per_source_gaps.go` — the largest natural SDEX gap measured was 574,674 ledgers, so the generic 1K threshold would page constantly on historical data). Sub-1M gaps never report on this target. Combined with the trailing scan window (200K steady-state / 2M first-scan), this alert only catches large, recent holes — deep-history completeness is the ADR-0033 verdict's job.
- **Network outage during an upgrade.** Mainnet halts (e.g. a chain upgrade gone wrong) leave a real ledger gap but it's chain-wide, not SDEX-specific. The Soroban target should show a similarly-shaped gap. If only SDEX is short, it's an ingest-side issue.

### Remediation

Targeted SDEX backfill (re-decodes the range via the dispatcher). There is no `-parallel` flag, and `-config` is required — dry-run first to confirm scope, then re-run with `-write -resume`:

```
# On r1, wrap heavy one-shots in the mandatory memory-capped scope:
/usr/local/sbin/run-heavy-job.sh sdex-backfill \
  stellarindex-ops backfill \
    -config /etc/stellarindex.toml \
    -from $GAP_START -to $GAP_END \
    -source sdex \
    -dry-run

# Then swap -dry-run for -write to apply:
/usr/local/sbin/run-heavy-job.sh sdex-backfill \
  stellarindex-ops backfill -write \
    -config /etc/stellarindex.toml \
    -from $GAP_START -to $GAP_END \
    -source sdex \
    -resume
```

Idempotent via the `trades` PK (`(source, ledger, tx_hash, op_index, ts)`). Re-runs over already-covered range are no-ops.

Verify the gauge decays on the next detector cycle — the sdex target's `ScanCadence` is **6 hours** (`per_source_gaps.go` override; it scans the 62M-row `trades` hypertable, so the generic 30-min cadence would pile concurrent scans on Postgres). Allow up to 6h for the gauge to refresh; a skipped/failed cycle retains the last-known-good value rather than zeroing:

```
curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
# expect: 0 within one 6h cycle of the backfill completing
```

### Why no `sdex-backfill` subcommand?

There is no per-source `*-backfill` subcommand for any source — the whole `*-backfill` family (`cctp-backfill`, `soroswap-skim-backfill`, …) was **deleted** in rc.97 / ADR-0032 Phase 5. Soroban-derived sources catch up by rewinding the projector cursor (`projector-replay -source <name> -from <ledger>`), which re-projects from the ClickHouse `contract_events` lake by default (ADR-0034; the Postgres `soroban_events` landing zone is the legacy fallback source, decommission-pending #803) — no MinIO re-walk. SDEX has no equivalent landing zone — the classic-DEX ingest path writes straight to `trades` — so its repair re-decodes the raw range via the generic `backfill -source sdex` subcommand, which is the existing tool.

### Changelog

- 2026-09-24 — removed the claimed `sdex-offers` gap-detector target: it was never wired (no `sdex_offer_events` writer ever existed) and was explicitly deleted from `per_source_gaps.go` in #358 (2026-09-02) rather than shipped; the runbook still promised a page that cannot fire.
- 2026-08-29 — first re-verification against HEAD: frontmatter added; fictional `--parallel 8` backfill replaced with the real flag set (`-config` required, dry-run → `-resume`, run-heavy-job.sh on r1); "30-min detector cycle" corrected to the sdex target's 6h `ScanCadence` (skipped cycles retain last-known-good); the "raise min-gap-size" follow-up bullet replaced with the shipped facts (per-target `MinGapSizeOverride` exists and sdex's is 1M ledgers; detector scans only a trailing window — 200K steady / 2M first-scan); "batch-write log line every ~5s" corrected (healthy writes are silent — use `stellarindex_source_last_insert_unix{source="sdex"}`); `last_insert_at` renamed to the real metric; projector-replay note updated to the ClickHouse-lake default (ADR-0034); duplicate Trigger line dropped; `sdex-offers` sibling target cross-referenced; dual-tree Detected-by. Status → current.

## Related

**stellarindex_ingest_gap_detected**

- [projector.md#stellarindex_projector_replay_stalled](projector.md#stellarindex_projector_replay_stalled) — per-source projection-table repair via projector cursor rewind. Replaces the former `cascade-window-drain` orchestrator subcommand (ADR-0032 Phase 5).
- `docs/operations/backfill-with-live-ingest.md` — operational posture for running backfills alongside live ingest (F-0020 closure).
- F-0020 (audit-2026-05-26) — original cascade-window incident that motivated this detector.
- `stellarindex-ops find-data-gaps` — the operator-facing diagnostic this alert points at.
- `ingest-gap.md#stellarindex_ingest_gap_detector_silent` — paired ticket-tier alert for when the detector itself wedges.

**stellarindex_ingest_gap_detector_silent**

- `ingest-gap.md#stellarindex_ingest_gap_detected` — the paging alert this meta-alert protects from going silent.
- `aggregator.md#stellarindex_aggregator_silent` — sibling meta-alert for the aggregator binary itself.
- `docs/operations/backfill-with-live-ingest.md` — F-0020 posture for managing Postgres pool pressure.

**sdex-gap-detected**

- [ingest-gap.md#stellarindex_ingest_gap_detected](ingest-gap.md#stellarindex_ingest_gap_detected) — the parent alert (matches any `source=` label)
- [projector.md#stellarindex_projector_replay_stalled](projector.md#stellarindex_projector_replay_stalled) — Soroban equivalent for the per-source projection tables (ADR-0032 supersedes the former `cascade-window-drain` subcommand)
- ADR-0030 — per-source coverage invariant; SDEX target is the canonical example of a non-Soroban source registered in the same scheme
- ADR-0033 — the completeness verdict that owns deep-history assurance beyond the detector's trailing scan window
