---
title: Maintainability audit — synthesis & sequenced streamlining plan
status: COMPLETE — all 10 dimensions executed
---

# Synthesis — streamlining Stellar Index

All 10 dimensions executed (D1-D10 + [CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md)).
The verdict is encouraging: **the fundamentals are strong** — idiomatically
consistent (D6), strong domain types (D7 bones), a clean acyclic import graph with
no source→storage/api leakage (D8), disciplined verbs/suffixes/routes (D2), a
well-shared kernel (D3). This is a well-built codebase. The maintainability debt is
**concentrated and coherent**, not diffuse.

## The through-line — your "recreated functionality" pain, root-caused

It is not random; it's a **causal chain across dimensions**, which is why the fix is
a single coherent thread:

1. **D4** — there's **no discoverability layer**: no intent→capability index, the repo
   map undercounts 3× and omits the exact reusable utilities (`xdrjson`, `usage`,
   `scval` detail), and 58/90 packages have no `doc.go`. An agent literally cannot find
   what exists.
2. **D3** — so it **copies**: ~50+ near-identical helper copies, concentrated in
   `internal/sources/external/` (scaling ×10, tx-hash ×15, WS-reconnect ×4) — plus SSRF
   guarding duplicated with *divergent* coverage, and ClaimAtom counting forked 3×.
3. **D9** — the **recipe institutionalizes it**: the CLAUDE.md "Add a CEX connector"
   recipe says "copy the binance package," and the "Add an on-chain source" recipe omits
   the production seam + all 6 wiring edits.
4. **D1** — the **structural ambiguity** is where the next duplication lands: FX feeds
   already exist in two homes with two frameworks; "where does a new FX source go" is
   genuinely ambiguous.
5. **D5** — and **no guardrail stops any of it**: no DRY/exhaustiveness lint, and even
   the guards that exist are advisory because `main` is unprotected.

**So the highest-leverage fix is one thread:** extract the shared helpers (D3) →
publish + enforce the capability inventory (D4) → rewrite the recipes as checklists
pointing at the shared helpers (D9). Do that and the copy-paste engine stops.

## Cross-cutting root-cause classes
1. **No discoverability → copy-paste** (D4→D3→D9→D1).
2. **Guards are advisory** — unprotected `main`, integration suite compiled-but-never-run,
   self-editable lint allowlists, missing DRY/exhaustive/layering lints (D5, D10, D7, D8).
3. **Docs drift / mislead** — CLAUDE.md package undercount + false ADR-0035 claim, recipes
   that mislead, test-convention docs that document CI gates that don't exist, ADR-0003's
   fictional analyzer (D4, D9, D10, D5, D7).
4. **The type system isn't asked to enforce invariants** — no `exhaustive` lint (ADR-0010
   mandated one, never built), the open `consumer.Event` interface forcing 3 hand-synced
   switches, typed enums downgraded to `string` at boundaries (D7).
5. **One structural inversion** — `storage/timescale` imports *upward* into compute +
   sources (~16 files) because persisted domain structs live in upward packages; and the
   import-lint enforces no layering at all (D8).
6. **Two "ambiguous home" hazards** — FX feeds and supply observers each span two package
   trees (D1).

## What's GOOD (do NOT churn — the strong baseline)
Idiomatic consistency (D6: `%w`+sentinels, ctx-first, slog-only, consumer-side interfaces);
`canonical.Amount`/`Asset`/`Pair` are exemplary (D7); clean DAG, `pkg/client` pure, no
source→storage/api (D8); disciplined `Get`/`List`/`New` verbs + `*View`/`*Row` suffixes +
plural routes (D2); the well-shared kernel `canonical`(284 importers)/`scval`/`cachekeys`
(D3); `TestDefault_MatchesStructTags` reflection-guard as the model to copy (D5); consistent
test practice + one testcontainers harness (D10); the projector one-writer + external
`Connector` framework + storage tiering (D1).

## Sequenced streamlining plan (by rework-reduction leverage)

### Tier 0 — the multipliers (cheap, unlock everything else)
1. **Branch-protect `main` + required checks** (D5 CS-097) — converts the entire existing
   guard layer from advisory to blocking. Repo setting, not code. *Single highest-leverage action.*
2. **Wire the integration suite into CI** (D5/D10 CS-070) — a `workflow_dispatch`+label job
   running `make test-integration` (reuses the harness) resurrects a whole tier of tests incl.
   the ADR-0034 retention guard.
3. **Fix the misleading docs** (D4/D9/D10) — CLAUDE.md's false ADR-0035 + storage claims, the
   "copy the binance package" recipe, the test-convention fiction (`make fixtures`, nightly).
   Cheap; stops active mislead today.

### Tier 1 — kill the duplication engine (the flagship pain)
4. **Extract `external/scale` + `external/wsclient` + 4 small utils** (D3) — deletes ~50 copies;
   also fixes D1's "copy the binance package" hazard.
5. **Commit `CAPABILITY-INVENTORY.md` at repo root + add a doc.go CI gate** (D4) — so agents
   discover instead of rebuild; add the "checked the inventory" Definition-of-Done line.
6. **Rewrite the recipes as `docs/contributing/` checklists pointing at the shared helpers** (D9)
   — the 6 checklists are drafted in D9-checklists.md.

### Tier 2 — enforce the invariants the types/graph should hold
7. **Enable golangci `exhaustive`** (`default-signifies-exhaustive:true`) (D7) — the ADR-0010-
   mandated guard; catches a new enum variant missing a switch arm.
8. **Add the missing import-boundary rules + acyclicity gate** (D8) — pkg-purity, foundation-
   purity, source-may-not-reach-storage; make Event dispatch polymorphic or add the
   `tradeFromEvent`↔`HandleEvent` coupling lint (D7/D3-11).
9. **Extract the SSRF guard to `internal/nettools.SafeDialer`** (D3/D4) — reconcile the two
   divergent blocklists to the stricter union (security consistency).

### Tier 3 — structural cleanups (higher effort, opportunistic)
10. **Resolve `storage`-imports-upward** (D8 M0-1) — move persisted domain structs to
    `canonical`/a new `internal/domain`; then turn the layering rule on.
11. **Fold FX feeds into `external/`** (D1 M0-1); **`Coin*`→`Asset*` rename** (D2 M0-1, zero
    wire impact); split the ops CLI + extract `api/v1/explorer_*` (D1).
12. **Codify the constructor/logger idiom** (D6) + the type-modeling nits (D7 M2); the naming
    rename map (D2); backfill the `AuditStore`/postgresstore test holes (D10).

## Durable artifacts this audit produced (to commit during remediation)
- **`CAPABILITY-INVENTORY.md`** (D4) — the intent→symbol index (→ repo root).
- The **6 contributor checklists** (D9) → `docs/contributing/`.
- A **`docs/testing-conventions.md`** draft (D10) — single source of truth.
- The **naming lexicon** (D2), **guardrail matrix** (D5), **dependency-direction map + rule
  set** (D8) — reference docs.

**Net:** the codebase is fundamentally sound and idiomatic; the debt is a concentrated,
causally-linked knot (no-discovery → copy-paste → institutionalized-by-recipe →
unguarded). Tier 0+1 (mostly cheap: one repo setting, one CI job, two extracted packages,
one inventory file, a doc rewrite) removes the bulk of the rework-generating surface.
