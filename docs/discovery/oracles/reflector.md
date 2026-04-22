# Reflector Network (Stellar-native oracle)

**Status:** ✅ Primary oracle for on-chain price validation + fallback.
**Important corrections to our proposal** — documented below.

**Spec:** SEP-40 (<https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0040.md>)
**Contracts repo:** <https://github.com/reflector-network/reflector-contract>
**Audit (v3):** <https://code4rena.com/audits/2025-10-reflector-v3>
**Verified against:** `reflector-contract` source at clone time
(2026-04-22) — `pulse-contract/src/lib.rs`, `beam-contract/src/lib.rs`,
`oracle/src/price_oracle.rs`, `oracle/src/types.rs`,
`oracle/src/events.rs`; plus `stellar-protocol/ecosystem/sep-0040.md`.

## SEP-40 — what the standard actually defines

Verified directly from the SEP-40 spec:

```rust
pub trait PriceFeedTrait {
    fn base(env) -> Asset;
    fn assets(env) -> Vec<Asset>;
    fn decimals(env) -> u32;
    fn resolution(env) -> u32;
    fn price(env, asset: Asset, timestamp: u64) -> Option<PriceData>;
    fn prices(env, asset: Asset, records: u32) -> Option<Vec<PriceData>>;
    fn lastprice(env, asset: Asset) -> Option<PriceData>;
}

#[contracttype]
pub struct PriceData {
    price:     i128,
    timestamp: u64,
}

#[contracttype]
pub enum Asset {
    Stellar(Address),   // SAC-deployed asset contract
    Other(Symbol),      // off-chain reference (e.g. "BTC", "USD")
}
```

**SEP-40 does NOT define `twap`, `x_last_price`, `x_prices`, or `x_twap`.**
These methods are *not part of the standard*. Our RFP proposal lists them
as if they were SEP-40 methods; that text is wrong and needs correction
(see "Corrections to our proposal" below).

**Semantics SEP-40 locks down:**

- `price` is `i128`. Price value = `price / 10^decimals`. Per
  [decisions.md](../decisions.md) this lands in our i128-no-truncation
  invariant — `NUMERIC` columns, `*big.Int` in Go, string in JSON.
- `decimals()` and `resolution()` are **immutable once deployed**. So
  once we learn them for a given oracle, we can cache them forever.
- `resolution()` is in seconds.
- Missing asset / missing timestamp → `Option::None`. **Never throw**;
  the consumer is supposed to test `is_some()`.
- `price(asset, ts)` — timestamps are normalised to
  `floor(unix_now() / resolution) * resolution`. Don't expect arbitrary
  granularity.

## Reflector v3 contract — the actual on-chain surface

Reflector ships two flavours, both SEP-40-compliant at the price-read
level:

### Pulse (free, public)

`pulse-contract/src/lib.rs` — **view methods are free**; anyone can call.

```
base(env) -> Asset
decimals(env) -> u32
resolution(env) -> u32
history_retention_period(env) -> Option<u64>   // seconds
cache_size(env) -> u32
assets(env) -> Vec<Asset>
last_timestamp(env) -> u64                     // seconds
version(env) -> u32
expires(env, asset) -> Option<u64>             // per-asset expiry (TTL)
estimate_retention_cost(env, period) -> (Address, i128)
extend_asset_ttl(env, sponsor, asset, amount) -> u64   // requires sponsor auth
fee_config(env) -> FeeConfig
admin(env) -> Option<Address>
price(env, asset, timestamp) -> Option<PriceData>
lastprice(env, asset) -> Option<PriceData>
prices(env, asset, records) -> Option<Vec<PriceData>>
```

Plus admin methods (`config`, `add_assets`, `set_cache_size`,
`set_history_retention_period`, `set_fee_config`, `set_price`,
`update_contract`) which we'll never call.

### Beam (paid-per-query, faster updates)

`beam-contract/src/lib.rs` — every `price`/`lastprice`/`prices` call
takes an additional `caller: Address` as its **first** argument,
requires `caller.require_auth()`, and **charges an invocation fee** in
the configured fee token (XRF on mainnet).

So Beam call shapes differ:

```
price   (env, caller: Address, asset, timestamp) -> Option<PriceData>
lastprice(env, caller: Address, asset)           -> Option<PriceData>
prices   (env, caller: Address, asset, records)  -> Option<Vec<PriceData>>
```

Plus one extra view method:

```
estimate_cost(env, periods) -> i128
invocation_costs(env) -> Vec<u64>
```

### Events — our push-based ingest path

`oracle/src/events.rs` — every `set_price` publishes a single
`UpdateEvent`:

```
#[contractevent(topics = ["REFLECTOR", "update"])]
pub struct UpdateEvent {
    #[topic] timestamp: u64,
    update_data: Vec<(Val, i128)>,   // (asset symbol, price) per updated asset
}
```

This means we can **subscribe via stellar-rpc `getEvents`** with filter
`topics = ["REFLECTOR", "update"]` and get every price update pushed to
us. No polling. Covered separately in the event-streaming section.

**Important:** zero-price updates are skipped in the event (they're
still stored in contract state but filtered out of the event payload,
`events.rs:24`). If we ever need the "this asset reported zero" signal
we must poll `lastprice` rather than rely on events.

## Deployed public oracles (mainnet + testnet)

From `stellar-docs/docs/data/oracles/oracle-providers.mdx`. Three
Reflector oracles, **each with its own data source and asset list**:

### Mainnet

| Contract | Data source |
| -------- | ----------- |
| `CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M` | Stellar Mainnet DEX (SDEX + Soroban AMMs) |
| `CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN` | External CEXs & DEXs |
| `CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC` | Fiat exchange rates |

### Testnet

| Contract | Data source |
| -------- | ----------- |
| `CAVLP5DH2GJPZMVO7IJY4CVOD5MWEFTJFVPD2YY2FQXOQHRGHK4D6HLP` | Stellar Mainnet DEX |
| `CCYOZJCOPG34LLQQ7N24YXBM7LL62R7ONMZ3G6WZAAYPB5OYKOMJRN63` | External CEXs & DEXs |
| `CCSSOHTBL3LEWUCBBEB5NJFC2OKFRC74OWEIJIZLRJBGAAU4VMU5NV4W` | Fiat exchange rates |

**Architectural implication:** There is no single "the Reflector
oracle". We integrate three. Each oracle has its own:

- `base()` (probably USD for CEX/DEX feeds; TBC for FX)
- `decimals()` (probably 14 based on Reflector public docs but verify)
- `assets()` list (different per feed — DEX feed covers Stellar-native
  assets, CEX feed covers major global pairs, FX feed covers fiat
  currencies)
- Update cadence (Pulse is 5 min; Beam is faster but costs XRF)

**Open items** (need live contract calls to fill in):

- [ ] What is each oracle's `base()` asset?
- [ ] What is each oracle's `decimals()`?
- [ ] What is the current `assets()` list on each mainnet oracle?
- [ ] What is the observed update cadence in production (should match
      the 5-min / Beam doc claims)?

## Pulse vs Beam — which we use where

| Aspect             | Pulse                                  | Beam                                   |
| ------------------ | -------------------------------------- | -------------------------------------- |
| Cost               | Free (no invocation fee)               | XRF token per invocation                |
| Update cadence     | 5 minutes (uniform)                    | Faster (paid tier)                      |
| Auth on read       | None                                   | `caller.require_auth()`                 |
| Public oracles     | All three public feeds are Pulse       | Custom / sponsored deployments          |
| Event subscription | Possible (same UpdateEvent topic)      | Possible (same UpdateEvent topic)       |

**Our plan:**

1. **Live ingest via events.** Subscribe to `REFLECTOR:update` via our
   self-hosted stellar-rpc for all three Pulse oracles. Decode the
   `Vec<(Val, i128)>` update data, map each asset → price → decimals →
   `NUMERIC`, write to Timescale.
2. **Periodic sanity read** via free Pulse view methods (`lastprice`,
   `assets`, `resolution`) for health checks and to detect any event-
   subscription drift.
3. **Do not call Beam** in hot path. If a customer needs sub-5-min
   Reflector prices we consider custom Beam deployment / sponsorship
   later — not Phase 1.

Because Pulse updates at 5-minute cadence and our RFP freshness target
is 30 seconds, Reflector alone cannot serve as our primary live-price
source. It is a **validation + fallback** feed, consistent with our
original proposal's stance.

## TWAP / cross-pair math is ours to do off-chain

Our proposal text:

> "Integration is via direct Soroban contract calls using the SEP-40
> interface: `lastprice(asset)` for current prices, `prices(asset, n)`
> for historical records, `twap(asset, n)` for time-weighted averages,
> and the cross-pair equivalents `x_last_price(base, quote)`, `x_prices`,
> and `x_twap`."

This is wrong on the last four methods. **Reflector v3 has no on-chain
`twap`, `x_last_price`, `x_prices`, or `x_twap`.** The ctx-proposal.md
needs correction.

Replacement plan:

- **TWAP:** fetch `prices(asset, n)` → n latest records at
  `resolution` cadence → compute TWAP in our service. Trivial.
- **Cross-pair (A/B):** fetch `lastprice(A)` and `lastprice(B)` from
  the same oracle (both normalised to the oracle's `base()`), then
  divide. Or chain through two oracles if A and B are on different
  feeds.

This is actually **better** for us: we control the TWAP window, the
weighting, and the consistency of cross-pair math, rather than
trusting an oracle function whose implementation we don't own.

## Data model implications

- `price` is `i128`, **not** `int64`. Our Go reader must use
  `big.Int` + sign-aware i128 conversion. See [decisions.md](../decisions.md).
- `decimals` is per-oracle — cache it on first read, but **always**
  apply it rather than hard-coding. The DEX oracle's decimals may
  differ from the FX oracle's.
- Timestamps are seconds (u64). Reflector stores ms internally and
  converts (`price_oracle.rs:32-34, 74`) — trust the exposed seconds.
- `Asset::Other(Symbol)` — symbol is a Soroban `Symbol` (max 32
  bytes, ASCII). Examples likely: `"BTC"`, `"ETH"`, `"USD"`, `"EUR"`.
  We need the actual `assets()` list to know the conventions.

## Failure modes

- **Missing asset on oracle** → `None` return. We log and skip, do not
  treat as error.
- **Stale last update** — `last_timestamp()` drift from `env.now()`
  tells us how fresh. If drift > 2× `resolution()`, flag the oracle
  as degraded in our status endpoint.
- **Asset expiry** — Reflector v3 has a per-asset TTL (`expires(asset)`)
  that requires fee-token burn to extend. A Reflector operator who
  stops paying lets the asset expire; subsequent reads return `None`.
  We monitor expiry and warn ops if a key asset's TTL runs low.
- **Oracle upgrade** — admin can `update_contract`. Contract storage
  survives. But if the WASM interface breaks, our client breaks. We
  pin to reading the specific methods above and alert on unexpected
  XDR shapes.

## Open items

- [ ] Live call each mainnet oracle to pull `base`, `decimals`,
      `resolution`, `history_retention_period`, `assets` and snapshot
      the values here.
- [ ] Inventory each oracle's complete asset list. Particularly the
      FX oracle — the RFP asks us to support AQUA/BRL, EUR/USD, etc.
      and we need to know which quote currencies are actually on-chain.
- [ ] Map the `Asset::Other(Symbol)` convention — are stable CEX
      prices quoted as `"BTC"` or `"BTC/USD"`? The base asset design
      implies bare symbol since base is separate, but confirm.
- [ ] Benchmark Reflector event stream throughput at our self-hosted
      stellar-rpc — check that `REFLECTOR:update` filter returns
      promptly within our 30s freshness budget.
- [ ] Watch Reflector governance for any migration from Pulse to
      Beam model, or introduction of new oracles we should track.

## Corrections to push back into our RFP proposal

When we next revise `ctx-proposal.md`:

- Delete the `twap(asset, n)` / `x_last_price(base, quote)` / `x_prices` /
  `x_twap` claims.
- Replace with "`prices(asset, n)` → we compute TWAP locally;
  cross-pair via two lastprice calls (same base) or via triangulation
  through our broader pricing pipeline".
- Mention that we integrate **all three Reflector public oracles**,
  not a single contract.
- Note that **event-stream subscription (`REFLECTOR:update` topic)** is
  our primary ingest path, not polling.

## References

- SEP-40: <https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0040.md>
- Reflector contract source: <https://github.com/reflector-network/reflector-contract>
- Reflector v3 audit: <https://code4rena.com/audits/2025-10-reflector-v3>
- Reflector operator docs: <https://reflector.network/docs>
- Oracle provider directory (Stellar docs):
  `stellar-docs/docs/data/oracles/oracle-providers.mdx`
- Related i128 handling: [../decisions.md](../decisions.md)
- Related Soroban event ingestion: [../notes/getevents-v2-proposal.md](../notes/getevents-v2-proposal.md)
