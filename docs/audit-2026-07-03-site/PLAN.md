---
title: Site quality audit — plan
last_verified: 2026-07-03
status: current
---

# Site quality audit: every page, every flow, the whole site

**Trigger (2026-07-03):** spot checks found a class of problems the
page-by-page build never caught: navigation labels that don't match
destinations, listings that show a curated sliver as if it were the
universe, list→detail flows that 404 on their own entries, entity
pages that are bare lists where an explorer should offer insight, and
special entities (SACs) handled as anonymous contracts. The site works
mechanically but doesn't yet think like its visitor.

**Goal:** find every issue of this class — not just the reported ones —
and leave behind (a) a severity-ranked register, (b) the fixes, and
(c) recurring guards so the class stays closed.

## Severity taxonomy

- **P0 — broken flow**: a reasonable click path dead-ends, 404s, or lies
  (the issuer list linking to an issuer detail that says "not found").
- **P1 — wrong or missing data**: the page renders but materially
  misrepresents the chain (11 assets presented as "Assets"; a contract
  with deployed WASM showing "no code").
- **P2 — missing insight**: correct data, but a bare list where the
  visitor's actual question needs aggregation, charts, tagging, or
  explanation (transactions/operations/ledgers pages today).
- **P3 — polish**: naming, formatting, labels, empty-state copy,
  responsiveness, SEO/OG correctness.

## Method — three passes per surface, then site-wide sweeps

### Pass A — mechanical crawl (finds P0s and hard P1s)

1. **Route inventory** from `web/explorer/src/app/` (static + dynamic
   patterns) + the sitemap + the status/docs/dashboard surfaces.
2. **Entity sampling per dynamic route** — never test only the happy
   entity. Per route, sample across the distribution:
   - assets: verified major (USDC) / unverified traded / SAC C-address /
     Soroban-only token / fiat / garbage id
   - contracts: SAC / WASM protocol contract (each protocol) / freshly
     deployed / dead-since-2023 / the user-reported
     `CDSOP5Y4…` / garbage
   - issuers: every row the LIST itself links to (list↔detail closure —
     the Circle bug is exactly a closure failure), plus unknown G
   - tx/ops/ledgers: recent / genesis-era / failed tx / fee-bump /
     Soroban-heavy / classic-only
3. **Automated checks per sampled URL**: HTTP status, console errors,
   render of an empty/skeleton state that never resolves, dead links
   (crawl every href), placeholder text ("undefined", "NaN", "null",
   raw ids where names exist), trailing-slash duplicates (suspected
   root cause of the issuer 404 — verify `/issuers/G.../` vs
   `/issuers/G...`).

### Pass B — per-page logic review (finds P1/P2s)

One reviewer per page TYPE with a fixed checklist; every answer gets
evidence (screenshot/URL/API response):

1. WHAT QUESTION does a visitor arrive with, and does the page answer
   it above the fold?
2. Is the nav label honest about the destination? (the "DEX/AMM" →
   `/protocols` mismatch)
3. Data completeness vs the chain's reality — is a filtered subset
   presented as the universe? (the 11-asset page)
4. Every entity mention: linked? resolving? named (not a raw strkey
   when a name exists)?
5. Empty/loading/error states: designed, honest, actionable?
6. Insight depth: would a block-explorer power user call this page
   USEFUL or a table dump? What chart/aggregate/tag/explainer is the
   obvious missing one?
7. Special-entity handling: SACs (name, "wrapped X" tag, explainer,
   link to the classic asset), protocol contracts (protocol badge →
   protocol page), system accounts.
8. Formatting: amounts at asset decimals, times humanized + absolute,
   addresses truncated with copy, USD equivalents where known.
9. SEO/OG: title/description/canonical per entity, no accidental
   noindex on valuable pages (and noindex kept on long-tail shells).
10. Mobile at 375px: layout, tap targets, horizontal scroll.

### Pass C — flow walks (finds the P0s that live BETWEEN pages)

End-to-end journeys executed in a real browser, counting dead-ends,
back-link gaps, and label lies:

- "I hold token X — what is it, is it legit, what's it worth?"
  (search → asset → issuer → markets → holders → back)
- "What happened in this transaction?" (paste hash → tx → ops →
  accounts touched → the assets moved)
- "Is this contract safe?" (contract → code + history → activity →
  protocol verification page)
- "How healthy is the network right now?" (home → ledgers →
  throughput → fees)
- "I'm a wallet dev evaluating the API" (site → docs → signup → key →
  first successful curl — the LC-061 path, re-walked)
- "Something looks wrong — can I verify the data?" (any number →
  /coverage → methodology docs)

### Site-wide sweeps (cross-cutting)

- **IA/navigation**: every nav item's label vs destination vs sibling
  overlap; is the top-level structure the one a Stellar user expects?
- **Census audits** (API-side, feed the P1s): full asset census vs the
  assets page; contract WASM coverage % (how many contracts with code
  in the lake resolve on /contracts/{id} — the user's example shows
  the Phase-C backfill may not be wired to the reader, or the reader
  queries a table the backfill didn't fill — VERIFY with the exact
  contract id); issuers list↔detail closure over ALL rows.
- **Consistency grid**: date/amount/address/name rendering rules,
  applied everywhere (one table, every page audited against it).
- **Performance**: page weight, request waterfalls, CDN cacheability
  per route.
- **Copy/brand**: tone, empty states, error pages, 404 page.

## Execution structure

Wave 0 (this doc): register seeded with the reported issues + incident
fixes. Waves are independent and mergeable per the repo's
one-PR-one-merge cadence:

1. **Wave A — crawl + censuses** (mechanical, fast, quantifies the
   problem): produces the route inventory, sample matrix, dead-link
   list, census gaps.
2. **Wave B — P0 fixes**: issuer list↔detail closure, trailing-slash
   handling, nav label/destination mismatches, contract code reader
   wiring.
3. **Wave C — P1 data**: full-census assets page (server-side search +
   pagination + verified/scam badges as filters), SAC naming/tagging
   everywhere (the SACClassicAssetName reader shipped 2026-07-03
   enables this), contract WASM coverage.
4. **Wave D — P2 insight**: tx/ops/ledgers pages get the analytics
   they deserve (op-type mix, fee stats, throughput, flow-of-funds on
   tx detail; the API already serves OperationTypeStats +
   NetworkThroughput — the UI never consumed them), contract pages get
   activity charts + protocol badges.
5. **Wave E — polish + guards**: consistency grid fixes; then the
   recurring guards — a scheduled crawl (dead links + console errors +
   placeholder text as CI/cron), census drift checks (assets page
   count vs API census), and a "new page checklist" in
   CONTRIBUTING/CLAUDE.md so new pages ship against the Pass-B list.

## Register

`docs/audit-2026-07-03-site/REGISTER.md` — one row per finding:
severity / URL / what a visitor experiences / root cause layer
(UI, API, data, IA) / fix wave / status. Seeded with the six reported
issues; every wave appends.
