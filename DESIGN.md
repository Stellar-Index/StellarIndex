---
name: Stellar Index
description: Stellar protocol explorer and API. Dark only; the category standard, played straight.
colors:
  surface-canvas: "#0b0c0f"
  surface-subtle: "#0f1115"
  surface: "#121418"
  surface-muted: "#181b21"
  line-subtle: "#15181d"
  line: "#22262e"
  line-strong: "#2e3340"
  line-input: "#6b7385"
  ink-strong: "#f5f6f8"
  ink: "#e6e8ec"
  ink-body: "#c2c6cf"
  ink-muted: "#8b929e"
  ink-faint: "#7f8692"
  brand-fill: "#2563eb"
  brand-fill-hover: "#1d4ed8"
  brand-50: "#0f1a33"
  brand-500: "#3b82f6"
  brand-600: "#4d8df8"
  brand-700: "#7aaaff"
  up-subtle: "#0f241c"
  up: "#31c48d"
  up-strong: "#4ade9e"
  down-subtle: "#2a1319"
  down: "#f6465d"
  down-strong: "#ff6b7a"
  warn-50: "#241c0c"
  warn-500: "#e0a63c"
  warn-700: "#f3ca7d"
  bad-50: "#2a1216"
  bad-700: "#ff9aa6"
  ok-50: "#0f241c"
  ok-700: "#6ee0b0"
typography:
  display:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "3rem"
    fontWeight: 600
    lineHeight: 1.1
    letterSpacing: "-0.02em"
  display-sm:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "2.25rem"
    fontWeight: 600
    lineHeight: 1.12
    letterSpacing: "-0.02em"
  headline:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "1.75rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.015em"
  title:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.35
    letterSpacing: "-0.006em"
  body:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.43
  label:
    fontFamily: "Onest, system-ui, sans-serif"
    fontSize: "11px"
    fontWeight: 500
    lineHeight: 1.5
    letterSpacing: "0.05em"
  data:
    fontFamily: "Geist Mono, ui-monospace, SFMono-Regular, monospace"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.43
    fontFeature: "\"tnum\""
  stat:
    fontFamily: "Geist Mono, ui-monospace, SFMono-Regular, monospace"
    fontSize: "1.5rem"
    fontWeight: 600
    lineHeight: 1.33
    letterSpacing: "-0.025em"
    fontFeature: "\"tnum\""
rounded:
  sm: "4px"
  md: "6px"
  lg: "8px"
  full: "9999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "12px"
  lg: "16px"
  xl: "24px"
  2xl: "32px"
  section: "56px"
components:
  button-primary:
    backgroundColor: "{colors.brand-fill}"
    textColor: "#ffffff"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 14px"
  button-primary-hover:
    backgroundColor: "{colors.brand-fill-hover}"
  button-primary-lg:
    backgroundColor: "{colors.brand-fill}"
    textColor: "#ffffff"
    rounded: "{rounded.md}"
    height: "40px"
    padding: "0 20px"
  button-secondary:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 14px"
  button-secondary-hover:
    backgroundColor: "{colors.surface-muted}"
  button-ghost:
    textColor: "{colors.ink-body}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 14px"
  button-danger:
    backgroundColor: "{colors.down}"
    textColor: "{colors.surface-canvas}"
    rounded: "{rounded.md}"
    height: "36px"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    height: "36px"
    padding: "0 12px"
  input-finder:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    rounded: "{rounded.lg}"
    height: "52px"
    width: "720px"
    padding: "0 136px 0 16px"
  card:
    backgroundColor: "{colors.surface}"
    rounded: "{rounded.lg}"
    padding: "16px"
  stat-tile:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink}"
    typography: "{typography.stat}"
    rounded: "{rounded.lg}"
    padding: "16px"
  table-header:
    backgroundColor: "{colors.surface-subtle}"
    textColor: "{colors.ink-muted}"
    typography: "{typography.label}"
    height: "40px"
    padding: "0 16px"
  table-cell:
    textColor: "{colors.ink-body}"
    typography: "{typography.body}"
    padding: "10px 16px"
  badge-neutral:
    backgroundColor: "{colors.surface-subtle}"
    textColor: "{colors.ink-body}"
    rounded: "{rounded.full}"
    padding: "2px 8px"
  badge-brand:
    backgroundColor: "{colors.brand-50}"
    textColor: "{colors.brand-700}"
    rounded: "{rounded.full}"
    padding: "2px 8px"
  nav-item:
    textColor: "{colors.ink-body}"
    rounded: "{rounded.md}"
    height: "32px"
    padding: "0 10px"
  nav-item-active:
    backgroundColor: "{colors.surface-muted}"
    textColor: "{colors.ink}"
  reveal-trigger:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.md}"
    padding: "2px 6px"
---

# Design System: Stellar Index

## Overview

**Creative North Star: "The Standard Instrument"**

A dark, quiet data console held to the craft level of Dune, Linear and Vercel. Nothing is themed; convention is the commitment. Near-black cool surfaces separated by hairlines, one working blue, one grotesk for words and one mono for figures. Numbers carry the page; chrome stays out of their way.

Dark is the only mode. Every semantic token is defined dark, so a component built on the surface, ink and line tokens needs no variant, and there is no toggle.

**Scope.** The landing `/`, the shared shell (sidebar, mobile header, footer, search modal), the `src/components/ui` primitives and the markets, assets, venue, protocol, ledger, transaction, operation, contract, account, issuer, transactions, contracts and oracles pages are built to this contract. Because tokens and fonts live in one `@theme`, the other routes already inherit these values, but their compositions and page-local styling are incumbent until each is rebuilt to this contract, page by page. Where an unmigrated page disagrees with this file, this file wins.

**Key Characteristics:**
- Tonal layering and 1px lines carry depth; cards cast no shadow.
- One blue, split into a fill family (behind white) and an ink family (on dark).
- Two faces: Onest for UI and display, Geist Mono for identifiers and figures.
- Every ink passes 4.5:1 on every ambient surface; every control edge passes 3:1.
- Motion only where data actually changes.

## Colors

Cool near-black neutrals with a single blue accent and a semantic set for direction and severity.

### Primary
- **Action Blue** (`brand-fill`): the fill behind white text: primary buttons, the finder's Search button, the avatar disc. Hover darkens to `brand-fill-hover`, because a fill must darken to keep carrying white.
- **Link Blue** (`brand-600`): the ink for links, the active nav icon and hover accents on dark. `brand-500` is the focus-ring, caret and data-bar blue; `brand-700` is chip text on `brand-50`.

### Neutral
- **Canvas** (`surface-canvas`): the page; also the PWA and address-bar tint.
- **Rail** (`surface-subtle`): sidebar, mobile header, table header row, disabled fields.
- **Panel** (`surface`): cards, stat tiles, tables, inputs, modals.
- **Raised** (`surface-muted`): hover and active rows, active nav item, inline tags.
- **Hairline** (`line`), **Quiet hairline** (`line-subtle`), **Firm line** (`line-strong`): dividers, card edges, secondary-button edges and hover borders.
- **Control edge** (`line-input`): the border of anything you type into or toggle.
- **Inks**, brightest to quietest: `ink-strong` (selection, emphasis), `ink` (headings, values), `ink-body` (running text, cells), `ink-muted` (labels, sub-lines), `ink-faint` (stamps, placeholders, group labels).

### Semantic
- **Up / Down** (`up`, `down` with `-strong` inks and `-subtle` tints): price direction only. Always paired with a sign or arrow; never colour alone. The one other use is the order book's sides, bids `up` and asks `down`, always under a "Bids" / "Asks" label. Series that are not price direction (received and sent, inbound and outbound, supplied and borrowed) take `brand-500`, `ink-muted` or a categorical hue, never `up`/`down`.
- **Warn / Bad / Ok** (`-50` tint backgrounds, `-700` inks): severity badges, stale markers, the degraded banner. `warn-500` marks "stale" inline. They mean severity only, never direction.
- **Chart hues** (`chart-1`..`chart-9`, `chart-tail`, exported as `CATEGORICAL_PALETTE`): composition and categorical series. The tail grey is the "Others" / unknown slot. One entity keeps one hue across a donut and its companion chart.
- Category chips (venue, protocol, type) use the violet, teal, indigo, purple and yellow `-subtle`/`-strong` pairs in `globals.css`, routed through `src/lib/pillTone.ts`.

### Named Rules
**The One Blue Rule.** Blue is the only accent. It marks the primary action, links, focus and the active place in the nav. Category tints are for chips, never for chrome.

**The Fill-and-Ink Rule.** White text sits only on `brand-fill`, `brand-fill-hover` or darker. Never white on `brand-500`/`brand-600` or on the red; danger buttons put canvas-dark text on red.

**The Every Ink Passes Rule.** Any new ink or surface must pass 4.5:1 for text over canvas, subtle, surface and muted, and 3:1 for control boundaries. `color-contrast.a11y.test.ts` is the arbiter.

## Typography

**Display Font:** Onest (with system-ui)
**Body Font:** Onest (with system-ui)
**Label/Mono Font:** Geist Mono (with ui-monospace, SFMono-Regular)

**Character:** A plain, slightly warm grotesk for every word, tightened at display sizes, beside a technical mono that makes ids and figures read as data. Both are self-hosted through `next/font` as `--font-sans` and `--font-mono`.

### Hierarchy
- **Display** (600, 3rem, 1.1; 2.25rem below 640px): the landing h1 only, centred, balanced.
- **Headline** (600, 1.75rem, 1.2): the page h1 in `PageHeader`.
- **Title** (600, 1.125rem, 1.35): every section h2 (`SECTION_HEADING_CLASS`); card titles drop to 14px semibold.
- **Body** (400, 14px, 1.43): cells, panel text, descriptions. The landing subtitle steps up to 16–18px in `ink-muted`.
- **Label** (500–600, 11px, 0.05em, uppercase): stat labels, table headers, sidebar group names, panel labels (`PANEL_LABEL_CLASS`).
- **Data** (Geist Mono, tabular): addresses, hashes, endpoints, prices, volumes, stat values.

### Named Rules
**The Two Faces Rule.** Onest for language, Geist Mono for data. No third face, no serif display, no system-font display.

**The Figures Align Rule.** Every number in a column or tile uses tabular figures (`tnum`) and right-aligns in tables. Ids, ledger sequences, hashes and years render as raw digits, never digit-grouped; counts may carry thousands separators (or compact form with the exact value in the title). Snapshot stamps read "as of HH:MM:SS UTC" in `ink-faint`.

**The Name Rule.** XLM is named "Stellar Lumens" in UI text, never "native".

## Layout

A persistent 240px left sidebar (sticky, full height) and a scrolling content column, no top bar. Below 1024px the sidebar becomes a 288px drawer opened from a 56px mobile header.

Content sits in `Container` (max 1728px, 16/24/32px side padding). The landing narrows to 1200px: a centred 860px hero, a 720px finder, then full-width sections stacked at 56px. Elsewhere, sections breathe at 24–32px. Grids gap 12–16px. The stat strip runs 2, 3 then 5 columns; panel rows run 1 then 3. Tables span the column and scroll horizontally inside their wrapper rather than reflow.

Every non-embed page opens with breadcrumbs, a headline title and optional meta through `PageHeader`; `/embed/*` renders chrome-free.

A stat strip (`StatGrid` with `StatCell` and `Stat`) forces `className="grid-cols-2"` below `sm`, so tiles never stack to one column on a phone. A table sits in a `Panel` (`src/components/reveal/Panel.tsx`) with `bodyClassName="-mx-4 -mb-4"` so the table runs edge to edge; controls above it take `mx-4 mb-4`. A panel that is a page's top-level section passes `headingLevel={2}`.

## Elevation & Depth

Flat. Depth is the step from canvas to rail to panel to raised, plus 1px lines; `shadow-card` and `shadow-xs` are deliberately empty. Only things that float above the page cast a shadow: menus, the mobile drawer, the skip link. Modal and drawer scrims are black at 60% with a small backdrop blur.

### Shadow Vocabulary
- **Elevated** (`box-shadow: 0 0 0 1px var(--color-line), 0 16px 40px -12px rgb(0 0 0 / 0.7)`): popovers, menus, drawer.
- **Focus** (`ring 2px brand-500 at 70%, offset 2px canvas`): every focusable element via `:focus-visible`.

### Named Rules
**The Hairline Depth Rule.** A surface earns separation with a lighter step or a line, never a shadow. A shadow means "this floats".

## Shapes

Small, consistent radii that read as tooling, not softness: 4px for inline tags and the reveal's link buttons, 6px for buttons, inputs and nav items, 8px for cards, tiles, tables, the finder and modals, fully round for badges, status dots and the avatar. Borders are always 1px.

## Components

### Buttons
- **Shape:** gently squared (6px).
- **Primary:** Action Blue fill, white 13px medium text, 36px tall (32px small, 40px large). Hover and active darken.
- **Secondary:** panel fill, `ink` text, `line-strong` edge; hover lifts to `surface-muted` with a `line-input` edge.
- **Ghost / Subtle:** text only, or rail fill; hover to `surface-muted`.
- **Hover / Focus:** 150ms colour transition; focus ring per Elevation. Disabled is 50% opacity.

### Chips (Badges)
- **Style:** fully round, 12px medium, 1px inset ring in the tone. Neutral, brand, up, down, warn, bad and ok tones; an optional leading dot in `currentColor`.

### Cards / Containers
- **Corner Style:** 8px.
- **Background:** `surface` on canvas.
- **Shadow Strategy:** none (see Elevation).
- **Border:** `line`; clickable cards shift to `line-strong` on hover.
- **Internal Padding:** 16px; header and footer rows 12px × 16px with hairline dividers.

### Stat Tile
The landing's metric unit: a linked card holding an 11px uppercase label, a 24px Geist Mono value, a sub-line and an "as of HH:MM:SS UTC" stamp in `ink-faint`. A lower bound is prefixed `≥` and labelled; staleness appends "· stale" in warn.

### Inputs / Fields
- **Style:** panel fill, `line-input` edge, 6px, 36px tall, `ink-faint` placeholder.
- **Focus:** edge to `brand-500` plus a 2px `brand-500` ring at 30%.
- **Error / Disabled:** error text in `bad-700` wired by `aria-describedby`; disabled drops to rail fill at 70%.
- **Finder:** the landing's 720×52px, 8px-radius field with an embedded 40px Search button and a live input-shape tag (Account, Contract, Tx hash, Ledger, Pair, Asset).

### Tables
Hairline-ruled, inside an 8px panel wrapper. Header row on rail with 11px uppercase `ink-muted` labels, 40px tall; cells 10×16px in `ink-body`; rows hover to `surface-muted` at 70%. Numbers right-aligned, tabular, in mono.

- **Sorting:** `useTableSort` with `SortableTh`. The header is a button with `aria-sort` on the `th`; the arrow sits on the column's outside edge (left of the label when right-aligned, right of it otherwise). The inactive arrow appears on hover and focus; empty values sort last in either direction.
- **Phone folding:** secondary columns carry `hidden sm:table-cell` (`md:` for the least important). A denomination or quote hidden with its column moves into the value cell as a muted `text-xs sm:hidden` suffix, so a bare number never loses its unit.
- **Bar beside a number:** `InlineBar` (80px track, `brand-500` fill, `surface-muted` ground, labelled) is shown from `sm` up, beside a fixed-width (`w-14`), right-aligned figure so the column's digits line up whatever the bar does.
- **Share column:** a ranked table whose rows are a listed subset heads its share column "Share of listed", in `ink-muted`, because the total is not the whole population.
- **Secondary count:** a count that qualifies a primary one follows it as a muted suffix ("12 +3 unmapped"); a zero primary shows an `ink-faint` dash, and the suffix then drops its plus.
- **Note row:** loading, empty and unavailable states are a single full-width row inside the body (`ink-muted`, 16px vertical padding, no hover). Loading is a skeleton; empty states what is absent; unavailable says the read did not return, so the answer is unknown rather than empty, and ends in an inline small secondary Retry button.
- **Table toolbar:** controls sit above the table in a wrapping row: a `Segmented` filter, or a pager of small secondary Newer and Older buttons (Newer is a disabled `Button` at the tip, not a dead link) followed by quiet `brand-600` `text-xs` links such as "Open ledger". While a filter is active the panel title reads "(n of N)".

### Segmented
`Segmented` (`components/ui/Tabs.tsx`) is the one compact toggle group for a filter or window: a `surface-subtle` track, 8px radius, `aria-pressed` buttons, the active option on `surface`. It takes a required `ariaLabel`, and is omitted when the filter would have a single real choice.

### Charts
Pure CSS or SVG marks drawn from tokens; geometry uses JS numbers, while every printed figure is a pre-formatted `display` string. Identity is never colour alone: a legend, label or value sits beside each mark.
- **HBarList:** labelled bars on one shared linear scale, `brand-500` by default, with the figure beside the bar. A lower bound ends in a hatched tail (`HATCH_BG`) paired with an annotation saying why.
- **PairedBars:** two thin bars per row on one scale, with a legend naming both series. A missing half draws no bar and shows "—", never a zero-length bar.
- **DivergingColumns:** a zero baseline with one side per direction, both on one scale; a side with no data collapses. Its defaults, `up` and `down`, are for price direction (premium or discount) only; any other pair passes `posColor` and `negColor` (a count of received against sent uses `brand-500` and `ink-muted`).
- **StackedColumns:** one column per bucket, one coloured segment per series, with a legend; series colours come from the chart hues.
- **DonutChart:** composition of a whole, slices largest first from `CATEGORICAL_PALETTE`, a centre total and a clickable legend whose percentages are computed from exact decimal strings.

### Issuer-authored content
Text and URLs that an issuer publishes (SEP-1 `stellar.toml`) are untrusted. A URL links only through `safeHttpsUrl` (plain https, no credentials, public hostname) with `rel="noreferrer noopener nofollow"`; anything else renders as `ink-muted` text. Handles (GitHub, X) are validated and rebuilt into a canonical URL, never linked raw. The organisation panel states in its hint whether the organisation confirmed the listing (`stellar.toml` lists the account back) or the claim is self-declared. Authorization flags show only the flags that are set, as `warn` dot chips with a title, followed by an `ink-faint` "Unset:" line naming the rest; with none set a neutral "None set" chip appears, and with no read yet a neutral "Not read yet" chip.

### Not found and unavailable
A read that answers "does not exist" and a read that fails are different states and never share copy. A true not-found is a `warn` `Callout` or an `EmptyState` saying what was not observed, with a route to the nearest page that may exist. A failed read is a `bad` `Callout`, an `EmptyState` or a table note row that says the answer is unknown rather than empty and carries an inline Retry. An absent response is never rendered as zero or as an empty list.

### Folds and notes
Records that are present but dormant (assets with no activity, raw XDR, coverage detail) sit in a `<details>` below the live rows, with an `ink-muted` summary stating the count and what is missing. `NoteBadge` (`components/NoteBadge.tsx`) is a `warn` "partial" badge that expands the qualifying note, for a partial-coverage figure.

### External references
`CrossReference` (`components/CrossReference.tsx`) is the only place a link leaves the explorer for another explorer: one `ink-faint` "External references" line under a hairline at the foot of an entity page, never mid-content. A target with no route for the entity or network is omitted.

### Navigation
- **Sidebar:** rail fill, `line` right edge. Wordmark "Stellar" semibold + "Index" light beside the Stellar mark. Search trigger with a ⌘K hint, then grouped 32px items (13px medium, 16px lucide icon). Group names are 11px uppercase `ink-faint`.
- **States:** rest `ink-body` with `ink-faint` icon; hover to panel fill; active on `surface-muted` with `ink` text and a Link Blue icon.
- **Mobile:** 56px header with mark and menu button; drawer is a focus-trapped dialog.

### Request Reveal (signature)
The `<>` button on every data panel: a small bordered 6px chip in `ink-muted` that turns Link Blue on hover and opens a modal with the exact curl call and URL. It is the visual proof that each panel is one public call; every new panel carries one.

### Rolling Number (signature)
Live counters (ledger tip) roll changed digits up one line like a mechanical wheel (0.65s, `cubic-bezier(0.22, 0.9, 0.28, 1)`); unchanged digits stay still. A live price tip may flash its direction colour for 0.9s on a tick. Reduced motion collapses both to an instant settle.

## Do's and Don'ts

### Do:
- **Do** build every surface from the surface, ink and line tokens so it needs no mode variant.
- **Do** put white text only on `brand-fill` or darker, and run new token pairs through `color-contrast.a11y.test.ts`.
- **Do** give control edges `line-input` and decorative dividers `line`.
- **Do** set ids, hashes, endpoints and figures in Geist Mono with tabular figures, and stamp every closed-bucket number with its "as of" time.
- **Do** fold secondary columns away on phones and keep each value's unit beside it.
- **Do** keep an unavailable read distinct from an empty one, with Retry on the former.
- **Do** put a Request Reveal on every data panel and use `SECTION_HEADING_CLASS` / `PANEL_LABEL_CLASS` for headings and in-panel labels.

### Don't:
- **Don't** add a light theme, a toggle or `dark:` variants.
- **Don't** give cards or tiles a shadow; reserve `shadow-elevated` for things that float.
- **Don't** introduce a second accent, gradients, glows or raw palette colours (`emerald-400`, `sky-…`); use tokens and `pillTone`.
- **Don't** add a third typeface or a serif display face.
- **Don't** animate for its own sake: no pings, pulses, entrance reveals or parallax. Motion marks a real data change.
- **Don't** use `up`/`down` for anything but price direction and order-book sides, or `warn`/`bad`/`ok` for anything but severity.
- **Don't** link an issuer-authored URL without `safeHttpsUrl`, or put an external explorer link anywhere but `CrossReference`.
- **Don't** digit-group ids, ledger sequences, hashes or years, and don't write "native" for Stellar Lumens.
- **Don't** set a kicker or eyebrow label above a heading; the trust line sits beneath the finder as a caption.
