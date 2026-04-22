# Existing CTX Rates implementation — reference audit

**Status:** 📚 **Reference only.** We study this for proven patterns and
verified provider integrations. **We do NOT inherit this codebase.**
Per user directive 2026-04-22: "we're not evolving from that production
baseline, it's old code and has a lot of problems. We will learn from
it but we won't inherit its technical debt."

**Source:** `~/code/rates/` on local machine.
**Verified against:** `main.go`, `rate_calculator.go`, `rate_source.go`,
`pair.go`, `http.go`, `docs/README.md`, and the full
`rate_source_*.go` family.

## What it is

The live production service serving "CTX Rates" today. Originally built
by Dash Retail as a Dash-oriented rates aggregator; now serves broader
crypto + fiat pairs including **XLM from CEX / reference sources**.

**Track record** (per user + proposal): >99.99% uptime for 5+ years,
millions of queries. No history yet, real-time only.

## Architecture at a glance

```
┌──────────────────────┐   channel of []Rate
│  20 rate_source_*.go │ ────────────────────► ┌───────────────────┐
│  (WS/REST per vendor)│                       │  RateCalculator   │
└──────────────────────┘                       │  - rates[]        │
        │                                      │  - averagedRates[]│
        │ hourly reconnect                     │  - compositeRates[]│
        ▼                                      │  (in-memory, mu)  │
┌──────────────────────┐                       └─────────┬─────────┘
│ connectToRateSource  │                                 │
│ per-source goroutine │                                 │ snapshot on read
└──────────────────────┘                                 ▼
                                                ┌────────────────┐
                                                │ Echo HTTP API  │
                                                │ /rates /sources│
                                                │ /symbols /health│
                                                └────────────────┘
```

All state is in-process memory. No Redis. No Postgres. No TimescaleDB.
No persistent store of any kind. A restart drops every rate.

## Verified rate sources (source files present)

20 rate source files in the repo. Only 16 are registered in
`rate_source.go:114-135`:

Active in the registry:

```
binance       bitfinex       bitstamp       coingecko
coinmarketcap coinmonitor    cryptocompare  dailyfx
dolartoday    exchangeratesapi               hitbtc
huobi         kraken         localbitcoins  poloniex
tradingview
```

Commented out (present in source, not registered):

```
// bitcoinAverage
// forexcryptostock
// oneforge
```

**Unregistered but still wired via SetApiKey in `main.go:191-205`**:
`oneforge` and `forexcryptostock` — so their API keys are loaded even
though the source isn't in the active registry. Dead config path.

This matches the proposal's "16 independent sources" — the commented-
out entries are the reason.

### Providers likely broken today (audit target, not verified)

Based on their liveness status in the real world:

- **bitcoinAverage** — service shut down ~2021. Already commented out.
- **localbitcoins** — **service shut down entirely Feb 2023**. Still
  registered. This will be emitting errors every reconnect cycle.
- **huobi** — rebranded as HTX; their API endpoints changed.
- **poloniex** — still alive but has changed API keys / endpoints
  multiple times.
- **dolartoday** — scrapes dolartoday.com; fragile vs. site changes.
- **dashcasa** — niche Brazilian exchange; status unknown.
- **coinmonitor** — Argentine BTC aggregator; status unknown.
- **tradingview** — if it's scraping TradingView values, fragile.

**For our audit**: each of these is worth a live liveness check. The
user's note "some providers may be broken" likely means this set.

### XLM coverage in the current system (verified)

Contrary to my earlier assumption that the system had no Stellar
pairs, it **does** cover XLM via CEX connectors:

- **Binance** (`rate_source_binance.go:63-67`): XLMUSDT, XLMEUR,
  XLMBTC, XLMETH.
- **Bitstamp** (`rate_source_bitstamp.go:127-130`): XLM/BTC, XLM/USD,
  XLM/EUR, XLM/GBP.
- **Kraken** (`rate_source_kraken.go:95-…`): XLM/AUD, XLM/CAD, XLM/CHF,
  XLM/EUR, XLM/GBP (and likely more).
- **CoinGecko** (`rate_source_coingecko.go:73,160`): XLM aliased to
  "Lumens"; ingested in the `[DASH, XLM, USDC]` fetch set.

**What it does NOT cover (the gaps the new system must fill):**

- Any non-XLM Stellar-native asset (classic issued assets, SEP-41
  tokens).
- Any **on-chain Stellar DEX/AMM** trade data (SDEX, Soroswap,
  Aquarius, Phoenix, Comet, Blend).
- Any **on-chain oracle** feed (Reflector, Redstone on Stellar, Band
  Soroban, DIA).
- Any **historical data** — current system is real-time only; no
  TWAP/VWAP windows, no OHLC, no "since inception."
- Any **persistence** — process restart = total state loss.

## Aggregation logic (pattern reference)

`rate_calculator.go:296-351` — the algorithm we can reuse
conceptually:

1. **`CalculateAverageRate(rates)`** — weighted arithmetic mean with
   outlier rejection. Each rate's `Weight` is used; defaults to 100.
2. **`OmitOutlierRates(rates, permitDivergentPercent=40)`** — compute
   baseline (simple mean), drop anything > 40% divergent. **Only
   applied if len(rates) > 3**. Below that, all rates kept.
3. **USDC/USD peg snap** (`rate_calculator.go:171-176`) — if averaged
   USDC/USD ∈ (0.999, 1.001), hard-snap to 1.0. Inline special case.
4. **Composite rates** (`rate_calculator.go:182-287`) — triangulate
   through **BTC or USD** only. Pre-declared composite pair list.
5. **"Ignored source" rule** (`rate_calculator.go:220-243`) — if a
   composite pair has a direct rate from source X, recalculate any
   averaged intermediary that included X so X isn't double-counted.
6. **Expiry** — 2-hour sliding window
   (`defaultExpiryTimeout = time.Hour * 2`). Rates older than 2 hours
   dropped on every input cycle.

### What we steal from this

- **40% divergence outlier rejection** — reasonable default, we adopt
  with tunable parameter.
- **USDC-peg snap** — defensible for UI/UX on dollar-pegged
  stablecoins, but **we should make this configurable per-asset, not
  inline** (DAI, USDT, PYUSD, etc should all have similar handling).
- **Composite triangulation with source-deduplication** — the rule
  about not reusing a direct source in intermediary rates is subtle
  and correct. Adopt.
- **Weighted average** — we'll keep weighted averaging but weight by
  **USD-denominated volume** (per our proposal), not a constant `Weight`.

### What we don't steal

- **In-memory only** — replaced with TimescaleDB + Redis cache.
- **Single 2-hour expiry** — replaced with per-window aggregation
  (VWAP/TWAP over rolling windows).
- **No outlier rejection below 4 sources** — we always apply
  deviation bounds because we need to be safe under low-liquidity
  conditions.
- **Hardcoded `BTC, USD` intermediaries** — we generalize to a
  pluggable set (likely includes `XLM`, `USDC`) with liquidity-
  ranked path selection per the proposal.
- **Floating-point (`float64`) prices everywhere** — **directly
  violates** our [i128 invariant](decisions.md). New system uses
  `*big.Int` / `decimal.Decimal` with `NUMERIC` columns, strings on
  the wire.
- **Single global calculator with mutex** — fine for in-memory at
  small volume; won't scale to 1000 req/min with windowed
  aggregation. Replaced by stateless serving + per-pair state in
  Timescale.
- **Goroutine-per-source + channel model** — the basic pattern is
  fine, but we want supervisord-style health monitoring + Prometheus
  metrics per source, not just Discord webhook errors.

## HTTP API surface (pattern reference)

`http.go` — Echo-based, 4 endpoints:

```
GET /health     — liveness
GET /sources    — enumerate configured rate sources
GET /rates      — flat list of all rates (raw, averaged, composite)
GET /symbols    — distinct pairs
```

Middleware:

- `middleware.Recover()` — crash-safe.
- `middleware.CORS()` — wide-open.
- `middleware.GzipWithConfig(Level: 5)`.
- HTTPErrorHandler redirects 404 → `/`.
- Rate-limit middleware is **imported and wired via `rateLimiter(r)`
  but the line is commented out** (`http.go:53`). So the custom
  `visitors` rate-limit map is dead code.

Server timeouts:

```
ReadTimeout:  5 s
WriteTimeout: 10 s
IdleTimeout:  120 s
```

Defaults to port 3000.

### What we keep conceptually

- Simple Echo-based HTTP surface. We'll use the same or chi/fiber.
- `/health` / `/sources` / `/rates` / `/symbols` are all endpoints
  our RFP expects analogues of. The shape is a good baseline.

### What we replace

- Slate-based docs — replace with OpenAPI/Swagger + Redoc (tool
  choices vary; point is a spec-driven reference, not a manually
  curated site).
- No auth / no rate-limit active — both are hard RFP requirements
  (1000 req/min per client, API keys). The commented-out visitor
  limiter is an ancient draft; we start over.
- No SSE streaming — RFP asks for it.
- No versioning (`/v1`) — RFP asks for it.

## Pair identification (verified, NOT Stellar-compatible)

`pair.go` supports three string formats:

```
"DASH/BTC"    → split on /
"DSHXBT"      → 6 chars → 3+3 split
"DASHBTC"     → 7 chars → 4+3 split (DASH is 4-char; assumed DASH as base)
```

**Anything longer is rejected.** This parser cannot represent
Stellar's `USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN`
classic asset pair identifier, nor Soroban contract addresses
(`C…56char`). Current system can't even express the full Stellar asset
space.

**Implication for the new system:** our internal canonical asset
identifier must be richer — at minimum a struct with
`{type, code, issuer, contract}`. API accepts the SEP-1 /
Stellar-canonical string forms. Only ever serialises out to strings
once known-valid.

## Currency model (verified, big but flat)

`currency.go` is 25 KB of:

- Constants like `var XLM = Currency("XLM")` for every supported
  currency (~180 total).
- A metadata map with `Name`, `Symbol`, `Unit`, `IsCrypto`, etc.
  `XLM` has `Name: "Stellar"` (`currency.go:1377-1379`).
- `type Currency string` — a newtype over string. No issuer, no
  contract, no asset-type distinction.

Works for 3-4 character fiat/crypto tickers; **does not scale to the
Stellar asset namespace** where two tokens can share a code
(`USDC:issuerA` vs `USDC:issuerB`) or have both a classic and
Soroban-contract representation.

We replace this with a richer type. The **list of supported currency
tickers** here is useful as a starting seed for fiat coverage (~160
fiat codes). We can lift it.

## Sources of API-key configuration

`main.go:55-85` — env vars the current system reads:

```
DR_PORT
DR_DEV_CACHE                   — dev-only JSON file cache
DR_DISCORD_WEBHOOK_URL         — ops alerting
DR_ONEFORGE_API_KEY            — OneForge (FX)
DR_COINMARKETCAP_API_KEY       — CoinMarketCap
DR_FOREXCRYPTOSTOCK_API_KEY    — ForexCryptoStock (FX/stocks)
DR_CRYPTOCOMPARE_API_KEY       — CryptoCompare
DR_EXCHANGERATESAPI_ACCESS_KEY — ExchangeRatesApi (fiat FX)
```

Note **DR\_** prefix — legacy from "Dash Retail." Our new system uses
a clean prefix (e.g. `CTX_`).

## Observability

- Logs to stdout in JSON (`logrus`).
- `main.go:93-114` hooks into `discordrus` to forward `log.InfoLevel`+
  to a Discord webhook. This is the full ops alerting pipeline — no
  PagerDuty, no Grafana, no Prometheus.
- No metrics endpoint, no health beyond `/health`, no structured
  error tracking (no Sentry).

New system needs: Prometheus metrics per source + per pair + latency
histograms; Grafana dashboards; pager-grade alerting for SEV-1 /
SEV-2; structured error tracking.

## Build / deploy (current)

- `Dockerfile` — Docker image.
- `.drone.yml` + `buildspec.yml` — CI pipelines.
- `docs/` — Slate-based static API docs, served from `/` via Echo.
- No k8s manifests, no terraform, no SRE tooling.

## Non-patterns to avoid

1. **Global variables** — `version`, `globalHttpClient`, `started`,
   `rateSources`. Testability and isolation suffer.
2. **`cli` v1** — replaced by `urfave/cli/v3` or `cobra` in modern
   Go code.
3. **`ioutil`** — deprecated since Go 1.16. New code uses `os` / `io`.
4. **Commented-out code** as config (`// bitcoinAverage:` in the
   source registry) — should be a feature flag, not a comment.
5. **Per-source float64 prices** — the core bug that makes this
   codebase unfit for i128-denominated Soroban assets.

## Provider integration code as documentation

Each `rate_source_*.go` file contains concrete knowledge of:

- The vendor's actual WebSocket / REST URL.
- Their pair format convention (`XLMUSDT` vs `xlm/usd` vs `XLM-USD`).
- Auth header convention when paid.
- Reconnect / keepalive quirks.
- Response JSON schema.

For each of the 16 providers we plan to re-use, **read the existing
connector as vendor-spec documentation** when we write the new Go
implementation. This saves us re-doing vendor discovery from scratch.

## Open items

- [ ] Liveness check each of the 16 registered sources. Label which
      still work, which 4xx, which 5xx, which dead. Produce a table.
- [ ] Extract the 160-ish fiat currency seed list into our new
      `external-refs/fx-feeds.md` or equivalent.
- [ ] Document the XLM-pair seed list (Binance / Bitstamp / Kraken /
      CoinGecko pairs actively fetched today) as Day-1 coverage our
      new system must match.
- [ ] Decide how we keep continuity for existing consumers of the
      current CTX Rates API during cutover. If they rely on the
      `/rates` shape we need a bridge.
- [ ] Audit the proposal's "CTX Rates serves millions of queries"
      claim with traffic numbers — how much headroom does our new
      infra need?

## Related

- [decisions.md](decisions.md) — i128 invariant this codebase violates.
- [adversarial-audit.md](adversarial-audit.md) — our gap analysis
  that flagged CEX audit as still TBD.
- [data-sources/withobsrvr-cdp-pipeline-workflow.md](data-sources/withobsrvr-cdp-pipeline-workflow.md)
  — another codebase we learn from but don't inherit.
