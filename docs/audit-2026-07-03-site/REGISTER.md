---
title: Site quality audit — findings register
last_verified: 2026-07-03
status: current
---

# Findings register

Severity / status legend in PLAN.md. Root-cause layer: UI (explorer
code), API (endpoint gap), DATA (index/backfill gap), IA (information
architecture / labels).

| # | Sev | Where | What the visitor experiences | Root cause (hypothesis → verified) | Wave | Status |
|---|-----|-------|------------------------------|-------------------------------------|------|--------|
| S-001 | P0 | nav "DEX/AMM" | Clicking "DEX/AMM" lands on /protocols — label promises a venue view, destination is protocol verification pages | IA: label/destination mismatch | B | open |
| S-002 | P1 | /assets | Shows 11 assets under the heading "Assets" — the verified catalogue presented as the asset universe | UI+API: page consumes the catalogue listing; the full census (every traded/trustlined asset) exists in the store but has no browse surface | C | open |
| S-003 | P0 | /issuers → /issuers/GA5Z…/ | The site's own issuer list links to a detail that says "Issuer not found" (Circle!) | ROOT-CAUSED (route agent): the page is PRE-RENDERED and the not-found panel was BAKED at build when the build-time issuer fetch hit the old 2s timeout (timeout fix already landed 2026-07-02). Fix = rebuild/redeploy the explorer; PLUS issuers outside the top-100 have NO CF function shell → hard 404 (add `functions/issuers/`) | B | open |
| S-004 | P2 | /transactions | A bare paginated list — no op-type mix, no fee stats, no volume context, no "what's moving" | UI: the API already serves OperationTypeStats + NetworkThroughput; the page never consumes them | D | open |
| S-005 | P2 | /operations, /ledgers | Same bare-list shape as S-004 | UI (same) | D | open |
| S-006 | P1 | /contracts/{id} | Top contracts lack cached/rendered code; the reported example (CDSOP5Y4…RG4R) shows no code AND no code history despite Phase-C claims | DATA/API: verify whether ledger_entry_changes holds the instance+code for this contract and whether ContractWasm/ContractCodeHistory query the tables Phase C actually filled | B | open |
| S-007 | P2 | /contracts/{SAC} | SACs render as anonymous contracts — no name, no "wrapped USDC" tag, no explainer, no link to the classic asset | API+UI: SACClassicAssetName reader shipped 2026-07-03 — wire it into contract detail + directory rows | C | open |
| S-009 | P0 | every entity detail page | Trailing-slash URLs (the static export's canonical form!) break data loading: the API 308-redirects `/v1/issuers/G.../` → clean path, but the redirect is emitted by Go's mux BEFORE the CORS middleware, so the 308 has no Access-Control-Allow-Origin → the browser kills the fetch → UI misreports "not found". Verified: API 200 clean, 308-sans-CORS with slash | API (CORS-on-redirect) — real defect, but demoted from S-003's cause: runtime param extraction is slash-safe (useLastPathSegment strips slashes); fix the API 308-CORS as defence-in-depth | B | open |
| S-010 | P0 | /issuers list | LOBSTR (major wallet, top-10 row) tagged SCAM; another row shows VERIFIED and SCAM simultaneously — trust-destroying false positives in the scam flagging | DATA/API: scam-list matching too broad + badges not mutually exclusive | B | open |
| S-011 | P1 | /assets table | EVERY analytics column (1H/24H/7D %, market cap, volume, circulating, 7D chart) renders "—" for all rows — the API serves these fields; the page consumes an endpoint/shape that doesn't carry them | UI (wrong endpoint/shape since the Unit-D wire collapse?) | C | open |
| S-012 | P1 | homepage 7D chart | Anomalous left-edge spike to ~$1.00 on the XLM 7D chart (price is ~$0.20) — a bad bucket renders as a wild wick | DATA (one bad 1h bucket?) or UI scale — investigate | C | open |
| S-013 | P1 | /lending pool table | "UTIL % 222.4%" and "NET SUPPLIED 578.1T" — base-unit/window-proxy artifacts presented as if they were real utilisation/TVL | UI/API derivation (caption admits proxy; display must not show impossible values) | C | open |
| S-014 | P2 | /dexes pool table | Row 1 renders "XLM/USDC by Centre Consortium" but SAC-form rows render raw "USDC:GA5Z…" strings and bare "CBIJBD…M6VN" — same asset, three renderings | UI + the SAC-naming reader (shipped 2026-07-03) | C | open |
| S-015 | P1 | /protocols cards | Aquarius card shows "CONTRACTS 0" (its pool roster isn't registered — ADR-0040 aquarius track); SDEX "CONTRACTS 0" reads as broken data instead of "classic protocol — n/a" | DATA (aquarius) + UI copy (SDEX) | C | open |
| S-016 | P2 | /contracts/{SAC} | SAC banner exists but identity unused: title "Contract" not "USDC — Stellar Asset Contract", no link to the asset page; code-history panel contradicts the banner with stale "fills in with Phase-C" copy; events table = 50 bare "contract transfer" rows with no amount/parties; interaction map stuck "Loading…" >6s | UI + API (event decode enrichment) | C/D | open |
| S-017 | P3 | nav PROTOCOLS section | "Lending" goes to the rich /lending page but "DEX / AMM" goes to /protocols (the /dexes page EXISTS and is good — wrong href); audit remaining nav items (Aggregators/Bridges/Oracles/Soroswap Router) for the same | IA/UI one-line fixes | B | open |
| S-018 | P3 | homepage stats | "SOURCES ONLINE 12 · Class = exchange" — jargon copy; site says 27 sources elsewhere | UI copy | E | open |
| S-019 | P0 | SearchModal "Account" | Search's static "Account" entry points at /account — a route that does not exist in the app; rescued only by a CF `_redirects` 301 whose rule covers the bare form only (`/account/` with slash falls through to 404) | UI (point at /dashboard) + _redirects hygiene | B | open |
| S-020 | P2 | /amm, /sdex, /liquidity-pools, /yield, /convert | An orphaned island: these pages link only to each other; NOTHING in sidebar/footer/search links in — ironic since the "DEX / AMM" nav label should plausibly point at /amm | IA (wire them into nav + cross-links, or fold their content) | C | open |
| S-021 | P3 | sidebar vs footer naming | Same routes, different names: /dexes = "AMM Pools" (sidebar) vs "DEXes" (footer); /exchanges = "External Markets" vs "Exchanges"; footer "Pricing" under About = API plans, reads as price data; footer "Account" column holds non-account items (Widgets/Methodology/Research) | IA naming pass | E | open |
| S-022 | P2 | /issuers/{beyond top-100} | Only the top-100 issuers are pre-rendered and there is NO CF function shell for issuers — any other issuer linked from search/asset pages hard-404s | UI/infra (add functions/issuers shell like accounts/contracts) | B | open |
| S-023 | P3 | /dexes/sdex framing | SDEX (an order book) rendered through the shared pools table, framed as "pools observed" | UI copy/framing | E | open |
| S-008 | P3 | site-wide | (placeholder for the consistency-grid findings — dates, amounts, addresses, names) | — | E | open |

## Same-day incident notes (2026-07-03, related operational state)

- `minio_exporter_down`: MinIO root rotation invalidated the
  Prometheus bearer JWT (`/etc/prometheus/minio.token`). Fixed —
  regenerated from the new credentials; scrape up. Follow-up: add
  token regeneration to the rotation runbook + a config-assertion.
- `completeness_incomplete`: stale sdex verdict (mismatch healed
  13:00, verdict computed 05:31). Manual recompute kicked.
- `ingest_gap_detector_silent`: restart churn during rotation/staged
  applies; cycles healthy again (3 clean runs/source).
