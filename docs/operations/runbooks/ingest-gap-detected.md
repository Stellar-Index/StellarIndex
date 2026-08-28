---
title: Runbook — stellarindex_ingest_gap_detected
last_verified: 2026-08-28
status: ratified
severity: P1
---

# Runbook — `stellarindex_ingest_gap_detected`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingest_gap_detected` |
| Severity | P1 (page) |
| Detected by | `stellarindex_ingest_gap_max_size_ledgers{source} > 1000` for 15 min |
| Typical MTTR | 30 min — 4 h (depending on gap size and bucket reachability) |
| Impact | A contiguous block of soroban_events ingest is missing. Per-decoder coverage for every Soroban source is incomplete across the gap window; price-history queries spanning the gap return holes. |

## Symptoms

- Prometheus alert `stellarindex_ingest_gap_detected{source="soroban-events"}` is firing.
- `stellarindex_ingest_gap_max_size_ledgers{source="soroban-events"}` reports the size of the largest contiguous gap.
- The status page's per-source density may still show 100% — cursor-derived density measures process state, this alert measures data state.

## Triage — 5 minutes

1. **Get the exact gap list:**

   ```sh
   ssh root@<region-host>
   stellarindex-ops find-data-gaps --config /etc/stellarindex.toml --output text
   ```

   Output prints each `[from, to]` range + a ready-to-paste `stellarindex-ops backfill` command per gap.

2. **Classify the gap pattern:**
   - **One large contiguous gap (>50 K ledgers)** → cascade signature. Suspect ingest halt (Redis MISCONF, Postgres back-pressure, AsyncSink wedge). Cross-check F-0020-cluster alerts (`stellarindex_redis_writes_blocked`, `stellarindex_postgres_connections_high`).
   - **Many small gaps (each ~100-500 ledgers)** → flaky-write pattern. Suspect MinIO blip, transient ledgerstream reconnect, or a partial-batch sink failure.
   - **Single small gap at the trailing edge** → likely active backfill or a brief Postgres pause; re-check in 5 min before acting.

3. **Confirm the source is actively ingesting NOW:**

   ```sh
   ssh root@<region-host> 'sudo -iu postgres psql -d stellarindex -c "SELECT MAX(ledger_close_time), MAX(ledger) FROM soroban_events;"'
   ```

   If `MAX(ledger_close_time)` is fresh (within ~30 s) the writer is healthy and the gap is historic; if stale, the writer is still wedged and the gap is growing.

## Remediation

### Healthy writer + historic gap

Run the targeted backfill commands the diagnostic emitted:

```sh
stellarindex-ops backfill --config /etc/stellarindex.toml \
  --from <gap.start> --to <gap.end> --source soroban-events
```

One invocation per gap. Each ~92 K ledger gap takes 15-30 min on r1. Confirm the gauge drops by re-running `find-data-gaps` or watching `stellarindex_ingest_gap_max_size_ledgers` decay.

### Wedged writer + growing gap

This is the F-0020 cascade pattern. Pause heavy walks (any running `stellarindex-ops backfill -source soroban-events` invocation — `pkill -INT -f 'stellarindex-ops backfill'`; `verify-archive-tier-a.service`) per `docs/operations/backfill-with-live-ingest.md`, then:

1. Check Redis (`redis-cli info persistence` — `rdb_last_bgsave_status: ok`?).
2. Check Postgres (`SELECT count(*) FROM pg_stat_activity;` — saturated?).
3. Restart the indexer (`systemctl restart stellarindex-indexer`).
4. Watch the live cursor advance via `/v1/diagnostics/cursors`.
5. Once live ingest is recovered, schedule the historic-gap backfill above.

## Known false-positive patterns

- **First boot after rc.84+ deploy.** The detector's first cycle runs immediately on startup (light targets are scanned; the 6h-cadence `sdex`/`soroban-events` targets are scanned only if their cadence has elapsed since the persisted `gap-detector-scan` cursor, otherwise their last-known gauges are re-emitted from `source_coverage_snapshots`), so the gauge is non-empty before the first tick; if a historic gap is preserved from before deploy the alert fires within 15 min. Resolve via the standard targeted-backfill path.
- **Genuinely-empty mainnet window.** Soroban activity dipped briefly below the `min-gap-size=1000` threshold (~1.5 h of zero contracts). Vanishingly rare on mainnet post-2024 but possible during testnet experiments — lower the threshold flag if your network is quieter.

## Scaling landmine: detector targets with no leading-`ledger` index

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

## Related

- [projector-replay.md](projector-replay.md) — per-source projection-table repair via projector cursor rewind. Replaces the former `cascade-window-drain` orchestrator subcommand (ADR-0032 Phase 5).
- `docs/operations/backfill-with-live-ingest.md` — operational posture for running backfills alongside live ingest (F-0020 closure).
- F-0020 (audit-2026-05-26) — original cascade-window incident that motivated this detector.
- `stellarindex-ops find-data-gaps` — the operator-facing diagnostic this alert points at.
- `ingest-gap-detector-silent.md` — paired ticket-tier alert for when the detector itself wedges.

## Changelog

- 2026-05-28 — initial draft alongside the gap detector worker ship.
