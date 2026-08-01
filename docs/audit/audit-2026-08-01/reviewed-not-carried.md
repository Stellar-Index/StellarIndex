# Audit №3 — reviewed and NOT carried forward (refuted/downgraded)

Recorded, not deleted, so false-positive drift is visible and the skeptic pass is auditable.

## R2 — "5 unauth account routes read the 8-conn CH pool inline; fabricated-key churn saturates it" — REFUTED
Skeptic aae/DoS, two independent fatal exonerations:
1. 2 of 5 routes don't touch the CH pool: /trades (Postgres ListAccountTrades,
   account_trades.go:138) and /positions (Postgres; CH only via per-fold label
   resolver, and a fabricated key returns zero fold rows → ZERO CH reads).
2. The 3 genuine CH routes do PK-PREFIX reads, not scans, since the 2026-07-30
   ops_by_source rewrite: sourced arm WHERE source_account=? (explorer_reader.go:747),
   participant arm WHERE account=? (:842), movements WHERE address=? (equality on
   ORDER BY). Old bloom-scan was 6.17s; PK arm 0.056s. Fabricated key → empty
   PK prefix → fast empty. The "cheap-request→unbounded-scan" amplification was
   the PRE-projection state; ops_by_source IS the fix.
Bounds regardless: 8s per-request deadline + max_execution_time:30 server-side;
gate caps DETACHED to 4 of 8, guaranteeing ≥4 inline conns (finding read it
backwards); max_threads:4 pin; 100 req/s/IP of sub-10ms empties → concurrency ≪1.
Residual honest caveat: a multi-IP botnet defeats per-IP limiting, but the finding
specified a single client and even then reads are empties not scans.
LESSON: recon over-claimed the DoS surface against a fixed subsystem — the
completeness-push projection already closed it.

## R10 — "/positions 6 sequential uncached PG reads → 600 q/s exhausts the 25-conn pool" — REFUTED
Skeptic aae/DoS. Facts accurate (6 sequential uncached folds, positions.go:228-233)
but the mechanism doesn't follow:
1. Sequential ≠ 6 connections: one request holds ONE conn at a time; conns consumed
   = concurrent requests, not queries/request. "600 q/s" conflates query count with
   conn holds.
2. Every fold is a sargable user-leading-index lookup (migration 0107; blend_positions
   _user_ts_idx etc.) → fabricated key = sub-ms empty index probe, not a scan.
3. statement_timeout (OpenServing, serving.go:35; default 30s, Validate enforces
   > request_timeout) backstops every hold.
100 req/s × one conn × tens-of-ms indexed empties vs 25 conns → concurrency a
handful. Worst case = transient acquire-queue resolving within the 8s deadline
(honest 503), not exhaustion. Residual = a minor latency/efficiency note (6
sequential round-trips is chatty), NOT a saturation vector.

## M-A — float64 usd_value on /v1/accounts — REFUTED as a material defect (LOW nit)
Skeptic a400/money. Mechanics all confirmed (float64 end-to-end, served+user-visible
at openapi:8124, no lint/test reaches it). But magnitude walked: float64 ~2.2e-16
rel; worst input SDF burn ~$11.3B → ~$1e-4 absolute error, 3-4 orders below the $0.01
display grain; a reorder needs two accounts within ~1e-15 rel (render identical
anyway). ADR-0003 protects raw i128 QUANTITIES carried to the API; usd_value is a
derived ranking aggregate (underlying balance stored NUMERIC, served as strings
elsewhere) — convention-adjacent, not the truncation class. Kept as a LOW follow-up
(a frontend/deploy-CH lint gap), not a MED-floor money defect — no wrong outcome
materializes. [Contrast: the ingest recon's guard-scope gap is the real lesson — the
SQL lint doesn't cover deploy/clickhouse/*.sql; that's worth a lint extension.]

## F-2 partial — backfill_chainlink + backfill_external — REFUTED
Skeptic a400/money. Neither chainlink (oracle_updates) nor external CEX/FX (trades)
is in buildReconciliationCatalogue; both vendor-API-sourced, not lake-derived → no
lake to reconcile, no projection claim to carry → no false certification possible.
The ch-rebuild + backfill-router half of F-2 stands (see findings.md).

## F-6 — redstone oracle_updates dual-write StateWriteKeys divergence → misattribution/phantoms — REFUTED
Skeptic afaa/ingest. Sub-claims check out (dispatcher writes redstone under
SkipSoleWriter; redstone reconcile is aggregate/netting; PK excludes asset) BUT the
corrupting divergence is UNREACHABLE:
1. The two provenances mirror the same data — lake ledger_entry_changes is populated
   from the same LCM meta the dispatcher reads; correlated, not independent oracles.
2. The "dispatcher has keys ∧ lake nil" window is EMPTY: state-write plumbing is recent
   (2026-07-31) so live only stamps keys on TIP ledgers (above the ~63.05M floor where
   lake coverage is 100%); below the floor the dispatcher never stamped keys either.
3. Concurrent dual-write is TIP-ONLY (100% coverage, both decode identically); below
   the floor only re-derives run (no race).
4. Attribution is AGREE-OR-REFUSE: equal-arity → identical output or whole-event refuse
   (ErrStateWriteFeedMismatch, 0 rows); subset → same unique subset or ErrAmbiguousSubset.
   No branch emits different assets to the same PK; op_index fans per vector position so
   the maps align 1:1. "First-writer-wins-a-wrong-value" cannot occur.
5. The one real residual is expected-COUNT-only (a re-derive over partial-coverage could
   falsely refuse → Expected<Actual) and is the documented/accepted CS-084 netting
   tradeoff; redstone verified complete=true full-range 2026-08-01.
The finding's causal chain is backwards: a lake HOLE→nil keys→skip corroboration makes
the lake side MORE permissive (accept), so it can't make the re-derive REFUSE what the
dispatcher accepted.

## F-4 — comet/phoenix legacy twins (migrations 0059/0060) — REFUTED
Skeptic afaa/ingest. The mechanic is genuinely identical to cctp 0112 (DEFAULT 0 +
PK-extend + "no DELETE needed"), BUT the outcome doesn't materialize:
1. comet + phoenix use STRICT per-ledger reconcile (no aggregateReconcile field,
   reconciliation_catalogue.go:164-172), unlike the oracle sources.
2. ReconcileCounts (reconcile.go:363-384) flags Actual>Expected per ledger as a phantom
   ProjectionGap → complete=false — the same net that caught the 19,366 cctp twins.
3. These tables carry NO retention → the reconcile covers their full served range
   (compute_completeness.go:622-632 verifies no target has retention).
4. Provably clean NOW: comet + phoenix complete=true, lake_complete=true full-range
   2026-08-01, where complete means row-EXACT per-ledger match — a twin would break it.
"no DELETE needed" is correct for the migration STEP (ON CONFLICT already deduped at
ingest; migration only adds column+PK). Any twin a re-derive COULD create is not silent
for these tables — strict reconcile surfaces it. Residual: reconcile is count-only
(DAT-15), so a twin masked by a coincident same-ledger drop is possible — the documented
value-vs-count limit (= recon D-1), not F-4's systematic twin-creation.

## R1 downgrade note
CONFIRMED as a fact but re-classed INFO (config correction, not a defect) — kept in
findings.md under informational.

## R4/R7 downgrade notes
R4 HIGH→MED (Caddy access log is a parallel detection surface; throttle intact).
R7 HIGH→LOW/MED (GATED on a prior host foothold; subsumed by the secret access it
requires). Both kept in findings.md at recalibrated severity.

## W2-pricing-F1 — /v1/observations unbounded detached full-scan DoS — REFUTED (DoS part)
Finder ab49 claimed each distinct-pair observations request spawns a detached 30s
full-trades-hypertable scan → pool exhaustion. Orchestrator refuted: migration 0037
added trades_pair_source_ts_idx (base_asset,quote_asset,source,ts DESC,ledger DESC),
which EXACTLY matches LatestTradePerSource's WHERE base=$1 AND quote=$2 ORDER BY
source,ts DESC,ledger DESC → an empty/nonexistent pair is a fast index SEEK to zero
rows, not a scan. The ">60s empty" measurement predates 0037; observations.go:115's own
comment says "index-covered". No connection-pool amplification. The residual
unbounded-MEMORY-growth (no cache eviction) is carried as W2-pricing-1 LOW.
