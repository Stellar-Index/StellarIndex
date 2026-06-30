---
title: Audit 2 — Logic / product-coherence audit — PLAN
status: planning (pass 1)
---

# Audit 2 — Logic / product-coherence audit — PLAN

Goal: find where Stellar Index is **clunky or conceptually incoherent** — places
the product still reads as the multi-chain "Rates Engine" it began as, redundant
or confusing information architecture, awkward API shapes, and broken mental
models — and propose a coherent target design. This is a *product-logic* audit,
not a code-bug audit (those are Audit 1).

Severity uses the P0/P1/P2 user-impact scale (see [README](README.md)); findings
are `LC-###`.

## The thesis

"Stellar Index" promises *the* protocol explorer + pricing API **for the Stellar
network**. Every surface should reinforce that. The audit hunts every place that
contradicts it — most visibly the **assets surface mixing Stellar and
non-Stellar (fiat, crypto-reference) assets** as if they were peers.

## Flagship — LC-001: fiat & non-Stellar coins modeled as first-class Stellar assets

**The incoherence:** "Stellar Index — the protocol explorer for the Stellar
network" lists **19 sovereign fiat currencies as browseable assets, ranked above
Stellar tokens by their M2 money supply**, in the flagship asset directory. This
is the single most visible "this isn't a Stellar explorer" signal.

**Evidence (verified by the coherence mapper):**
- `internal/currency/data/seed.yaml` holds 3 populations: 11 browseable Stellar
  assets, 15 `reference_only` non-Stellar coins (USDT/BTC/ETH/SOL/XRP/…), 19
  `class: fiat` (USD/EUR/CNY/… with `circulating_supply` = M2 broad money).
- `Browseable()` excludes only `reference_only`, **NOT fiat** (`verified.go:333-345`).
- `/v1/assets/verified` returns all 19 fiats with `market_cap_usd` = M2×FX
  (`assets_global.go:364-376,402-421`); explorer `VerifiedCurrenciesStrip` sorts
  desc → leads with **CNY ($302T), JPY, USD** before any Stellar token.
- `/v1/assets?asset_class=all` (what the explorer `/assets` table requests) emits
  catalogue rows "ordered by market_cap_usd desc (fiats top at $44T/$21T)" then
  classic assets (`assets.go:1001-1106`).
- Fiat is a first-class filter tab; fiat detail pages render under the Stellar
  namespace (`/assets/us-dollar` → `VerifiedCurrencyView`); `reference_only` coins
  are reachable at `/assets/btc` (a Bitcoin page on a Stellar explorer).
- **Already half-planned:** `public/_redirects:106-109` names `/external/forex` as
  a never-built "follow-up content migration"; `/external` today just 301s to
  `/sources`. The refactor plan §3a already says "stop projecting [fiat M2] as a
  comparable market cap" — the cross-chain drift was removed but the *fiat-as-
  asset* drift was left intact.

**Target design (the user's `/external/fiat-currencies` split):**
- *API:* drop `ClassFiat` from `Browseable()` and from the `asset_class=all`
  unified listing; `/v1/assets/verified` returns Stellar-issued verified assets
  only. Expose fiat via a dedicated surface — `/v1/external/fiat-currencies` (FX
  board) — and keep fiat as a price **quote-unit** (`fiat:USD`, XLM/EUR), which is
  the correct, protected role. Stop the mixed-unit `market_cap_usd desc` ranking
  that sorts M2 against crypto caps.
- *Explorer:* build `/external/fiat-currencies` as the home for the "World
  currencies" board + the 19 fiat detail pages; 301 `/assets/{fiat-slug}` →
  `/external/fiat-currencies/{slug}`; remove the "Fiat Currency" tab + fiat rows
  from `/assets` table and `VerifiedCurrenciesStrip`; add an `/external` nav
  section. Decide `reference_only` coins (`/assets/btc`): hide from the Stellar
  namespace or move under `/external/reference-prices` (they back pricing, not the
  explorer).
- *Sequencing:* the listing/UI changes are non-breaking and shippable now; the
  wire-shape parts that touch `pkg/client`/OpenAPI fall under the reserved Unit-D
  pre-v1 break window.

## Workstreams (from the coherence map — Pass 2)

### W1 — Asset taxonomy & the fiat/external split  ⭐ (LC-001 above, + LC-002…)
- **LC-002 — `reference_only` coins reachable as Stellar asset pages.** `/assets/btc`
  returns a `GlobalAssetView` (Catalogue.LookupBySlug populates `bySlug` for all
  entries incl. reference_only, `verified.go:211-215`). *Target:* route to
  `/external/reference-prices/{slug}` or 404 from the Stellar namespace.
- **LC-003 — verified-currency catalogue is doing double duty** (Stellar trust
  registry AND non-Stellar reference-price ticker map — 15 coins live in it).
  *Target:* extract the reference map into a pricing-only structure so the
  catalogue is a pure Stellar trust registry (refactor-plan §3a).

### W2 — Explorer IA overlap
- **LC-010 — Cluster B: dexes/exchanges/sources are 3 filtered views of one
  endpoint** (`/v1/sources`) with **dual detail pages per entity** (`/dexes/[s]`
  or `/exchanges/[n]` AND `/sources/[n]`), both just `/v1/markets?source=` slices.
  Biggest structural IA debt. *Target:* one canonical source registry + one detail
  route; themed views become filters, not separate URLs.
- **LC-011 — Cluster A: singular/plural inconsistency.** `/tx`,`/ledger`,`/contract`
  are alias→canonical redirects, but `/operation` is a real detail page (≠
  `/operations` index) — breaks the pattern. Dual detail impls (`*View` legacy +
  `*PathView` new) are dead-weight. *Target:* make `/operation`→`/operations/[…]`
  consistent; retire legacy `*View` once redirects bake in.
- **LC-012 — Cluster C: protocol category stubs** (`/amm`,`/yield`,`/bridges` =
  `/protocols?category=`; `/amm`,`/liquidity-pools`,`/sdex` thin SEO funnels).
  Same protocol reachable via 3-4 doors. *Target:* keep SEO landing pages but make
  the canonical data home explicit; de-duplicate nav labels.
- **LC-013 — Cluster D: `/divergences` vs `/anomalies` overlap** (divergence is a
  freeze reason on /anomalies; both track `divergence_warning`). *Target:* merge or
  clearly delineate.
- **LC-014 — Dead routes:** bare `/convert`, `/convert/[from]`, `/research/adr`
  have no index page (404). *Target:* add index pages or redirect.

### W3 — Nav / discoverability
- **LC-020 — Sidebar `/account/*` active-state bug** (links `/account/*` but pages
  are `/dashboard/*`; works only on CF via 301; `isActive()` never matches → no
  active state; 404 in `next dev`). *Target:* point Sidebar hrefs at `/dashboard*`.
- **LC-021 — four nav labels for the DEX concept** (AMM Pools / DEX-AMM / SDEX
  Markets / External Markets). **LC-022 — fiat board has no nav home** (the missing
  `/external`). **LC-023 — `/markets` is footer-only**; **Soroswap Router** is an
  oddly-specific top-level item; two competing doc destinations (`/docs` vs
  external). *Target:* coherent rail with an `/external` section.

### W4 — Naming / conceptual drift
- **LC-030 — coins vs currencies vs assets** three-way split: product says
  "assets," internals pervasively "coin" (`CoinsReader`, `CoinRow`, `useAssets→Coin`)
  and "currency" (`VerifiedCurrenciesStrip`, `CurrenciesReader`). *Target:* converge
  on "asset" in the codebase + UI copy.
- **LC-031 — residual cross-chain wording in the wire** (`AssetDetail.Class` =
  "cross-chain asset class"; `NetworkEntry` vestigial `Contract`/`ExternalLink`;
  **Unit D unshipped** so `pkg/client`/OpenAPI still carry multi-network shapes).
  **LC-032 — repo dir still `ratesengine`** (old brand).

### W5 — API-shape clunkiness
- **LC-040 — dual-shape `/v1/assets/{slug}`** (GlobalAssetView vs AssetDetail; the
  explorer shape-sniffs `typeof data.asset_id` and fetches both). *Target:* split
  routes or add an explicit `kind` discriminator. (Unit-D-gated.)
- **LC-041 — two-phase mixed-population cursor** bakes fiat-first ordering into the
  wire. **LC-042 — `/v1/assets/verified` fiat-dominated** (LC-001). **LC-043 —
  redirect migration debt** (~50 `/currencies/*` 301s near CF's 100-rule cap).

### Per-finding template

```
### LC-<N>: <one-line problem>
- **Surface / Evidence:** <routes / endpoints / file refs>
- **Why incoherent:** <how it breaks the Stellar-first mental model>
- **Target design:** <concrete fix — IA, API, copy>
- **Impact:** P0/P1/P2   **Effort:** S/M/L   **Wire-break? (Unit-D-gated):** y/n
```

## Execution protocol

1. Map every product surface (routes + endpoints) and classify each as coherent /
   redundant / stub / mis-scoped for a Stellar-first product.
2. For each incoherence, write the target design (not just "it's confusing").
3. Sequence findings into a remediation roadmap; the fiat/external split gets a
   concrete implementation sketch (API filter + new explorer section + redirects).

## Pass-3 self-review additions (workstreams the map under-weighted)

Re-walking as a first-time Stellar user surfaced surfaces the route-IA sweep
under-weighted:

- **W6 — Onboarding / dashboard / pricing-product coherence.** Stellar Index is
  BOTH an explorer AND a pricing API, but the explorer barely surfaces the pricing
  product (`/pricing`,`/methodology` are footer-only). The signup→verify→key-issue
  →first-call journey: is it coherent end-to-end? (Recent bugs: rek_/sip_ key
  prefix, "last used 2055", instant-revoked, magic-link — UX symptoms of the same
  rough edge.) *Investigate:* the auth/dashboard journey + how a developer goes
  from landing page to first authenticated request.
- **W7 — Accessibility (WCAG 2.1 AA).** Prior coverage = one unverified pass
  (06-14 Q3: **1 Critical** — Cmd-K modal no focus trap — **+ 7 Serious**,
  remediation untracked) and **zero frontend tests exist**. *Do:* a real keyboard/
  focus/contrast/aria sweep of the top journeys + confirm the Q3 Critical/Serious
  actually landed. (Routed here from PLAN-1.)
- **W8 — Empty / error / loading / first-run states.** Clunkiness lives here:
  null market-cap/supply on listings (page-audit found these), `/convert` inverted
  rates, slow `/protocols` (5-17s) + `/v1/tx` (~6s) with no skeleton, search hint
  copy ("every asset — crypto, fiat, stablecoins") reinforcing LC-001. *Do:* walk
  the empty/slow/error path of each top route.
- **W9 — Cross-surface terminology in user-facing copy** (not just code): hero
  ("alongside live world fiat rates"), `HomeCurrencies` "World currencies", embed
  `/embed/currency/[ticker]` — every place the copy says "currency/coin/rates"
  instead of the Stellar-first "asset/price" vocabulary. Folds into LC-030.

## Pass log

- **Pass 1:** thesis, flagship workstream, finding template, execution protocol.
- **Pass 2:** folded the coherence map → flagship LC-001 fiat-split design +
  workstreams W1-W5 with concrete target designs + LC-### IDs.
- **Pass 3 (plan FROZEN):** adversarial self-review → added W6 (onboarding/
  pricing-product coherence), W7 (a11y/WCAG + missing FE tests), W8 (empty/error/
  loading states), W9 (user-facing copy). Re-walked as a first-time user.
- **Pass 4 (execution):** verify each LC finding against current code + the LIVE
  site; write the fiat-split implementation sketch in detail; sequence a
  remediation roadmap (non-breaking now vs Unit-D-gated). Status: **EXECUTING.**

## Execution protocol (frozen)

1. **Verify** every LC-### against current code AND the live explorer
   (stellarindex.io) — a coherence claim must reproduce on the running site.
2. **Target design** per finding (not just "it's confusing").
3. **Roadmap:** split into (a) ship-now non-breaking (listing filters, nav,
   redirects, dead routes, copy) vs (b) Unit-D-gated wire breaks (dual-shape,
   NetworkView collapse). The flagship fiat-split gets a step-by-step impl sketch.
4. Findings → `02-logic-coherence-findings.md`.
