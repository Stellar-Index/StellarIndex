# RedStone

**Status:** ✅ **Primary-source verified** (2026-04-22, round 2).
Earlier version of this doc used WebSearch summaries; now fully
verified against `redstone-finance/redstone-public-contracts` and
stellar.expert's public contract API.

## Architecture — Adapter + per-feed proxies

Verified from
`redstone-public-contracts/packages/stellar-connector/deployments/stellarMultiFeed/`
source:

```
┌────────────────────────────────┐
│     RedStone Adapter           │     single authoritative contract
│     CA526…DXUSG                │     storing every feed's PriceData
│                                │
│  read_price_data_for_feed()    │
│  read_price_data(Vec<String>)  │  ← batch read, our primary call
│  read_prices(Vec<String>)      │  ← batch, prices only
│  write_prices()  (relayer only)│
│  emits REDSTONE event on write │
└─────────────┬──────────────────┘
              │  (19 per-feed contracts — thin proxies)
              │
    ┌─────────┴─────────┬─────────┬──── …
    ▼                   ▼         ▼
  BTC feed          ETH feed   USDC feed …
  CBLE…KTXA         CBC3…777O  CC2G…3CXM
  (stores "BTC"     (stores "ETH" …)
   as feed_id)
```

Each per-feed contract is a **thin proxy** that:

1. Stores its `feed_id` String on init (`"BTC"`, `"ETH"`, etc.).
2. On `read_*` calls, invokes the Adapter's
   `read_price_data_for_feed(feed_id)`.
3. Extends its own storage TTL on every read.

All 19 mainnet per-feed contracts share the same WASM hash
`3e464b6dda26a8d5b0bf13d2cb7781a6142bf2c2e4b64edcba093911fa16df5c`
(verified via stellar.expert's public contract API). That hash is
verified against the public repo commit
`eaab0a1e32310f98f311e95e0fda547c21b8e79e`.

## Mainnet contract IDs (verified from app.redstone.finance/push-feeds)

### Adapter (one)

```
CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG
```

### Price feeds (19)

| Feed                    | Contract ID                                                |
| ----------------------- | ---------------------------------------------------------- |
| BTC                     | `CBLEPHLNPTCW3DY2NF5BYCZVECIP2K6YWBA3JDTU3HMQV6AKXB5GKTXA` |
| ETH                     | `CBC3S2VH7CM3DLGC62QXM4UD4YKCQULRLENAEEO7ZZ6U4OMZIQ5B777O` |
| USDC                    | `CC2GB2TZTX24XIAM375YDWUDXA3PR2GDMNFUL2Z5VNXUMKPPOBES3CXM` |
| XLM                     | `CAXRDLYBL7ZNE754UX33XTVGTA7MNTIFWURTFJBTRKHPTNP2P36TU2ZF` |
| PYUSD                   | `CB3PNR7FSAHQH3UAVYKSJVPLWEQRPPYVFL2UIR4P3RYEPZLTOF7PL2L4` |
| EUROC/EUR               | `CD4H2CN6ROCXVZFDTMLFCSU4O5CKLJP5BCFECNMNZEATNABEDRVGFKNI` |
| EUROB                   | `CAY3Q4DVBMS6OXWQWZWNOCVG7M3JNO6GL2PNZSQ6YH3NJOLW7O7IUWES` |
| MXNe                    | `CDYTKTOHHMMZQSBGEVTFSOAMN6ABFCRZ6TCNLZNFQZ6OK3ZGDB3666IV` |
| BENJI (Franklin Templeton) | `CCDM2U5E3SBMPJUQCBIER4BSFAVSJTY5I5EQEZIRDZSA3YOJH3YM4TW5` |
| iBENJI                  | `CCFFK56RHKVSJF6ADIB35TLLRG7IN76H2WHO7P4SFVNYMLZOSVLLSDI2` |
| GILTS                   | `CDIBUKDR3AUJFOG3Z5PH2UIKTNK6FQEUBODVH4FA7DST4NITPKKYHMAP` |
| CETES                   | `CCYGBQZ5JKE2TGK2XNNVIZRKFEIT27NZADS3XJFSO4CCIH3LHIUP34LG` |
| KTB                     | `CDF24LSSKG55VEN2MRKNHEWK3XT5YKQ6XRXNVTNSRBMFL2FOMAC2MZ5O` |
| SolvBTC                 | `CA5ZDXNDNOR7RA6M6HAB5ZPITD2NU3DIW2AFJCBTV2MJHRUPQXNI2LSE` |
| SolvBTC/FUNDAMENTAL     | `CCE4HNAVDIJJAJQYETON5CCER57MDYXLW45JEXYXI2QMASWFAHAPL5PT` |
| SolvBTC.BBN/FUNDAMENTAL | `CBCVJGBJGNPMYCEU3YVYQIH5Y6KSNATRLRLCZCMS26QTCUV367AD54BE` |
| SPXU                    | `CBKC3LPRNTAOOJLNLG2GFKSMLLSDNHEUBVJO22Y2MY2RJLLYIXCRPK53` |
| TESOURO                 | `CAO3JDDO52PV5SNIR4WRLBNTE72CY6B4JO5PNSSRRM4TC5DZ5X5ZSJVC` |
| USTRY                   | `CAEGYZFAJ3HLSDBUIVJ77SK5HLLS4MPE4LO5OB2W3L2TEDMAS6RCTMZ4` |

**Asset categories**:

- **Crypto**: BTC, ETH, USDC, XLM, PYUSD.
- **Euro stables**: EUROC/EUR (Circle), EUROB.
- **Mexican peso stable**: MXNe.
- **Tokenized treasury / money-market** (RWA, big deal): BENJI +
  iBENJI (Franklin Templeton BENJI + its iBENJI index), GILTS (UK
  gilts), CETES (Mexican treasury), KTB (likely Korean treasury
  bonds — verify), TESOURO (Brazilian treasury), USTRY (US
  treasury).
- **Tokenized BTC variants**: SolvBTC, SolvBTC/FUNDAMENTAL,
  SolvBTC.BBN/FUNDAMENTAL (Babylon-staked).
- **Inverse ETF**: SPXU (3x inverse S&P 500).

**Implication for our coverage:** RedStone gives us a uniquely rich
**RWA price set** on-chain. Reflector doesn't cover any of these
tokenized-treasury assets. RedStone becomes our primary pricing
source for anything in that list — not just a cross-check.

## Update cadence (verified from app.redstone.finance)

Every mainnet feed:

```
Deviation threshold: 0.2%
Heartbeat:           24h
```

Meaning: a feed updates **when price moves ≥ 0.2% from the last
stored value, OR at least once every 24 hours**, whichever comes
first.

Compared to Reflector Pulse's uniform 5-minute cadence this is
**materially slower**. RedStone is not a real-time price source —
it's a secondary / validation / RWA-coverage source. For RWA feeds
(BENJI, GILTS, etc.), where off-chain NAV updates daily at best, the
24h heartbeat is appropriate.

**Testnet feeds** come in two flavours (per the app page):

- "BTC (Testnet Rust)" / "ETH (Testnet Rust)":
  `0.1% deviation / 5m heartbeat` — much tighter.
- "BTC (Testnet)" / "ETH (Testnet)":
  `0.5% deviation / 24h heartbeat`.

We use mainnet in production; testnet's Rust-path values suggest
RedStone has considered faster cadence. Not available on mainnet
today.

## PriceData struct (verified, common/src/lib.rs:12-18)

```rust
pub struct PriceData {
    pub price:             U256,
    pub package_timestamp: u64,   // when RedStone signed the package off-chain
    pub write_timestamp:   u64,   // when the update landed on-chain
}
```

Three fields — matches our proposal's description exactly. **`price`
is `U256`** — our decoder needs 256-bit integer support (bigger than
the i128 most other Stellar events use). `stellar-extract`'s
`uint256ToString` handles this correctly (see
[../data-sources/withobsrvr-stellar-extract.md](../data-sources/withobsrvr-stellar-extract.md)).

## Decimals (verified, config.rs:1)

```rust
pub const DECIMALS: u64 = 8;
```

All feeds: **8 decimals**. Simpler than Reflector (14 typical) or
Band (9/18 dual). Our normaliser divides price by 1e8.

## Per-feed contract interface (verified, price-feed lib.rs)

Every per-feed contract exposes:

```
decimals()                 -> u64          // = 8
description()              -> String        // "RedStone Price Feed for <FEED_ID>"
feed_id()                  -> String        // "BTC", "ETH", …
read_price()               -> U256
read_timestamp()           -> u64           // package_timestamp
read_price_and_timestamp() -> (U256, u64)
read_price_data()          -> PriceData     // delegates to Adapter
```

Plus `init`, `change_owner`, `accept_ownership`,
`cancel_ownership_transfer`, `upgrade` — ownable/upgradable admin.

## Adapter contract interface (verified, adapter lib.rs)

Public methods:

```
read_price_data_for_feed(feed_id: String)     -> Result<PriceData, Error>
read_price_data(feed_ids: Vec<String>)        -> Result<Vec<PriceData>, Error>   // BATCH
read_prices(feed_ids: Vec<String>)            -> Result<Vec<U256>, Error>        // BATCH, prices only
read_timestamp(feed_id: String)               -> Result<u64, Error>
check_price_data(price_data: PriceData)       -> Result<PriceData, Error>        // freshness check
get_prices(/* signed payload */)              -> …                               // pull-oracle path
write_prices(/* signed payload */)            -> …                               // relayer only
unique_signer_threshold()                     -> u64                             // threshold sig config
```

Plus admin: `init`, `change_owner`, `accept_ownership`,
`cancel_ownership_transfer`, `upgrade`.

**For us, `read_price_data(Vec<String>)` is the preferred read path**
— single contract call returns every feed we ask for. No need to
call each per-feed proxy individually.

## Adapter events (verified, adapter/src/event.rs)

**Big correction to the earlier doc** — I said "needs verification
whether Adapter emits events." It does:

```rust
const WRITE_PRICES_TOPIC: Symbol = symbol_short!("REDSTONE");

struct WritePrices {
    updater:       Address,
    updated_feeds: Vec<PriceData>,
}
```

- **Topic**: `["REDSTONE"]` — single-element topic, literal symbol.
- **Body**: `WritePrices` struct containing the relayer address + a
  vector of every `PriceData` updated in that transaction.
- Emitted **once per `write_prices` call**, carrying all feeds
  updated in the batch (not once per feed).

**Consumer pattern for us:**

```
Subscribe to stellar-rpc getEvents with:
    contractIds: [CA526…DXUSG]   # the Adapter
    topics:      [["REDSTONE"]]

Each event payload gives us a batch of fresh (price, timestamps) for
multiple feeds at once. Decode the Vec<PriceData>, update our cache.

Poll read_price_data(Vec<String>) only as a reconciliation fallback.
```

One subscription for all 19 feeds, fewer RPC calls than polling each
per-feed contract.

**Caveat:** `updated_feeds` in the event is `Vec<PriceData>` — the
event tells us *what* was updated (price/timestamps) but the feed
*identity* of each entry isn't in the event struct. We'd need to
correlate against the order in the relayer's `write_prices` call
(which is called with an explicit `feed_ids` list). Either we read
the tx envelope args to learn the feed-id list, or we follow up with
a read-call on the Adapter. Worth verifying in the adapter's
write_prices function.

## TTL management (verified)

Per-feed contracts bump their own TTL on every `read_price_data`
call (`price-feed lib.rs:124-129`):

```rust
CONTRACT_TTL_THRESHOLD_LEDGERS = 7 days / 5 s = 120,960 ledgers
CONTRACT_TTL_EXTEND_TO_LEDGERS = 21 days ≈ 362,880 ledgers
```

Our cross-check poll (e.g. every 60 s) also serves as free TTL
keep-alive for the per-feed contracts we query. No action needed by
us. If we ever stop calling a feed contract we care about, TTL
expires after 21 days and the contract's storage entry archives —
at which point we'd need to call `extend_ttl` or the Adapter's
maintenance would.

## Stellar.expert cross-verification

Per the public contract API at
`https://api.stellar.expert/explorer/public/contract/<id>`:

- `CBLEPHLNPTCW3DY2NF5BYCZVECIP2K6YWBA3JDTU3HMQV6AKXB5GKTXA` (BTC)
  — created `1756897073` (2025-09-03), WASM verified, 7 events.
- `CAXRDLYBL7ZNE754UX33XTVGTA7MNTIFWURTFJBTRKHPTNP2P36TU2ZF` (XLM)
  — created `1772797538` (2026-03-06), WASM verified, 0 events.

The per-feed contracts are **storage-only proxies** — they don't
emit events themselves. All event activity is on the **Adapter**.
Subscribing to individual per-feed contracts will get nothing
useful; subscribe to the Adapter.

**User's observation** ("addresses don't link to anything on
stellar.expert"): StellarExpert's UI renders the pages but may not
show contract details for these specific addresses because the
per-feed contracts are simple proxies with only instance storage +
few (or zero) events. The data **is** queryable via their JSON API.
We'll rely on API, not UI.

## Ingest plan

1. **Subscribe** to Adapter events: `contractIds: [CA526…DXUSG]`,
   `topics: [["REDSTONE"]]`. Decode `WritePrices` struct to get
   batch updates.
2. **Periodic reconciliation** via
   `Adapter.read_price_data(feed_ids)` at ~5-min cadence. Guards
   against missed events.
3. **Per-feed reads only when needed** (e.g. Freighter asset detail
   page for one asset) — use the per-feed contract to get auto-TTL
   plus a single-symbol call.
4. **Role:** RedStone contributes to cross-validation for crypto
   feeds (BTC, ETH, USDC, XLM) and is **primary** for RWA feeds
   (BENJI, GILTS, CETES, TESOURO, USTRY, etc.) which aren't covered
   by Reflector.

## Open items

- [ ] Verify `write_prices` emits one event per batch containing the
      `feed_ids` list — need to read the relayer-side function in
      adapter/src/lib.rs:78 to see how the event body is populated
      and how feed identity is captured.
- [ ] Call the Adapter on mainnet and capture current asset TTL
      status for each of the 19 per-feed contracts.
- [ ] Decide decoding priority: RWA feeds (BENJI, GILTS, etc.) are
      unique value — probably highest priority in Phase 2.
- [ ] Track the testnet "Rust" deployment (5-min cadence) in case
      RedStone promotes it to mainnet — that would make RedStone a
      viable primary live-price source alongside Reflector.

## Corrections to push back into proposal

Our `ctx-proposal.md` says:

> "Deployed price feeds include BTC, ETH, USDC, EUROC, EUROB, PYUSD,
> and others, with per-symbol Soroban contracts on mainnet."

Update to:
- 19 feeds at audit time (2026-04-22), not the 6 listed.
- Explicitly call out the RWA feed set (BENJI, GILTS, CETES, TESOURO,
  USTRY) — this is an important differentiator.
- Note the **single Adapter + thin-proxy architecture**.
- Note **on-chain update cadence is `0.2% deviation OR 24h heartbeat`**,
  not continuous. RedStone is not a real-time price source.

## References

- Public repo: <https://github.com/redstone-finance/redstone-public-contracts>
- Deployments path:
  `packages/stellar-connector/deployments/stellarMultiFeed/`
- RedStone push-feeds UI: <https://app.redstone.finance/push-feeds?networks=stellar>
- Stellar.expert contract API example:
  `https://api.stellar.expert/explorer/public/contract/<CID>`
- Related: [reflector.md](reflector.md) (primary on-chain oracle),
  [../decisions.md](../decisions.md) (U256 → our 256-bit-capable
  numeric handling applies uniformly).
