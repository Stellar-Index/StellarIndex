# Phoenix DEX

**Status:** ✅ Code-verified. **Not in our original proposal.** Added
during adversarial-audit follow-up (2026-04-22).

**Repo:** <https://github.com/Phoenix-Protocol-Group/phoenix-contracts>
**Verified against:** `contracts/pool/src/contract.rs`,
`contracts/factory/src/contract.rs`,
`contracts/pool_stable/src/contract.rs`, `scripts/*.sh` at clone time
(2026-04-22).

## What it is

A Stellar-native DeFi hub with a constant-product DEX and stableswap
pools. Launched May 2024. Uses Phoenix governance token `PHO`.

Contract workspace includes:

```
contracts/
├── factory/       — pool factory
├── pool/          — xy=k constant-product (2 tokens)
├── pool_stable/   — stableswap (2 tokens, Curve-style)
├── multihop/      — multi-pool routing
├── stake/         — staking / rewards
├── token/         — SEP-41 pool-share tokens
├── trader/        — trader module
└── vesting/       — PHO vesting
```

## Mainnet addresses (verified from `scripts/*.sh`)

```
XLM          CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC
FACTORY      CB4SVAWJA6TSRNOJZ7W2AWFW46D5VR4ZMFZKDIKXEINZCZEGZCJZCKMI
MULTIHOP     CCLZRD4E72T7JCZCN3P7KNPYNXFYKQCL64ECLX7WP5GNVYPYJGU2IO2G
VESTING      CDEGWCGEMNFZT3UUQD7B4TTPDHXZLGEDB6WIP4PWNTXOR5EZD34HJ64O

Pools:
  PHO/USDC   CD5XNKK3B6BEF2N7ULNHHGAMOKZ7P6456BFNIHRF4WNTEDKBRWAE7IAA
  XLM/PHO    CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH
  XLM/USDC   CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX
  XLM/EURC   CBISULYO5ZGS32WTNCBMEFCNKNSLFXCQ4Z3XHVDP4X4FLPSEALGSY3PS
  USDC/VEUR  CDQLKNH3725BUP4HPKQKMM7OO62FDVXVTO7RCYPID527MZHJG2F3QBJW
```

**Note:** `VEUR` (tokenized EUR) and `EURC` (Circle euro stablecoin)
appear here — both useful for our FX coverage. `PHO` is Phoenix's
own token.

## Event model — unusual, high-cardinality

**Verified from `contracts/pool/src/contract.rs`.** Unlike
Soroswap/Aquarius/Blend/Comet which all publish a structured event
body per action, **Phoenix emits multiple independent events per
action**, each with a 2-tuple topic `(<action>, <field_name>)` and
a single-value body.

### A single `swap` produces 8 separate events

From `contract.rs:1172-1185`:

```
topic                                  body
("swap", "sender")                     Address
("swap", "sell_token")                 Address
("swap", "offer_amount")               i128
("swap", "actual received amount")     i128        ← note spaces in key
("swap", "buy_token")                  Address
("swap", "return_amount")              i128
("swap", "spread_amount")              i128
("swap", "referral_fee_amount")        i128
```

**Three consequences for our indexer:**

1. **Correlation required.** To reconstruct one swap we must group 8
   events by `(ledger_sequence, tx_hash, op_index)` and assemble them
   into a single record. Our `CanonicalTrade` consolidator needs a
   Phoenix-specific collator.
2. **Event filter limitations.** stellar-rpc `getEvents` filters on
   topic prefix. With the tuple `("swap", <field>)` we have to
   subscribe to 8 topic values per swap — inefficient, or we
   subscribe only to the `("swap", "sender")` event and then
   fetch the rest by `tx_hash` on match.
3. **Robustness.** If even one of the 8 events is missing (truncation,
   partial read), the swap record is incomplete. Batch-read by
   transaction is safer than topic-based aggregation.

### `provide_liquidity` — 5 events

```
("provide_liquidity", "sender")        Address
("provide_liquidity", "token_a")       Address
("provide_liquidity", "token_a-amount") i128
("provide_liquidity", "token_b")       Address
("provide_liquidity", "token_b-amount") i128
```

### `withdraw_liquidity` — 4 events

```
("withdraw_liquidity", "sender")           Address
("withdraw_liquidity", "shares_amount")    i128
("withdraw_liquidity", "return_amount_a")  i128
("withdraw_liquidity", "return_amount_b")  i128
```

### `pool_stable` — symmetric pattern

`contracts/pool_stable/src/contract.rs` emits the same pattern with
`provide_liquidity` and `withdraw_liquidity` topic tuples. Swap
events in stable pool should be verified separately (didn't fully
read).

### Factory event — pool creation

From `contracts/factory/src/contract.rs:178`:

```
topic: ("create", "liquidity_pool")    body: lp_contract_address: Address
```

Unusual topic key — `"create"` as the first element, not
`"SoroswapFactory" / "new_pair"`-style namespacing. Our factory-
tracker must subscribe to exactly this topic.

## Data model implications

All amounts are `i128` — our [i128 invariant](../decisions.md)
applies uniformly.

Asset identifiers are `Address` (contract IDs). Phoenix pools hold
SEP-41 tokens, which for classic Stellar assets means the SAC-
wrapped contract address (not the `code:issuer` pair string).

## Ingest plan

```
1. Subscribe to factory events → enumerate pools.
2. For each pool, subscribe to 8 `swap` topic variants (or batch-read
   by tx_hash after matching `("swap", "sender")`).
3. Assemble each (ledger, tx, op) into a single CanonicalTrade.
4. Also subscribe to provide_liquidity / withdraw_liquidity for LP
   metrics and for reserve inference.
```

Phoenix does **not** emit a post-state reserves event (like
Soroswap's `sync`). To compute current reserves we must either:

- Maintain our own running reserve state from `provide`/`withdraw`/
  `swap` deltas, or
- Query pool contract state via `getLedgerEntries`.

The second approach is simpler but adds an RPC call per snapshot.

## Verdict

Include in Phase-2 ingestion. Three Stellar-native DEXes ingested
(Soroswap, Aquarius, Phoenix) + one aggregator-mode (Soroswap router
also routes through external pools) gives us wide Soroban coverage.

## Open items

- [ ] Read `contracts/pool_stable/src/contract.rs` swap event details
      — stable-pool swap math differs; topic set probably differs
      too.
- [ ] Read `contracts/multihop/src/contract.rs` — this is the
      aggregator that routes through multiple pools. Understand
      whether multihop emits its own events or just relies on pool
      events.
- [ ] Check whether Phoenix's pools can be multi-asset (like Aquarius
      stableswap's 4-asset pools). The `token_a-amount` / `token_b-
      amount` naming suggests 2-asset only — confirm.
- [ ] Test-fetch current reserve state from one Phoenix pool to
      confirm our pool-state-query approach.

## References

- Phoenix contracts repo:
  <https://github.com/Phoenix-Protocol-Group/phoenix-contracts>
- Launch announcement (May 2024):
  <https://medium.com/stellar-community/phoenix-building-the-first-defi-hub-on-stellar-cae669829ab5>
- Related: [soroswap.md](soroswap.md), [aquarius.md](aquarius.md),
  [comet.md](comet.md).
