# Full-product cold audit — 2026-06-11

Comprehensive read-only audit of the entire Rates Engine repository at HEAD `a1f96b58`.

## What this is

A four-wave audit touching **every one of the 2,349 tracked files** and the material cross-file interactions, covering 22 dimensions (invariants, traps, security, decoder fidelity, aggregation math, storage, API contract, wiring, concurrency, error handling, DRY, observability, ops/deploy, CI, docs-truth, RFP execution, config drift, web, deps, performance).

Method: 35 per-file slice agents (Wave 1) → 3 cross-cutting matrices (Wave 2: RFP execution, observability chain, CLAUDE.md verification) → adversarial refute-by-default verification of every High/Critical (Wave 3) → this synthesis (Wave 4).

## Read order

- [`00-plan.md`](00-plan.md) — the audit plan (3-pass refinement record).
- [`01-coverage-ledger.md`](01-coverage-ledger.md) — every tracked file → its slice (the totality proof).
- [`05-findings-register.md`](05-findings-register.md) — **start here.** Promoted High/Critical + material Mediums, F-1316+, deduped, with Wave-3 verification verdicts.
- [`06-exclusions-register.md`](06-exclusions-register.md) — depth calibration + verified-good (don't re-litigate).
- [`07-remediation-plan.md`](07-remediation-plan.md) — prioritized by risk × launch-proximity.
- [`evidence/wave1/batch{1..5}-findings.md`](evidence/wave1/) + [`evidence/wave2-findings.md`](evidence/) — the full ~230-finding long-tail with per-slice IDs.

## Headline numbers

- **2,349/2,349** files covered; ~230 findings.
- **1 Critical → downgraded to High** after verification (sep41 projector loss is latent, not live — r1 runs the safe config; but a config-default foot-gun makes "enable the projector per docs" silently destructive).
- **~19 High** (F-1316…F-1334), all adversarially verified or doc-truth-high-confidence.
- Largest categories: doc-truth drift (~6 weeks of change outran ADRs/runbooks/CLAUDE.md/binding-architecture-docs/deployed-OpenAPI), coarse-PK silent-loss (≥6 tables), dead observability layer (18/106 alerts can't fire), config default-tag drift, test rot.

## What's solid (verified-good)

i128/NUMERIC discipline, auth primitives (keys/JWT/rate-limit/SEP-10/webhooks), ClickHouse lake design, ADR numbering/immutability, no tracked secrets, dependency hygiene. See the exclusions register.

## Caveat

Repo-artifact audit at one commit. Live r1 state was checked only where a finding hinged on it; several findings are flagged "settle by checking r1" for follow-up. No code changed during the audit.
