# Band Protocol

**Status:** ⚠️ Secondary validation source. Live on Stellar mainnet via
a Soroban Rust contract. Interface verified from
`bandprotocol/band-std-reference-contracts-soroban`.

## Deployed contracts (from stellar-docs)

Source: `stellar-docs/docs/data/oracles/oracle-providers.mdx`.

| Network | Contract address |
| ------- | ---------------- |
| Mainnet | `CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M` |
| Testnet | `CBRV5ZEQSSCQ4FFO64OF46I3UASBVEJNE5C2MCFWVIXL4Z7DMD7PJJMF` |

## Contract interface (code-verified 2026-04-22)

**Update 2026-04-22:** the prior version of this doc relied on a
WebFetch summary. Now re-verified by cloning and reading
`bandprotocol/band-std-reference-contracts-soroban` (commit at that
time). Corrections made inline.

Full trait (`src/contract.rs:12-43`):

```rust
pub trait StandardReferenceTrait {
    fn init(env, admin_addr, instance_threshold, instance_ttl,
            temporary_threshold, temporary_tll);
    fn upgrade(env, new_wasm_hash: BytesN<32>);
    fn version() -> u32;                    // = 1 per VERSION const
    fn current_admin(env) -> Address;
    fn transfer_admin(env, new_admin);
    fn current_ttl_config(env) -> TTLConfig;
    fn update_ttl_config(env, ...);
    fn is_relayer(env, address) -> bool;
    fn add_relayers(env, Vec<Address>);
    fn remove_relayers(env, Vec<Address>);
    fn relay(env, from: Address,
             symbol_rates: Vec<(Symbol, u64)>,
             resolve_time: u64, request_id: u64);
    fn force_relay(env,
             symbol_rates: Vec<(Symbol, u64)>,
             resolve_time: u64, request_id: u64);
    fn delist(env, symbols: Vec<Symbol>);
    fn get_ref_data(env, symbols: Vec<Symbol>)
        -> Result<Vec<RefDatum>, StandardReferenceError>;
    fn get_reference_data(env,
             symbol_pairs: Vec<(Symbol, Symbol)>)
        -> Result<Vec<ReferenceDatum>, StandardReferenceError>;
}
```

### `RefDatum` — single-symbol (`src/storage/ref_data.rs:11-15`)

```rust
struct RefDatum {
    rate:         u64,
    resolve_time: u64,
    request_id:   u64,
}
```

**Correction vs earlier doc:** the type is `RefDatum` (singular), not
`RefData`.

### `ReferenceDatum` — pair-quoted (`src/reference_data.rs:8-15`)

```rust
struct ReferenceDatum {
    rate:                u128,   // pair rate (e.g. BTC/USD)
    last_updated_base:   u64,
    last_updated_quote:  u64,
}
```

**Correction vs earlier doc:** the type is `ReferenceDatum`
(singular), not `ReferenceData`.

### Decimals / precision (verified)

From `src/reference_data.rs:30-42` — the pair rate is computed as:

```
rate = base.rate * E18 / quote.rate
```

where `base.rate` and `quote.rate` are themselves in `E9` scale
(`src/storage/ref_data.rs:31` uses `E9` for the USD baseline). The
resulting `ReferenceDatum.rate` is therefore **E18-scaled** — i.e.
18 decimal places of fixed-point precision.

Example: BTC/USD at $50,000 is stored as `5e22` in `ReferenceDatum.rate`.

Our [i128 invariant](../decisions.md) applies to the `u128` reads.

### USD is special-cased

From `src/storage/ref_data.rs:30-38`:

- `RefDatum::usd(env)` returns `Self::new(E9, env.ledger().timestamp(), 0)`
  — always rate = 1 in E9 scale.
- The `"USD"` symbol cannot be set (relayer writes are rejected in
  the `set` method).

So querying `get_reference_data([(X, "USD")])` always works without
requiring relayers to have pushed a "USD" rate; only `X` needs a
relayed value.

### No events (verified)

**Grepped `.events().publish` across the whole repo — zero
matches.** Band's Soroban contract does **not** emit events on
`relay` / `force_relay`. Consumers must poll.

This contradicts my earlier speculative note that "if Band emits
events, subscribe." Confirmed: no event subscription path exists.
We poll `get_ref_data` / `get_reference_data` at our validation
cadence (≥60 s is fine — Band is secondary validation, not hot-path).

### Update mechanism

Push model. Relayer functions on the contract:

```rust
fn relay(...)        // relayers push symbol rates
fn force_relay(...)  // forced updates
```

The relayer runs off-chain, aggregates from BandChain (Cosmos-SDK-based
oracle chain), and submits signed updates to the Stellar contract.
Consumers read via `get_ref_data` / `get_reference_data` — no caller
fee on reads.

## Our integration plan

### Role: secondary validation

Same as Reflector / RedStone / Chainlink:

- Poll or event-subscribe Band's mainnet contract for our headline
  symbol set (BTC/USD, ETH/USD, XLM/USD, USDC/USD, etc.).
- Compare against our aggregated values.
- Flag divergence, do not contribute to VWAP.

### Why Band at all given we already have Reflector + Redstone

- Independent data source diversity. Reflector federates Stellar-
  native nodes; RedStone uses its own threshold-sig relayer set;
  Band is Cosmos-BandChain-derived. Three independent failure modes.
- BandChain's validators and Reflector's federation have minimal
  overlap — so a correlated outage is unlikely.
- Small incremental integration cost (one more Soroban contract
  to read).

## Implementation notes

### Prefer `get_reference_data` over `get_ref_data`

- `get_reference_data` returns `u128` rates, supports arbitrary
  base/quote pairs (not just USD), and is the canonical Band
  interface across chains.
- `get_ref_data` is convenience for USD-quoted single-symbol reads
  but loses precision at large values (u64 rate).

Our i128-no-truncation decision favours `get_reference_data`
uniformly.

### Both methods take `Vec<...>` — batch reads

`get_ref_data(symbols: Vec<Symbol>)` and
`get_reference_data(symbol_pairs: Vec<(Symbol, Symbol)>)` both accept
vector inputs. Batch all our symbols of interest in one call per poll
cycle. Cheaper than N individual calls.

### Events?

The README doesn't document event emission. Our proposal says "the
BandChain REST API" rather than on-chain events. Two ingest options:

1. **Poll the on-chain contract** at fixed cadence (say, every 60 s
   for the validation layer). Uses one Soroban RPC read per cycle.
2. **Poll BandChain's REST API** directly (skipping the Stellar
   deployment) — cheaper per-call but adds a non-Stellar network
   dependency and loses the on-chain signed guarantee.

Decision: go with (1) once live. The 60 s cadence is fine for a
validation layer (we're not serving Band prices in the hot path).

## i128 handling

Reads of `ReferenceData.rate` (u128) parse via `big.Int` or
`stellar-extract`'s `uint128ToString`. Persist as `NUMERIC`. JSON
as string.

## Known (quoted from our proposal)

> "Band Protocol operates BandChain, a Cosmos-based oracle network.
> Integration will be via the BandChain REST API for reference prices
> on supported symbol pairs. As with Chainlink, Band data is
> classified as a secondary validation source and does not contribute
> directly to VWAP or TWAP aggregation."

**Update:** Band now has a native Soroban contract (`CCQX…GG5M`
mainnet). We should prefer reading from the on-chain contract over
BandChain REST — it gives us a signed, on-Stellar source and we keep
our pricing inputs entirely within Stellar. The proposal text should
be refreshed accordingly.

## Open items

- [ ] Clone `bandprotocol/band-std-reference-contracts-soroban` (we
      only WebFetched it). Confirm exact errors in
      `StandardReferenceError` enum.
- [ ] Call the mainnet contract and enumerate which symbols have
      fresh updates. Produce a supported-pairs list for our
      validation layer.
- [ ] Measure update cadence per symbol — relayer model means this
      varies.
- [ ] Decide polling vs. event subscription. If Band emits events,
      subscribe; if not, poll at 60 s.
- [ ] Update ctx-proposal.md to reflect on-chain Soroban integration
      rather than BandChain REST.

## References

- Band Stellar contract repo:
  <https://github.com/bandprotocol/band-std-reference-contracts-soroban>
- Oracle providers directory:
  `stellar-docs/docs/data/oracles/oracle-providers.mdx`
- Related: [reflector.md](reflector.md), [redstone.md](redstone.md),
  [chainlink.md](chainlink.md).
