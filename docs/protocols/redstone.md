# RedStone — contract & event verification

> **For the RedStone team:** this is the Adapter contract and the 19-feed
> registry Stellar Index ingests. Please confirm the Adapter address and
> tell us if new feeds have been added since our 2026-05-22 capture (a
> feed we don't have in the registry is skipped, not mis-attributed —
> see Q3).
>
> - **Enumeration method:** single Adapter contract (pinned by ID) + an
>   in-code registry of the 19 mainnet `feed_id` strings, each mapped to a
>   canonical `(base, quote)` pair.
> - **Last verified:** 2026-07-06 (source: `internal/sources/redstone`;
>   feed_ids captured on-chain 2026-05-22; WASM audit
>   `docs/operations/wasm-audits/redstone.md`, 2026-04-29).
> - **Gate status:** ✅ Gated (ADR-0035): the decoder matches ONLY the
>   configured Adapter contract ID.

## What RedStone is

[RedStone](https://app.redstone.finance) is a multi-feed oracle. On
Stellar, **one Soroban Adapter contract owns price storage for every
feed**; 19 thin per-feed proxy contracts delegate reads to the Adapter
but emit no events (they only serve `price()` reads, so we do not
subscribe to them).

| Role | Mainnet address |
|---|---|
| Adapter (the only subscribed contract) | `CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG` |

## Event decoded — one batch event, N feed updates

RedStone emits a single `("REDSTONE",)` event each time the relayer
pushes a batch update. Decoding one event produces **one
`canonical.OracleUpdate` per `(feed_id, price)` pair** in the batch
(synthetic `op_index` spaced by 1024 so each feed keeps a distinct
identity in `oracle_updates`).

| Field | Where it appears | Decoded as |
|---|---|---|
| `updater` | body Map | relayer `Address` (kept for audit, ignored for VWAP) |
| `updated_feeds` | body Map → `Vec<PriceData>` | one row per feed updated this batch |
| `price` (per feed) | `PriceData.price` | `U256` at fixed `DECIMALS = 8` |
| `package_timestamp` / `write_timestamp` | `PriceData` | `u64` Unix **milliseconds** |
| **`feed_ids`** | **InvokeContract op args** (NOT the event body) | `Vec<String>` — see Q1 |

### Q1 — `feed_ids` are not in the event body

The relayer calls `adapter.write_prices(updater, feed_ids, payload)`;
the emitted event carries prices + timestamps but **no feed identifiers**.
The decoder reads `feed_ids` from `events.Event.OpArgs` (populated by
`internal/dispatcher` from the InvokeContract op envelope) and zips
one-to-one against `updated_feeds`. **Lengths must match** — when the
Adapter's freshness verifier rejects a feed, the entry drops from
`updated_feeds` without dropping from `feed_ids`, breaking the zip. The
decoder treats a mismatch as `ErrFeedIDCountMismatch` and **skips the
whole event** rather than attributing prices to the wrong assets (counted
on `stellarindex_source_decode_errors_total{source="redstone"}`).

### Q2 — Event body is wrapped in `ScVal::Bytes`

The Rust Adapter does `self.to_xdr(env).to_val()`, yielding an
`ScVal::Bytes` holding XDR-serialised body bytes — not the `ScVal::Map`
you'd expect. The decoder type-tests and unwraps the inner XDR before
destructuring.

## Feed registry (ADR-0028)

`feeds.go` holds all 19 mainnet feeds keyed on the **exact** on-chain
`feed_id()` string (captured 2026-05-22). The feed_id is not always the
display name — `EUROC` is `EUROC/EUR`, `BENJI` is
`BENJI_ETHEREUM_FUNDAMENTAL`, SolvBTC variants carry `_FUNDAMENTAL`
suffixes. Two correctness consequences:

- **Quote asset is per-feed.** RedStone publishes USD-denominated prices
  unless the feed_id carries an explicit `/<QUOTE>` suffix; only
  `EUROC/EUR` is EUR-quoted today. The registry carries the quote per
  feed. (Pre-registry the decoder hardcoded USD, mislabelling EUROC.)
- **RWA feeds** (BENJI, GILTS, TESOURO, CETES, KTB, USTRY, SPXU, iBENJI)
  decode as `canonical.AssetRWA`, deliberately NOT `crypto`, so a
  tokenized T-bill never lands in a crypto-scoped surface.
- **A feed_id outside the registry** (a future 20th feed) is skipped
  per-entry and counted on `redstone_unknown_symbols_total` — skipped,
  never mis-attributed.

## Aggregator treatment — reported, not counted

Class `Oracle` / `IncludeInVWAP=false` (`external.Registry`). Surfaced on
`/v1/sources` for transparency, excluded from VWAP (RedStone publishes
already-aggregated derived prices under its own methodology). RWA feeds
are additionally never eligible for market VWAP.

## Backfill safety

`BackfillSafe = true` (audited 2026-04-29). The Adapter's events are
durable Soroban events; backfill decodes identically to live, subject to
`events.Event.OpArgs` availability (populated for backfill ledgers via
`internal/dispatcher`).

## Update cadence / staleness

A feed may go quiet up to 24h if the underlying price hasn't moved > 0.2%.
The decoder publishes `DefaultResolutionSeconds = 86400` so the
`oracle-stale` alert (fires at > 10× resolution) uses the correct
threshold for a legitimately quiet feed.

## References

- Source package: `internal/sources/redstone/README.md`
- ADR-0028 (RWA asset modelling); ADR-0014 (crypto-ticker representation)
- Sibling oracles: [reflector.md](reflector.md), [band.md](band.md)
