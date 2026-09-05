# SushiSwap V3 connector

Ingests trades from SushiSwap V3 on Stellar — a Uniswap-V3-shaped
concentrated-liquidity AMM on Soroban. See the protocol verification
page: [`docs/protocols/sushiswap_v3.md`](../../../docs/protocols/sushiswap_v3.md).

## Shape

One pool factory, 58 pools. The factory's `pool_created` event carries
the new pool's address **and its two token identities**, so it is both
the ADR-0035 gate seed and the money mapping — nothing else on chain
states a pool's tokens. `MainnetPools` is that table, decoded from the
lake, and `MainnetGatedSet()` is the gate trust root built from it.

`swap` is the only trade-forming event, and its body is self-contained:
both signed pool deltas, the post-swap `sqrt_price_x96` and `tick` all
arrive in one event. There is **no correlation buffer** here — unlike
Soroswap (swap+sync) or Phoenix (8 field events), nothing is held
across events, so there is no orphan class.

## Three things worth knowing before editing this package

**1. The identity gate is the safety story, not a formality.** Every
event carries a ONE-element Symbol topic and the pool address is not in
it. A bounded 10,000-ledger pubnet census puts this protocol's own
`mint` at 33% and `burn` at 12% of *all* contract events. A topic-only
`Matches()` would swallow a large slice of the network's token traffic
and would let any contract announce a pool and mint trades at prices of
its own choosing. `TestMatches_ForeignContractEmittingOurTopicsIsRejected`
is the regression that pins it.

**2. Direction comes from the signed deltas, and one real event has
none.** `amount0` / `amount1` are deltas from the *pool's* point of
view: positive = the pool received it (the trader sold it), negative =
the pool paid it out. Base is the positive leg, quote the magnitude of
the negative one. Ledger 62,712,211 carries `amount0=1, amount1=0` — a
dust swap whose output rounded to nothing. It is refused
(`ErrNonDirectionalSwap`) as a recognized no-op: zero rows, no error, so
the ADR-0033 re-derive counts the ledger as expected-zero rather than
going blind. It is the only such event in 97,349 swaps.

**3. Concentrated liquidity means the constant-product helpers do not
apply.** A V3 pool prices from a tick range and a Q64.96
`sqrt_price_x96`, not from reserves. This package derives **no** TVL and
**no** price — reserve-shaped numbers would be quietly wrong here. The
`sqrt_price_x96` (a U256, wider than i128) and `tick` are decoded and
carried; nothing floats them.

## Scope

`swap` → `trades` is the whole projected surface today. `mint`, `burn`,
`collect`, `init`, `upgraded` and `migrated` are gated and recognized,
and project zero rows. A V3 position is `(owner, tick_lower,
tick_upper)` and wants a table of its own rather than a reserve-shaped
liquidity row; that table is the natural next increment.

## Replay

Projected source (`internal/projector/registry.go` +
`internal/pipeline/sink.go::IsProjectedEvent`), so history is refilled
with `projector-replay` or `projected-rebuild` per the replay decision
rule in [`docs/architecture/ingest-pipeline.md`](../../../docs/architecture/ingest-pipeline.md).
A MinIO walk (`backfill`) is never the answer.
