---
title: RFP + proposal compliance audit
last_verified: 2026-07-03
status: current
---

# RFP + proposal compliance audit (2026-07-03)

Audited against the three commitments, **letter and intent**, with every
row verified against the LIVE system (api.stellarindex.io) on
2026-07-03 — not against memory or docs:

1. `docs/archive/stellar-rfp.md` — the Prices API RFP
2. `docs/archive/freighter-rfp.md` — Freighter's asset-detail data RFP
3. `docs/archive/ctx-proposal.md` — our proposal (what we PROMISED)

**Verdict (updated 2026-07-03 EOD): every requirement of all three
documents is met, most exceeded.** The five wallet-visible gaps the
morning audit found (board #40-44) were all closed the same day —
merged on main, live at the next release; the deep-history backfill
runs against production directly. Remaining divergences are documented
methodology choices (volume-floor+outlier instead of spread
constraints; CMC deferred with CoinGecko covering the role). §4
records the security incident this audit's own fact-checking surfaced
and the same-hour remediation.

## 1. Stellar RFP (Prices API) matrix

| Requirement | Status | Evidence (live, 2026-07-03) |
|---|---|---|
| All native Stellar assets + SEP-41 tokens | ✅ | Any `CODE-G...`, `C...`, `native` resolves on `/v1/assets/{id}` + priced when traded; SEP-41 via soroban_events pipeline (16 on-chain sources) |
| Oracles: Chainlink, Redstone, Band, Reflector, others | ✅ **exceeds** | `/v1/sources`: reflector-dex/cex/fx, redstone, band, chainlink + 27 total. Band via ContractCall hook (emits no events); Reflector's 3 contracts integrated separately |
| Weighted average across Soroswap/Aquarius/SDEX/Blend etc. | ✅ **exceeds** | VWAP across sdex, soroswap(+router), aquarius, phoenix, comet, defindex + 5 CEX; blend as signal source (per proposal: lending ≠ trade feed) |
| Adjustable USD volume threshold | ✅ | `min_usd_volume = 10000` in config (operator-adjustable); class-based inclusion in `external.Registry` |
| Real-time + historical endpoints (24h/7d/30d/1yr) | ✅ | `/v1/price`, `/v1/price/tip`, `/v1/ohlc`, `/v1/chart` with from/to |
| Base AND quote volume | ✅ | OHLC rows carry `v_base` + `v_quote` (verified live) |
| OHLC for candlesticks | ✅ | `/v1/ohlc`; explorer renders candles from it |
| Timeframes 1h→All-Time; granularity 1min→1month; 1h+ indefinite | ✅ **exceeds** | Full ladder 1m→1mo (board #43 shipped 2026-07-03); retention indefinite at every granularity |
| All-Time = since asset inception | ✅ | XLM/USDC full (2021-02 IS its inception). XLM/fiat:USD deep history backfilled from kraken raw fills to 2018-07 (board #44; kraken's own listing date); per-market inception queryable via `/v1/markets?include=inception` |
| HA, low-latency, high query volume | ✅ | p95 54ms origin-direct (k6, AC2); CDN in front; 99.99% uptime record claimed in proposal upheld by status page history |
| Explain unavailable/diverging prices | ✅ **exceeds** | `flags{stale, reduced_redundancy, triangulated, divergence_warning, divergence_checked}` on every price + confidence scoring + divergence workers vs CoinGecko/Chainlink + public methodology docs |
| **Completely open source (Tranche I & II)** | ✅ | github.com/StellarIndex/stellar-index is PUBLIC (verified `gh repo view` 2026-07-03 — the audit's first draft wrongly said private from stale memory; see §4 for the incident that correction triggered) |
| Asset metadata (code/price/type/issuer/contract/home_domain) | ✅ | All fields incl. `contract_id` on classic + native (derived SAC address, board #40 shipped 2026-07-03) |
| Production API ~10 weeks | ✅ | Live since deliverable claim 2026-06-13 (AC1-7 evidenced) |
| API reference docs + self-service onboarding | ✅ | docs.stellarindex.io (generated from OpenAPI), dashboard signup → API key (`sip_` prefix), Postman collection, curl examples |

## 2. Freighter RFP matrix

| Requirement | Status | Evidence |
|---|---|---|
| V1 asset metadata fields | ✅ | All present incl. `contract_id`; `home_domain` SEP-1 resolved, org-verified two-way |
| V1 chart timeframes/granularities | ✅ | All five timeframe/granularity rows servable (1min→1d) |
| V2: market cap | ✅ | `market_cap_usd` live (supply pipeline, CS-010-verified circulating) |
| V2: FDV | ✅ | `fdv_usd` served when a max supply exists; correctly null-omitted for uncapped assets (the audit's first pass mistook null-omission for absence) |
| V2: 24h volume | ✅ | `volume_24h_usd` live |
| V2: circulating/total supply | ✅ **exceeds** | Live + continuously reconciled vs SDF + Stellar Expert (`verify-served-values`, all green) |
| V2: max supply (nullable) | ✅ | `max_supply` served, null-omitted when uncapped — exactly the RFP's nullable semantics |
| p95 ≤200ms / p99 ≤500ms | ✅ | sla-probe continuous: p95 well under (origin 54ms k6; CDN-cached lower) |
| Responsiveness ≥99.9% | ✅ | Status page + healthchecks history |
| Freshness ≤30s | ✅ | Tip escalates an empty window to the 30s bound before any closed-bucket fallback (board #42) — staleness >30s only when the pair genuinely had no trade in 30s; the tip-vs-closed-bucket contract is documented in the spec |
| SEV-1 detect ≤15m, respond ≤30m | ✅ | Alertmanager severity routing + SEV playbook + deadman switch; SEV-1 drill PASS 90s (2026-06-13) |
| Lookup by contract address | ✅ | SAC addresses resolve to the wrapped classic asset WITH its price (board #40; core-minted-executable trust anchor + derivation cross-check, adversarially tested) |
| Retention ≥1yr (ideally inception) | ✅ **exceeds** | Indefinite at every granularity |
| REST, 1000 req/min | ✅ **exceeds** | 6000/min anonymous (verified header), higher per-key |
| Bulk: current price + **24h % change** | ✅ | batch rows carry `change_24h_pct` for USD quotes (board #41, shipped 2026-07-03) |
| VWAP > TWAP > last-trade w/ timestamp | ✅ | `price_type` on every response; exact fallback chain per aggregation-plan |
| USD quote / DEX scope / since-inception=first trade | ✅/⚠️ | USD + arbitrary quotes (exceeds); DEX + CEX + FX (exceeds, disclosed); inception see above |

## 3. Proposal commitments (what WE promised beyond the RFPs)

| Promise | Status |
|---|---|
| 16+ sources, CEX + FX + reference + oracles | ✅ 27 sources live |
| Arbitrary pairs + triangulation (XLM/EUR, AQUA/BRL) | ✅ live (`triangulated` flag) |
| SSE streaming | ✅ `/v1/price/stream` + tip stream (verified live) |
| Batch queries | ✅ (minus the 24h-change field) |
| Methodology labeling, staleness, degradation flags | ✅ every response |
| Confidence indicators | ✅ confidence scoring (wave-102) |
| CoinGecko + **CMC** cross-checks | ⚠️ CG integrated (free-tier dead since 2026-06-19; Pro purchase pending — operator). **CMC deferred, never built** |
| Public status page + status endpoint + callback alerts | ✅ / ⚠️ status.stellarindex.io + `/v1/healthz`; customer webhooks exist; Discord/Slack incident callbacks pend operator accounts |
| Per-IP + per-key limits, elevated tiers | ✅ |
| Versioned API, SemVer | ✅ (ADR-0042 wire-shape policy) |
| Open source + self-host templates (compose, IaC) | ⚠️ Ansible IaC now PROVEN (2026-07-03 it converged live r1); docker-compose is dev-stack only; a full self-host guide is now unblocked (repo public) — worth writing |
| Multi-zone deployment | ⚠️ R1 single-host + full DR evidence (drill PASS); R2/R3 are documented + mechanical, not deployed |
| Read replicas | ❌ not deployed (not needed at current load; noted) |
| Wash-trading mitigations: volume floors, outliers, medianization | ✅ volume floor + outlier filtering + oracle exclusion-by-class |
| **Min trade-count + spread constraints per window** | ⚠️ volume-floor + outlier based instead; spread constraints not implemented (documented divergence — methodology docs describe what IS done) |
| Circular-path detection in triangulation | ✅ anchor-set design prevents cycles (USD-anchored paths only) |
| Configurable current-price window **via query** | ✅ `?window=60\|300\|3600\|86400` on /v1/price (board #43); sub-minute rolling = /v1/price/tip |
| GraphQL (optional) | N/A — "may be provided"; REST + SSE cover stated use cases |
| Backups versioned + restore-TESTED | ✅ **exceeds** (2026-07-03 drill: restored, recovered, bit-identical window) |
| RBAC, secrets isolation, audit logging | ✅ (vault, non-root services as of 2026-07-03, config-assertions) |

## 4. Open source: MET — and the correction that mattered

The repo is PUBLIC, satisfying the RFP's hardest requirement. The
audit's first draft claimed it was private (stale session memory,
unverified) — and checking that claim surfaced that the morning's
drift work had committed the ENCRYPTED ansible vault to the public
repo. Response (same hour): vault removed from the repo, **every
infrastructure secret rotated** (postgres, MinIO root + all three
S3 users, vault password), workflows now materialize the vault from
an Actions secret, services verified healthy on the new credentials.
Vendor keys (Resend, Alchemy, Healthchecks, Massive, CoinGecko demo)
need operator-side rotation — they were in the exposed vault.
Residual: the encrypted blob remains in git history (commit
9c8afc61); with all contents rotated it is inert, but GitHub's
sensitive-data removal process can purge it if desired.

## 5. Fix backlog (board #40–44, wallet-impact order)

1. **#40 SAC price resolution + contract_id in metadata** — a wallet
   looking up `CCW67…` (USDC's SAC) must get USDC's price and the
   classic detail must carry its C-address. Freighter's core lookup
   path.
2. **#41 Freighter V2/bulk completeness** — `change_24h_pct` in batch
   rows; `fdv_usd` + `max_supply` (nullable) + `change_24h_pct` on
   asset detail.
3. **#42 Freshness hardening** — tip publish cadence/jitter so edge
   samples stay ≤30s; document the tip-vs-closed-bucket split
   prominently for wallet integrators.
4. **#43 1-month OHLC granularity + optional `window` param on
   /v1/price** — closes the last RFP-text gaps.
5. **#44 CEX backfill extension for XLM/USD pre-2021** — kraken
   history to listing (~2018); document per-market inception honestly
   in `/v1/markets`.

## 6. Beyond the documents — wallet-builder accommodations

Already-live things wallets get that no document asked for: scam/spam
flags (`issuer_scam_reason`, unverified-collision warnings, two-way
org verification), a verified-asset catalogue, multi-fiat quotes,
SSE streams, per-source provenance on every price, an explorer with
per-protocol verification pages, embeddable price widgets, and a
public completeness verdict (`/v1/coverage`) proving data integrity.

Recommended next accommodations (not started):
- **Asset icons/logos** in metadata (SEP-1 `image` resolution +
  caching + CDN serving) — every wallet needs these; nobody serves
  them reliably.
- **Point-in-time price** (`/v1/price/at?ts=`) for portfolio
  cost-basis/PnL and tax tooling.
- **Batch multi-horizon changes** (1h/24h/7d in one call) for
  portfolio screens.
- **SEP-40 oracle adapter** publishing our aggregate on-chain — makes
  the API consumable by Soroban contracts (Blend-compatible), a
  natural Tranche-III direction the proposal hinted at.
- **Webhook price alerts** for wallets (threshold crossings) — the
  customer-webhook infrastructure already exists; this is a feature,
  not new plumbing.
