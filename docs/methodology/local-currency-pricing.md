---
title: Local-currency pricing
last_verified: 2026-09-28
status: current
---

# Local-currency pricing

How Stellar Index produces a price in your currency, and what that
number does and does not mean.

## The short version

For most currencies there is no market to observe. Nobody runs an
XLM/BRL order book of any size. So we compose two things we *do*
observe:

```
price(asset, BRL) = price(asset, USD) × rate_usd[BRL]
```

The USD price comes from our own aggregation across the venues we
ingest. The USD→BRL rate comes from our foreign-exchange feed. The
result is a derived value, and we label it as one.

## When you get an observed price instead

Three currencies have real markets in our data: **USD**, **EUR** and
**GBP** — Kraken quotes XLM directly in all three. USD and EUR also
trade on Binance, Bitstamp and Coinbase; GBP trades on Bitstamp too
(Binance has no XLM/GBP product). Every other currency, AUD, CAD and
CHF included, is derived.

Where a market exists, **you get the market** — the derivation never
overrides an observed print. So `XLM/EUR` is a real volume-weighted
average of real trades, not `XLM/USD × USD→EUR`. Only currencies
with no market at all are derived.

You can tell which you received:

```json
"flags": { "triangulated": true }
```

`triangulated: true` means the number was composed rather than
observed. `false` means it came from trades.

## Provenance

A derived price credits both legs in `sources`:

```json
"sources": ["binance", "massive", "sdex"]
```

`massive` is the FX feed; the rest are the venues that set the USD
price. If you are reconciling a number, those are the inputs.

## Freshness, and what this is not for

The two legs have different clocks, and this matters:

- The **USD leg** is a closed-bucket aggregate, typically seconds old.
- The **FX rate** on `/v1/price` is the vendor's **hourly** close
  (daily before a currency's hourly series begins) bound to the USD
  bucket's end: the newest bar that closed at least 3 hours before it.
  The same bucket therefore converts at the same rate whenever and
  wherever you ask. It pauses over market closes and can be up to ~76
  hours old (Friday's close is the freshest thing that exists until
  Monday). Older than that and the price is withheld
  (`errors/price-withheld`, "FX leg unavailable") rather than composed
  from a stale or a live rate (`pricing_guard.fx_cross_max_age_hours`).
  `/v1/price/tip` converts at the live rate instead.

`observed_at` on the response is the USD leg's timestamp — the market
observation the price derives from. The FX side is on the row itself:

```json
"fx_rate": "5.1837", "fx_as_of": "2026-09-30T09:00:00Z",
"fx_source": "massive", "fx_resolution": "hourly",
"usd_leg": { "price": "0.2", "observed_at": "2026-09-30T12:01:00Z", "sources": ["sdex"] }
```

`usd_leg.price × fx_rate` is `price`. `flags.stale` is set when the USD
leg is stale, or when an hourly FX close trails its bind point by more
than 4 hours outside the weekend close.

For displaying a balance in someone's local currency — the thing
wallets do — a daily FX fix is normal and is what you would get
anywhere else. **It is not a settlement rate.** Do not use it to
price a conversion you are about to execute, or to mark a book you
have to defend. For those, use an execution venue's own quote.

## What you will not get

- **A price we are withholding.** If a price is withheld — a
  scam-flagged issuer, a failed decimals check — it stays withheld in
  every currency. The conversion is not a way around it. You will get
  `errors/price-withheld`, whose title and detail name the gate that
  fired. Only a too-thin market points you at the raw surfaces
  (`/v1/observations`, `/v1/ohlc`, `/v1/history`) to judge it
  yourself; a flagged issuer's trades are not a price signal.
- **An invented rate.** A currency with no FX close within the
  lookback of the bucket gets a 404 (`errors/price-withheld`, "FX leg
  unavailable") rather than a guess or today's live rate.
- **A price for an asset we cannot value in USD.** The USD leg is the
  anchor; without it there is nothing to convert.

## The XLM cross

This XLM cross is not limited to the series surfaces. When a declared
USD-pegged classic asset has no USD-quoted market under any spelling,
`/v1/price` derives it through XLM too — inside the same USD-leg
resolution a non-USD price for that asset depends on — so a BRL price
for such an asset can carry an XLM leg the formula above doesn't show.
It is flagged `triangulated` either way.

The series surfaces (`/v1/chart`, `/v1/history/since-inception`) carry
the same exception: when no USD-quoted market exists under any
spelling or declared-peg proxy, a series is derived through XLM — the
asset's XLM series multiplied bucket by bucket by XLM's series in the
requested fiat. The USD peg's own USD series can only come from
there.

## Currency coverage

133 currency codes are accepted (the ADR-0010 allow-list); roughly
109 carry live FX rates. A code that parses but has no rate returns
404 rather than a fabricated number.

## Design record

The decision, the alternatives, and why USD is the anchor:
[ADR-0051](../adr/0051-usd-anchored-fiat-derivation.md).
