---
adr: 0041
title: Ingest durability semantics — cursor advance, lake sink defaults, drop visibility
status: Accepted
date: 2026-07-02
supersedes: []
superseded_by: null
---

# ADR-0041: Ingest durability semantics

## Context

The ledgerstream cursor advances when a ledger's events are enqueued, not when they are durably written, so a hard crash loses whatever was buffered.
The lake sink drops whole ledger extracts under buffer pressure by design, healed by `ch-live-catchup`. The "100% coverage" claim rests on the lake capturing everything.

## Decision

1. **Cursor semantics.** Keep advance-on-enqueue. Advancing only past the minimum durable watermark of all sinks would couple live throughput to the slowest sink. The loss window is one crash times the buffer depth, every lost row is recoverable from the lake, and the ADR-0033 per-ledger verdict detects any residue. The cursor is a resume hint, not a durability claim. The completeness verdict is the durability claim.
2. **Lake defaults on.** `clickhouse_live_sink` and `clickhouse_projector_source` default to `true`, matching the only production topology. A deployment that cannot run ClickHouse opts out explicitly and loses the certified-lake substrate, the CH completeness path and lake-derived supply.
3. **Drops are alerted.** `stellarindex_ingestion_ch_live_sink_drops` is a ticket (any drop increase over 10m, `for: 10m`). `stellarindex_ingestion_ch_live_sink_drops_sustained` is a page (drops persisting 1h, the heal path losing the race).
4. **Lake reads carry a watermark.** Lake-backed responses stamp `as_of_ledger` from the lake's contiguous watermark (`ExplorerReader.LakeWatermark`, clamped to the contiguous tip), and `flags.stale` fires when it lags.

## Invariant

- A change that weakens the verdict's ability to see a post-crash hole is a regression against this ADR, for example disabling the completeness timer or moving to aggregate-only reconciles.
- The two lake config defaults stay `true` (`internal/config/config.go`), and config validation rejects `clickhouse_projector_source = true` with `clickhouse_live_sink = false` (`internal/config/validate.go`).
- Sustained live-sink drops page. The two alerts are in `deploy/monitoring/rules/ingestion.yml` and `configs/prometheus/rules.r1/ingestion.yml`, and `deploy/monitoring/rule-tests/ingestion-core_test.yml` covers them.
- Lake-backed supply and state reads surface the lake watermark as `as_of_ledger` and set `flags.stale` against it.

## Consequences

- Fresh deployments capture the lake by default. A crash's loss window remains, but it is bounded, detected, healable from the lake, and visible within minutes through the drop alert.
- This covers lake-backed on-chain ingest only. The CEX/FX sinks do not flow through the lake and need their own backpressure treatment.
- Status note: Decision 1's "every lost row is recoverable and the verdict detects any residue" does not hold for the five LedgerEntry observer sinks (`accounts`, `trustlines`, `claimable_balances`, `liquidity_pools`, `sac_balances`). They are never a reconciliation source (`entryDecoderSourceNames`, `internal/ops/chops/reconciliation_catalogue.go`), and the indexer upserts the cursor and the observer watermark once a ledger is enqueued (`processAndPersistCursor`, `cmd/stellarindex-indexer/main.go`), so a row lost in a crash leaves current balance state wrong with no verdict, alert or flag. Nothing detects an unclean exit: the cursor rewind for abandoned rows (`rewindCursorForSinkLoss`) runs only on the clean-shutdown path, and no scheduled job re-seeds. Recovery after an unclean indexer exit is manual and partial; each command is a dry run without `-write`:
  - `supply seed-observations -write` restores `account_observations` for `[supply] sdf_reserve_accounts` only, not `watched_issuer_accounts` (`internal/ops/supply/supply_seed.go`).
  - `supply seed-claimable-balances -write` restores `claimable_observations`.
  - `supply seed-sac-balances -write` restores `sac_balances`.
  - Known limit: `trustline_observations` and `lp_reserve_observations` have no re-seed command. A row lost there stays wrong until that holder's trustline or that pool next changes on chain, and it feeds the classic supply sums (`SumTrustlineBalancesAtOrBefore`, `SumLPReservesAtOrBefore`) unflagged.
- Status note: the Consequences line "visible within minutes through the drop alert" covers buffer-pressure drops only. The drop counter is in memory (`internal/storage/clickhouse/live_sink.go`) and increments only when the sink sheds a ledger, so a hard crash's lost buffer never reaches `stellarindex_ingestion_ch_live_sink_drops*`, and the restart resets the counter. A crash hole in the lake is normally healed by `ch-live-catchup` (every 10 min); if that is failing, the first signal is the daily completeness verdict (05:30 UTC, then its alert's `for`). There is no crash-specific alert.

- Implementation note (2026-11-23), superseding the cursor claims of the first status note above: the indexer no longer commits the cursor on enqueue. `processAndPersistCursor` still enqueues each ledger without waiting, but it commits the cursor and both observer watermarks at `pipeline.LedgerMarks.DurableThrough`. That is the highest processed ledger at or below which every row the dispatcher sink lane was handed is durable: it landed, or the sink's permanent-fault policy (`persistEventResilient`, `TradeDroppedError`) dropped it (a deterministic data fault, a recovered panic, a `TradeDroppedError`). A dropped row counts as durable because replaying it would fault again. Decision 1's concern holds: processing never waits on a sink, and only the committed cursor lags, by about one ledger in steady state (`stellarindex_cursor_durable_lag_ledgers`). A crash, OOM or forced exit resumes below every unlanded lane row, including the trustline and LP-reserve observations, and replays them. Replay is ON CONFLICT idempotent for rows. Some diagnostic counters are not idempotent and over-count the replayed rows: `source_entry_counts`, `discovered_assets.event_count`, and `decoder_stats_5m` (`events_seen`, `decode_errors`, `orphan_events`, which add on conflict). Replayed trades the late-trade refresher classes as late trigger extra cagg refreshes; those are idempotent and cost only time. A clean shutdown that drains with nothing abandoned commits the processed ledger, so it replays nothing. The forced-exit branch needs no rewind. The lane's durable mark does not cover the soroban-events, discovery and ClickHouse live sinks, which keep their own recovery paths. One latent gap: a non-trade write that fails with a ctx error while the lane's ctx is still live is carried unacked until shutdown, which freezes the durable mark for that whole run. No sink write has an inner timeout today, so the case is unreachable; adding one must ack or abandon such a row rather than carry it.

## Evidence

`cmd/stellarindex-indexer/main.go` (`processAndPersistCursor`), `internal/storage/clickhouse/live_sink.go`, `internal/storage/clickhouse/protocol_reader.go`, `deploy/monitoring/rules/ingestion.yml`.
