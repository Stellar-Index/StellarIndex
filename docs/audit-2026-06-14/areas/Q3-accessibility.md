# Q3 — Accessibility (a11y) audit — web/explorer

**Scope:** read-only WCAG 2.1 AA review of the Next.js 15 static-export
explorer at `web/explorer/` (`src/`). Static source review only — no
browser, no axe run. Color-contrast figures below are *computed from the
Tailwind palette* and flagged where a live tool should confirm.

**Method:** the project has no `eslint-plugin-jsx-a11y` configured
(`eslint-config-next` ships a subset but `next lint` was not runnable
offline). Findings are from reading the nav/modal/table/form/chart
components plus grep sweeps for the usual anti-patterns
(`onClick` on non-interactive elements, `<img>`, `role=`, `aria-live`,
`tabIndex`, focus rings, motion, heading levels).

**Headline:** the codebase is *better than average* for a hand-rolled
component set — buttons are real `<button>`s, every interactive control
I found is natively focusable, tables use `<th scope="col">`, the search
input and filters are labelled. The real barriers are concentrated in
the **custom overlay/widget components** (Cmd-K modal, RequestReveal
modal, nav dropdowns, the combobox, the tab strip): they look right but
lack the ARIA widget contract and focus management that AT users depend
on. There are **no keyboard traps** and **no unlabelled primary
controls** — so nothing is fully *unusable*, but several things are
*degraded* for screen-reader / keyboard-only users.

---

## Findings

### Critical barrier

| severity | file:line | WCAG | issue | fix |
|---|---|---|---|---|
| Critical | `src/components/nav/SearchModal.tsx:384-470` | 2.4.3 Focus Order, 4.1.2 Name/Role/Value, 2.1.2 No Keyboard Trap (inverse — *no* trap) | The Cmd-K dialog has `role="dialog" aria-modal` but **no focus trap, no focus return, no `aria-label`/`aria-labelledby`, and Tab is not confined to the dialog**. A keyboard/SR user opens it, but Tab walks straight out into the page *behind* the overlay (which is not `inert`/`aria-hidden`), and on close focus is lost to `<body>`. The result list is plain `<ul><li><a>` with no `role="listbox"`/`option`, no `aria-activedescendant`, and the footer advertises "↑↓ navigate" but **arrow-key navigation is not implemented** — only Tab works. This is the site's flagship control and its primary "find anything" affordance. | Add focus trap + restore-focus-on-close; `aria-modal="true"` (string) + `aria-label="Site search"`; mark the background `aria-hidden`/`inert` while open; either implement the listbox/`aria-activedescendant` arrow-key model the footer promises, or remove the "↑↓ navigate" hint. The `<input>` also needs an associated label (see Serious row). |

### Serious

| severity | file:line | WCAG | issue | fix |
|---|---|---|---|---|
| Serious | `src/components/nav/SearchModal.tsx:397-403` | 3.3.2 Labels, 1.3.1, 4.1.2 | The modal search `<input>` has only a `placeholder`, no `<label>`/`aria-label`. Placeholder is not an accessible name. | Add `aria-label="Search coins, pairs, protocols, accounts, transactions"`. |
| Serious | `src/components/reveal/RequestReveal.tsx:62-83` | 2.4.3, 4.1.2, 2.1.2 (no-trap) | The "API request" reveal modal (`role="dialog" aria-modal`) — same class of defect as Cmd-K: **no focus trap, no focus return, no `aria-labelledby`** pointing at the "API request" `<h3>`, background not inerted. Reachable on every Panel on the site (dozens of instances). | Trap focus, restore on close, `aria-labelledby` the heading id, inert the background. |
| Serious | `src/components/nav/Navbar.tsx:393-472` (`Dropdown`) and `:204-291` (`SignedInWidget`) | 4.1.2, 2.1.1 Keyboard | Disclosure menus use `aria-haspopup="menu"` + `role="menu"`/`role="menuitem"` but **implement none of the menu keyboard contract** (no Arrow/Home/End navigation, no focus move into the menu, no Esc-returns-focus-to-trigger). Declaring `role="menu"` *promises* that behaviour to a screen reader, so this is worse than leaving it off. Items are `<a>`/`<Link>` so they remain Tab-reachable, but the announced role lies. | Either drop `role="menu"`/`menuitem` and treat it as a plain disclosure (`aria-expanded` on the trigger + a `<ul>` of links — already mostly there), or implement the full menu keyboard model. The simpler disclosure route is recommended. |
| Serious | `src/components/CurrencyCombobox.tsx:78-135` | 4.1.2, 1.3.1 | Custom combobox advertises itself as "Keyboard-friendly" and implements arrow/Enter/Esc on the input, but exposes **no combobox ARIA**: trigger is a bare `<button>{value} ▾</button>` with no `aria-label`/`aria-expanded`/`aria-haspopup`; the panel input has no `role="combobox"`/`aria-controls`/`aria-activedescendant`; the `<ul>` is not a `role="listbox"` and options are not `role="option"` with `aria-selected`. A SR user hears "button, ▾" and an unlabelled text field. The visual `highlight` state is invisible to AT. | Apply the APG combobox-with-listbox pattern: label the trigger, `aria-expanded` it, give the input `role="combobox" aria-expanded aria-controls aria-activedescendant`, the list `role="listbox"`, options `role="option" aria-selected`, and `id` the highlighted option. |
| Serious | `src/components/charts/CandleChart.tsx:120-127`; `src/components/charts/LineChart.tsx`; sparklines `src/components/primitives/Sparkline.tsx`, `SourceSparkline.tsx` | 1.1.1 Non-text Content | Lightweight-charts renders to a `<canvas>` with **no text alternative, no `role="img"`, no `aria-label`, and no accessible data fallback**. Price/candle history is conveyed *only* visually. | Wrap the chart container with `role="img"` + an `aria-label` summarising the series (e.g. "XLM/USD candles, last 90 days, open … close …"), and/or expose the underlying OHLC as a visually-hidden `<table>` or a "view as table" toggle. |
| Serious | (palette) `src/app/**/*.tsx` — `text-slate-400` (97 files) on white/`bg-slate-50`; e.g. `SearchModal.tsx:427,453,458` `[10px]`/`text-slate-500` badges; `LedgersTable.tsx:103` `text-[11px] text-slate-500` headers; `RequestReveal.tsx:48` `text-[10px] text-slate-500` | 1.4.3 Contrast | `text-slate-400` (#94a3b8) on white is **≈3.0:1 — fails AA 4.5:1** for the body/label text it's used as (timestamps, hrefs, hints, "current", footer kbd hints). `text-slate-500` (#64748b ≈4.8:1) passes for normal text but the pervasive `text-[10px]`/`text-[11px]` micro-badges in `text-slate-500` on `bg-slate-100`/`bg-slate-800` should be confirmed in a tool. | Bump informational `text-slate-400` → `text-slate-500`/`600` (and dark-mode `text-slate-500` → `400`). Verify the small badges with a contrast checker; this is a broad sweep, not a one-liner. |
| Serious | `src/components/nav/DegradedBanner.tsx:85-105` and `src/lib/markdown`/`HomeTop*`/`DexesView.tsx:233` using `text-ink`, `bg-bad-50`, `text-bad-700`, `bg-warn-50`, `text-warn-700`, `text-ink-faint` | 1.4.3, 1.3.1 | These semantic color classes are **not defined in `tailwind.config.ts`** (theme only defines `brand`, `up`, `down`, `timepin`). So the degraded/outage banner's intended red/amber severity tint **silently does not apply** — text falls back to inherited body color on a near-transparent background, which both removes the visual severity cue and is likely low-contrast. `text-ink` likewise renders as default text (cosmetically OK in light mode, undefined in dark). This is a correctness bug with an a11y consequence: the one in-product "the data you're seeing may be stale" warning is visually defanged. | Define `bad`/`warn`/`ink`/`ink-faint` in the Tailwind theme (or switch the banner to the existing `down`/`rose` + `amber` scales) and verify the resulting fg/bg contrast. |

### Moderate

| severity | file:line | WCAG | issue | fix |
|---|---|---|---|---|
| Moderate | `src/app/layout.tsx:175-182` | 2.4.1 Bypass Blocks | No **skip-to-content link**. `<main>` exists (good) but with a multi-item nav + dropdowns on every page, keyboard users must Tab through the whole header on each navigation. | Add a visually-hidden-until-focus "Skip to main content" link targeting `#main`, and give `<main id="main">`. |
| Moderate | `src/app/assets/[slug]/AssetTabs.tsx:35-54` | 4.1.2, 1.3.1 | The asset tab strip is `<nav>` of `<Link>`s (URL-state tabs) with **no `role="tablist"`/`tab`/`aria-selected`/`aria-current`**. As link-based navigation it is operable, but the active tab is conveyed only by color/border — no programmatic "selected" state. | Add `aria-current="page"` to the active link (lightest fix), or adopt the full tabs pattern if these become in-page panels. Same applies to the `AssetsTable` asset-class pill row (`AssetsTable.tsx:289-298`) — active pill is color-only; add `aria-pressed`. |
| Moderate | `src/components/nav/Navbar.tsx:70`, `MobileDrawer` `:75-116` | 4.1.2, 1.3.1 | The mobile drawer is rendered/removed by `{mobileOpen && …}` with no `aria-controls` linking the hamburger to the drawer region and no focus move into it on open. `aria-expanded` is present (good). | Add `aria-controls` + an `id` on the drawer; optionally move focus to the first drawer link on open. |
| Moderate | `src/components/CurrencyCombobox.tsx:51-65` | 2.1.1 | Combobox closes on outside **mousedown** only; there is no document-level handling for it, but more importantly clicking the trigger again toggles fine — acceptable. The list options use `onMouseEnter` to set highlight; keyboard highlight has no scroll-into-view, so a long filtered list can highlight off-screen options. | Add `scrollIntoView` on highlight change (paired with the listbox ARIA fix above). |
| Moderate | react-query panels — `LedgersTable.tsx:63-84`, and every `Panel`-wrapped loading/error/empty state across the explorer | 4.1.3 Status Messages | Async "Loading…", "Failed to load…", and empty states swap in silently with **no `aria-live`/`role="status"`**, so screen-reader users get no announcement when data arrives or a fetch fails. (The `DegradedBanner` *does* use `role="status" aria-live="polite"` — good pattern to copy.) | Wrap the Panel body's loading/error region in `role="status"` (loading) / `role="alert"` (error), or add an `aria-live` region. |

### Minor

| severity | file:line | WCAG | issue | fix |
|---|---|---|---|---|
| Minor | `src/components/charts/CandleChart.tsx:49-127`, all `transition-*`/`animate-pulse`/`animate-spin` (14 occurrences incl. `Navbar.tsx:502` status dot pulse, `SignInForm.tsx:110` spinner, chevron rotations) | 2.3.3 Animation from Interactions / 2.2.2 | **`prefers-reduced-motion` is not honored anywhere** (no `motion-reduce:` utilities, no media query in `globals.css`, no chart override). The animations are all small/non-essential, so this is Minor, but the perpetual status-dot `animate-pulse` is the kind of thing reduced-motion users opt out of. | Add a `@media (prefers-reduced-motion: reduce)` block in `globals.css` to neutralise transitions/animations, or sprinkle `motion-reduce:animate-none`. |
| Minor | `src/app/page.tsx:79-98` | 1.3.1 | The Diagnostics card is a single `<Link>` wrapping a heading + paragraphs (whole-card link). Operable and has discernible text, but the long link name is verbose for SR users. Acceptable; note for polish. | Optional: make only the title the link, or add `aria-label`. |
| Minor | `src/app/explorer-shared.tsx:286-293` (`CopyHash`) | 1.1.1 | Truncated hashes rely on the `title` attribute for the full value; `title` is not exposed to all AT and not keyboard-discoverable. The adjacent `CopyValue` button *is* labelled (good). | Optionally add an `aria-label` with the full value, or a visually-hidden full string. |
| Minor | `src/app/layout.tsx:107` | 3.1.1 | `<html lang="en">` is set (good) — no issue; recorded as verified. | — |
| Minor | decorative SVGs/icons | 1.1.1 | lucide icons are largely paired with text or `aria-hidden`; the inline verified-currency check SVG in `SearchModal.tsx:437-449` is correctly `aria-hidden` with an `aria-label` on its wrapper. Spot-checked clean. | — |

---

## What's already good

- **Real semantic interactive elements.** Grep for `onClick` on
  `<div>/<span>/<li>/<tr>` returned **zero** hits — every clickable
  thing is a `<button>` or `<Link>`/`<a>`. No "div soup" interactivity,
  the single most common React a11y failure. (`AssetsTable`, `Navbar`,
  `SearchModal`, `RequestReveal`, `CurrencyCombobox` all use `<button
  type="button">`.)
- **Landmarks present.** `layout.tsx` renders `<body>` → `<nav>` (Navbar)
  → `<main>` → `<footer>` (Footer); `Navbar` is a real `<nav>`, `Panel`
  is a `<section>` with `<header>`/`<h3>`, the home hero is `<header>`.
- **`<html lang="en">`** set (`layout.tsx:107`).
- **Tables are accessible-by-default.** `LedgersTable` (and the shared
  `Th` helper) emit `<th scope="col">`; tables sit in `overflow-x-auto`
  wrappers; numeric columns use `tabular-nums`.
- **Forms are labelled.** `SignInForm` uses a real `<label>` wrapping the
  email `<input type="email">` with `autoComplete="email"` + `required`;
  `AssetsTable` search has `aria-label`; the "Per page" select is in a
  `<label>`.
- **Focus rings are mostly preserved.** Inputs use
  `focus:ring-1 focus:ring-brand-500` (`SignInForm`, `AssetsTable`,
  `MarketsTable`, `IssuersTable`, `SourcesTable`, `CursorsTable`,
  `AccountDashboard`). `outline-none` appears only where a `focus:ring`
  replaces it (no bare focus removal except the two bg-transparent inline
  numeric inputs in `AssetConverter`/`ConvertPair`/`VenueChart`, worth a
  glance but low-risk).
- **`aria-expanded` on every disclosure trigger** (hamburger, both
  dropdowns, mobile sections, account widget).
- **Heading coverage is broad** — 50+ pages carry an `<h1>` (or delegate
  to a `*View.tsx` that does); custom 404 has an `<h1>`. No systematic
  heading-skip pattern found (Panel uses `<h3>` for card titles, which
  is a minor demotion but consistent).
- **Status dot has a text alternative** — `Navbar StatusPill` is an `<a>`
  with `aria-label="API status: …"` and the colored dot is `aria-hidden`.
- **`DegradedBanner` is a model status region** — `role="status"
  aria-live="polite"`, icon `flex-shrink-0`, text + link. (Its *colors*
  are broken — see Serious — but the a11y semantics are exemplary.)
- **ThemeToggle** has a descriptive `aria-label` and renders a sized
  placeholder pre-mount to avoid layout shift.
- **`prefers-color-scheme`** is honored as the dark-mode fallback
  (`layout.tsx` inline init script + `ThemeToggle.applyMode`).

---

## Files read

- `src/app/layout.tsx`
- `src/app/globals.css`
- `tailwind.config.ts`
- `src/app/page.tsx`
- `src/app/not-found.tsx`
- `src/components/nav/SearchModal.tsx`
- `src/components/nav/Navbar.tsx`
- `src/components/nav/ThemeToggle.tsx`
- `src/components/nav/DegradedBanner.tsx`
- `src/components/reveal/Panel.tsx`
- `src/components/reveal/RequestReveal.tsx`
- `src/components/CurrencyCombobox.tsx`
- `src/components/charts/CandleChart.tsx`
- `src/app/explorer-shared.tsx`
- `src/app/ledgers/LedgersTable.tsx`
- `src/app/ledger/LedgerView.tsx`
- `src/app/assets/AssetsTable.tsx` (filter/sort region)
- `src/app/assets/[slug]/AssetTabs.tsx`
- `src/app/signin/SignInForm.tsx`
- Grep sweeps across all `src/**/*.tsx` for: `onClick` on non-interactive
  elements, `<img`, `role=`, `aria-live`, `aria-current`/`aria-selected`/
  `role="tab"`, `tabIndex`, `role="listbox|option|combobox"`,
  `focus-visible`/`focus:ring`/`outline-none`, `animate-*`/`transition-*`,
  `prefers-reduced-motion`, `<h1>` coverage across 55 `page.tsx`,
  undefined `ink`/`bad`/`warn` palette classes, `text-slate-400` /
  `text-[10px]` / `text-[11px]` contrast surfaces.

_Not exhaustively read: per-page detail views beyond ledgers/ledger
(tx/contract/accounts/exchanges/dexes/oracles `*View.tsx`), embed pages,
the `dev/primitives` showcase, and the markdown renderer — these reuse
the same `Panel`/table/`CopyHash` primitives audited above, so the
findings generalise but specific instances were not enumerated._
