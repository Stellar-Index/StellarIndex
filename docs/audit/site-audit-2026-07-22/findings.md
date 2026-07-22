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

### S1b — CORRECTED DIAGNOSIS: it is build-time drift, not the limit

My first read blamed the top-500 cutoff. That is wrong, and the correction
matters because it changes the fix.

`generateStaticParams` fetches the **same** endpoint the listing uses
(`/v1/markets?limit=500&order_by=volume_24h_usd_desc`). Checking the two
404ing pairs against the **live** top-500:

```
rank  27  USDCAllow-GDIEKKIQ… / USDC-GA5ZSEJY…   vol $6,382,566
rank  51  GoogleLiquid-GCYYXO7F… / native        vol $40,578
rank 100  HBAR-GACZWLOZ… / native                vol $1,893
```

All three are comfortably **inside** the 500 limit, and all three 404. The
set is therefore not too small — it is **stale**. A static export freezes
the market list at build time while markets churn continuously (new pairs
list, volumes reorder), so any pair that enters the ranks after the last
build 404s until the next build.

This is why the 2026-05-08 fix recurred: raising 100 → 500 treated the
cutoff, but the cause is that the set is a build-time snapshot of live,
moving data. **Raising it to 1000 would not fix this either.**

The durable fix is to give `/markets/[pair]` the same client-fetch
fallback `/ledgers/[seq]` and `/transactions/[hash]` already have (S2) —
those never 404 precisely because they do not depend on a build-time
snapshot. That also removes the CI-build coupling entirely.

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


---

## S7 — HIGH: the `/markets` listing links to its own dead pages

The listing page renders "55 Stellar markets (top 100 by volume)" with an
`On Stellar` / `Reference feeds` / `All` filter (so the contamination in
S1 is at least partly mitigated in the UI — see Corrections).

But **5 of those 55 rows link to a detail page that 404s (9%)**, including
**row 1**:

```
404  USDCAllow-GDIEKKIQ…        / USDC-GA5ZSEJY…     <- row 1 of the listing
404  GoogleLiquid-GCYYXO7F…     / native             <- row 7, operator-reported
404  CAUP7NFABXE5TJRL…          / CBIJBDNZNF4X…
404  GOLD-GBLP6EEEUPLP3DC4EHYHZNF / native
404  HBAR-GACZWLOZCENULENM…     / native
```

A user landing on the primary markets page and clicking the top row gets a
404. Same root cause as S1b.

## S8 — MEDIUM: table data overflows the viewport

On `/markets` the table is clipped horizontally — the `24H CHART` column
header is cut mid-word and its sparklines are sliced. Full asset
identifiers are rendered untruncated
(`USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN`,
`AQUA:GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA`), blowing
out the Base/Quote column widths and pushing the rest off-screen. Screenshot
captured.

## S9 — MEDIUM: a "Major incident in progress" banner is showing to all visitors

Site-wide red banner: *"Major incident in progress. 6 active alerts · top:
stellarindex_slo_availability_burn_fast"*. Whether or not the incident is
real, this is the first thing every visitor sees. Given `/v1/accounts` and
`/v1/liquidity-pools` are genuinely 500ing (S3), the alerts are plausibly
real — but the public-facing default should be reconsidered before showing
this to Stellar.

## S10 — MEDIUM: pagination gaps

- `/v1/assets?limit=5` **page 2 takes 5.99 s** (page 1 is 0.2 s)
- `/v1/ledgers` and `/v1/contracts` return **no `pagination.next`** — there
  is no way to page beyond the first result set on either

## S11 — LOW: `changelog.atom` is 1.19 MB

151 entries, 1.19 MB. Feed readers poll this repeatedly. `/changelog/`
itself is 1.51 MB raw (301 KB brotli) — the heaviest page on the site by an
order of magnitude.

## S12 — LOW: 39 non-Stellar market pages are in the sitemap

Of 501 `/markets/` entries submitted to search engines, 39 are
`crypto:`/`fiat:` pairs (e.g. `/markets/crypto%3AETH~crypto%3AUSDT/`). We
are actively asking Google to index Binance pairs as Stellar Index content.

---

## Corrections to earlier findings in this document

Recording these because two of my own checks produced false results:

- **Compression is fine.** I initially recorded "NO content-encoding" on
  every page. That was a curl artefact — curl does not request compression
  unless asked. With `--compressed`, brotli is served everywhere
  (`/changelog/` 1,514,012 → 301,400 bytes). **Not a finding.**
- **The sitemap is clean.** An early run reported 60/60 sitemap URLs
  failing; BSD `sed` does not support `\?`, so I was requesting malformed
  URLs. Corrected sample: **0 failures / 50**. **Not a finding.**
- **`/markets` contamination is partly mitigated in the UI.** The listing
  defaults to an `On Stellar` filter. The API-level 88%-off-chain figure in
  S1 is still what drives the sitemap (S12) and the pre-render population,
  so S1 stands — but the listing page itself is not showing users a wall of
  Binance pairs by default.
