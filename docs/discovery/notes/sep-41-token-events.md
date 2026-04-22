# SEP-41 Soroban Token — event reference

**Status:** ✅ Read primary. Locks down our Soroban token-transfer
decoder spec.

**Source:** `.discovery-repos/stellar-protocol/ecosystem/sep-0041.md`
(v0.4.1, updated 2025-08-28).

## Why this matters

Every Soroban token (SEP-41) emits standard events on
mint/burn/transfer/approve/clawback. Our pricing extractor must
decode these to track:

- **Supply** changes (for Freighter V2's `circulating_supply`,
  `total_supply`, `max_supply`, `market_cap`, `FDV`).
- **Transfer flows** (for token-transfer volume analytics).
- **Cross-contract token movement** (e.g. when an AMM receives/sends
  SEP-41 tokens as part of a swap).

SEP-41 is defined as a **subset of the Stellar Asset Contract (SAC)**
interface, so classic Stellar assets wrapped in SAC and custom
Soroban tokens **emit the same events**. This means one decoder
works for both.

## Interface (verified)

```rust
pub trait TokenInterface {
    // Read methods
    fn allowance(env, from, spender) -> i128;
    fn balance(env, id: Address) -> i128;
    fn decimals(env) -> u32;
    fn name(env) -> String;
    fn symbol(env) -> String;

    // Write methods (each emits a standard event — see below)
    fn approve(env, from, spender, amount: i128, live_until_ledger: u32);
    fn transfer(env, from, to: MuxedAddress, amount: i128);
    fn transfer_from(env, spender, from, to, amount: i128);
    fn burn(env, from, amount: i128);
    fn burn_from(env, spender, from, amount: i128);
    // NOTE: mint() and clawback() are NOT in the trait — tokens
    // define their own mint/clawback entry points but MUST emit the
    // standard events.
}
```

Every amount is `i128`. [i128 invariant](../decisions.md) applies
uniformly.

## Events (verified, primary source)

### `transfer` — the workhorse

Topics:

```
Symbol "transfer"
Address  from  (balance drawn-from)
Address  to    (balance credited)
```

Data (**two forms** — must support both):

1. **Simple i128**:

   ```
   data = i128  // amount transferred
   ```

2. **Map with `to_muxed_id`** (post-CAP-67 enhancement for muxed
   addresses):

   ```
   data = map {
     "amount":      i128,
     "to_muxed_id": Option<u64| String | BytesN<32>>,
     // other impl-defined keys allowed
   }
   ```

**Implication:** our decoder **must type-test the `data` scval**
before assuming a shape. A naive
`data.MustI128()` crashes on the map form. Correct pattern:

```go
switch v := data.Type; v {
case xdr.ScValTypeScvI128:
    amount = data.MustI128()
case xdr.ScValTypeScvMap:
    // walk map entries for "amount" and "to_muxed_id"
}
```

### `mint`

Topics:

```
Symbol "mint"
Address  to
```

Data: same two forms as `transfer` — i128 or map with
`amount` + `to_muxed_id`.

A `mint` event **increases total supply** by `amount`. This is the
canonical definition we use for supply tracking.

### `burn`

Topics:

```
Symbol "burn"
Address  from
```

Data:

```
data = i128  // amount burned
```

A `burn` event **decreases total supply** by `amount`.

### `clawback`

Topics:

```
Symbol "clawback"
Address  from
```

Data:

```
data = i128  // amount clawed back
```

A `clawback` event is a **burn-type** event. From the spec:

> "A clawback is a type of burn: it reduces both the holder's balance
> and the total supply by the amount clawed back. The `clawback` event
> itself indicates the burn. **No separate `burn` or `transfer` event
> is emitted alongside the `clawback` event.**"

So our supply tracker counts `mint` (+), `burn` (-), and `clawback`
(-). Crucially, we do **not** double-count clawback as a
burn+clawback.

### `approve`

Topics:

```
Symbol "approve"
Address  from
Address  spender
```

Data:

```
data = [i128 amount, u32 live_until_ledger]
```

Less useful for pricing — only tells us an allowance was set. We log
it for audit but it doesn't move supply or generate a trade. Expired
entries (`live_until_ledger < env.now_ledger()`) should be treated
as zero allowance per the spec.

## Supply tracking algorithm (for Freighter V2)

Per the SEP-41 spec, a token's **total supply** is the running sum of:

```
+ (sum of all mint event amounts)
- (sum of all burn event amounts)
- (sum of all clawback event amounts)
```

Our indexer maintains a TimescaleDB hypertable of every mint/burn/
clawback event per token contract and exposes the running sum as
`total_supply` at any ledger timestamp.

**Circulating supply** per the Freighter RFP can then be derived as
`total_supply` minus any configured "locked" set (issuer balance,
admin accounts, vesting contracts, burn addresses). The "locked"
set is metadata we maintain per-asset from stellar.toml or explicit
operator config.

**Max supply** is metadata — not derivable from events. Must come from
stellar.toml `max_supply` or be `null`.

## Classic assets under SAC

Classic Stellar assets can be wrapped in a Stellar Asset Contract (SAC)
and thereby become callable from Soroban contracts. SAC implements
SEP-41, so:

- A classic asset transferred via SAC emits standard `transfer`
  events.
- A classic asset "minted" via SAC (typically by the asset issuer
  sending from reserve) emits a `mint` event.
- Our decoder handles classic-wrapped-SAC tokens identically to
  custom Soroban tokens.

**But:** classic-asset transfers that happen via *native* classic
operations (payments, path payments) emit events only from **Protocol
23+** under the CAP-67 "unified events" regime. Pre-P23 ledgers
have only operation+effect records for classic transfers, no events.

Captured in [protocol-versions.md](../protocol-versions.md) and
[cap-67-unified-events.md](cap-67-unified-events.md).

## Spec version / changelog

- v0.4.1 (2025-08-28, current) — clawback semantics clarified: "no
  separate burn or transfer event is emitted alongside the clawback
  event."
- v0.4.0 — muxed support added to `transfer` and `mint` (the map-data
  form).
- v0.3.0 — added `mint` and `clawback` events.
- v0.2.0 — removed `spendable_balance`.
- v0.1.0 — initial draft based on CAP-46-6.

**Status**: Draft. We track the ecosystem-wide adoption state but
treat v0.4.1 as authoritative for our decoder.

## Implementations

- `rs-soroban-sdk` contains the trait verbatim + generated client.
- Soroban host env contains a native implementation used by SAC.
- OpenZeppelin `stellar-contracts` — widely used community reference.

We read from all three when spec corners need verification.

## Open items

- [ ] Verify which specific tokens on pubnet (USDC, yUSDC, AQUA,
      KALE, etc.) use the **simple i128** data form vs. the **map
      with to_muxed_id** form. Our decoder must handle both but the
      mix tells us what to optimize.
- [ ] Decide our policy when a token's `decimals()` changes mid-
      history. SEP-41 does not forbid mutating decimals on upgrade —
      we assume immutability and alert if violated.
- [ ] Capture OpenZeppelin `stellar-contracts`' event-emission code
      for reference. Their tests are a good source of fixtures.

## References

- SEP-41 primary: `stellar-protocol/ecosystem/sep-0041.md` (v0.4.1).
- CAP-46-6 (Stellar Asset Contract):
  `stellar-protocol/core/cap-0046-06.md`.
- CAP-67 (Unified Events): [cap-67-unified-events.md](cap-67-unified-events.md).
- Related: [../decisions.md](../decisions.md) (i128 invariant),
  [../protocol-versions.md](../protocol-versions.md) (when classic
  transfers start emitting these events).
