---
title: D10 — Documented test conventions
---

# D10 — Test conventions

**Headline:** **remarkably consistent test PRACTICE** (stdlib-only, one testcontainers
harness, one fixture style) but **fragmented, partly-FALSE documentation** — conventions
live in 3 places with no single source of truth, and the most authoritative-looking one
(`repo-hygiene-plan.md §9`) documents CI gates that **don't exist** (nightly,
`ready-for-integration` label, `make fixtures`). 458 test files, ~3,016 test funcs.

## M0
- **M0-1 — integration + chaos suites compiled but NEVER executed** (CI runs only unit
  `-race` + `test-integration-build` = compile-only; chaos in no workflow; k6 cron disabled).
  Every NUMERIC/i128 round-trip, migration, decoder→PG path, and resilience test is verified
  **only by the compiler** (CS-070 confirmed).
- **M0-2 — coverage targets (70%/80%) documented as "CI-enforced" but NOT gated** — coverage.txt
  is an artifact with no threshold; can silently decay green.
- **M0-3 — `web/explorer` has ZERO frontend tests + no tooling** (no vitest/jest/playwright);
  the whole explorer is verified only by `tsc --noEmit` + `next lint`.

## M1
`platform/postgresstore` near-zero unit coverage + **`AuditStore` untested at every layer**;
documented table-driven form is `map[string]struct` but the code uses **slice** `[]struct{name}`
(~92 files) — the doc trains the minority form; `make fixtures` documented but doesn't exist
(real: `scripts/dev/capture-<source>-fixtures.sh`); §9 lists 3 layers (5 exist — omits chaos +
obstest) and its Gate column is fiction.

## M2
Naming loosely followed (76% two-part `TestSubject_scenario`, doc overstates the 3-part strictness
— spirit holds at ~92%); the strongest real convention (**stdlib-only**, zero testify/go-cmp) is
undocumented AND contradicted (standards cites testify as an example dep); minor harness asymmetry.

## Deliverable + recommendation
The reviewer produced a ready-to-drop-in **`docs/testing-conventions.md`** (the layers table +
unit/integration boundary rule + naming/table-driven/fixture conventions + a "add a test for X"
guide) — commit it as the single source of truth and replace §9 / standards §4.10 with a link.
**Priority:** (1) fix the doc/reality gap FIRST (cheapest — it actively misleads); (2) add a
`workflow_dispatch`+label integration workflow running `make test-integration` (reuses the harness,
makes the label real, converts M0-1 from compile-only to runnable within the spend cap); (3) soft
coverage report on PRs; (4) minimal vitest harness for web/explorer; (5) backfill the AuditStore +
postgresstore holes.

*Note: the load/chaos disablement is a documented cost decision (Actions spend cap) — acceptable-
with-a-note. The integration gap is the sharp edge: the suite exists, is maintained, gates nothing,
and its own docs claim it runs nightly.*
