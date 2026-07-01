---
title: Maintainability audit — PLAN
status: planning (pass 2)
---

# Plan — per-dimension method

Each dimension is executed by an independent reviewer producing its artifact +
findings (M0/M1/M2) + a "what's already good" note. Inputs reused from the
2026-06-30 correctness audit are cited, not re-derived.

## D1 — Structural soundness
- **Method:** map every package's responsibility + LOC + fan-in/fan-out. Flag
  god-packages (`internal/api/v1` = 154 files, `internal/storage/timescale` = 75 —
  are these cohesive or grab-bags?), packages that do two jobs, and "where would a
  new protocol / endpoint / source class go?" ambiguity. Judge growth-readiness:
  adding feature N+1 — is the seam obvious?
- **Good =** one clear home per concept; new features have an obvious insertion point.

## D2 — Semantic naming & consistency
- **Method:** build the domain lexicon from the code (asset/coin/currency,
  price/rate, source/venue, ledger/block, observation/update/event, tier/plan).
  For each concept list every term used + where. Flag the multi-term concepts
  (coins/currencies/assets is known — LC-030), plural/singular route+identifier
  drift, and type/func naming-convention inconsistency (Get vs Fetch vs Load vs
  Read; List vs All; New vs Make).
- **Good =** one word per concept, enforced.

## D3 — DRY (adversarial duplication sweep)  ⭐ (the "recreated functionality" pain)
- **Method:** hunt near-duplicate implementations of one capability: decimal/i128
  string parsing (`decimalStringToScaledInt` reportedly ×7), claim-atom extraction
  (SDEX ×5), HTTP clients + retry/backoff (each external venue), cache-key builders,
  throttles/limiters (login/signup/rate — 3 shapes), readers with the same shape,
  dual detail-page components, the two event type-switches. For each cluster: list
  every copy, pick the **canonical** one, note why the dups exist. Also: copy-paste
  helpers that should be one shared func.
- **Good =** one implementation per capability; shared helpers discoverable.

## D4 — Documentation for agents / discoverability  ⭐ (flagship: CAPABILITY-INVENTORY)
- **Method:** build the **capability inventory** — an intent-keyed index: "need to
  X → it's in Y" for every reusable capability (price a pair, fetch SEP-1, sign a
  webhook, a Redis counter, a token bucket, parse an SCVal, emit a metric, add a
  cache key…). Then audit CLAUDE.md AS AN AGENT ONBOARDING DOC: does it let an
  agent *find* existing functionality before writing new? (It already has drift:
  CS-127 false ADR-0035 claim, CS-005 3× package undercount.) Produce a redesign
  spec (what CLAUDE.md should contain to prevent the rebuild-what-exists failure).
- **Good =** an agent can answer "does this exist?" in one lookup.

## D5 — Guardrails against regression
- **Method:** enumerate every INVARIANT the project claims (ADRs, CLAUDE.md, the 8
  invariants) and for EACH: is it machine-enforced (lint/test/CI), and does the
  guard actually fire? Build the guardrail matrix. The correctness audit already
  found the big gaps (CS-070 tests-not-run, CS-097 unprotected-main, CS-007 missing
  i128 analyzer, CS-098 bypassable lints, CS-052 `.Handle()` blind spot, CS-131
  regex gaps) — systematize into "invariant → enforced? → gap → proposed guard."
- **Good =** every load-bearing invariant has a firing guard.

## D6 — Style / idiom consistency
- **Method:** beyond gofumpt/golangci format — audit IDIOM consistency: error
  wrapping (`%w` vs `%v` vs sentinel), option patterns (functional-options vs config
  struct), constructors (`New` returning value vs pointer vs (T,error)), context
  handling, logging (slog fields), package doc conventions (doc.go vs README).
  Sample across mature vs recent packages; flag where the codebase disagrees with itself.
- **Good =** one idiom per concern, followed everywhere.

## D7 — Type-modeling quality
- **Method:** review the domain types (`canonical`, `consumer`, `events`, `pkg/client`,
  wire shapes). Hunt primitive obsession (stringly-typed asset classes / sources /
  tiers that should be enums), invariants that should be encoded in types but are
  runtime-checked, the dual-shape `/v1/assets/{slug}` (LC-040), and any place a
  `map[string]any` / `interface{}` hides structure. Judge whether types make illegal
  states unrepresentable.
- **Good =** types encode the invariants; illegal states don't compile.

## D8 — Module boundary / dependency-direction
- **Method:** derive the actual import graph. Check for cycles, layering violations
  (does a low layer import a high one? does `canonical` import anything it shouldn't?),
  and whether `scripts/ci/lint-imports.sh` encodes the intended direction completely
  (the correctness audit found it bypassable — CS-098 — and a `/decode.go` blanket
  exemption). Produce the dependency-direction map + the lint-gap list.
- **Good =** acyclic, layered, and the lint provably enforces the layering.

## D9 — Convention docs as checklists
- **Method:** inventory the "how to add X" guidance (CLAUDE.md "Common task recipes"
  are prose). For the top recipes (new on-chain source, new CEX connector, new
  endpoint, new metric, new migration, new supply observer) produce an actionable
  **numbered checklist**: exact files to touch, the existing helper to reuse, the
  guard that will catch you, the test to add. Audit the existing recipes for
  accuracy (the correctness audit found some drift).
- **Good =** adding a feature is a checklist, and the checklist points at reuse.

## D10 — Documented test conventions
- **Method:** audit test conventions + whether they're documented: naming
  (`TestX_scenario`), table-driven consistency, fixture patterns (golden frames vs
  inline), the unit/integration/`obstest` split, "what is tested where," and the
  execution gap (CS-070: integration compiled-not-run). Produce a test-convention
  doc + a consistency-findings list.
- **Good =** a contributor knows what to test, where, and how, from one doc.

## Execution
Fan-out: one adversarial reviewer per dimension (D3, D4 may get a second for depth),
each producing its artifact. Consolidate into per-dimension files + the capability
inventory + a cross-dimension SYNTHESIS with a **sequenced streamlining plan**
(what to change first for the most rework-reduction). Findings-only + plan; no code
changes applied until approved.

## Pass log
- **Pass 1:** 10 dimensions defined, each first-class (not discoverability-centric
  per operator direction), scoring rubric (M0/M1/M2 = rework-cost), deliverable per
  dimension.
- **Pass 2 (this):** per-dimension method + "good =" bar + reuse-inputs from the
  correctness audit; execution shape.
- **Pass 3 (self-review before freeze):** does every maintainability concern the
  operator named map to a dimension? (structural ✓ naming ✓ DRY ✓ agent-docs ✓
  guardrails ✓ style ✓ types ✓ boundaries ✓ checklists ✓ test-conventions ✓) — plus
  added: growth-readiness (D1), the capability inventory (D4). No white space.
