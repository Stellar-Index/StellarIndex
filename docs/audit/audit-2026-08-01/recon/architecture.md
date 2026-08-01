# Architecture map — Stellar Index @ HEAD f8c099ee (2026-08-01)

Cold re-derivation for the recipe refresh. Supersedes docs/audit/recipe.md
§§1-6 (2026-07-16 @ f84e2d0b). `git diff f84e2d0b..f8c099ee` = 1,244 files,
+139,589/-11,725, 737 commits. (Full detail in tasks/a4f1afb1a751b04f6.output.)

## Scale deltas
Go files 1,339→1,534 · migrations 108→120 (through 0125) · ops subcommands
55→60 · routes 120→132 · alert rule files +4 new · runbooks 140.

## Topology — CONFIRMED unchanged
ONE Hetzner host R1 (136.243.90.96), deploy enum still `[r1]`, R2/R3 examples
only, HA roles (haproxy/patroni/redis-sentinel) exist but NO playbook invokes
them, **Redis has NO AUTH** (16-prometheus-exporters.yml:54).

## In-process-authoritative state: ~2 → ≥10 (mostly on the SERVING path)
SDEX order book map (sdex_orderbook.go:83) · rate-limit in-proc fallback
(inprocess.go, localStoreMaxKeys=100_000) · login/signup throttles (deliberate
nil-Redis) · protocol-detail SWR cache · 6 explorer SWR caches · detached-refresh
gate (refresh_gate.go:33, limit 4) · markets/network-stats/dex-tvl/decimals caches.
The "safe today because single instance" claim now covers 5× more surface.

## THIRTEEN CONTRADICTIONS with recipe §1 (the core recipe-refresh output)

- **C-1** Rate-limit authority is NO LONGER purely Redis: `ratelimit.New(nil,…)`
  is an in-process fixed-window bucket (can't fail open), selected when rdb==nil
  AND used unconditionally for login/signup throttles even when Redis exists
  (bucket.go:137,169; inprocess.go; main.go:1932,1977).
- **C-2** NEW authoritative record: the served SDEX order book is an in-process
  Go map on the API process with its own quarantine set — no durable store, lost
  on restart, divergent per instance (sdex_orderbook.go:64-95).
- **C-3** NEW ClickHouse store `stellar.ops_by_source` is now authority for
  account-history key resolution, and readers FAIL CLOSED without it
  (errOpsBySourceMissing, explorer_reader.go:1018). Its DDL is operator-applied
  (deploy/clickhouse/ops_by_source.sql), NOT in migrations/ — version-coupled
  deploy-order hazard; a fresh region or table rollback darkens /v1/accounts/{g}/*.
- **C-4** Completeness verdict now depends on TWO more PG tables that can block or
  force it: completeness_target_floors (0116) + projection_dirty_windows (0125,
  fail-closed if unreadable).
- **C-5** FIXED/superseded: the recipe's trap-8 hardcoded retentionStart floor is
  gone, replaced by per-target data-derived targetScope + 0116 durable floor
  (compute_completeness.go:594-650).
- **C-6** PG `classic_movements` table DROPPED (migration 0113) + its Go file
  deleted — every recipe line referencing it is stale.
- **C-7** soroban_events PG landing zone still written UNCONDITIONALLY by the
  indexer (no config gate; main.go:351-384) — clickhouse_projector_source only
  switches the READ. Dual-write permanent until code changes.
- **C-8** API-key authority stated BACKWARDS: r1 runs the Redis backend; Postgres
  is the cutover validator and DISABLES /v1/account/keys when selected
  (main.go:479-496).
- **C-9** All three OpenAPI generators (docs-api, postman, types.ts) are now
  CI-drift-gated + verify.sh runs dual-mode gitleaks — making CLAUDE.md:610-616
  ("postman + types.ts NOT drift-guarded") factually WRONG (doc-drift finding).
- **C-10** 60 ops subcommands not 55; tmpxdrdump gone; new MUTATING verbs
  freeze-unfreeze (moves served prices) + usage-rollup-backfill (billing-adjacent).
- **C-11** The ledger walk now runs THREE divergent backpressure policies: PG
  events sink BLOCKS (dispatcher.go:545), CH LiveSink DROPS whole ledgers
  (live_sink.go:41), external connectors drop.
- **C-12** In-process-authoritative state ~2 → ≥10, most on the serving path.
- **C-13** Operator-applied CH artifacts outside migrations/ grew into a PATTERN:
  ops_by_source.sql, ttl_live_until.sql, ledger_entries_current_intra_ledger_seq.sql,
  lake-dedup-driver.sh — and at least one (C-3) now gates binary functionality.

## Six seams that concentrate interaction bugs (was 4)
1. ledgerstream→dispatcher→8 sink workers (3 backpressure policies now, C-11).
2. dispatcher↔projector SinkMode truth table + lockstep (17 sources) + NEW
   sinkfault quarantine + adaptive window shrink.
3. aggregate→Redis→api (+ triangulate_corroborate.go 7th confidence factor, freeze lifecycle).
4. **NEW: dual state-write-keys derivation** — internal/dispatcher/state_write_keys.go
   (LCM) vs internal/storage/clickhouse/state_write_keys.go (lake): ONE rule written
   TWICE, reconciled against itself by compute-completeness. Only redstone opts in.
5. **NEW: SWR/prewarm layer** — 6 explorer caches whose failures are invisible by
   construction (metrics.go:3246) + protocol-detail cache's "degraded may not displace
   healthy" guard (protocols.go:113-124, itself a 2026-07-31 incident) + bounded refresh gate.
6. **NEW: in-process order book** with lake-arbitrated zombie quarantine — a
   CORRECTNESS rule (intra_ledger_seq==0 → exclude) expressed in cache-eviction code.

## Highest-value audit targets (from this map)
1. The two state-write-keys implementations (divergence → silent redstone
   misattribution between live and replay).
2. ops_by_source deploy coupling (operator DDL + fail-closed reader + no fallback + not in migrations).
3. The SWR honesty layer (6 caches, failures invisible; protocol-detail degraded-displace guard).
4. In-process order book as a served money-adjacent product surface.
5. projection_dirty_windows fail-closed paths (new hard-stop + indefinite-pending modes).
6. Bespoke analytics string formatting — 3,256 NEW lines where the store (not
   canonical.Amount) owns money formatting, OUTSIDE TestI128TruncationGuard blast radius.
7. freeze-unfreeze + usage-rollup-backfill (mutate served prices / billing counters).

## Authority-per-entity corrections (verified against writers)
trades→PG trades (unchanged) · prices→PG CAGGs+Redis · ledger_entry_changes→CH
ONLY (reinforced, 0113 dropped PG twin) · soroban_events→STILL DUAL-WRITTEN
(C-7) · completeness→PG +2 new tables (C-4) · api keys→Redis default (C-8) ·
rate-limit→conditional (C-1) · **order book→NEW in-proc map (C-2)** ·
**ops_by_source→NEW CH store, fail-closed (C-3)** · tx_hash_index→CH (+ hourly parity assertion).
