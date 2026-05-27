# W30 — Cold-tier read path (ADR-0027)

## Scope

The cold-tier datastore wiring + read path:

- `internal/ledgerstream/seamed.go`, `tiered.go`
- `internal/pipeline/datastore.go::LedgerstreamConfig`
- ADR-0027 (LCM cache tiering) text vs implementation
- `cmd/ratesengine-ops/galexie-archive-trim` subcommand (if it
  exists; otherwise: trim flow doc)
- `configs/healthchecks/galexie-archive-trim.timer` (if it
  exists)
- the §3+§4-together invariant per memory
  `feedback_cold_tier_premature_enable`

## Inputs

- ADR-0027 (`docs/adr/0027-lcm-cache-tiering.md`)
- `internal/ledgerstream/seamed.go` + `seamed_test.go`
- `internal/ledgerstream/tiered.go` + `tiered_test.go`
- `internal/pipeline/datastore.go`
- `docs/operations/lcm-cache-tiering.md`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W30.1 | ADR-0027 invariants match `seamed.go` / `tiered.go` semantics | per-claim cross-ref |
| W30.2 | `TieredDataStore` wraps hot + cold; reads miss locally → fallback to cold | `tiered.go` code |
| W30.3 | Writes ALWAYS target hot (no cold writes) | `tiered.go` |
| W30.4 | Cold init failure degrades gracefully (Warn + hot-only path) — does NOT abort | `streamTiered` code path |
| W30.5 | `LedgerstreamConfig` only attaches ColdDataStore when (a) operator opted in via config AND (b) the bucket is the archive bucket (not live) | `internal/pipeline/datastore.go` |
| W30.6 | Cold-tier reads are hashed-chain-verified equivalently to hot reads (no special-casing that would let a hostile cold endpoint poison) | code path |
| W30.7 | Per memory `feedback_cold_tier_premature_enable`: ADR §3 (enable) and §4 (trim) must be operationally tied | runbook + ADR text |
| W30.8 | If §3 is enabled without §4, the only failure mode is unbounded local galexie growth (no correctness break) | ADR text + runbook |
| W30.9 | If §4 trims a partition while a backfill walker is mid-flight, the walker falls back to cold transparently | integration test or live behaviour analysis |
| W30.10 | On r1: cold tier currently NOT enabled (per memory) — verify | r1 probe: `grep cold /etc/ratesengine.toml`; check ColdTieringEnabled |
| W30.11 | TolerateTrailingMissing interacts correctly with TieredDataStore (the missing-file fallback applies to BOTH tiers) | code review |
| W30.12 | metric: `tier_read_total{tier="hot"|"cold"}` registered + scraped | metrics catalogue |
| W30.13 | metric: `cold_read_duration_seconds` registered + scraped | — |
| W30.14 | When cold tier IS enabled on r1, the §4 trim subcommand has been run + verified before any data is trimmed | operator runbook |
| W30.15 | The `trim-galexie-archive` subcommand's `--verify-upstream` flag actually verifies presence in cold before deleting locally | code path |

## Evidence expectations

- Per-claim ADR-0027 line excerpt + matching code line.
- `streamTiered` walk-through with cold fallback.
- r1 probe showing whether cold tier is enabled.
- metric scrape proof.

## Closure criteria

Every check terminal. Findings on:

- any cold-init failure that aborts (should degrade)
- any §3-without-§4 footgun (the runbook must enforce together)
- any path where a hostile cold response can poison data
  without detection
