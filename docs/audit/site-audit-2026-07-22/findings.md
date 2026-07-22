---
title: Live site audit — stellarindex.io
last_verified: 2026-07-22
status: IN PROGRESS — headless sweeps + API timing done; browser/UX pass ongoing
---

# Live site audit — 2026-07-22

Empirical audit of the **live** public surface (not a code review): every
static route probed, every link extracted and checked, every API endpoint
the web apps reference timed, and the dynamic-route generation model
reverse-engineered against real data.

Method note: everything below was reproduced against production and the
reproduction command is included. Where a hypothesis did **not** survive
checking it is recorded in "Checked, not a finding" rather than dropped —
that section is as important as the findings.

---

## S1 — CRITICAL: one root cause behind both "dead links" and "non-Stellar data on Stellar pages"

These were reported as two separate complaints. They are the same bug.

`/markets/[pair]` is statically exported (`next.config`: `output: 'export'`)
and pre-renders **the top 500 pairs by 24h USD volume from `/v1/markets`**.
Anything outside that set is a hard 404 — a static export has no fallback
render.

But `/v1/markets` is dominated by **off-chain CEX pairs**:

```
top 100 markets: 45 involve off-chain assets (45%)
first 12 rows, all non-Stellar:
  crypto:BTC/crypto:USDT   crypto:BTC/fiat:USD    crypto:ETH/crypto:USDT
  crypto:ETH/fiat:USD      crypto:SOL/crypto:USDT crypto:XRP/fiat:USD
  crypto:XRP/crypto:USDT   crypto:SOL/fiat:USD    crypto:BNB/crypto:USDT
  crypto:DOGE/crypto:USDT  crypto:TRX/crypto:USDT crypto:NEAR/crypto:USDT
```

So the 500-page pre-render budget is spent on Binance/Coinbase pairs
(BTC/USDT alone is $1.3B/24h) while **real Stellar markets fall outside it
and 404**. The two symptoms are the same defect seen from either end:

1. a Stellar explorer whose flagship markets listing is ~half non-Stellar
2. Stellar market links that dead-end

**Reproduce:**
```sh
curl -s "https://api.stellarindex.io/v1/markets?limit=100"   # 45% off-chain
curl -sL -o /dev/null -w '%{http_code}\n' \
  "https://stellarindex.io/markets/GoogleLiquid-GCYYXO7FEIY6ZMGOIDLUGXPLTBESCY5ZPYJSMGUPFFA5CTOAWEW7IRKH~native/"   # 404
```

This already recurred once: `generateStaticParams`' own comment records the
2026-05-08 audit where AQUA pairs 404'd at top-100 and the limit was raised
to 500. **Raising the number is a treadmill, not a fix** — the population
being ranked is wrong, not the cutoff.

### S1a — the `/network` "Top Stellar markets" widget is structurally guaranteed to emit dead links

`NetworkView.tsx:369` builds `/markets/{base}~{quote}` from
`/v1/pools?limit=8&order_by=volume_24h_usd_desc` — **on-chain pools only**.
It ranks a different population than the pre-render list, so its rows can
never be guaranteed present. Measured right now: **2 of 8 links 404 (25%)**,
and which rows break moves with volume ranking.

```
404  /markets/USDCAllow-GDIEKKIQ…~USDC-GA5ZSEJY…
404  /markets/GoogleLiquid-GCYYXO7F…~native          <- operator-reported
200  (six others)
```

**Fix direction:** the durable fix is to stop pre-rendering a *global
top-N* and instead pre-render *the union of every set the UI can link to*
(on-chain pools ∪ top markets ∪ asset top-markets), or give
`/markets/[pair]` the same client-fetch fallback that `/ledgers/[seq]`
already has (see S2 — the mechanism exists in this codebase already).

---

## S2 — HIGH: dynamic routes split into two opposite failure modes

Under `output: 'export'` every dynamic route is a bounded pre-render.
Empirically the families behave in two incompatible ways:

| family | nonsense param | valid-but-unlisted param | verdict |
|---|---|---|---|
| `/ledgers/[seq]` | **200** shell | 200 | never 404s |
| `/transactions/[hash]` | **200** shell | 200 | never 404s |
| `/accounts/[g]` | **200** shell | 200 | never 404s |
| `/contracts/[id]` | **200** shell | 200 | never 404s |
| `/markets/[pair]` | 404 | **404** | 404s valid data |

Both halves are wrong, in opposite directions:

- **`/markets/*` 404s real entities** (S1).
- **`/ledgers|transactions|accounts|contracts/*` return HTTP 200 for
  entities that do not exist.** `/ledgers/99999999999/` and
  `/transactions/deadbeef…/` both serve a "Ledger"/"Transaction" shell.
  There is no honest 404 — the page renders chrome and then never
  populates, which is indistinguishable to a user from "data never loads",
  and to a crawler from a real page (soft-404, an SEO liability).

**Reproduce:**
```sh
curl -sL -o /dev/null -w '%{http_code}\n' https://stellarindex.io/ledgers/99999999999/   # 200
curl -sL -o /dev/null -w '%{http_code}\n' https://stellarindex.io/transactions/deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef/   # 200
```

Note `public/_redirects` contains **no 200-rewrite rules at all** (69 rules,
all 301s), so this fallback comes from the route implementations, not the
edge config — worth confirming which mechanism, since it is the thing
`/markets` needs.

---

## S3 — CRITICAL: endpoints that are down or unusably slow

Measured warm (second request), production, 2026-07-22:

| endpoint | code | warm | note |
|---|---|---|---|
| `/v1/accounts` | **500** | **8.12s** | spends 8s to fail. Backs `/accounts` |
| `/v1/liquidity-pools` | **500** | 0.13s | fails instantly. Backs `/liquidity-pools` |
| `/v1/sources?include=stats` | 200 | **8.15s** | `/v1/sources` alone is 0.13s — `include=stats` costs **8s** |
| `/v1/pools/reserves` | 200 | 3.42s | also **202 KB** |
| `/v1/contracts` | 200 | 2.83s | |
| `/v1/external/assets` | 200 | 1.82s | |
| `/v1/protocols` | 200 | 1.11s | |

`/v1/accounts` returning **500 after 8 seconds** is the worst shape
available: the user waits the full timeout and still gets nothing.

`include=stats` being a 60× multiplier (0.13s → 8.15s) is the single
highest-leverage fix here — it is requested on page load by `/network`.

**Reproduce:**
```sh
curl -s -o /dev/null -w '%{http_code} %{time_total}\n' https://api.stellarindex.io/v1/accounts
curl -s -o /dev/null -w '%{http_code} %{time_total}\n' https://api.stellarindex.io/v1/liquidity-pools
curl -s -o /dev/null -w '%{time_total}\n' 'https://api.stellarindex.io/v1/sources?include=stats'
```

---

## S4 — MEDIUM: `/network` fires 11 API calls, one still pending after load

Captured from the live page via Chrome network instrumentation:

```
/v1/assets?limit=100        200      /v1/assets?limit=25     200
/v1/assets/verified         200      /v1/account/me          401 (expected, anon)
/v1/status                  200      /v1/operations?limit=1  200
/v1/pools?limit=8…          200      /v1/network/stats       200
/v1/ledgers?limit=12        200      /v1/network/throughput  200
/v1/sources?include=stats   PENDING  <- still unresolved after page load
```

Two separate `/v1/assets` calls (limit=100 and limit=25) on one page is a
redundant fetch. The pending call is S3's 8-second endpoint.

---

## S5 — MEDIUM: dead external links

| status | URL | source |
|---|---|---|
| 404 | `https://pkg.go.dev/github.com/Stellar-Index/StellarIndex/pkg/client` | `app/sdk/page.tsx:255` |
| 404 | `https://stellarindex.io/embed/asset/USDC` | docs/embed example |
| 404 | `https://docs.cloud.coinbase.com/advanced-trade-api` | source attribution |
| 500 | `https://docs.kraken.com/websockets` | source attribution |

The Go SDK link is the operator-reported one. `pkg/client` **does exist in
the repo** and `github.com/…/tree/main/pkg/client` resolves — the failure is
that the module was never published to the Go module proxy, so pkg.go.dev
has nothing to show. Either publish/tag it or link to the tree URL.

Note the operator-cited form `github.com/Stellar-Index/StellarIndex/pkg/client`
is a malformed GitHub path shape (GitHub needs `/tree/<ref>/`); it is valid
as a **Go import path**, which is why it appears verbatim in the `go get`
snippet. Both the snippet and the pkg.go.dev link need attention, for
different reasons.

## S6 — MEDIUM: RFC 7807 `type` URIs are all unresolvable

Every API error returns a `type` URI under `https://api.stellarindex.io/errors/…`.
**All of them 404.** Sampled:

```
/errors/account-store-unavailable  /errors/asset-not-found  /errors/internal
/errors/invalid-max-age            /errors/invalid-status   /errors/missing-asset
/errors/rate-limited               /errors/unauthorized     /errors/not-found
```

RFC 7807 specifies `type` SHOULD be a dereferenceable URI documenting the
error. We publish nine dead documentation links on the error path, i.e. at
exactly the moment an integrator is trying to debug. (`docs.stellarindex.io`,
also cited in error bodies, *does* resolve — so the fix is a redirect map.)

---

## Checked, NOT a finding

Recording these so they are not re-investigated:

- **All 56 static routes return 200** and render in 0.07–0.41 s. No dead
  top-level pages.
- **169 internal static links: 1 failure**, and it is
  `/cdn-cgi/l/email-protection` (a Cloudflare email-obfuscation artefact,
  not our link).
- **`/v1/oracles` and `/v1/currencies` 404** — not bugs. The pages call
  `/v1/oracle/lastprice`, `/v1/oracle/streams`, `/v1/chart`,
  `/v1/price/batch`. My guessed paths were wrong.
- **The ~25 endpoints returning 400** in the sweep are parameter-required
  endpoints probed without parameters (`/v1/price`, `/v1/ohlc`, `/v1/vwap`,
  `/v1/search`, …), not failures.
- **`/v1/ledger/stream` "45 s"** is an SSE stream held open — expected.
- **`/ledgers/<live seq>/` resolves** for genuinely recent ledgers.

---

## Still to cover (audit backlog)

Carried forward, not yet executed:

- [ ] Per-widget render timing on every page type (Chrome), not just `/network`
- [ ] Screenshots for UX overflow / clipped tables / broken layout
- [ ] Non-Stellar contamination sweep across **all** pages, not just `/markets`
      (`/assets`, `/exchanges`, `/sources`, search results, homepage widgets)
- [ ] Console errors + failed XHR per page
- [ ] Empty-widget audit: which widgets render `—`, `0`, or a permanent skeleton
- [ ] Stale-timestamp audit: `as_of` vs now, per widget
- [ ] Pagination: does page 2+ work on every listing
- [ ] Search: does it return results, and are they Stellar-scoped
- [ ] Sitemap ↔ route drift; feeds (`blog.atom`, `changelog.atom`)
- [ ] `og:image` / social card reachability per route
- [ ] Title/description presence and duplication
- [ ] Security + cache headers (`/changelog` ships **1.5 MB** of HTML)
- [ ] Embed surfaces (`/embed/*`) — third-party facing, `limit=100` pre-render
- [ ] Status page (`web/status`) as a separate app
- [ ] Mobile/responsive breakage
- [ ] Accessibility pass
