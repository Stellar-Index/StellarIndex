---
title: Design system — Stellar Index web (v3, dark)
date: 2026-07-21
status: active
last_verified: 2026-07-21
supersedes: v2 (light-mode, 2026-06-17)
---

# Design system

The shared visual language for every Stellar Index web surface — the public
explorer (`web/explorer`, which also hosts the in-site customer account at
`/account/*`) and the status page (`web/status`). One system, applied
everywhere.

**Live reference:** `/dev/styleguide` (+ `/dev/primitives`) in the explorer
renders every token and component. **Tokens:** the `@theme` block in
`web/explorer/src/app/globals.css` — Tailwind 4 defines the design tokens inline
in CSS, there is **no `tailwind.config.ts`**. **Components:**
`web/explorer/src/components/ui/` (barrel: `@/components/ui`). Direction +
rationale: `docs/frontend/dark-redesign-direction.md`. Restructuring companion:
`docs/frontend/redesign-readiness.md`.

## Principles

1. **Dark only.** A neutral near-black canvas (`#0a0b0d`, a whisper of cool).
   Design for depth via **surface layering + hairline borders**, not shadows.
2. **Minimal, instrument-like, dense.** Hairline "ledger-grid" dividers and
   tabular data. Let the data read like a readout; cut chrome.
3. **Functional colour.** Blue (`brand`) = identity/action (links, focus,
   active, primary); green/red (`up`/`down`) = price direction + freshness;
   amber (`warn`/time-pin) = severity/time. Never decoration.
4. **Serif ⟷ mono is the signature.** **Fraunces** (serif) carries the display/
   identity voice — page titles (`h1`), hero figures (`.font-display`). **Inter**
   is body/UI. **JetBrains Mono** carries *all data* — prices, volumes, ledger
   seqs, hashes, table cells, and eyebrow/section labels (uppercase, tracked).
   Aligned numbers use tabular figures (`.tnum` / `font-mono`).
5. **Reuse the primitives.** Build pages from `@/components/ui`, not bespoke
   markup. If a pattern recurs, promote it to a primitive.

## Tokens

| Group | Tokens | Use |
|---|---|---|
| **brand** | `brand-50…950` (accent `brand-500/600`, bright text `700/900`, dark tints `50/100`) | Actions, links, focus, active, selection |
| **surface** | `surface-canvas` (`#0a0b0d` page bg), `surface` (card), `surface-muted` (raised/hover), `surface-subtle` (well) | Layering: canvas → card → raised |
| **line** | `line-subtle`, `line`, `line-strong` | Hairline borders (default `border` colour is `line`) |
| **ink** | `ink` (headings), `ink-body`, `ink-muted`, `ink-faint` | Text hierarchy (light on dark) |
| **up / down** | `up`, `down` (+ `subtle`/`strong`) | Price direction, deltas, freshness |
| **warn / bad / ok** | `*-50/300/500/700` (`-50` = **dark tint bg**, `-700/900` = **bright text**) | Severity (banners, status, validation) |
| **type** | `text-display`, `display-sm`, `h1`, `h2`, `h3`; fonts `font-serif` / `font-sans` / `font-mono` | Display serif, body sans, data mono |
| **radius** | `rounded-card` (**6px**), `rounded-lg` (6px), `rounded-md` (4px), `rounded-full` | Crisp, not childish; `full` only for dots/avatars |
| **shadow** | `shadow-elevated` (overlays only), `shadow-focus` | On dark, depth = borders + layering; shadows for menus/overlays only |
| **width** | `max-w-page` (1728px), `max-w-prose` | Page frame / reading measure |

Charts (lightweight-charts) theme from these same tokens via
`src/components/charts/chartTheme.ts` — no hardcoded colours.

## Components (`@/components/ui`)

`Button`/`ButtonLink`, `Card`/`CardHeader`/`CardBody`/`CardFooter`, `Badge`,
`Stat`/`StatGrid`/`StatCell`, `Table` primitives, `Container`/`Section`/
`PageHeader`/`Breadcrumbs`/`SectionHeader`, `Input`/`Textarea`/`Select`/`Field`,
`EmptyState`/`Skeleton`/`Callout`, `TabNav`/`Segmented`, `Mono`/`CopyButton`.

Compose class strings with `cn()` (`@/lib/cn`) — clsx + tailwind-merge, so
overrides resolve last-wins without specificity fights.

## Patterns

- **Page scaffold:** `<Container><Section><PageHeader …/> … </Section></Container>`
  (the `h1` renders in the serif display voice).
- **Metric strip:** `StatGrid` of `StatCell` + `Stat` (tabular figures).
- **Data table:** `TableWrap > Table > THead/TBody`; right-align + `tnum`; the
  hero surface — dense, hairline rows, mono numerics, no zebra.
- **Empty / loading:** `EmptyState` for no-data, `Skeleton` for loading.
- **Identifiers:** `Mono` with `truncate` for G-strkeys / C-ids / tx hashes.
- **Freshness / verification:** a small `ok`-toned chip (`✓ 21h`) — Stellar
  Index's "verified, fresh on-chain data" made visible.

## Do / Don't

- ✅ Hairline borders + layering for depth. ❌ Heavy drop shadows / boxes-in-boxes.
- ✅ Functional colour (blue action, green/red direction). ❌ Decorative accents.
- ✅ `ink-muted` for secondary text. ❌ Low-contrast grey-on-grey body copy.
- ✅ Serif for display/titles, mono for all data. ❌ Serif in body/UI; sans for numerics.
- ✅ Tabular figures for aligned numbers. ❌ Proportional figures in tables.
- ✅ Reuse `@/components/ui`. ❌ Re-implement buttons/cards/badges inline.
- ✅ Dark is the only mode (semantic tokens are defined dark). ❌ Adding `dark:`
  variants or a light/dark toggle — there's one mode.
