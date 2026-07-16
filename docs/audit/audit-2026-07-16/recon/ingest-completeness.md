# Recon: ingest + storage + completeness/projection (HEAD f84e2d0b)

## Data flow (verified seams)
- Galexie MinIO → `ledgerstream.StreamArchiveThenLive` (single goroutine, ordered) → per-LCM callback (indexer main.go:616-656).
- Callback order per ledger: (1) ProcessLedger→dispatcher, write `ledger_ingest_log` substrate + advance cursor **AFTER enqueue to sink channel, NOT after durable in PG** (main.go:1457-1466, sink.go:139-150); (2) hashdb append (sync best-effort); (3) CH dual-sink (non-blocking, drops on full buffer).
- Dispatcher 4 seams first-match: Soroban events (raw-event sink PushEvent BEFORE decode, may block=backpressure), contract-call auth-tree (band/soroswap_router), LedgerEntryChange (supply observers), classic ops (sdex). No decoder writes DB (except classicmovements→ops writer).
- PG sink: events channel cap 256, drained by **8 PersistEvents workers** (per-source order NOT preserved, PK dedup makes safe). Trade-shaped batch; else per-event type-switch HandleEvent. Unhandled kinds counted+logged, never silently dropped.
- CH sink: writes child tables then **`stellar.ledgers` LAST as per-ledger commit marker** (I6). All lake tables ReplacingMergeTree(ingested_at), partition intDiv(ledger,1e6), NO TTLs.
- Projector (ADR-0031/0032): one goroutine/source, per-source cursor, decodes via SAME dispatcher.Decoder, reads CH `contract_events` by default (ClickHouseProjectorSource=true). IsProjectedEvent split kept in lockstep by AST test.

## IDEMPOTENCY / DEDUP — the systemic trap
- **Every event table: ON CONFLICT (identity) DO NOTHING; NO amount/price/reserve/volume in ANY PK/UNIQUE (repo-wide sweep).** trades (source,ledger,tx_hash,op_index,ts), oracle_updates same, sep41, classic_movements, all protocol tables. **CONSEQUENCE: a replay/re-derive computing a DIFFERENT amount/price for the same identity is silently absorbed — the stored wrong value is NEVER corrected by replay. Only DELETE/TRUNCATE + re-derive fixes it.** (This is the ROADMAP re-derive trap, now confirmed systemic.)
- DO UPDATE exceptions (mutable): ledger_ingest_log (re-census corrects counts), ingestion_cursors (monotonic), completeness_snapshots (no-regress WHERE), sep41_supply_rollup (**additive fold** mint_total+=… keyed on watermark → the KALE 2× double-fold incident, now guarded by ReconcileRunningTotals + auto-reset in ch-rebuild -sep41).
- mev_events dedup on partial index WHERE dedup_key IS NOT NULL → NULL-key rows duplicate freely.
- CH RMT dedup is EVENTUAL (merge-time); readers handle un-merged dups 3 ways (FINAL / adjacent-identity O(1) dedup relying on ORDER BY / GROUP BY argMax). Ad-hoc queries may not.

## COMPLETENESS — all 3 ADR-0033 claims REAL + implemented
- Substrate (Claim 1): ledger_ingest_log per-ledger census (decoder-INDEPENDENT 2nd LCM walk so a dispatch bug can't mask itself); CH SubstrateProblem windowed contiguity+hashchain.
- Recognition (Claim 2a): AuditRecognition vs Dispatcher.Recognize; CH always full-history from sorobanEraGenesis 50457424.
- Projection (Claim 2b): decoder re-derive vs served counts per-ledger (strict since CS-084); oracle sources totals-compare w/ netting residual; SDEX decoder-census; event-less call sources distinct-PK census.
- **Two-axis verdict IMPLEMENTED** (not just planned): `lakeComplete := srW.Complete` before projection eval (compute_completeness.go:257-268); migration 0108 adds `lake_complete`; commit 1d56d5b2. **UNVERIFIED: whether /v1/coverage surfaces the lake axis (DECISION item 2 may be open) — web/plans agent to confirm.**

## INVARIANT TIERS (recipe §5 seed; weakest writer sets tier)
| # | Invariant | Tier | Note |
|---|---|---|---|
| I1 | One writer per Soroban domain (projector) | **C (convention)** | runtime+test for 2 live writers; ops re-derives (ch-rebuild/reproject/projected-rebuild/backfill/COPY-merge) are convention, absorbed by PK. projected-rebuild -allow-live-overlap bypass exists |
| I2 | Ledgers contiguous + hash-chained | **W (watcher)** | post-hoc detection ONLY; ingest never blocks on a gap; hashdb opt-in (default off) |
| I3 | Coverage data-derived not cursor-derived | **W** | edges: ledger_ingest_log written on ENQUEUE not durability; **projector advances cursor past decode AND sink failures** (see LEAD); ch-live-catchup TIP=max over ALL cursors |
| I4 | Money columns NUMERIC / integer-exact | **DB** | every PG money col numeric+sign CHECK; CH Int128; *big.Int end-to-end. float64 only off money path (blend APR display, confidence, coverage_pct) |
| I5 | No amount/price in dedup key | **DB** | corollary: DO NOTHING never corrects values |
| I6 | CH ledgers row = commit marker | **C/RT** | code ordering + comments, NO test asserts the ordering |
| I7 | Cursor monotonicity | **RT** | in-statement WHERE guard |
| I8 | Verdict no-regress | **RT** | CS-083 WHERE guard, single writer |
| I9 | No RPC in ingest | **CI lint** | |
| I10 | Raw-event capture precedes decode | **RT+W** | |

## TOP LEADS for finders
1. **LEAD (DAT/COR, high):** projector cursor advances unconditionally on stream success (projector.go:393-401) — but its doc (projector.go:91-93) claims "does not advance cursor for that row" on sink failure. A 60s-deadline-abandoned HandleEvent insert → row lost, cursor moved past → only caught by reconcile. Doc contradicts code = silent projection loss.
2. **LEAD (DAT):** G12-03 — live dual-sink NEVER populates `stellar.ledger_entry_changes` (sink.go:437-440); only ch-backfill does. reconcile-balances + verify-contiguity Check-2 treat it as substrate → their freshness silently bounded by backfill cadence.
3. **LEAD (COR/completeness):** retentionStart=tip−1.5M hardcoded "~90d" (compute_completeness.go:196-199) contradicts the repo's own DECISION doc (trades has NO retention). retentionFloor further shrinks reconcile to first-served → served-tier history loss below min-served is INVISIBLE to the `complete` axis. DECISION item 3 (fix to actual-min-served) NOT implemented.
4. **LEAD (DAT/guardrails):** classic-movements-backfill (ops) bypasses pipeline.HandleEvent AND the lockstep AST guard AND the ADR-0033 catalogue (no account_movements entry). Verification only optional -verify (default FALSE). A whole write path outside every guard.
5. **LEAD (CON):** routed_via.go:91 UPDATE trades from the sweeper races the projector's inserts on a projector-era table (tag-only, acknowledged main.go:251).
6. **LEAD (durability):** hard crash before drain loses up to channel-cap + 8×200 buffered trades while cursor stays advanced; -resume won't replay; recovery is reconcile+re-derive from lake (ADR-0041 accepts this).
7. **LEAD (drift):** ced-v2 DDL (contract_events_daily_v2.sql) is OPERATOR-APPLIED, not in migrations/ → deployed-vs-repo drift. Also `recognition` system snapshot hardcodes SubstrateOK:true (synthetic row, different semantics same table).
8. **LEAD (consistency):** migration 0096 blend_emitter_events `amount > 0` CHECK vs `>= 0` convention elsewhere — a zero-amount emitter event would be rejected.
9. Freshly-fixed uint64-underflow in coverage subtraction (33645e74) — re-probe siblings for the same saturating-subtraction class (contiguity_reader.go:140-157 now saturates).
10. hashdb verify sweep reads LIVE bucket only (main.go:1636) — archive-bucket drift missed.

## Migrations 0090-0108 notable
0092/0094 cctp CHECK 5→26 values; 0093 nonstandard_decimals (CHECK decimals<>7); 0096 blend_emitter amount>0 (inconsistent); 0098 phoenix_stake drops NOT NULL + widens CHECK; 0101 router call_path; 0102 sac_balance_seed_provenance; 0104 data-seed INSERT (aggregator router row — a data row in a migration); 0105 classic_movements (UNPOPULATED by design, superseded ADR-0048); 0108 lake_complete (no backfill, old snapshots read false until recompute).

## Lower-confidence corners (flag as NOT-fully-examined)
internal/archivecompleteness beyond doc.go; verify_contiguity.go/verify_lake.go bodies; gap-detector internals; aggregator/API binaries (separate agents).
