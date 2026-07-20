---
title: Dark redesign — design direction
date: 2026-07-20
status: draft (direction locked, implementation in progress)
supersedes: the "light mode only" decision in design-system.md (§Principles)
references: Dune.com product UI (dark data aesthetic), docs/frontend/redesign-readiness.md
---

# Dark redesign — design direction

## Subject (grounding)

**Stellar Index** — protocol explorer + independent VWAP pricing API for the
Stellar network. Audience: protocol teams, quant/trading, integrators who care
about **precision and verifiability** of on-chain value. The interface's job:
make dense, verified on-chain data (ledgers, contracts, trades, prices) legible
and trustworthy at a glance. The material — **ledger sequences, tx hashes,
tabular price/volume, protocol identity, as-of time-pinning, freshness/
verification** — is where the distinctive choices come from.

Reference: **dune.com's product UI** (not its marketing site) — near-black
neutral canvas, hairline "ledger grid" structure, serif page titles over dense
mono data tables, thin line icons, green/red deltas, freshness checkmarks.

## The signature: editorial serif ⟷ terminal mono (and the one risk)

The memorable tension is **two voices**: a **refined serif** carries *identity* —
page titles, section heads, hero figures — with editorial restraint; **JetBrains
Mono** carries the *data* — every table cell, price, ledger seq, hash, and
eyebrow — like a terminal readout. A serif "Stellar" sitting over a dense mono
ledger table *is* the personality: institutional-grade identity, machine-grade
data.

**The risk (one, justified):** an editorial serif inside a data tool. Justified
because the serif↔mono contrast is the identity, it matches the "institutional
pricing API" positioning, and it's grounded in the reference — while being the
opposite of the two AI-default darks (near-black + single acid accent; or
all-Inter flatness). The serif is used *only* for display; everything else stays
quiet.

Second signature thread: the **freshness / verification chip** (a small green
`✓ 21h` recency stamp, and the two-axis completeness verdict) — Stellar Index's
actual product truth ("complete, verified, fresh on-chain data"), rendered as a
recurring element rather than decoration.

## Palette (near-black **neutral**, a whisper of cool — not "blue")

Color **encodes meaning** (this is why it isn't the generic "near-black + one
accent" default — every hue is functional):

| Token group | Role | Values (intent — finalized in code w/ contrast checks) |
|---|---|---|
| **canvas / surface / raised / well** | layering, darkest→up | `#0A0B0D` · `#101216` · `#161A20` · `#0C0E12` |
| **line-subtle / line / line-strong** | hairline borders (the ledger grid) | `#16181D` · `#22262E` · `#2E333C` |
| **ink / body / muted / faint** | text hierarchy | `#F5F7FA` · `#C8CED8` · `#8B92A0` · `#5C6472` |
| **brand (blue)** | identity + interactive (links, focus, active, primary) | accent `#4C7DFF`, hover `#6E97FF`, tint-bg `#0F1E45` — the existing Stellar Index blue, brightened for dark |
| **up / down** | price direction + deltas only | up `#31C48D` · down `#F6465D` (+ dark-wash `-subtle` tints) |
| **warn / time-pin** | severity + the as-of time-machine | amber `#E0A63C`, pin-tint `#241C0C` |
| **ok / verify** | freshness / verification chip | green `#31C48D` (shares `up`) |

Blue = identity/action, green/red = price direction & freshness, amber =
time/severity. Three functional hues on a neutral near-black.

## Type (tri-face)

- **Display — serif, restrained.** Page titles, section heads, hero figures.
  Pick a characterful-but-modern open-source serif, self-hosted via `next/font`
  (render-test **Fraunces** / **Newsreader** / **Source Serif 4** — leaning
  Fraunces at low softness for character without childishness). Display weights
  only, latin subset.
- **Body / UI — Inter** (already loaded).
- **Data / numerics / eyebrows / identifiers — JetBrains Mono** (already loaded),
  tabular (`.tnum`), often uppercase+tracked for eyebrows. Prices, volumes,
  ledger seqs, hashes, table cells.
- **Nav & section labels — sentence-case sans**, muted (per Dune — quiet, not
  shouty uppercase). Mono is reserved for *data*, not chrome.

## Structure, radius, elevation

- **Radius — crisp, not childish:** cards `6px` (down from 14px), controls `4px`,
  full only for avatars/status-dots/pills.
- **Elevation without shadows:** depth = **surface layering + hairline borders**,
  not drop shadows. Card = raised surface + 1px `line`. Only overlays/menus get a
  real shadow (`0 8px 24px rgba(0,0,0,.5)` + border).
- **The ledger grid:** hairline dividers form spreadsheet-like cells (Dune's
  "structure is information"). **Tables are the hero surface** — dense,
  tabular-mono, muted-gray headers, right-aligned numerics, subtle row hover, no
  zebra.
- **Stat/counter cells:** small muted label over a large figure, green/red delta
  in parens; cells split by hairlines inside a bordered container.
- **Left nav kept** (per brief): a quiet dark rail, **minimal line icons**
  (lucide, ~1.5px stroke, consistent), sentence-case muted items, brand-blue
  active state via a thin left-edge marker + faint raised surface (not a fat
  pill). Collapsible groups.

## Icons

Keep **lucide-react** (already a line-icon set). One stroke width, used only
where an icon aids scanning (nav, status, actions). No filled/duotone. Action
clusters = low-radius bordered icon buttons (per Dune's top-right toolbar).

## Charts (recon complete — verdict: KEEP lightweight-charts, augment)

Only two engines: **lightweight-charts v5.2.0** (all time-series) + custom SVG
(donut/sparkline/bars). No switch, no roll-your-own. Work items:

1. **Combined price + volume via native panes.** v5.2.0 has a real multi-pane
   API (`chart.addPane()`, `addSeries(HistogramSeries, opts, paneIndex=1)`,
   `moveToPane`). Move the current bottom-overlay volume (`CandleChart.tsx:115-124`)
   to a true volume pane below the price pane, shared time axis — the canonical
   trading layout the brief asks for.
2. **Theme-tokenize the charts (the biggest dark lever).** Colors are hardcoded
   slate literals at create-time (`CandleChart.tsx:69-124`, `LineChart.tsx:107-198`,
   `DonutChart` palette, `Sparkline` strokes). Drive
   background(transparent)/textColor/grid/border/crosshair/series from the dark
   tokens (read CSS vars off the container; re-`applyOptions` on change).
3. **Data-driven granularity selector.** The API imposes **no** forbidden
   timeframe×granularity matrix (`internal/api/v1/chart.go:747`, `ohlc_series.go`)
   — any grain valid for any window; limits are the 1000-bar OHLC cap + runtime
   coverage. `/v1/ohlc` serves `1m,5m,15m,30m,1h,4h,1d,1w,1mo`. Offer set from a
   bar budget (`MIN_BARS≈24 ≤ ceil(W/i) ≤ cap`); default = finest ≤ ~500–720 bars
   (= today's default, so additive):

   | Window | Selectable | Default |
   |---|---|---|
   | 24h | 5m · 15m · 30m · 1h | 5m |
   | 7d | 15m · 30m · 1h · 4h | 15m |
   | 30d | 1h · 4h · 1d | 1h |
   | 90d | 4h · 1d | 4h |
   | 1y | 1d · 1w | 1d |
   | all | 1w · 1mo | 1w |

   Then **refine at runtime from coverage**: `/v1/chart` exposes
   `truncated`/`data_starts_at`; `/v1/ohlc` infers from the returned series.
   Show an honest "history begins &lt;date&gt;" note; grey out no-data combos.
   NOTE: only ~7d of high-res history exists today (backfill in progress) — long
   fine-grain windows are truncated until D1 lands, then fill in automatically.
4. **Unify the 3 duplicate embed sparklines + PriceSparklines** while retheming.

Unifying on a granularity-aware `MarketChart` upgrades five surfaces at once
(`ChartPanel.tsx:68`, `PairChart.tsx:22`, `HomeHeroChart.tsx:46`,
`SourceTopChart.tsx:63`, `VenueChart.tsx:94`).

## Implementation plan (token-first — the leverage)

Because the system is **semantic-token-based**, most of the flip happens in
`globals.css @theme`:

1. **Tokens** — redefine surface/line/ink to dark; re-tune brand; add dark
   up/down/warn tints; drop radius; swap shadows for border elevation; add the
   serif font var. Set `<html class="dark">` (or make dark the only mode).
2. **Primitives** — fix the few that assume light (`Badge`/`Callout` tone maps
   use raw `-50/-700` steps → semantic tone tokens; audit `bg-white`/hardcoded
   light). Restyle Button/Card/Table/Stat/Input; add the freshness chip.
3. **Nav** — dark rail, tuned line icons, sentence-case items, thin active marker.
4. **One exemplar page + the combined chart** — build, **screenshot**, get a read.
5. **Roll out** across routes (mostly free from step 1), gating on
   `pnpm test && pnpm typecheck`; update `design-system.md` + the CLAUDE brief.

Validate every step **rendered** (screenshots) — a picture beats a plan.
