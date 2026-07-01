---
title: D1 — Structural soundness — findings + target decomposition
---

# D1 — Structural soundness

**Headline:** well-structured where designed up front (per-source convention,
projector single-writer, external framework, storage tiering), but with **2
"ambiguous home" hazards** (FX feeds; supply observers) and **3 flat god-packages**
whose insertion points are obvious but whose internal decomposition hasn't kept
pace. **96 packages exist; CLAUDE.md names ~33 (3× undercount).**

## M0 — causes rework
- **M0-1 — FX/fiat feeds have TWO homes + TWO frameworks (an existing duplication).**
  `internal/sources/external/{ecb,exchangeratesapi,polygonforex}` use the
  `external.Connector` framework (wired into the **indexer**); `internal/sources/forex/`
  + `internal/sources/frankfurter/` are a bespoke worker with their own `FXQuoteWriter`
  seam (wired into the **API binary**). `frankfurter` wraps the ECB-backed Frankfurter
  API while `external/ecb` is ALSO ECB-backed → overlapping upstreams via two code
  paths. "Add a new FX source" has two plausible homes → the next one gets built wrong
  or duplicated. **This is a live example of the rebuild-what-exists pain.** → also
  feeds D3. **Fix:** fold `forex`+`frankfurter` into `external/`, reconcile with `ecb`.

## M1 — friction
- **M1-2 — "supply observer" is split across `internal/sources/*` (observer half:
  accounts/trustlines/claimable_balances/sac_balances/liquidity_pools/sep41_*) AND the
  flat 29-file `internal/supply/` (refresher half).** Adding one touches both trees +
  main.go + a migration; "is a supply observer a source or a supply thing?" = both.
- **M1-3 — `internal/sources/` is a grab-bag:** 5 kinds are flat siblings (Soroban
  DEX decoders, classic ledger observers, the `sorobanevents` landing zone, the
  `childgate` shared registry [infra], and the `forex`/`frankfurter` feeds). Only
  `external/` is sub-grouped.
- **M1-4 — the documented "five-file convention" has drifted into two undocumented
  ones:** README (Soroban DEXes) vs `doc.go` (classic observers); `consumer.go` absent
  from defindex/sdex/sorobanevents/forex/frankfurter/childgate; **defindex has none**.
  "Copy the template" has no single referent. (Note: `dispatcher_adapter.go` IS uniform
  across all sources — that hypothesis was refuted.)
- **M1-5 — `cmd/stellarindex-ops` is a 41-file / 52-case flat switch** in one
  `realMain()` carrying `//nolint:gocyclo,gocognit,funlen`; no subcommand registry →
  merge-conflict magnet on a shared file.
- **M1-6 — `internal/storage/timescale` is a 194-method god-`Store`** (file-per-table
  split is good, but no compile-time domain isolation — explorer reader == billing
  reader). Ceiling, not rework.
- **M1-7 — `internal/api/v1` is 76 flat non-test files;** extraction began (middleware,
  dashboard*) but the `explorer_*` cluster (10 files, zero shared state with pricing)
  is the obvious next extraction.

## M2 — polish
- **M2-8** CLAUDE.md map 3× undercount + documents the drifted convention (ties D4).
- **M2-9** `childgate` (shared contract-id registry) misfiled under `sources/` → belongs
  in `internal/contractid`.

## Target decomposition (worst offenders)
1. Fold `forex`+`frankfurter` into `external/`, one FX framework (M0-1).
2. `childgate` → `internal/contractid` (M2-9).
3. Ratify ONE doc convention (`doc.go`) + the real per-sub-kind template in CLAUDE.md (M1-4).
4. `cmd/stellarindex-ops` → `internal/ops/{ingest,archive,discovery,supply,diagnostics,clickhouse}` each `Run(ctx,args)`; main.go = thin router (M1-5).
5. Extract `api/v1/explorer_*` → `internal/api/v1/explorer` (M1-7).
6. (low priority) split `timescale.*Store` only if compile-time isolation becomes valuable (M1-6).

## Already GOOD (don't churn)
Per-source code shape uniform across 25 pkgs; `dispatcher_adapter.go` uniform; projector
single-writer (tiny, legible); `external.Connector` framework + Registry; storage tiering
(timescale/clickhouse/redisclient); api middleware + dashboard extraction (right direction);
small single-purpose kernel packages (canonical/consumer/cachekeys/scval/obs/events).

**Growth-readiness:** on-chain source ✅ · CEX connector ✅ · **FX source ❌ ambiguous
(M0-1)** · **supply observer ❌ split (M1-2)** · API endpoint 🟡 · storage reader ✅
(god-struct ceiling) · ops subcommand 🟡 (52-way switch).
