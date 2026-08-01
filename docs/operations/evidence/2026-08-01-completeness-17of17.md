---
title: 🏆 COMPLETENESS 17/17 — every source complete=true, first time ever
captured: 2026-08-01 08:33Z (aquarius scoped full-range verify, v0.21.12)
command: stellarindex-ops compute-completeness -ch -source aquarius + all-source verdict table + public /v1/coverage
verdict: 🟢 17/17 complete=t AND lake_complete=t; /v1/coverage serves it publicly
---

# The verdict table (2026-08-01 08:33Z)

aquarius, band, blend, cctp, comet, defindex, phoenix, redstone,
reflector-cex, reflector-dex, reflector-fx, rozo, sdex, sep41_supply,
sep41_transfers, soroswap, soroswap-router — **all complete=true**.
Public `/v1/coverage`: 17 sources, 0 incomplete, per-source
`complete:true, lake_complete:true` with full verification details.

# What it took (the final week's ladder)

- redstone: 1,626 blind → empty-batch recognition → payload-median
  attribution → order-preserving alignment → **state-write exact
  attribution** (value-changing contract-data keys) → 0.
- soroswap: skim legacy twins (20, deleted) + non-directional-swap
  recognition + taker retro-fill to 100.00% → 0.
- aquarius: first-ever full-range reconcile surfaced 41 old-WASM blind
  events (40 zero-amount dust → recognized no-ops; 1 router v2 schema
  arm) + 480 pre-gating phantom rows from the ADR-0035-excluded
  parallel router (lake-classified, deleted) + 2 P27-SEV-window sink
  drops (replayed) → 0. En route: the projector's sink-side
  adaptive-shrink hotfix (v0.21.12) after a 3.5h dense-window wedge.
- Plus: 19,366 cctp legacy twins lake-classified + deleted; the
  carried-claim verifier gap closed with replay-rewind dirty windows —
  which PROVED THEMSELVES in this very verdict: "replay-rewind window
  [63400600,63743905] re-verified clean this run — clearing it".

# What the claim means (ADR-0033/0034 two-axis honesty)

lake_complete: the ClickHouse substrate holds every ledger, contiguous
and hash-chained from each source's genesis. complete: additionally,
the served tier's projection reconciles row-exactly against lake-derived
expectations across THE FULL RANGE THE SERVED TIER HOLDS — verified by
re-running the real decoders, not by trusting cursors. No carried
claims: aquarius/redstone/soroswap all re-verified full-range within
the last 24h, and replay rewinds now force re-verification by
construction.
