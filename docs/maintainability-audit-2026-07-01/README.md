---
title: Maintainability / streamlining audit — 2026-07-01
status: ✅ COMPLETE — all 10 dimensions executed; see SYNTHESIS.md
goal: make the project streamlined — easy for agents AND humans to understand, maintain, and scale
---

> **Start here:** [SYNTHESIS.md](SYNTHESIS.md) (the through-line + the sequenced
> streamlining plan). Flagship artifact: [CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md).
> Per-dimension: D1-structural · D2-naming · D3-dry · D4-agent-docs · D5-guardrails ·
> D6-style · D7-type-modeling · D8-dependency-direction · D9-checklists · D10-test-conventions.

# Maintainability / streamlining audit

A distinct audit from the 2026-06-30 correctness/security pass. Different lens:
not "is it correct" but **"is it streamlined — structurally sound, semantically
consistent, DRY, agent-discoverable, guarded against regression, and scalable to
the next feature without re-inventing what exists."**

Origin: repeated real pain where an agent **rebuilt functionality that already
existed** because it wasn't discoverable, plus accumulated naming/style/duplication
drift. Each dimension below gets **true, first-class focus** — none is subordinated
to another.

## The 10 dimensions (each a first-class workstream)

| # | Dimension | Primary deliverable |
|---|-----------|---------------------|
| D1 | **Structural soundness** (module decomposition, cohesion/coupling, god-packages, growth-readiness) | package-structure findings + target decomposition |
| D2 | **Semantic naming & consistency** (domain lexicon, plural/singular, type/func/package naming) | a **naming lexicon** + inconsistency register |
| D3 | **DRY — adversarial duplication sweep** (multiple impls of the same capability) | a **duplication register** with the canonical choice per cluster |
| D4 | **Documentation for agents** (CLAUDE.md as onboarding, "where is X", discoverability) | a **capability inventory** ("does this exist? where?") + CLAUDE.md redesign spec |
| D5 | **Guardrails against regression** (machine-enforced invariants vs prose; CI coverage) | a **guardrail matrix** (invariant → is it enforced? how? gap) |
| D6 | **Style / idiom consistency** (error handling, options, constructors, ctx, logging) | idiom-convention findings + a canonical-idiom list |
| D7 | **Type-modeling quality** (primitive obsession, invariants-in-types, stringly-typed, wire shapes) | type-design review + refactor recommendations |
| D8 | **Module boundary / dependency-direction enforcement** (import graph, cycles, layering, lint soundness) | a **dependency-direction map** + boundary-lint gap list |
| D9 | **Convention docs as CHECKLISTS not prose** ("how to add a source/endpoint/metric/migration") | rewritten (or spec'd) **checklist docs** |
| D10 | **Documented test conventions** (naming, table-driven, fixtures, unit-vs-integration, what-tested-where) | a **test-convention doc** + consistency findings |

## Scoring

Each finding gets a **maintainability severity** keyed to *cost of the status quo*,
not runtime risk:
- **M0 — Actively causes rework** (the "agent rebuilt it" class): duplication that
  will be re-duplicated; a capability no one can find; a convention that traps.
- **M1 — High friction / drift magnet**: inconsistency that compounds; a prose doc
  that should be a checklist; an unenforced invariant that will regress.
- **M2 — Polish / consistency**: cosmetic naming, minor idiom drift.

Each workstream ALSO reports **what's already good** (so we don't churn what works).

## Adversarial stance

Same as the correctness audit: claims are verified against code; "this looks
duplicated" must name both implementations; "this is inconsistent" must cite the
competing conventions. Reviewers produce their **artifact**, not just a list.

## Layout
```
docs/maintainability-audit-2026-07-01/
  README.md            ← this file
  PLAN.md              ← per-dimension method + passes
  D1..D10-*.md         ← per-dimension findings + artifact
  CAPABILITY-INVENTORY.md  ← the D4 flagship artifact
  SYNTHESIS.md         ← cross-dimension roll-up + sequenced streamlining plan
```
