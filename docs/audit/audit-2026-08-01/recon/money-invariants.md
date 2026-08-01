# Money invariants — enforcement-tier re-derivation (audit-2026-08-01, HEAD f8c099ee)

Cold, read-only; every test BODY read (names not trusted). Tier ladder:
DB > RT (runtime fail) > T (CI test) > CI-lint > W (watcher) > C (convention).
Tier = the WEAKEST writer across ALL writers to the state.
(Full per-invariant ledger + Part 1-4 in tasks/ac73680d9ccd53a07.output.)

## The 7 weak-tier findings (candidates for the finder→skeptic wave)

- **M-A — HIGH — the only float64 money computation on a served surface.**
  `accountsByWealthQuery` computes `/v1/accounts.usd_value` as
  `sum(toFloat64(balance)/1e7 * price)` in ClickHouse, prices arrive as
  []float64, scanned into float64, FormatFloat onto the wire
  (account_state_reader.go:355-368; explorer/account_state.go:95-99,165-171).
  Magnitude negligible (~$0.000001 on an $11B row) — the TIER is the finding:
  convention-only, NO lint reaches it (lint-migrations.sh is migrations/*.up.sql
  only; ClickHouse DDL + inline SQL are outside it). Not a demotion (born at C,
  commit 4ddbf72b). Remedy: Decimal128 arithmetic + extend the SQL lint to
  deploy/clickhouse/*.sql.

- **M-B — HIGH — TWAP two-sided combine still trade-count-weighted.** The exact
  bug fixed on the VWAP twin (combineDirVWAP, volume-weighted, 2026-07-24
  9e72d89b, with TestCombineDirVWAP_IsNotTradeCountWeighted) was NOT propagated
  to TWAP: aggregates.go:576-578 still does Σ(twap·trade_count)/Σ(trade_count)
  PLUS the 1.0/twap inversion the VWAP path deliberately avoids (born 2026-07-05
  9730a4fd). The only test (storage_test.go:541) is SINGLE-direction, so the
  flipped-row combine is never exercised. Un-propagated fix, not a demotion.

- **M-C — HIGH — AmountScaleDecimals() default-8 trap, DEX-only guard.**
  framework.go:200-205 returns 8 on unset; the registry guard
  (onchain_decimals_test.go:25-45) iterates SubclassDEX ONLY — no assertion that
  SubclassFX⇒6 or SubclassCEX⇒8. A new FX venue registered without
  AmountDecimals:6 silently mis-values 100× (the CS-040 bug, re-openable) — and
  BOTH the trades path and the aggregator gate would take it.

- **M-D — MEDIUM — trades.usd_volume has NO CHECK** (migrations/0001:37) while
  its row-siblings base_amount/quote_amount carry CHECK(>0). Most-summed money
  column in the schema; domain is runtime-only (tradeUSDVolume returns nil for
  non-positive).

- **M-E — MEDIUM — lint-i128.sh outside the CI trusted-restore set**
  (ci.yml:270 restores lint-migrations + baseline-growth from base ref, NOT
  lint-i128). The Go-side money lint is self-weakenable in one PR; its SQL
  sibling is not.

- **M-F — MEDIUM — min_usd_volume dust floor SKIPPED for unpegged on-chain
  quotes** (orchestrator.go:1626-1633): WARN + counter, returns false (passes
  unguarded). Deliberate (failing closed would blackout a future legit unpegged
  quote) but an unguarded VWAP-manipulation surface. Secondary: the gate value
  is float64 AND persisted to price_source_contributions.volume_usd (NUMERIC fed
  from float64).

- **M-G — LOW-MED — bespoke_lending money sums have no numeric-safety shape
  test.** Its three siblings (dex/yield/oracle) each have an assert*NumericSafe
  guard; lending has only assertLendingCountsNotAmountSums (counts vs sums, not
  floats). Clean by inspection, unguarded against a future ::float.

## What HELD (verified, so the finder wave can skip re-deriving)

- The summer FX-scaling fix (usdVolumeDecimals reads md.AmountScaleDecimals(),
  NOT hard-coded 8) HOLDS in both the trades path (trades.go:431-451) and the
  aggregator gate (orchestrator.go:1400-1440), pinned by real assertion-bearing
  tests (the polygon-forex 1e6→"0.50000000" case explicitly).
- Re-derive corrective-never-destructive (M-5): DB+RT+T, the STRONGEST money
  invariant — derive_generation guard column + reDeriveNullVolumeGuard
  fail-closed at both write choke points + 6-case precision test.
- VWAP inputs = ClassExchange ∧ IncludeInVWAP, fail-closed Lookup; an
  unregistered venue can NEVER stamp a usd_volume (fail-closed on both axes).
- Stablecoin fiat-proxy is aggregation-time only, single map, quote-only rewrite
  (ADR-0026). Supply 3-domain all big.Int/NUMERIC (the toInt256 widening before
  the SEP-41 sum is the load-bearing overflow guard). CCTP/Rozo bridge divides
  by exact NUMERIC powers; mint_and_forward excluded from sums (no double-count).

## Demotion hunt — NEGATIVE results (recorded so next audit skips)

1. No money CHECK ever dropped (all DROP CONSTRAINT are PK-granularity or enum
   widenings). 2. No ::float/::double/::real in any serving-path SQL beyond two
   documented baseline casts. 3. lint-money:ok allowlist has 2 entries, shrink-only.
4. M-A born at C tier (not demoted). 5. M-B born trade-count-weighted (un-propagated fix).

## Guard SCOPE GAPS (why M-A survived — not findings, but the recipe must record)

- lint-migrations.sh: migrations/*.up.sql ONLY (CH DDL + inline Go SQL invisible).
- Its name regex omits median/mad/twap/vwap/rate/tvl/wealth/cap — a price-derived
  DOUBLE PRECISION statistic already entered uncaught (0007:40-43, 0008:20-24).
- lint-i128.sh matches one regex shape; the deep guard only XDR-parts conversions.
  Neither sees float64(x) on an already-decoded canonical.Amount.
