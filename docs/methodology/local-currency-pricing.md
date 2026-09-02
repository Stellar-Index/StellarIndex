---
title: Local-currency pricing
last_verified: 2026-09-02
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
**GBP**. Binance and Kraken quote XLM directly in EUR and GBP, and
USD coverage is broad across every venue.

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
- The **FX rate** is a **daily** fix. It can be up to ~24 hours old.

`observed_at` on the response is the USD leg's timestamp — the market
observation the price derives from. It is not a claim that the FX
rate was refreshed at that instant.

For displaying a balance in someone's local currency — the thing
wallets do — a daily FX fix is normal and is what you would get
anywhere else. **It is not a settlement rate.** Do not use it to
price a conversion you are about to execute, or to mark a book you
have to defend. For those, use an execution venue's own quote.

## What you will not get

- **A price we are withholding.** If a price is withheld — a
  scam-flagged issuer, a failed decimals check — it stays withheld in
  every currency. The conversion is not a way around it. You will get
  `errors/price-withheld`, which tells you the data exists on the raw
  surfaces (`/v1/observations`, `/v1/ohlc`, `/v1/history`) even though
  we decline to publish an aggregate.
- **An invented rate.** A currency with no FX rate returns a plain
  404 rather than a guess.
- **A price for an asset we cannot value in USD.** The USD leg is the
  anchor; without it there is nothing to convert.

## Currency coverage

133 currency codes are accepted (the ADR-0010 allow-list); roughly
130 carry live FX rates. A code that parses but has no rate returns
404 rather than a fabricated number.

## Design record

The decision, the alternatives, and why USD is the anchor:
[ADR-0051](../adr/0051-usd-anchored-fiat-derivation.md).
