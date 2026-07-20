---
title: Frontend redesign-readiness map — web/explorer
date: 2026-07-20
status: active
last_verified: 2026-07-20
audience: whoever drives the upcoming design refresh (human pointers + Claude design pass)
companion: docs/architecture/design-system.md
---

# Frontend redesign-readiness map

A restructuring companion to `design-system.md`. That doc is *what the visual
language is*; this doc is *how amenable the codebase is to changing it* — where
everything lives, which pages are ready to reorder vs. which need a
decompose-first pass, and the safe playbook for each kind of change.

Scope: `web/explorer` (the public site + in-site account). `web/status` is a
tiny separate app (one client component) and out of scope here.

## TL;DR — difficulty by change type

| Change | Difficulty | Why |
|---|---|---|
| **Restyle** (color, type, spacing, component look) | 🟢 Easy | One semantic token layer (`globals.css @theme`) + a real primitives library used by 47/70 routes. Change tokens or a primitive → propagates site-wide. |
| **Reorder sections within a *decomposed* page** | 🟢 Easy | It's rearranging composed children / tab arrays. Most routes are here. |
| **Reorder sections within a *monolith* page** | 🟡 Medium | Sections are inline JSX in one big file — extract first (list below), then reorder. |
| **Restructure IA** (move a feature between pages, new layouts, new nav) | 🟡 Medium | Nav is centralized, routes are independent feature folders, data is decoupled via generated types — feasible, but some sections assume their page's loaded data shape. |
| **Theme** | ⚫ Dark only (2026-07-21) | The system is now dark-only — semantic tokens are defined dark; no light/dark toggle. See `dark-redesign-direction.md`. |

**Biggest single lever before a heavy redesign:** the test net (there is
currently *zero* — see [Safety net](#safety-net)). Everything else is in good
shape.

## Architecture at a glance

- **Stack:** Next 16.2 (app router, `output: 'export'` static) / React 19.2 /
  Tailwind 4 / TanStack Query. Build → `out/` static; an `OPEN_NEXT=1` path
  switches to Cloudflare Workers SSR (spike).
- **Data flow (decoupled from presentation — important for restructuring):**
  - Build-time: `lib/buildFetch.ts` — **fail-hard** (transport failure throws &
    fails the build; `null` only for authoritative 4xx / CI stub).
  - Client-time: `api/hooks.ts` — TanStack Query hooks.
  - Types: `api/types.ts` **generated** from `openapi/stellar-index.v1.yaml`
    (`pnpm generate:api`); CI drift-gated. **Never hand-edit.**
  - Fetch base: `api/client.ts` `API_BASE_URL` — import this, don't re-derive it.
- **Component layers:**
  - `components/ui/` — design-system primitives (barrel `@/components/ui`):
    Button, Card, Badge, Stat/StatGrid/StatCell, Table set, Container/Section/
    PageHeader/Breadcrumbs/SectionHeader, Input set, EmptyState/Skeleton/Callout,
    TabNav/Segmented, Mono/CopyButton.
  - `components/primitives/` — presentational domain atoms (barrel
    `@/components/primitives`): Sparkline, DirectionPill, MultiWindowDelta,
    RankBadge, StreakIndicator, AccelerationArrow. Contract: **props-only, no
    data-fetching, deterministic** — ideal redesign + test targets.
  - `components/charts/` — LineChart, CandleChart, DonutChart, MarketChart
    (wrap `lightweight-charts`).
  - `components/nav/` — Sidebar, Footer, SearchModal, DegradedBanner,
    ConsoleShell (the shell/IA — change nav here, once).
  - `app/<route>/` — feature folders: `page.tsx` (data + shell) + `*View.tsx` +
    section components + `error.tsx` boundary.
- **Styling:** Tailwind utilities composed with `cn()` (`@/lib/cn`; clsx +
  tailwind-merge, last-wins). Tokens are semantic — restyle by editing tokens,
  not by find/replacing hex values.

## Where the tokens & primitives live (the restyle surface)

| You want to change… | Edit here |
|---|---|
| Colors / type scale / radius / shadow | `web/explorer/src/app/globals.css` → `@theme` block |
| A primitive's look (button, card, table…) | `web/explorer/src/components/ui/<Name>.tsx` — one edit, all call sites |
| A data atom (sparkline, delta pill…) | `web/explorer/src/components/primitives/<Name>.tsx` |
| Global nav / footer / search | `web/explorer/src/components/nav/` |
| See it all rendered | `/dev/styleguide` and `/dev/primitives` routes (living reference) |

## Per-route decomposition status

Verdict key: 🟢 decomposed (reorder = rearrange children) · 🟡 page-heavy
(sections inline in `page.tsx`, otherwise fine) · 🔴 monolith (one file is the
whole route — decompose before reordering) · ⚪ content/simple page (fine as-is).

| Route | Route LOC | Files | Largest file (LOC) | Verdict |
|---|---:|---:|---|:--:|
| `assets` (incl. `[slug]`) | 4660 | 19 | `[slug]/page.tsx` (1526) | 🟡 route decomposed into 15 panels, but `page.tsx` inlines OverviewBody/AssetFAQ/VerifiedCurrencyView |
| `dashboard` | 2454 | 8 | `page.tsx` (556) | 🟢 |
| `status` | 2301 | 4 | `StatusPageClient.tsx` (2035) | 🔴 but special-case (live 20-endpoint probe matrix, not a reorderable content page — leave) |
| `accounts` | 1800 | 8 | `AccountView.tsx` (864) | 🟢 (View big but sections split: Movements/Positions/DefiPositions) |
| `protocols` | 1706 | 7 | `[name]/ProtocolView.tsx` (809) | 🟢 (BespokeSection/TimeSeriesChart split out) |
| `dexes` | 1603 | 10 | `DexesView.tsx` (441) | 🟢 |
| `markets` | 1231 | 6 | `[pair]/page.tsx` (741) | 🟡 page inlines SourceBreakdownPanel etc. |
| `sources` | 1106 | 5 | `[name]/page.tsx` (521) | 🟡 |
| `issuers` | 1100 | 5 | `[g_strkey]/page.tsx` (661) | 🟡 |
| `diagnostics` | 1054 | 7 | `CursorsTable.tsx` (248) | 🟢 |
| `embed` | 1016 | 5 | `asset/[slug]/page.tsx` (403) | 🟢 (widgets, intentionally standalone) |
| `contract` | 990 | 3 | `ContractView.tsx` (947) | 🔴 **top extract-first candidate** |
| `exchanges` | 917 | 6 | `ExchangesView.tsx` (335) | 🟢 |
| `lending` | 901 | 5 | `LendingPoolsTable.tsx` (300) | 🟢 |
| `network` | 568 | 2 | `NetworkView.tsx` (553) | 🔴 **extract-first candidate** |
| `tx` | 535 | 3 | `TxView.tsx` (492) | 🔴 (small — lower priority) |
| `ledger` | 500 | 3 | `LedgerView.tsx` (456) | 🔴 (small — lower priority) |
| `operation` | 379 | 2 | `OperationView.tsx` (352) | 🟡 (small) |
| `methodology`, `sdk`, `docs`, `pricing`, `company`, `changelog` | — | 1 | `page.tsx` | ⚪ prose/content pages |

(Routes not listed are 🟢/⚪ and small.)

## Extract-first list (ranked) — do these before reordering their sections

1. **`contract/ContractView.tsx`** (947, 96% of route) — split into
   header / metadata / interface / activity / holders sections.
2. **`assets/[slug]/page.tsx`** (was 1526) — extract the still-inline
   `OverviewBody`, `VerifiedCurrencyView` into co-located files (the tab panels
   are already split — follow that pattern). ✅ **`AssetFAQ` done 2026-07-20 as
   the worked example** → `./AssetFAQ.tsx` (+ `AssetFAQ.test.tsx`); replicate its
   shape for the rest.
3. **`network/NetworkView.tsx`** (553, 97%) — split the metric/health/chart
   sections.
4. **`markets/[pair]/page.tsx`** (741) & **`issuers/[g_strkey]/page.tsx`**
   (661) — lift inline panels to sibling components.
5. `tx`/`ledger`/`operation` views — small monoliths; extract only if the
   redesign reorders them.

Extraction is behavior-preserving (move JSX + props into a co-located
`*.tsx`, import it back). With the [safety net](#safety-net) in place it's low
risk. **The worked example (`AssetFAQ`) is the template:** move the trio
(data-builder + component + item), add a render test, run
`pnpm test && pnpm typecheck`. **Lesson from it:** a helper may have more than
one caller — `assetFaqFor` also feeds the FAQ JSON-LD schema, so it had to be
*exported*, not just moved. `pnpm typecheck` catches that coupling instantly;
always run it after an extraction rather than assuming a section is self-contained.

## The redesign playbook

- **To restyle globally:** edit `@theme` tokens in `globals.css`. Verify on
  `/dev/styleguide`. Avoid raw hex in components — add/adjust a token instead.
- **To restyle one component everywhere:** edit its file in `components/ui/`.
  If a page has a *local* re-implementation (see smells), fold it into the
  primitive so the change lands once.
- **To reorder sections on a decomposed page:** rearrange the composed children
  / the tab array in the `*View.tsx`. No data changes needed.
- **To reorder a monolith page:** extract-first (list above), then reorder.
- **To move a feature between pages:** the section usually takes typed props;
  move the component, then wire the destination page's loader to supply the
  same shape (types are generated, so mismatches are compile errors — lean on
  `pnpm typecheck`).
- **To change nav/IA:** `components/nav/Sidebar.tsx` + `Footer.tsx`; routes are
  independent folders so adding/removing a page is local.
- **Verify any change:** `pnpm typecheck` (fast, offline) + `pnpm lint` +
  `pnpm test` (once the net lands). A full `pnpm build` hits the live API
  (fail-hard) so prefer typecheck locally.

## Known smells to clean up en route (not bugs)

- **Local primitive re-implementations** shadow the design system — e.g.
  `assets/[slug]/page.tsx` declares its own denser `Stat` (with an accent chip)
  instead of using `ui/Stat`. Fold these into the primitive during the redesign
  so restyles land once. (Grep for `^function (Stat|Card|Badge|Button)\b` under
  `app/` to find them.)
- **API-base fallback duplicated** across several files instead of importing
  `API_BASE_URL` from `api/client.ts` — consolidated 2026-07-20 (see git log);
  `api/hooks.ts` keeps its own on purpose (needs `credentials:'include'`).
- **`design-system.md` said tokens live in `tailwind.config.ts`** — corrected
  2026-07-20; there is no such file (Tailwind 4 `@theme`).

## Safety net

**Status: landed 2026-07-20** — `pnpm test` (vitest + React Testing Library +
jsdom). 34 tests, all green:

- `src/components/ui/ui.test.tsx` — every design-system primitive renders with
  the right semantics (roles, text, element type — *not* Tailwind classes, so a
  restyle won't trip it).
- `src/components/primitives/primitives.test.tsx` — the domain atoms render
  across their key branches (delta/no-data, streak/ath, etc.).
- `src/lib/format.test.ts` + `src/lib/cn.test.ts` — the pure formatters and the
  class-merge helper (exact-output assertions — these *should* fail if logic
  changes).

Infra: `vitest.config.ts` (esbuild automatic JSX — no `@vitejs/plugin-react`
needed; `@/` alias + hermetic `next/link` / `next/navigation` stubs under
`test/stubs/` so `<Link>`-using components render without the Next runtime),
`vitest.setup.ts` (jest-dom matchers + auto-cleanup).

This is a **starting net over the redesign surface**, not full coverage — extend
it as you extract sections from the monoliths (a render test per newly-extracted
section is the cheap, high-value habit). Gate a restructure with
`pnpm test && pnpm typecheck`.
