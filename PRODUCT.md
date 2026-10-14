# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary: builders and integrators evaluating or wiring the Stellar Index API into
wallets, exchanges, bots and dashboards. They read a number on screen, open the
`<>` reveal, and copy the exact public call that produced it. When a design
decision has to favour one audience, they win (Ash, 2026-10-14).

Other confirmed audiences (docs/architecture/explorer-data-inventory.md §2):
traders and holders, DeFi users, protocol teams and issuers, researchers and
journalists, operators and auditors.

Situation: anonymous, no sign-in to browse. Desktop-first; mobile gets search,
asset pages and the pulse. Only the customer dashboard (`/dashboard/*`) needs an
account. Test-net deployments (testnet, futurenet) are SDEX-only with no pricing
and carry network-specific copy.

## Product Purpose

Stellar Index is a protocol explorer and public API for the Stellar network:
complete, verified, per-protocol on-chain data (every contract, event and trade
for SDEX, Soroswap, Aquarius, Phoenix, Comet, Blend, DeFindex, CCTP, Rozo,
SoroCredit and the Reflector / Redstone / Band oracles) captured from a certified
raw ledger lake, plus asset supply and holders, price history, anomaly and MEV
analytics, and one independent VWAP price per asset.

The explorer at stellarindex.io is the product (Ash, 2026-10-14). The API is the
plumbing made visible through it: every panel is one copy-pasteable public call.
Success at v1: stellarindex.io is the destination for Stellar on-chain data, and
a builder can go from any number on screen to the request that produced it.

Four lenses organise the site: Prices (what anything is worth), Protocols
(per-protocol deep dives), Network (ledgers, transactions, accounts, contracts,
events), Coverage (why to believe it).

## Positioning

What a neighbouring explorer cannot truthfully copy:

- Protocol-aware attribution by contract identity (ADR-0035), with `/contracts/[id]`
  as the hinge between protocol, asset and network views.
- Provable completeness: ADR-0033 verdicts rendered as product, backed by
  `completeness_snapshots` and `/v1/coverage`.
- Independent cross-venue pricing: VWAP with visible confidence, oracle
  cross-checks, exact decimal money end to end.
- One panel, one API call, revealed on every card.
- Independent of Horizon and stellar-rpc (AGENTS.md invariants 2 and 6); open
  (Apache-2.0, self-hostable); free with no paid plans.

## Operating Context

- Live: stellarindex.io (explorer and `/status`), api.stellarindex.io,
  docs.stellarindex.io, status.stellarindex.io (standalone incident page in
  `web/status/`, builds from `internal/incidents/data` without the API so it
  survives an outage).
- Web stack: `web/explorer/`, Next 16 app router with `output: 'export'`,
  React 19, Tailwind 4 with tokens in the `@theme` block of `src/app/globals.css`
  (no `tailwind.config`), a hand-rolled component library in `src/components/ui`
  (not shadcn/ui), TanStack Query, TradingView lightweight-charts, lucide-react,
  OG cards rendered at the edge by `functions/og/`. Deployed to Cloudflare Pages.
- Dev: `cd web/explorer && pnpm dev` (http://localhost:3000). Gate:
  `pnpm test && pnpm typecheck && pnpm lint`. A full `pnpm build` fetches from
  the live API and fails hard.
- 90 routes (2026-10-14): entity pages (assets, markets, accounts, contracts,
  transactions, ledgers, issuers, protocols, sources, dexes, exchanges,
  liquidity pools, lending, oracles), analytics (anomalies, divergences, MEV,
  diagnostics, insights), research, blog, changelog, embed widgets, customer
  dashboard, company and legal pages.
- The omnibox routes by input shape: G-strkey to account, C-address to
  contract, 64-hex to transaction, integer to ledger, asset code to assets,
  `XLM/USD` to pair, protocol name to protocol.
- Terminology: an asset is `(code, issuer)`, a SAC address, or `native`; XLM is
  "Stellar Lumens" or "XLM" in the UI, never "native". Ledger sequence,
  transaction hash, G-strkey and C-contract id are first-class identifiers.
  "Complete", "lake_complete", "verified", "as_of", freshness and confidence
  are defined product terms (ADR-0033, ADR-0015), not adjectives.
- Status: pre-v1, live in development, no consumer traffic yet. Price history
  is CEX-sourced from 2018 and on-chain SDEX from March 2026, not since
  inception.

## Capabilities and Constraints

- Panels are API queries 1:1. No client-side joins; a UI need the API cannot
  serve becomes a gap-list entry, not a client workaround.
- URL state is config: every selection lives in the query string; tables are
  sortable by default with `sort=<col>:asc|desc` in the URL.
- Never render an in-progress bucket (ADR-0015). Live only where it is real
  (SSE price tip, trade tape, ledger tip); everything else is a closed bucket
  with an explicit timestamp. Confidence is always visible.
- Money is exact. Amounts arrive as decimal strings; the UI renders a readable
  precision via `formatReadable` / `xlmReadable` with the exact value in
  `title=`. Never format ids, ledgers, hashes or years. A partial total is a
  labelled lower bound, never a total.
- Every page shares one shell: breadcrumbs, title and subtitle via
  `PageHeader`, one `Container` width. `/embed/*` is exempt; `/signin` is
  narrow and `/signup` wide on purpose (Ash, 2026-10-09).
- Prose is minimal. Page descriptions were removed; prefer badges and tooltips
  over paragraphs; a sentence is often too much (Ash, 2026-10-10).
- Other explorers appear only in one "External references" footer line per
  entity page (stellar.expert, stellarchain.io, steexp), never inline
  (Ash, 2026-10-10). Protocol pages carry an Info panel with website, docs,
  GitHub, audits and contract source.
- Vendor price data (CoinGecko, CoinMarketCap) is shown only where Stellar
  Index adds value: aggregation or cross-check, not a passthrough
  (Ash, 2026-10-09).
- Pricing: free, no paid plans, no card. Anonymous and keyed tiers see the same
  data; only rate limits differ (published on `/pricing`).
- Static export with a per-route JS budget (`scripts/ci/explorer-file-budget.sh`),
  SEO lint, an OG card on every page, no third-party trackers. The stated
  performance targets (LCP < 1.5 s on 3G p95, API p95 < 200 ms) are goals, not
  verified claims.
- Formatting is pinned to en-US and the UI is English only (`lang="en"`); there
  is no i18n pipeline. Localisation is undecided.
- A `/search` page, `/coverage` pages and a point-in-time "time machine"
  (`as_of_ledger`) are planned but not built; `as_of` is not a pin today.

## Brand Commitments

- Name: "Stellar Index", two words. Site tagline: "Stellar Index — Stellar
  protocol explorer & API". Landing h1: "The protocol explorer for the Stellar
  network." Eyebrow: "Independent · open · public-tier free".
- Binding (Ash, 2026-10-14): dark only. One mode, no light toggle.
- Direction (Ash, 2026-10-14): the category standard, played straight. The explorer
  sits alongside Dune and Linear / Vercel and is held to their craft level; no
  world metaphor, no smuggled quirk. Convention is the commitment.
- Open to replacement in a redesign (Ash, 2026-10-14): the tri-face type
  (Fraunces display, Inter body, JetBrains Mono data), Stellar Index blue as the
  identity hue, and dune.com's product UI as the reference. These describe the
  incumbent system (`docs/architecture/design-system.md`,
  `docs/frontend/dark-redesign-direction.md`) and are evidence, not commitments.
- Voice: dry, concise, instrument-like. Numbers speak; no superlatives, no
  marketing adjectives. Flagship numbers are rendered, not described.
- Legacy names (Rates Engine, CTX Rates) survive only in immutable archives
  (ADRs, changelog, dated posts) and never on a surface.
- No contributor, vendor or AI attribution anywhere on a surface or in a commit.

## Evidence on Hand

- Real data: the production API and 90 shipped pages; per-protocol verification
  pages in `docs/protocols/`; methodology in `docs/methodology/` (VWAP, TWAP and
  OHLC, stablecoins, RWA definition and reconciliation, DEX TVL, local-currency
  pricing); decisions in `docs/adr/`; the incident corpus in
  `internal/incidents/data`; coverage verdicts from `/v1/coverage`; sixteen
  runnable curl examples and a Postman collection in `examples/`; the Go SDK in
  `pkg/client/`.
- Assets: `web/explorer/public/og.svg` and `og.png` (static social card in an
  older slate and sky palette, predating the dark system; live cards render from
  `functions/og/`), `icon-192.png`, `icon-512.png`. No standalone wordmark or
  logo file exists; the name is set in type.
- Accessibility proof: `src/app/color-contrast.a11y.test.ts`,
  `keyboard-operable.a11y.test.ts`, `heading-order.a11y.test.tsx` and
  per-feed a11y tests.
- Absent, never fabricate: customers, testimonials, user logos, case studies,
  press, usage or traffic numbers, uptime claims beyond the status page, and
  since-inception price history.

## Product Principles

1. Every number is one public API call away, and the `<>` reveal is the proof.
2. Honest over fast: never make a number faster by making it less true; label
   lower bounds; never render an in-progress bucket.
3. The answer above the fold, deep data one click below.
4. Verifiability is product: completeness, freshness and confidence sit on the
   surface, not in a footnote.
5. The explorer is the destination: cross-references are a quiet footer, never
   a handoff.

## Accessibility & Inclusion

WCAG 2 AA is enforced in tests, not aspired to: text contrast (1.4.3) and UI
component boundaries (1.4.11) in `color-contrast.a11y.test.ts`, keyboard
operability, heading order (also a post-build gate), and a skip link in the root
layout. Any token or component change must keep those tests green. Tabular
figures for aligned numbers; direction and status are never colour alone.
