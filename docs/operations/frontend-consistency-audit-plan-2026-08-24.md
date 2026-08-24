# Frontend consistency & DRY audit — run plan (2026-08-24)

**Trigger:** the container-drift incident — the design-system `Container`
(1728 px) was used by the home page while 29 data pages hand-rolled a
1280 px wrapper and SDEX sat at prose width, unnoticed across multiple
audits. Prior passes hunted *existing near-duplicates* (that's how
COR-14 caught the price-formatter forks) but never asked the inverse
question — "which surfaces are NOT using the shared thing?" — which is
the only shape of question that catches adoption gaps. Ash's concern,
verbatim scope: hand-rolled code we don't need, and inconsistencies in
templating / component usage (DRY).

**Method:** the new `frontend-consistency.md` (FEC) checklist in the
audit suite. Three phases; the census phase is mechanical and cheap, and
its scripts are kept afterwards as CI tripwires so the classes can't
silently refork.

## Phase 1 — mechanical census (scripts, ~no judgment, half a day)

Every census output is a table checked into the audit workspace
(`docs/audit/audit-<date>/` — gitignored, mirrored to the private repo).

1. **Design-system inventory + adoption matrix** (FEC-02): enumerate all
   exports of `components/ui`, `components/*`, `lib/format`,
   `lib/live/hooks`, `lib/useDialog`, chart primitives,
   `explorer-shared`. For each: usage count per file, plus
   pattern-signatures for hand-rolled equivalents (raw `<table>` vs
   Table primitives; custom empty/loading divs vs EmptyState/Skeleton;
   `<header><h1>` blocks vs PageHeader; hand-built crumbs vs
   Breadcrumbs; inline `toFixed`/`toLocaleString` vs lib/format;
   `setInterval` polling vs useLedgerFollow/useObservationsFollow;
   hand-rolled dismiss/focus vs useDialog).
2. **Route frame table** (FEC-01): script over `src/app/**/page.tsx` +
   their views extracting outermost wrapper, header shape, breadcrumb
   presence, vertical rhythm. (The container sweep of 2026-08-24 fixed
   width; this verifies the REST of the frame and pins the end state.)
3. **Duplicate-function families** (FEC-03): jscpd over `src/` plus a
   semantic pass per family. Known seeds already spotted in passing —
   the census must complete these lists, they are not the ceiling:
   - relative time: `lib/format.formatRelative`,
     `explorer-shared.relativeAge`, `HomeRecentTrades.timeAgo` (3 impls)
   - asset-code shortening: `HomeRecentTrades.short()` vs AssetLabel's
     canonical handling
   - cursor-stack pagination state: PairsTable, DexesView (+ others to
     enumerate)
   - sort-pill / toggle button clusters: MarketsTable venue pills,
     PairsTable SortPill, NetworkView metric/window pills
   - envelope unwrapping (`Envelope<T>` + `.data`) variants
4. **Class-cluster frequency** (FEC-04): normalized className token-set
   counts; top-N repeated long strings = unextracted primitives
   (buttons, pills, badge/label chips, card shells).
5. **Formatter semantics per field** (FEC-06): for price, USD volume,
   counts, timestamps, ledger numbers: every render site + which
   formatter/precision it uses.
6. **Dead UI** (FEC-07): exports never imported under `src/`.

## Phase 2 — judgment (auditor agents on the census, ~a day)

One auditor per FEC dimension, input = the census tables + the FEC
checklist + this plan. Output: findings docs in the standard format
(claim, file:line, census row as evidence, proposed consolidation,
deliberate-vs-drift verdict with reasoning). A skeptic pass verifies
the high-impact findings — especially "the shared component is actually
equivalent" (the LastPriceCell precedent: forks sometimes carry an
intentional behavioral difference, and consolidating can silently drop
or restore one — each consolidation must state which behavior wins and
why).

## Phase 3 — consolidation waves (remediation, sized after Phase 2)

Extract-then-adopt, standard wave discipline (fixer → verifier →
integrator, gate green per wave). Every consolidation ends in a guard:

- extend `no-duplicated-price-formatters.test.ts`-style guards per
  consolidated family;
- new CI lint tripwires: forbid `mx-auto max-w-7xl` (and siblings) in
  `src/app`; forbid new relative-time implementations (regex on
  `Math.round(.*/ 1000)`-style seeds is imperfect — prefer the guard
  test asserting the canonical util's adoption list);
- the route-frame census script itself runs in CI and fails on new
  deviations not listed in a reviewed allowlist (the deliberate-narrow
  pages).

Visual safety: template-level changes get a before/after browser pass on
the affected page types (we have no screenshot-diff infra; if Phase 2
finds broad template surgery is needed, stand up a minimal Playwright
screenshot job first and fold it into CI — decide at wave-planning).

## Explicit non-goals

Marketing/prose/auth surfaces' deliberate narrow layouts; pixel-perfect
visual QA (separate concern); backend Go code (covered by existing
dimensions).

## Why audits missed this (recorded for the audit suite)

Instance-driven duplication hunting finds forks that exist; it cannot
find the N surfaces that each independently *didn't adopt* a primitive.
The fix is method, not effort: census-first (inventory × usage), then
judgment — now codified as the FEC checklist with the census scripts
retained as permanent tripwires.
