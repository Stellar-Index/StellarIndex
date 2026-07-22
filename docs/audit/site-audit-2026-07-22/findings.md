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

---

## S13 — CRITICAL: the entire `/embed/asset/*` surface collapsed to XLM-only

`/embed/asset/[slug]`'s `generateStaticParams` builds its route list from
`/v1/assets`, and explicitly emits lowercase/uppercase variants because a
prior audit (2026-06-19) found `/embed/asset/xlm` 404ing. Live, **only one
route exists**:

```
200  /embed/asset/XLM/
404  /embed/asset/xlm/      404  /embed/asset/native/
404  /embed/asset/USDC/     404  /embed/asset/AQUA/    404  /embed/asset/EURC/
404  /embed/asset/SHX/      404  /embed/asset/yXLM/    404  /embed/asset/VELO/
```

The absence of the *case variants* is the tell: the code always emits them,
so the deployed build must have taken the `catch` branch and returned
`fallback = [{ slug: 'XLM' }]` — a single route, no variants. The asset
index fetch failed at build time and **the failure was swallowed silently**.

`/assets/USDC/`, `/assets/AQUA/` etc. all return 200, so this is specific to
the embed route's param generation, not the data.

This is a **policy inconsistency with a product cost**: `/markets/[pair]`
deliberately calls `requireRows(...)` so that "an unreachable or empty
markets listing throws so the build fails instead of exporting only the
fallback route". The embed route does the opposite — it catches and degrades
silently. The route that fails loudly is the one nobody noticed; the route
that fails quietly took out an entire product surface.

### S13a — `/widgets/` advertises embeds that 404

The `/widgets/` marketing page ships three copy-paste asset-card examples:
**XLM, USDC (Centre), and AQUA**. Two of the three are 404s. A customer
copying the USDC snippet gets a dead iframe.

## S14 — CRITICAL: `/embed/*` sends two contradictory CSPs, so framing is blocked

Every `/embed/*` response carries **duplicated, opposing headers**:

```
content-security-policy: … frame-ancestors 'none' …    <- from the /* rule
content-security-policy: … frame-ancestors *     …    <- from the /embed/* override
x-frame-options: DENY
x-frame-options: ALLOWALL
```

`_headers` intends the second rule to *override* the first, and its comment
reasons that "X-Frame-Options: DENY from the parent rule is harmless because
modern browsers ignore X-Frame-Options whenever a CSP [frame-ancestors] is
present". The X-Frame-Options half of that is right. The CSP half is the
bug: CF Pages **appends** the header rather than replacing it, and when a
response carries multiple CSP headers the browser enforces **all of them —
the intersection, not the last one**. `'none'` ∩ `*` = `'none'`.

**So no third party can iframe these widgets — the entire embed product is
non-functional**, independently of S13.

Evidence: on `/widgets/` the previews are live `<iframe>` elements
(`widgets/page.tsx:217`), and **all of them render broken — including the
XLM card, whose URL returns 200**. A 200 response that still won't frame
isolates the cause to frame policy rather than the missing routes in S13.

**Fix:** the `/embed/*` rule must emit a *single* CSP with
`frame-ancestors *` (i.e. the parent `/*` rule cannot also apply), not a
second CSP alongside it.

## S15 — MEDIUM: lowercase embed slugs 404 (regression)

`/embed/currency/USD/` 200 but `/embed/currency/usd/` 404; same for `eur`.
The 2026-06-19 audit fixed exactly this for `/embed/asset` by emitting case
variants — S13's fallback silently dropped them again, and `/embed/currency`
appears never to have had the fix. Embeds are hand-typed into `src=`
attributes, where lowercase is the natural instinct.

## S16 — LOW: `og:image` missing on `/sdex/` and `/docs/`

13 routes sampled; the other 11 have a reachable card. No broken images.

## S17 — LOW: no `/embed/*` route appears in the sitemap

0 of 1,140 sitemap entries are embeds, so the widget surface is entirely
unindexed.

---

## Additional "checked, not a finding"

- **Data freshness is good.** `as_of` on `/v1/status`, `/v1/network/stats`,
  `/v1/markets`, `/v1/assets`, `/v1/ledgers`, `/v1/pools` is all within a
  few seconds of now. No staleness anywhere.
- **Contamination is confined to `/v1/markets`.** `/v1/assets` (100 rows),
  `/v1/issuers` (50), `/v1/contracts` (50), `/v1/external/assets`,
  `/v1/sources`, `/v1/pools` contain **zero** off-chain references.
- **`/v1/anomalies` returning nothing is correct** — `firing_count: 0`,
  `events: []`. Genuinely no anomalies firing, not a broken widget.
- **`/embed/pair/*` works** for every pair tested, including non-top pairs.
- **`status.stellarindex.io` and `/status/` both 200** and are fast.

---

## S18 — HIGH: search cannot find any Stellar asset by its code

`/v1/search` resolves **only** `XLM`/`native` and full 56-character
canonical asset IDs. Every other Stellar asset code is rejected:

```
XLM     asset    supported=True   canonical=native
native  asset    supported=True   canonical=native
USDC    unknown  supported=False
AQUA    unknown  supported=False      SHX   unknown  supported=False
EURC    unknown  supported=False      yXLM  unknown  supported=False
VELO    unknown  supported=False      SSLX  unknown  supported=False
GOLD    unknown  supported=False
```

Every one of those has a working `/assets/<CODE>/` page (all 200), and
`?q=USDC-GA5ZSEJY…` (the full canonical form) resolves fine. So the data
exists and the page exists — only the lookup by the string a human would
actually type fails. Nobody types a 56-character issuer key.

Same shape as S13: the only inputs that work are the hardcoded special
cases, with everything data-driven falling through.

## S19 — MEDIUM: the Stellar Assets page leads with 19 fiat currencies

`/v1/assets/verified` — rendered as the "Verified currencies" strip at the
top of `/assets/`, above the actual asset table — breaks down as:

```
fiat        19   USD EUR CNY JPY GBP INR BRL KRW HKD AUD CAD
                 CHF MXN SGD ZAR TRY NZD SEK NOK
crypto       7   XLM AQUA yXLM SHX VELO BLND PHO
stablecoin   4   USDC PYUSD EURC yUSDC
```

**63% of the "verified" strip on a Stellar explorer's asset directory is
national fiat currencies**, each with its own `/assets/<currency>/` page
(`/assets/japanese-yen/`, `/assets/turkish-lira/`, …). These are FX
reference currencies from the pricing-API era, not Stellar assets — the
clearest instance of the legacy-positioning class.

All 19 links resolve, so this is a positioning/contamination finding, not
a dead-link one.

## S20 — LOW: 47% of assets have no icon

`image` is null/empty for **47 of 100** rows in `/v1/assets`. Visible in the
table as a blank ASSET column for roughly half the listing.

---

## Further corrections to my own checks

- **`/assets/EUR/` and `/assets/JPY/` "404s" were my error.** The verified
  chips link to friendly slugs (`/assets/euro/`, `/assets/japanese-yen/`),
  not codes. Re-tested all 19 fiat chips: **0 dead of 19**. Not a finding.
- **The assets table is not missing much data.** My reading of the
  screenshot suggested widespread gaps; the API says otherwise —
  `price_usd` missing in 2/100, `market_cap_usd` 4/100,
  `volume_24h_usd` 3/100, and **0 rows have volume without a price**. Only
  `image` (S20) is materially sparse.
- **The UI search modal is not simply `/v1/search`.** It gates that call
  behind a `looksLikeExplorerEntity` check and separately loads
  `/v1/assets/verified` as a local index, so the S18 API gap may be partly
  masked in the UI for verified assets. Worth confirming interactively —
  the API-level gap is real either way.

---

## S18 — DOWNGRADED after checking the UI

I flagged search as HIGH on the strength of the API alone. Checking the
actual user path changes the severity.

Typing `USDC` into the site's search modal **works**: it returns USDC,
yXLM, AQUA, SHX, VELO, yUSDC, AFR, XRP, WGUARDIAN, LIBRE — each linking to
a valid `/assets/<CODE>` page. The modal builds a client-side index from
`/v1/assets/verified` + the assets listing and only falls through to
`/v1/search` for things that look like explorer entities
(`looksLikeExplorerEntity`).

So the S18 gap is **real at the API level but masked for site users**:

- **Affected:** direct `/v1/search` consumers — integrators, the public
  API surface, anything not carrying the UI's local index.
- **Not affected:** people using the site's search box.

Severity **HIGH → LOW/MEDIUM**. Still worth fixing (we publish `/v1/search`
as a public endpoint that cannot resolve `USDC`), but it is not the
user-facing outage I first implied.

### S18a — search result relevance is weak (needs a second look)

Typing `USDC` returns USDC first, then nine assets with no textual relation
to the query (AFR, XRP, WGUARDIAN, LIBRE, …). Either the list is
unfiltered beyond the top match, or unmatched "popular assets" are appended
without a visual separator. Not confirmed which — flagged for follow-up
rather than asserted.

---

## Coverage status — what is verified vs what is NOT

Being explicit so the gaps are not mistaken for clean results.

**Verified this audit:** all 56 static routes · 169 internal static links ·
75 external links · 60 API endpoints (status + warm latency) · 5 dynamic
route families probed for both nonsense and valid-unlisted params · the
`/markets` pre-render population vs live ranking · sitemap (50-URL spread
sample) · feeds · og:image on 13 routes · title/description presence ·
security + cache headers on 5 surfaces · compression · contamination across
6 listing endpoints · data freshness on 6 endpoints · pagination on 4
endpoints · the full `/embed/*` surface · the `/widgets/` product page ·
`/network` widget-level network waterfall · `/accounts`, `/liquidity-pools`,
`/markets`, `/assets` rendered states.

**NOT verified — do not read absence of findings as absence of problems:**

- **Mobile / responsive.** Attempted via `resize_window` to 390×844; the
  screenshot pipeline continued to capture at 1568×776, so the narrow
  viewport was never actually observed. **No mobile conclusion of any kind
  should be drawn from this audit.**
- **Console errors per page.** The console-capture tool returned no
  messages on repeated attempts, including after reloads with tracking
  armed. Unverified, not clean.
- **Per-widget render timing beyond `/network`.** Only `/network` had its
  network waterfall captured.
- **Accessibility.** Not started.
- **Authenticated surfaces** (`/dashboard/*`, `/signin`, `/signup` flows)
  beyond an anonymous 200 check.
- **Interactive behaviour**: filters, sorting, per-page selectors, tab
  switches, "load more" — none exercised.
- **`/status` app internals** beyond a 200 + latency check.

---

## S21 — HIGH: time-to-data per page type — three page types block for 8+ seconds

Measured as the slowest API dependency each page type must resolve before
its primary widget can render (warm, production):

| page type | slowest dep | time | blocking endpoint |
|---|---|---|---|
| `/accounts` | 1 of 1 | **8.23 s** | `/v1/accounts` — *then 500s* |
| `/network` | 1 of 9 | **8.13 s** | `/v1/sources?include=stats` |
| `/sources` | 1 of 1 | **8.11 s** | `/v1/sources?include=stats` |
| `/contracts` | 1 of 1 | **3.89 s** | `/v1/contracts` |
| `/liquidity-pools` | 1 of 2 | **2.08 s** | `/v1/pools/reserves` (202 KB) |
| `/protocols` | 1 of 1 | 1.27 s | `/v1/protocols` |
| `/oracles` | 1 of 2 | 0.62 s | `/v1/oracle/streams` |
| `/markets` | 1 of 2 | 0.13 s | — |
| `/assets` | 1 of 2 | 0.17 s | — |
| `/ledgers` | 1 of 1 | 0.13 s | — |

**Five page types exceed 2 s; three exceed 8 s.** This is the measured form
of the "widgets are too slow to load" report.

Note `/network` fires 9 dependencies summing 10.3 s of API time but is
governed by one 8.1 s call — so it is not fan-out that hurts, it is a
single endpoint. Fixing `?include=stats` alone fixes two page types
(`/network` and `/sources`) and removes 8 s from the worst path on the
site. Combined with S3's `/v1/accounts`, **two endpoint fixes address every
page over 8 s.**

## S22 — MEDIUM: `/accounts` ships with no `<h1>`

Every other page sampled has exactly one `<h1>`; `/accounts/` has **zero**
(no `<h2>` either). `/markets/` for comparison emits
`<h1 class="text-h1 …">Markets`. The visible "Accounts" title is not a
heading element, so the page has no document outline — an accessibility
defect (screen-reader navigation) and an SEO one.

---

## Method limits on the a11y pass

The static-HTML a11y sweep across 10 routes found **zero `<img>` and zero
`<input>` elements in the served markup** — icons are inline SVG and the
search box is client-rendered. So alt-text and form-label coverage
**could not be assessed statically** and remain **unverified, not clean**.
The `<h1>` check was meaningful because headings *are* server-rendered,
which is what makes S22 a real result rather than an artefact.

Mobile prerequisites are present and correct on the routes checked
(`<html lang="en">`, `<meta name="viewport" content="width=device-width,
initial-scale=1">`) — but per the coverage-status section, actual
narrow-viewport rendering was never observed.

---

## S23 — MEDIUM: on the "All" view of the Markets page, XLM ranks 12th

Exercising the `On Stellar` / `Reference feeds` / `All` filter (all three
work correctly — see Interactive behaviour below), the `All` view orders by
24h USD volume and produces this as the Markets page of a **Stellar**
explorer:

```
 1 BTC/USDT  $1.28B     2 BTC/USD  $545.98M    3 ETH/USDT $492.49M
 4 ETH/USD   $256.43M   5 SOL/USDT $104.77M    6 XRP/USD   $96.39M
 7 XRP/USDT   $77.21M   8 SOL/USD   $66.32M    9 BNB/USDT  $45.94M
10 DOGE/USDT  $26.86M  11 TRX/USDT  $23.88M   12 XLM/USD   $20.80M
13 NEAR/USDT  $20.64M  14 ADA/USDT  $14.07M   15 USDCAllow $14.04M
```

**XLM — the native asset of the chain this explorer covers — is 12th,
behind Dogecoin and Tron.** The `On Stellar` default (S1) keeps this off the
initial view, but the tab is one click away and is what "All" naturally
invites. It is the same off-chain reference-feed data that drives S1's
pre-render population and S12's sitemap entries.

Not a defect in the filter — the filter is correct and useful. It is a
positioning question about whether reference-feed pairs belong in the same
ranked table as on-chain markets at all, or whether they should live only
under `/external/assets` and `External Markets` (both of which already
exist in the nav).

---

## Interactive behaviour — verified

- **`/markets` source filter** (`On Stellar` / `Reference feeds` / `All`):
  all three states work, update the row count honestly ("55 of 100 rows" →
  "100 of 100 rows"), and update the panel title.
- **Search modal** (`⌘K`): opens, filters, and emits valid
  `/assets/<CODE>` links (see S18 correction).

## Mobile — CONFIRMED NOT OBSERVABLE with the available tooling

Second attempt, different dimensions (414×896 as well as 390×844):
`resize_window` reports success both times, but every subsequent capture
returns 1568×776 desktop layout. The narrow-viewport rendering was never
seen.

**This audit therefore contains no mobile evidence whatsoever — neither
positive nor negative.** Responsive behaviour needs a real device, a
browser devtools device-emulation session, or a headless runner with a set
viewport. It should not be signed off on the strength of anything here.

The only mobile-adjacent facts established are that `<meta name="viewport">`
and `<html lang>` are correctly set — necessary but nowhere near
sufficient.

---

## S24 — HIGH: `/sources` shows SDEX as 19 days stale while it is ingesting normally

The `/sources` page renders a `LAST INGEST` column. Every venue reads
`5s ago @ ledger 63,597,719` — except **`sdex`, which reads `19d ago @
ledger 63,301,500`, highlighted in red** as a staleness warning.

It is wrong. Ground truth from the database at the same moment:

```
sdex      latest trade 2026-07-22 16:27:38+00   23,782 trades in 20 minutes
aquarius  latest trade 2026-07-22 16:26:54+00      138 trades in 20 minutes
```

SDEX is ingesting thousands of trades a minute. `/v1/sources?include=stats`
independently reports `trade_count_24h: 1,450,756` and
`markets_count_24h: 26,008` for it — healthy on every other measure, and
the page renders those same healthy numbers *in the adjacent columns*.

So the flagship Stellar DEX — the single most important on-chain source on
a Stellar explorer — is publicly displayed as dead while it is the busiest
venue we index. This is worse than a missing value: it is a **confident,
colour-coded false negative** on the one page an evaluator would open to
judge pipeline health.

Note `/v1/diagnostics/cursors` contains **no `sdex` cursor at all**, so
whatever the page derives this from, the derivation has no live input for
sdex and is falling back to something 19 days old rather than declining to
render.

## S25 — MEDIUM: `/v1/diagnostics/cursors` serves 1,483 rows / 117 KB, almost all dead

```
1202  projected-rebuild     194  backfill        36  gap-detector-scan
  31  census-backfill        15  projector        3  backfill-router
   1  ledgerstream            1  tag-routed-via
```

Only `ledgerstream` is live (lag 3 s). The rest are abandoned one-shot job
cursors — `backfill` entries last touched **2026-05-03**, `census-backfill`
**2026-06-02** — reported with lags of 4–6 *million* seconds. A public
diagnostics endpoint returning 117 KB of months-dead job state makes the
one genuinely useful row (`ledgerstream`) impossible to find, and would
read to an outsider as a pipeline full of stalled work.

---

## Method note on the empty-state sweep

I scanned all 56 routes' served HTML for empty/error strings. It produced a
hit on **every** route — because `couldn't find` is the 404 copy bundled
into the shared JS chunk, not a rendered state. **Discarded as a false
positive.** Runtime empty states cannot be detected from static markup on
this site, since every widget hydrates client-side; they require the
browser, which is how S24 was actually found.

---

## S26 — LOW: all five dashboard sub-pages share one title

```
/dashboard              <title>Account · Stellar Index</title>
/dashboard/keys         <title>Account · Stellar Index</title>
/dashboard/usage        <title>Account · Stellar Index</title>
/dashboard/settings     <title>Account · Stellar Index</title>
/dashboard/price-alerts <title>Account · Stellar Index</title>
/dashboard/admin        <title>Account · Stellar Index</title>
```

Six routes, one title. Browser tabs and history are indistinguishable for a
signed-in user with several open.

---

## Authenticated surfaces — verified, and they behave correctly

- `/dashboard/keys/` (and siblings) **redirect an anonymous visitor to
  `/signin/`**, which renders a clean passwordless magic-link form. The gate
  works.
- **No credential or key material leaks into the served HTML.** Scanning
  `/dashboard/admin` and `/dashboard/keys` anonymously surfaces only the
  strings `unauthorized` / `forbidden` / UI copy — the shape you want.
- `/signin`, `/signup`, `/auth/callback` all render correctly and carry
  distinct, accurate titles.

The only defect on this surface is S26.

---

# Findings index

| # | Sev | Finding |
|---|---|---|
| S1 / S1a / S1b | **CRITICAL** | `/markets/[pair]` 404s valid markets — **build-time drift**, not the top-500 limit. `/network`'s widget ranks a different population, so its links are structurally unguaranteed |
| S2 | HIGH | Dynamic routes fail two opposite ways: `/markets` 404s real entities; `/ledgers`/`/transactions`/`/accounts`/`/contracts` return **200 for nonsense** (soft-404) |
| S3 | **CRITICAL** | `/v1/accounts` **500 after 8.1 s**; `/v1/liquidity-pools` 500; `?include=stats` is a **60× latency multiplier** |
| S4 | MEDIUM | `/network` fires 11 API calls; two redundant `/v1/assets`; one still pending after load |
| S5 | MEDIUM | Dead external links (pkg.go.dev SDK, embed example, 2 vendor docs) |
| S6 | MEDIUM | **Nine RFC 7807 error `type` URIs all 404** |
| S7 | HIGH | `/markets` listing links to its own dead pages — **5 of 55, including row 1** |
| S8 | MEDIUM | Table overflows viewport; untruncated asset IDs clip the chart column |
| S9 | MEDIUM | Site-wide "Major incident in progress" banner shown to every visitor |
| S10 | MEDIUM | `/v1/assets` page 2 takes 5.99 s; `/v1/ledgers` + `/v1/contracts` expose no `pagination.next` |
| S11 / S12 | LOW | `changelog.atom` 1.19 MB; 39 non-Stellar market pages in the sitemap |
| S13 / S13a | **CRITICAL** | `/embed/asset/*` collapsed to **XLM-only** via a swallowed build error; `/widgets` advertises two embeds that 404 |
| S14 | **CRITICAL** | `/embed/*` sends **two contradictory CSPs** → framing blocked → the entire widget product cannot work |
| S15–S17 | LOW | Lowercase embed slugs 404; `og:image` missing on 2 routes; no embed route in the sitemap |
| S18 / S18a | LOW-MED | `/v1/search` can't resolve any asset by code (**masked in the UI**); result relevance weak |
| S19 | MEDIUM | **19 fiat currencies lead the Stellar asset directory** (63% of the "verified" strip) |
| S20 | LOW | 47% of assets have no icon |
| S21 | HIGH | **Three page types block 8+ s**; two endpoint fixes clear all of them |
| S22 | MEDIUM | `/accounts` ships **no `<h1>`** |
| S23 | MEDIUM | On the "All" markets view, **XLM ranks 12th** — behind DOGE and TRX |
| S24 | HIGH | **`/sources` shows SDEX as 19 days dead** while it is the busiest venue indexed |
| S25 | MEDIUM | `/v1/diagnostics/cursors` serves **1,483 rows / 117 KB**, one of them live |
| S26 | LOW | Six dashboard routes share one `<title>` |

**Highest leverage:** four fixes — `?include=stats`, `/v1/accounts`, the
`/embed/*` CSP rule, and the swallowed `catch` in the embed params — clear
every 8-second page *and* both defects that make the widget product
unusable.

## Corrections I made to my own findings during this audit

Kept deliberately visible; each was caught by checking rather than assuming.

1. **S1 diagnosis** — blamed the top-500 limit; it is build-time drift
   (the 404ing pairs rank 27, 51, 100 in live data). Changes the fix.
2. **Compression** — reported missing; was a curl artefact. Brotli works.
3. **Sitemap 404s** — reported 60/60 failing; BSD `sed` built malformed
   URLs. Actual: 0/50.
4. **`/assets/EUR/` 404** — tested the wrong URL shape; all 19 fiat chips
   resolve.
5. **Assets "missing data"** — misread a screenshot; only icons are sparse.
6. **S18 severity** — HIGH on API evidence alone; the UI masks it. Downgraded.
7. **Empty-state sweep** — hit all 56 routes; `couldn't find` is bundled 404
   copy, not a rendered state. Discarded.

---

## S14 — CONFIRMED empirically (upgraded from header inference)

Earlier this was argued from headers plus broken previews. It is now tested.

Framing `/embed/asset/XLM/` from a page on the **same origin**:

```
onloadFired:       true
onerrorFired:      false
contentAccessible: false   (contentDocument reachable, body EMPTY)
cspViolations:     []      (reported to the framed doc, not the embedder)
```

And the embed itself is **not** empty — served directly it is 26,268 bytes
of real rendered content:

```
"XLM  Stellar  $0.192229  -0.23% 1h  +2.80% 24h  +6.57% 7d
 Powered by Stellar Index  $2.1M 24h vol"
```

A page with live content that yields an **empty document when framed** is
the signature of a blocked frame: the browser loads `about:blank`, `onload`
fires, and the body is empty. `frame-ancestors 'none'` blocks **same-origin
framing too**, which is exactly why `/widgets`' own previews are broken —
they are same-origin iframes.

Headers re-confirmed on the embed route — **two** CSP headers:

```
frame-ancestors 'none'
frame-ancestors *
```

I briefly doubted this finding when the same-origin test loaded without a
CSP violation event. That was the wrong reading: `frame-ancestors`
violations are reported to the *framed* resource's policy, not the
embedder's, so their absence in the parent proves nothing. The empty body
against known-good content is the real signal. **S14 stands.**

## S27 — MEDIUM: accessibility defects in the rendered DOM

Measured against the live DOM (not static markup, which has no icons):

| check | `/markets` |
|---|---|
| `<img>` without `alt` | 0 (there are no `<img>` at all) |
| **`<svg>` with no `aria-label`, no `aria-hidden`, no `<title>`** | **54 of 86 (63%)** |
| `<input>`/`<select>` with no accessible name | 0 of 1 |
| `<button>` with no accessible name | 0 of 8 |
| **heading-level skip** | **`h1 → h3`** |

Form controls and buttons are properly named — genuinely good. The defects
are **54 unlabelled SVG icons** (decorative ones need `aria-hidden="true"`,
meaningful ones need a label; right now a screen reader meets 54 unnamed
graphics) and a **skipped heading level**, which breaks the document
outline alongside S22's missing `<h1>` on `/accounts`.

## S28 — refines S8: the table overflows even at 2560 px

The horizontal clipping seen in the screenshots is now measured:

```
div.overflow-x-auto   client 1662 px   scroll 1807 px   overflow 145 px
document scrolls horizontally: NO
```

Two corrections to how I first described S8:

1. It is **handled, not broken** — the wrapper carries `overflow-x-auto`, so
   the *page* never scrolls sideways; the table scrolls within its own
   container. The layout is not blown out.
2. But the content is **1,807 px wide on a 2,560 px viewport**, so the
   rightmost column (`24H CHART`) is off-screen **by default on a very wide
   desktop**. Users never see it without discovering the inner scroll.

**Mobile inference (inference, NOT an observation):** 1,807 px of table
content in a 390 px viewport implies roughly **4.6× horizontal scrolling**.
This follows arithmetically from the measurement above; it has still not
been visually confirmed, and the mobile caveat elsewhere in this document
stands.

---

## Tooling blocks — resolved

Three classes were previously recorded as unverifiable. Two are now done:

- **Accessibility (alt/label):** solved by querying the **rendered DOM** via
  the JS bridge instead of static HTML. → S27.
- **Overflow/responsive measurement:** solved the same way. → S28.
- **Console errors:** still unresolved. The MCP console reader returns
  nothing across reloads with tracking armed. The JS bridge cannot
  retroactively recover errors thrown before it was injected, so
  page-load-time console state remains **unverified**.

Note the JS bridge itself was initially rejected ("Cookie/query string
data") — that was triggered by URLs with query strings inside the submitted
code, not by the tool being unavailable. Rewriting the payload without
query strings made it work, which is what unblocked S27 and S28.

---

## S29 — per-widget render timing, measured (`/network`)

From the browser's own `performance` timeline, not endpoint probing:

```
DOMContentLoaded      141 ms
load event            271 ms
10 API calls all start ~285 ms   (well parallelised — no waterfall)
9 of 10 settle by     404 ms
/v1/sources settles  8356 ms     <- 8,070 ms duration
/v1/operations       1300 ms
```

So the page shell is interactive in **0.27 s**, nine widgets are populated
by **0.4 s**, and **one widget shows a skeleton for 8.4 seconds**.

This kills a plausible theory: the problem is *not* fan-out or a request
waterfall. Ten calls fire simultaneously and nine finish in ~120 ms each.
A single endpoint accounts for the entire perceived slowness of the page.

## S30 — HIGH: the `/accounts` failure is silent to developers and misattributed to users

Captured live by hooking `console.error`, `window.onerror`,
`unhandledrejection` and `fetch` **before** SPA-navigating into `/accounts`:

```
failedFetches:        ["500 /v1/accounts?limit=100"]
consoleErrors:        []
warnings:             []
unhandledRejections:  []
```

Two distinct problems:

1. **Silent to developers.** A 500 on the page's only data source produces
   **no console output whatsoever**. Anyone debugging in devtools sees a red
   Network row and nothing else.
2. **Misattributed to users.** The rendered message is:

   > "The accounts directory is unavailable right now (the current-state
   > projection is still backfilling, or pricing is offline)."

   Neither is true. The projection is not backfilling and pricing is not
   offline — `/v1/accounts` takes 8.1 s and returns HTTP 500. The copy names
   two innocent subsystems and omits the real one, actively pointing an
   operator (or an evaluator) at the wrong place.

An honest degraded state here would say the directory query failed, and
ideally return 200 with a `degraded` flag rather than nothing.

---

## Console / runtime state — now verified

Previously recorded as unverifiable. Resolved by installing hooks via the JS
bridge and then using **SPA navigation** (which preserves the JS context)
instead of a full page load.

- **`/network`**: 13 s observation — **0 console errors, 0 warnings,
  0 unhandled rejections, 0 failed fetches.** Genuinely clean.
- **`/accounts`**: 1 failed fetch (the 500), and — the finding — **0 console
  errors** despite it.

The remaining true gap is errors thrown *before* hooks can be injected on a
cold page load; those still cannot be captured with this tooling. Everything
after hydration is now covered.

---

## S31 — HIGH: `/status` says "All systems operational" while its own panels disagree

The status page — the surface you would point an evaluator or customer at —
leads with a green banner:

> **All systems operational** · Operational
> *Every service is reporting healthy.*

Directly beneath it, **on the same screen**:

| panel | value | target | state |
|---|---|---|---|
| Request latency **P95** | **840.5 ms** | 200 ms | **red, 4.2× over** |
| Request latency **P99** | **2096.4 ms** | 500 ms | **red, 4.2× over** |
| Active sources | **23 / 25** | 25 | 2 inactive |

And contradicted by three things this audit established independently:

1. `/v1/accounts` returns **HTTP 500** (S3)
2. `/v1/liquidity-pools` returns **HTTP 500** (S3)
3. Every other page on the site carries a red banner reading **"Major
   incident in progress. 6 active alerts"** (S9)

So the site simultaneously tells a visitor "major incident in progress" on
`/markets` and "all systems operational, every service is reporting
healthy" on `/status`. The headline is computed from service liveness
(`Api / Indexer / Aggregator — last seen Ns ago`) alone and ignores its own
latency SLO panel, its own active-source count, and the endpoint health the
rest of the site is alarming on.

A green status page over two 500ing public endpoints and two breached SLOs
is the single most damaging surface in this audit to show to Stellar — it
is not merely wrong, it is confidently wrong on the page whose entire job
is to be trustworthy.

*(Positive note from the same panel: it correctly reports `r1 PRODUCTION ·
Hetzner Frankfurt · v0.19.2`, ingest lag `0s`, latest ledger 63,597,854 —
so the underlying telemetry is right. It is the roll-up that is wrong.)*

## S32 — MEDIUM: `/status` fires 46 API calls with a 2-second waterfall

```
load event            240 ms
API calls               46      <- vs 10 on /network
first call starts     254 ms
last call STARTS     2318 ms    <- 2,064 ms waterfall spread
last settles         4737 ms
```

Unlike `/network` (which fires everything in parallel — S29), `/status`
serialises: `/v1/observations` is called twice, the second starting at
2,318 ms, one millisecond after the first finishes at 2,317 ms. That is a
sequential dependency, not concurrency.

Net effect: the status page takes **4.7 s** to finish populating, roughly
half of which is avoidable ordering rather than slow endpoints.

---

## S33 — MEDIUM: the markets table has no responsive column handling

Resolved without ever observing a narrow viewport, by checking the
rendered DOM for the mechanism that would adapt it.

```
table columns:                    8   (#, Base, Quote, Last price,
                                       24h volume, 24h trades,
                                       24h chart, Last trade)
columns with a responsive class:  0
table content width:           1807 px
container width:               1662 px
```

**Zero of the eight columns carry `hidden` / `sm:` / `md:` / `lg:`
classes**, so all eight render at every viewport. The CSS bundle *does*
contain `.sm\:table-cell` rules (a responsive-table pattern is used
somewhere in the app) — just not on this table.

By contrast the sidebar **is** properly responsive:
`sticky top-0 hidden h-screen w-64 shrink-0 border-r border-line lg:block`
— hidden below 1024 px, shown above. So the app has the pattern and applies
it to chrome, but not to its densest data surface.

Consequence at 390 px: 1,807 px of table content in a ~390 px viewport,
with no columns dropped ⟹ roughly **4.6× horizontal scrolling** inside the
container to reach `24h chart` / `Last trade`.

### Correction chain on this finding, for the record

1. S8 first described this as "data overflowing the UX / broken layout".
2. S28 corrected that: it is **contained** (`overflow-x-auto`; the page
   never scrolls sideways) but the rightmost column is off-screen even at
   2560 px.
3. I then found `.sm\:table-cell` in the CSS and **provisionally withdrew**
   the mobile inference, on the theory that columns drop below 640 px.
4. Checking the actual table DOM showed **0 of 8 columns carry any
   responsive class** — those rules belong to a different table. The
   inference **stands**, now on structural evidence rather than arithmetic.

---

## Mobile — status after all attempts

Narrow-viewport **rendering** was never visually observed: `resize_window`
reports success but the viewport stays 2560 px (confirmed via
`window.innerWidth`), and framing the site to get an independent viewport
is blocked by its own `frame-ancestors 'none'` (S14).

What *was* established, structurally and without observation:

- ✅ Responsive infrastructure exists — 5 Tailwind breakpoints (40/48/64/80/96 rem)
- ✅ The sidebar adapts correctly (`hidden … lg:block`)
- ✅ `prefers-reduced-motion` handled in **both** directions
- ✅ `forced-colors: active` handled (Windows high-contrast)
- ✅ `<meta name="viewport">` and `<html lang>` correct
- ❌ The markets table does **not** adapt (S33)

That is a real, defensible mobile result. It is not a substitute for
looking at the site on a phone, and the visual check should still happen —
but "mobile is unaudited" is no longer accurate.

---

## S34 — per-widget render timing across page types (browser timeline)

Measured from each page's own `performance` timeline — actual widget
settle times, not endpoint probes:

| page | load | API calls | waterfall spread | last widget settles | rows |
|---|---|---|---|---|---|
| `/transactions` | 216 ms | 7 | 162 ms | **462 ms** ✅ | 200 |
| `/contracts` | 190 ms | 6 | **2 ms** | **2,741 ms** ⚠️ | 100 |
| `/network` | 271 ms | 10 | ~3 ms | **8,356 ms** ❌ | — |
| `/status` | 240 ms | **46** | **2,064 ms** | **4,737 ms** ⚠️ | — |
| `/accounts` | — | 1 | — | **500 after 8.1 s** ❌ | 0 |

Reading across them:

- **Every page shell is fast** — 190–271 ms to `load`. The static export is
  doing its job; nothing here is a front-end weight problem.
- **Fan-out is well-parallelised almost everywhere.** `/contracts` starts
  all 6 calls within **2 ms** of each other; `/network` within ~3 ms.
  Slowness is never concurrency — it is always one endpoint.
- **`/status` is the exception**: a 2,064 ms waterfall spread across 46
  calls (S32), so roughly half its 4.7 s is ordering, not endpoint latency.
- **`/transactions` is the model** — 7 calls, everything settled in 462 ms,
  200 rows rendered, no empty states.

## S35 — LOW: `/v1/assets` is fetched twice on at least three page types

Confirmed by counting duplicate endpoints in the browser timeline:

```
/network      /v1/assets x2   (limit=100 and limit=25)
/transactions /v1/assets x2
/contracts    /v1/assets x2
```

Two independent components each fetch the asset listing rather than
sharing one cached query. Cheap (~120 ms, warm) but it is a consistent
pattern across pages, and it doubles a 57 KB payload on every page load.

---

## Final coverage statement

**Audited and verified:** 56 static routes · 2,718 extracted links (169
internal, 75 external) · 60 API endpoints · 5 dynamic-route families ·
per-widget browser-timeline timing on 5 page types · endpoint-level timing
on 10 page types · rendered-DOM accessibility · responsive CSS + DOM
structure · console/runtime state on 2 pages · authenticated gating ·
interactive filters on 2 surfaces · sitemap, feeds, robots, og:images,
headers, compression, pagination, data freshness, contamination across 6
listing endpoints, and the full embed/widget surface.

**Genuinely still open** (small, and each has a stated reason):

1. **Narrow-viewport visual rendering** — structurally resolved (S33) but
   never seen. Blocked by `resize_window` not taking effect *and* by the
   site's own `frame-ancestors 'none'` (S14) preventing an iframe viewport.
   Needs a device or a headless runner.
2. **Cold-load console errors** — hooks cannot be injected before
   hydration; post-hydration state is verified clean on `/network`.
3. **Interactive behaviour on the long tail** — filters/sorting verified on
   `/markets` and the search modal; the remaining listing pages use the
   same shared table components, so the risk is low but it is untested.

Nothing in the "still open" list is expected to change the priority order
below.

## Recommended fix order

1. **S31** — `/status` reporting green over two 500s and two breached SLOs.
   Worst surface to show Stellar; the telemetry underneath is already
   correct, so this is a roll-up change.
2. **S3 / S21** — `?include=stats` (fixes `/network` *and* `/sources`) and
   `/v1/accounts`. Two endpoints clear every page over 8 s.
3. **S14 + S13** — one `_headers` rule and one swallowed `catch`; together
   they restore the entire embed/widget product.
4. **S1b** — client-fetch fallback on `/markets/[pair]`, not a bigger
   pre-render limit.
5. **S24** — `/sources` showing SDEX 19 days stale while it is the busiest
   venue indexed.

---

## S36 — MEDIUM: the asset directory cannot be sorted; `/markets` can

`/assets` renders 92 rows across 11 columns — `#`, Asset, Class, **Price,
1h %, 24h %, 7d %, Market cap, Volume 24h, Circulating**, 7d chart — and
**not one of them is sortable**:

```
every <th>:  hasButton false · hasLink false · role null
             aria-sort null  · tabIndex -1
             class "whitespace-nowrap px-4 py-2.5 font-medium"
```

No click target, no sort affordance, no keyboard access. Meanwhile
`/markets` — the sibling listing built from the same table primitives — is
sortable, and advertises it (`Base ↕`, `24h volume ↓`).

So the two flagship directories behave differently: on Markets you can rank
by volume; on Assets, which carries market cap and four change columns
expressly inviting comparison, you cannot rank by anything. A user wanting
"biggest Stellar assets by market cap" has no way to get it.

Also an a11y consequence: sortable-looking data columns with `tabIndex -1`
and no `aria-sort` give assistive tech nothing to work with (compare S27).

---

## Interactive behaviour — coverage now

| surface | control | result |
|---|---|---|
| `/markets` | source filter (`On Stellar`/`Reference feeds`/`All`) | ✅ works, counts update honestly (55→100) |
| `/markets` | column sort | ✅ present (`↕` / `↓` affordances) |
| search modal | `⌘K`, type-to-filter | ✅ works, emits valid links |
| `/assets` | column sort | ❌ **absent entirely** (S36) |
| `/assets` | text filter + per-page select | ✅ present (1 input, 1 select) |
| `/assets` | class tabs (`All`/`Crypto`/`Stablecoin`) | ✅ present (5 buttons) |

The remaining listing pages reuse these same table primitives, so the
inconsistency to chase is S36's specific one — Markets got sorting wired
up and Assets did not.

---

## S36 — BROADENED: sorting is missing from *every* listing except `/markets`

I first framed this as an Assets-vs-Markets inconsistency. Walking the
remaining listing pages via SPA navigation with instrumentation attached
shows it is far wider:

| listing | columns | sortable | rows |
|---|---|---|---|
| `/markets` | 8 | **yes** (`↕`, `↓`) | 55–100 |
| `/assets` | 11 | **0** | 92 |
| `/ledgers` | 5 | **0** | 50 |
| `/operations` | 6 | **0** | 50 |
| `/contracts` | 4 | **0** | 100 |
| `/issuers` | 6 | **0** | 100 |
| `/oracles` | 9 | **0** | 106 |

**41 columns across six listing pages, none sortable.** `/markets` is the
only listing in the explorer where a user can reorder data — and the pages
that most invite it (`/assets` with market cap and four change columns,
`/issuers`, `/oracles`) are precisely the ones that cannot.

## S37 — per-page timing, remaining listings

Captured during the same walk (SPA navigation, so these are warm — a cold
load would be slower):

| page | API calls | slowest | endpoint | rows | empty/loading |
|---|---|---|---|---|---|
| `/ledgers` | 2 | 104 ms | `/v1/network/throughput` | 50 | clean |
| `/issuers` | 0 (cached) | — | — | 100 | clean |
| `/operations` | 2 | **1,856 ms** | `/v1/operations` | 50 | clean |
| `/contracts` | 3 | **3,093 ms** | `/v1/contracts` | 100 | clean |
| `/oracles` | 3 | **3,217 ms** | `/v1/sources` | 106 | clean |

Three more page types over 1.8 s, and `/oracles` at 3.2 s on `/v1/sources`
— the same endpoint behind `/network` and `/sources` (S3). That endpoint
now demonstrably gates **four** page types.

**No empty states, no stuck loading spinners, and no failed fetches on any
of these five.** They render their data correctly; they are just slow.

---

## Page-type coverage — final

Data-bearing page types with per-widget timing captured: `/network`,
`/status`, `/transactions`, `/contracts`, `/accounts`, `/assets`,
`/ledgers`, `/operations`, `/issuers`, `/oracles`, `/markets`,
`/liquidity-pools`, `/sources` — **13 of the ~20 data-bearing routes**.
The remainder (`/dexes`, `/lending`, `/bridges`, `/aggregators`, `/amm`,
`/mev`, `/anomalies`, `/divergences`, `/yield`, `/external/assets`,
`/exchanges`, `/protocols`, `/diagnostics`) had their **endpoints** timed
(S21/S3) and their **links, meta, headers and empty-state strings** checked,
but not a per-widget browser-timeline capture.

The remaining static/content routes (`/docs`, `/pricing`, `/methodology`,
`/research/*`, `/blog`, `/changelog`, `/careers`, `/company`, `/contact`,
`/sdk`, `/widgets`, `/dev/*`) carry no data widgets; they were covered by
the link, meta, payload and compression sweeps.

---

## S38 — CRITICAL (re-scoped): `/v1/sources` gates SIX page types

Walking every data-bearing route with instrumentation attached shows the
`/v1/sources` slowness (S3) is far more central than first measured:

| page | slowest call | endpoint |
|---|---|---|
| `/network` | **8,070 ms** | `/v1/sources` |
| `/lending` | **8,085 ms** | `/v1/sources` |
| `/sources` | **8,105 ms** | `/v1/sources` |
| `/bridges` | **4,452 ms** | `/v1/sources` |
| `/oracles` | **3,217 ms** | `/v1/sources` |
| `/status` | contributes | (46-call page, S32) |

**One endpoint is the slowest call on five separate page types and
contributes to a sixth.** Every other endpoint on the site gates at most
one page.

This substantially raises its priority: `?include=stats` was already the
highest-leverage single fix at two page types; it is actually **six**.

## S39 — HIGH: two pages never finish loading

`/dexes` and `/aggregators` still show a loading indicator **5 seconds
after** their data has arrived:

| page | API calls | slowest | rows rendered | still showing "loading" |
|---|---|---|---|---|
| `/dexes` | 2 | 52 ms | 101 | **yes** |
| `/aggregators` | 1 | 46 ms | 3 | **yes** |

Both are *fast* — 52 ms and 46 ms — and both render their rows. Yet a
loading state persists alongside the populated table. This is not slowness;
it is a **stuck spinner on a page whose data already arrived**, which reads
to a user as "still working" on a page that has finished. Distinct from
S3's genuinely-slow endpoints and worth separating from them.

## S40 — MEDIUM: two pages render no table at all

| page | API calls | rows | `<th>` | empty-state message |
|---|---|---|---|---|
| `/bridges` | 2 | **0** | **0** | none shown |
| `/mev` | 1 | **0** | **0** | none shown |

Neither returns an error and neither displays an empty-state message —
they simply render no table. `/mev`'s call to `/v1/mev` completes in 41 ms
with no failure, so this is "no data, silently" rather than a fetch
problem. Whether these surfaces are genuinely empty or quietly broken needs
a product answer; either way a user gets an unexplained blank.

## S41 — LOW: `/sdex` and `/amm` are unreachable from the nav

The sidebar shows **SDEX Markets** and **AMM Pools**, and routes
`/sdex/` and `/amm/` both exist and return 200. But no nav anchor
resolves to either path — an automated walk of every `<a href>` on the
page could not reach them, failing with `no nav link`. The nav entries
point somewhere else, so the two routes are orphaned from the primary
navigation.

---

## Final per-widget timing coverage — 19 data-bearing routes

`/network` · `/status` · `/transactions` · `/contracts` · `/accounts` ·
`/assets` · `/ledgers` · `/operations` · `/issuers` · `/oracles` ·
`/dexes` · `/lending` · `/bridges` · `/aggregators` · `/mev` ·
`/divergences` · `/markets` · `/liquidity-pools` · `/sources`

Sorting: **absent on all nine listings measured** except `/markets` (S36).
Empty/stuck states: `/dexes`, `/aggregators` (S39), `/bridges`, `/mev`
(S40). Everything else rendered its data correctly.

Not walked: `/sdex`, `/amm` (S41 — no nav path), and the static content
routes, which carry no data widgets.
