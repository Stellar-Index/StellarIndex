---
title: Wiring decomposition — api main.go, Server options, ops CLI (D1)
last_verified: 2026-07-02
status: current
---

# Wiring decomposition spec (delegation-ready)

Three structural debts from the maintainability audit (D1), specced
here for execution as independent, linearly-mergeable units. None
changes behavior; each is measured by "adding the next reader/
subcommand touches N fewer places".

## Unit 1 — extract `cmd/stellarindex-api`'s inline adapters

`main.go` is 3,338 lines; its `run()` carries dozens of inline
adapter types (`storeAssetReader`, `cachedAssetReader`,
`storeMarketsReader`, `cachedMarketsReader`, `storeHistoryReader`,
`storeOracleReader`, `globalPriceReader`, ready-checkers, …).

- Create `cmd/stellarindex-api/internal/wiring/` (binary-private;
  NOT internal/api — these adapters glue storage+cache+api and must
  not pull cache/storage into the handler package).
- Move each adapter type + its constructor verbatim into
  `wiring/<area>.go` (assets.go, markets.go, prices.go, history.go,
  oracle.go, readiness.go). Zero logic edits — this unit is pure
  file motion, reviewable by `git diff --color-moved`.
- `run()` shrinks to config-load + wiring calls + server start.
  Target: main.go under 800 lines after this unit alone.

## Unit 2 — collapse the Options/Server triple-touch

Adding a reader today edits three synchronized sites (~60 deps each):
`v1.Options` field, `v1.Server` field, the `New()` assignment.

- Change `v1.Server` to EMBED `Options` (one struct, one assignment:
  `Server{Options: opts, …}`); keep `Options` as the public wiring
  surface. Handlers change `s.assets` → `s.Assets` mechanically
  (gofmt-scriptable, one commit, no hand edits beyond the sed).
- The nil-safe degradation contract (nil reader → 503/empty) is
  documented per-field on Options TODAY — that documentation and
  behavior must survive verbatim. The existing handler tests are the
  guard; run the full v1 suite.
- REJECTED alternative: a reflection/registry DI container —
  boring-over-clever; embedding gets the 3→1 win without magic.

## Unit 3 — group the ops CLI dispatch

`cmd/stellarindex-ops/main.go` is a flat ~55-case switch. A
framework (cobra/urfave) is REJECTED: new dep, new idiom, zero user
benefit for an operator CLI. Instead:

- Introduce `type subcommand struct{ name, synopsis string; run func([]string) error }`
  and a `var subcommands = []subcommand{…}` table, grouped by the
  doc.go section headings (ingest/backfill, lake, completeness,
  archive, supply, customer, seed, diagnostics).
- The switch becomes a map lookup; `stellarindex-ops help` renders
  the table (today's hand-maintained usage text is generated from it
  — one source of truth).
- The `default:` case's references to never-built subcommands
  (`cache-prime`, `verify-invariants`) are DELETED, not carried.

## Unit 4 (optional, last) — the per-source pipeline registry

The five lockstep wiring sites (HandleEvent / IsProjectedEvent /
tradeFromEvent / projector buildSource / BuildDispatcher) are now
GUARDED by `internal/pipeline/lockstep_ast_test.go` (commit
8d501400), so this is maintainability, not correctness-urgency.
If executed: one `pipeline.SourceSpec{Name, Decoder, Projected,
TradeShaped, Persist}` registration per source, from which all five
sites derive. Preconditions: Units 1–3 landed, a quiet ingest window
for the deploy, and the lockstep test retargeted to assert the
REGISTRY is complete (it keeps its role as the guard). Do NOT touch
the indexer's `run()` shutdown ordering (20+ LIFO defers,
correctness-critical, G20-02) as part of this — it is explicitly out
of scope for the decomposition.

## Sequencing + verification

Units are independent except 4-after-1..3. Each: `go build ./...`,
full package tests, `bash scripts/dev/verify.sh` before push. Unit 2
additionally: the API contract tests + one manual smoke
(`scripts/dev/r1-smoke.sh` against a locally-run binary if the
stack is up).
