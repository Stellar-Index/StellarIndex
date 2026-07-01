---
title: D8 — Module boundary / dependency-direction
---

# D8 — Dependency direction

**Headline:** the internal graph is a **clean DAG (0 cycles, verified Tarjan)** and
the `pkg`/`cmd` boundaries hold today — but there's **one load-bearing inversion
(storage imports upward)** and **the import-lint enforces essentially none of the
layering** (incl. the ADR-0005 `pkg→internal` ban).

## Measured layers
L0 canonical/events/platform (foundation) → L1 config/scval/cachekeys/supply/xdrjson/
aggregate → L2 obs/consumer/metadata/divergence → L3 sources/dispatcher → L4 storage →
L5 pipeline/projector/completeness/orchestrator/api/v1 → L6 cmd. `pkg/client` imports
nothing internal.

## M0
- **M0-1 — `storage/timescale` (L4) is an INVERTED god-package importing L2/L3 compute +
  sources (~16 files).** `trades.go`→`aggregate`+`sources/external`; `supply.go`→`supply`;
  `mev.go`→`aggregate/mev`; `divergence_observations.go`→`divergence`; `freeze_events.go`→
  `aggregate/{anomaly,freeze}`; `blend_*.go`/`account_observations.go`→`sources/{blend,accounts}`.
  **Root cause:** the persisted domain structs (`supply.Supply`, `blend.PositionEvent`,
  `mev.StoredEvent`) + the Sink interfaces are owned by UPWARD packages, so storage must
  import up. Makes "storage is below compute" unstatable. **Fix:** move persisted structs
  to `canonical`/a new `internal/domain`, Sink interfaces to a neutral port — then the rule
  can be enforced.
- **M0-2 — the lint enforces ZERO layering; the `pkg→internal` invariant (ADR-0005) is
  UNGUARDED.** `lint-imports.sh` has only rules A (no rpc-in-ingest), B (xdr→scval), C
  (no-Horizon). No pkg-purity, no foundation-purity, no source-may-not-reach-storage/api,
  no cycle check. `pkg/client` is pure today but nothing stops a future edit breaking SemVer.

## M1
clickhouse→`sources/blend` (same inversion, smaller); **`auth`→`platform`** (auth can't be a
low primitive while it reaches the account domain — split the abstract Subject/Tier from the
postgres adapter); **baseline self-serve bypass** (any `(rule,path)` grandfatherable incl.
no-Horizon — CS-098); **`/decode.go` blanket exemption** (any decoder may import stellarrpc —
stale justification); aggregate near-cycle tangle (base `aggregate` pulled back down via storage).

## M2
`canonical/discovery` is depth-3, mislabeled foundational (relocate under sources/dispatcher);
`sources/external` is a de-facto foundational registry misfiled under sources/; observers import
`dispatcher` (lateral — the decoder interface + `*Context` belong in a neutral port); lint parser
misses dot-imports + `import(`-no-space (latent evasion).

## Proposed complete boundary rule-set (add to lint + a graph check)
1. `pkg/** → internal/**` forbidden (clean today; codify). 2. `canonical/` (excl. discovery) →
any `internal/` forbidden. 3. `sources/** → {api,storage,platform,pipeline,projector,aggregate}`
forbidden. 4. `storage/** → {aggregate,divergence,supply,sources}` forbidden (**VIOLATED** —
remediate M0-1 then enable). 5. `api/**` importable only by api+cmd. 6. nothing imports `cmd/**`.
7. **acyclicity gate** via `go list -deps` SCC in `make verify` (textual scan can't see near-cycles).
8. harden A/B/C: per-file allow instead of `/decode.go` blanket; forbid baselining no-Horizon;
fix the parser (or migrate the lint to a `go list`-graph engine that can express layering).

## Already CLEAN
No cycles (DAG); `pkg/client` pure; nothing imports cmd; **no source imports api/storage/platform**
(the most important rule holds); canonical base pure (stdlib+SDK only); config→canonical only;
A/B/C pass with 0 baseline entries.
