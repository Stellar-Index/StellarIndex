# Forex feeds

**Status:** 🧪 Planning — drawn from the existing CTX Rates FX
connectors + RFP requirements.

**Related:** [../existing-ctx-rates.md](../existing-ctx-rates.md).

## RFP context

Proposal, §"Forex Providers":

> "Foreign exchange data is sourced from institutional-grade FX
> feeds and used to anchor fiat-denominated pricing. This ensures
> accurate USD conversion and supports synthetic pricing paths such
> as XLM to EUR or XLM to GBP, even AQUA to BRL can be supported."

Role: anchor USD (and other major fiat) rates used as the
intermediaries in our triangulation paths. FX accuracy determines
how correct `XLM/JPY` is when we compute it as
`XLM/USD × USD/JPY`.

## FX connectors in the existing codebase

| Provider | REST | Auth | Coverage | Liveness |
| -------- | ---- | ---- | -------- | -------- |
| **ExchangeRatesApi** | `https://api.exchangeratesapi.io/v1/latest?base=<X>&access_key=<K>` | API key | Major fiat pairs | ✅ Alive (fiat.sh / ECB-backed). |
| **OneForge** | `https://api.1forge.com` (via API key) | API key | 700+ FX + commodities | Commented out of registry; used to be paid tier. Verify current status. |
| **ForexCryptoStock** | (URL not in top lines of file, check source) | API key | FX + crypto + stocks | Commented out; paid tier. Verify. |
| **DailyFX** | Scrape-ish | n/a | Major FX pairs | `rate_source_dailyfx.go` — fragile, scrapes the DailyFX site. |
| **DolarToday** | Scrape of dolartoday.com | n/a | VES (Venezuelan Bolivar) only | `rate_source_dolartoday.go` — fragile, but only source for VES. Special-cased in the existing system (`main.go:209-214`: all other sources explicitly `IgnoreCurrencies(VES)`). |
| **DashCasa** | Scrape | n/a | BRL (Brazilian Real) specific | Niche. Verify still operating. |
| **CoinMonitor** | Scrape | n/a | ARS (Argentine Peso) specific | Niche. Verify. |

### Per-region special-cases (existing system)

The existing system handles hyperinflation / crypto-restricted
fiat zones as dedicated connectors rather than relying on
institutional FX. Real operational lesson: the "institutional" FX
providers won't price VES or ARS reliably during hyperinflation
periods — local scrape sources win. We preserve this pattern but
acknowledge the code is fragile.

## FX provider categories worth evaluating for the new system

### Institutional / ECB-backed

- **ECB daily reference rates** (free, CSV) — for major G10 pairs.
  Official, daily granularity.
- **exchangeratesapi.io / fixer.io** — tiered. Free tier gives EUR
  base only; paid tier gives USD/any base. 1-minute updates on paid
  tiers.
- **Polygon.io Forex** — professional tier; tick-level FX data.
- **Xignite** — institutional, expensive.
- **OANDA FX** — via REST or FIX; professional-grade.

### Crypto-friendly aggregators

- **CurrencyLayer / currencylayer.com** — same org as fixer.
- **Open Exchange Rates** (`openexchangerates.org`) — simple REST,
  dev-friendly tier.

### Free / unreliable

- **Free APIs** (e.g. `frankfurter.app` — ECB-only, free).
  Acceptable as a redundancy layer, not as primary.

## Role in our aggregation

Per the proposal, FX rates are a **pricing anchor**, not a VWAP
contributor. They're the `USD/EUR` multiplier when we derive an
`XLM/EUR` price from `XLM/USD` on Binance + `USD/EUR` from ECB.

Specifically:

1. FX rate has **lower update cadence** (minutes for free tiers,
   seconds for pro) than crypto spot markets. So the staleness
   budget on the FX leg usually dominates.
2. FX rate must be **multi-source median** — a single provider's
   numerical glitch propagates into every derived crypto/fiat
   pair. Minimum 2 providers; prefer 3.
3. **Weekends** — FX markets close. Many providers freeze rates
   over the weekend. Our pricing layer must either freeze the
   derived pair price explicitly (with a `fx_market_closed: true`
   flag) or extrapolate (risky).

## Assets the RFP implies we must price through FX

- **EUR / GBP / JPY / CHF / CAD / AUD** — G10 majors. Any
  Stellar-listed asset paired against these via FX triangulation.
- **BRL (AQUA/BRL example)** — emerging market. DashCasa or a
  local Brazilian provider.
- **KRW / TWD / IDR / SGD / HKD** — Asia-Pacific if we target
  that customer base.
- **VES / ARS** — hyperinflation exceptions, dedicated scrape
  sources (as the existing system does).

## Licensing and redistribution

**Important caveat not in the proposal**: many FX providers have
strict redistribution clauses in their ToS. If we use ECB rates
(public domain, OK) or an API that explicitly permits rehosting
(exchangeratesapi.io's paid tiers do), we're fine. Institutional
providers (Bloomberg, Refinitiv, ICAP) prohibit redistribution —
we can consume them internally but cannot expose them to our API
customers.

**Action**: before selecting a provider, verify their
redistribution terms and confirm we can publish their values
(even transformed) to our public API.

## Open items (Phase 3)

- [ ] Pick the **primary FX provider** — likely exchangeratesapi
      paid tier ($20-50/mo) or Polygon.io Forex (pricier but
      professional-grade).
- [ ] Pick **≥2 redundant providers** for medianisation. ECB for
      majors, a secondary paid API.
- [ ] Decide handling for: weekend/holiday FX market close,
      hyperinflation assets (VES/ARS/…), commodity-pegged (XAU,
      XAG).
- [ ] Confirm redistribution rights for every provider selected.
- [ ] Decide whether the existing local-scrape patterns
      (DolarToday, DashCasa, CoinMonitor) are worth porting to the
      new system or if we source these differently.

## Related

- [../existing-ctx-rates.md](../existing-ctx-rates.md) — vendor-
  spec code for the current FX connectors.
- [cex-feeds.md](cex-feeds.md) — crypto spot price anchors.
- [coingecko.md](coingecko.md) — third-party aggregator with FX
  coverage as a redundancy layer.
