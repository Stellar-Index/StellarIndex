---
adr: 0011
title: Three-domain supply algorithm — XLM hard-coded, classic from ledger entries, SEP-41 from event sums
status: Accepted
date: 2026-04-27
supersedes: []
superseded_by: null
---

# ADR-0011: Supply algorithm — total / circulating / max

## Context

The API must publish `total_supply`, `circulating_supply` and `max_supply` for every indexed asset (requirement F2.4, `docs/architecture/coverage-matrix.md`).
Native XLM, classic credit assets and SEP-41 tokens have three structurally different sources of truth, so no single pipeline serves all three.

## Decision

Three domain-specific algorithms share one schema, plus an operator-configurable locked-set policy for circulating supply.

**Algorithm 1, native XLM.**
- `total_supply` is the constant `50_001_806_812 * 10^7` stroops (50 B genesis plus inflation pool, frozen October 2019), and `max_supply` equals it.
- `circulating_supply` is `total_supply` minus the SDF reserve account balances; the reserve list is version-controlled config, not derivable on chain.

**Algorithm 2, classic credit assets.**
- `total_supply` is the sum of trustline, claimable-balance, LP-reserve pro-rata and SAC-wrapped contract balances, reconstructed from Galexie ledger meta and kept as a running total per (asset, ledger).
- `max_supply` is `null` by default. An operator override wins; otherwise a SEP-1 `[[CURRENCIES]]` declaration applies (`max_number`, else `fixed_number`; blocked by `is_unlimited = true`), scaled from display units to raw units by the asset's decimals.
- `circulating_supply` is `total_supply` minus the locked set: by default the issuer's own balance, extendable per asset in the supply policy YAML. LP-reserve balances are not excluded.

**Algorithm 3, SEP-41 tokens.**
- `total_supply` is the lifetime sum of mint minus burn minus clawback events.
- `max_supply` follows the same order: operator override, then SEP-1, then `null`.
- `circulating_supply` is `total_supply` minus the locked set: by default the token's admin balance, extendable per token.

**SEP-1 overlay.** The overlay applies at serving time only (`internal/api/v1/assets_f2.go`) and never rewrites `asset_supply_history`.
The declaration is passed verbatim as `max_number` / `fixed_number` / `is_unlimited`, and an applied overlay sets `max_supply_basis: "sep1_declared_max"`.
`supply_basis` keeps naming the policy behind total and circulating (`xlm_sdf_reserve_exclusion`, `issuer_exclusion`, `admin_exclusion`, `override`, `no_metadata`).

**SAC-wrapped classics** are computed by both Algorithm 2 and Algorithm 3 and cross-checked by `internal/supply.WrapClass`.
- `WrapClassFull`, selected only for an operator-attested pair in `[supply].fully_wrapped_sacs`, requires equality within 1 stroop.
- Every other pair is `WrapClassPartial`, where classic total legitimately exceeds the SAC total, and it checks one bound: `classic.SACWrappedStroops <= sac.TotalSupply`.
- That bound alone drives `DivergenceStroops` and the `stellarindex_supply_cross_check_divergence_stroops` alert.
- The over-mint leg, `sac.TotalSupply <= classic.TotalSupply`, is computed as `OverMintStroops` and is diagnostic only, because classic-side burns and one-way distributions (BLND, PHO) falsify it.
- When `SACWrappedStroops` is nil the bound is not evaluated and `SubsetBoundChecked` is false; a reader of `WithinTolerance` must read `SubsetBoundChecked` with it.
- Known blind spots: an under-counted `SACWrappedStroops` passes silently (cured by `supply seed-sac-balances -full-history`), and the non-SAC half of Algorithm 2 has no second observation.

**Schema.** All supply fields are decimal strings on the wire (ADR-0003); any field without a defensible value is `null`, never fabricated.
`asset_supply_history` is a hypertable keyed by `(asset_key, ledger_sequence)` carrying total, circulating, nullable max, basis and ledger.
It is idempotent-corrective, not append-only: the writer (`internal/storage/timescale/supply.go`) upserts a re-derive in place when `derive_generation` is at least the stored value (migration 0109).

Rejected: one unified algorithm, importing third-party aggregator supply, per-asset hard-coded locked sets, omitting `max_supply`, and deriving max from auth flags.

## Invariant

- Supply amounts are `*big.Int` / `NUMERIC` and strings on the wire; ADR-0003 and its guard tests enforce it.
- An unknown supply field is `null`, never zero or a guess.
- The SEP-1 overlay labels `max_supply_basis` and leaves `supply_basis` untouched; `internal/api/v1` tests enforce it.
- The cross-check pages only on the `WrapClassPartial` escrow bound, or equality for `WrapClassFull`; `internal/supply/crosscheck_test.go` enforces it.
- The application layer is the only enforcement of single-writer on `asset_supply_history`; no Postgres role split exists.

## Consequences

Three algorithms mean three test surfaces, and the locked-set YAML needs curation per asset; uncurated assets fall back to issuer-only exclusion and say so in `supply_basis`.
Market-cap, FDV and supply-percentage fields depend on this table.

## Evidence

`internal/supply/` (policy, overlay, crosscheck), `internal/storage/timescale/supply.go`, `docs/architecture/supply-pipeline.md`, and `docs/operations/runbooks/supply-cross-check-divergence.md`.
