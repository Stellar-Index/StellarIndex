---
title: Storage layering — eliminating the storage→domain upward imports (D8 M0-1)
last_verified: 2026-07-02
status: current
---

> **CORRECTION (audit 2026-07-16):** where this spec says the team decided NOT
> to create an `internal/domain` package, that is superseded — the D8 refactor
> (2026-07-10) DID create `internal/domain/` and moved the extracted structs
> there (storage→compute import-boundary violations dropped 15→6). Treat the
> "no internal/domain" statements below as historical.

# Storage layering spec (delegation-ready)

**Problem (maintainability audit D8):** `internal/storage/timescale`
imports UPWARD into compute + sources in exactly 13 files (verified
2026-07-02): `aggregate{,/baseline,/mev,/anomaly,/freeze}`,
`sources/{blend,sorobanevents,accounts,external}`, `supply`. The
storage layer knowing domain packages inverts the dependency
direction, and it is WHY `lint-imports.sh` cannot enforce any
layering rule today — the rule would fail immediately.

**Target (decided):** storage defines its own persistence row types
(the `*Row` naming convention the codebase already uses elsewhere);
the WRITERS (pipeline sink, projector, aggregator, supply refresher —
which already import both sides) convert domain→row at the call
site. We do NOT create `internal/domain` and do NOT move domain
structs into `canonical` wholesale — `canonical` stays the small
shared kernel; persistence shape is storage's own concern.

## The mechanical recipe (per file)

1. In the `timescale` file, define `type XRow struct {…}` mirroring
   exactly the fields the INSERT reads today (big.Int stays big.Int;
   NUMERIC discipline unchanged, ADR-0003).
2. Change the store method signature from the domain type to the row
   type.
3. At each caller (they are all in `internal/pipeline/sink.go`,
   `internal/projector/*`, `cmd/stellarindex-aggregator`, or the
   supply refresher — never in another storage file), add a small
   `toXRow(domain) timescale.XRow` conversion next to the call.
4. Delete the upward import from the storage file. Run the affected
   package tests + `go build ./...`.

## Work units (grouped to ~4 reviewable commits)

| Commit | Files | Upward imports removed |
|---|---|---|
| 1 — blend family | blend_auctions.go, blend_positions.go, blend_emissions.go, blend_admin.go | sources/blend |
| 2 — events + samples | soroban_events.go, topic_samples.go, account_observations.go | sources/sorobanevents, sources/accounts |
| 3 — aggregate family | baseline.go, mev.go, freeze_events.go, aggregates.go | aggregate/{baseline,mev,anomaly,freeze} |
| 4 — trades + supply | trades.go, supply.go | aggregate, sources/external, supply |

Commit 4 is the trickiest: `trades.go` uses `external.Lookup` (source
classification at persist time) — that LOGIC belongs in the caller;
move the classification result into the row (a plain string field),
not the lookup into storage.

## The payoff gate (do this or the refactor was decorative)

After commit 4, add to `scripts/ci/lint-imports.sh`:

```
storage-purity: internal/storage/** may import only
  canonical, cachekeys, obs, config, stdlib, drivers
```

with the shrink-only baseline mechanism for any transition remainder
(there should be none). The rule is the point; the 13 file moves are
just what unlocks it.

## Verification per commit

`go build ./...`; the affected packages' tests; the integration
round-trip tests for the touched tables
(`test/integration/*_storage_test.go` — they pin NUMERIC exactness);
finally `bash scripts/ci/lint-imports.sh` green with the new rule in
the last commit.
