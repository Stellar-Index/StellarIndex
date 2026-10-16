---
title: Chainlink on Stellar — contract & event verification
last_verified: 2026-10-15
status: current
---

# Chainlink on Stellar — contract & event verification

> **For the Chainlink team:** this is how Stellar Index ingests Chainlink Data Feeds on Stellar.
> Please confirm the cache address and tell us if feeds are added or move to another contract.
>
> - **Enumeration method:** one DataFeedsCache contract pinned by contract ID; every feed's
>   `FeedUpdated` event is emitted there. The per-feed proxy contracts emit nothing.
> - **Last verified:** 2026-10-15 (source: `internal/sources/chainlink_stellar`; WASM audit
>   `docs/operations/wasm-audits/chainlink-stellar.md`).
> - **Gate status:** ✅ Gated (ADR-0035): matches only the pinned contract ID and the
>   `FeedUpdated` topic.

## What it is

Chainlink Data Feeds went live on Stellar mainnet in 2026-10. The Data Streams DON signs reports;
a forwarder delivers them to the DataFeedsCache contract's `on_report`, which stores each feed's
latest answer and emits one `FeedUpdated` per accepted feed. The source name is
`chainlink-stellar`, kept apart from `chainlink`, the retired off-chain Ethereum poller, so the two
histories never merge in `oracle_updates`.

| Contract | Address |
| --- | --- |
| DataFeedsCache | `CAAFKBE6AMTERARISMMT4TCC4UGNO35XCKQNC5XVLL5WY5GRWXH6AMIO` |

## Event decoded

`FeedUpdated`: topics `[Symbol("FeedUpdated"), BytesN<32> data_id]`, body map
`{answer: I256, ledger_seq: u32, primary: bool, round_id: u64, timestamp: u64}`.

- `answer` is stored exactly at 18 decimals (each feed's `FeedConfigSet` declares 18).
- `timestamp` is seconds; it is clamped to the ledger close time.
- A report that is not permitted or is stale emits `InvalidUpdatePermission` / `StaleReport`
  instead, and nothing is written. Those, `FeedConfigSet`, `FeedAdminRemoved`, `Upgraded` and the
  `ownership_transfer` pair are matched as no-ops so the completeness census recognises them.
- `op_index` is `operation_index × 1024 + event_index`, since one report updates several feeds.

## Feeds

`data_id` → pair, from the `FeedConfigSet` descriptions (`internal/sources/chainlink_stellar/feeds.go`):

| Feed | Stored as |
| --- | --- |
| BTC/USD, ETH/USD, LINK/USD, XLM/USD | `crypto:<code>` / `fiat:USD` |
| USDC/USD, USDT/USD, EURC/USD | `crypto:<code>` / `fiat:USD` (never normalised; a depeg stays visible) |
| XAUM Reference Rate | `rwa:XAUm` / `fiat:USD` |
| FLHY/USD | `raw:FLHY` / `fiat:USD` (no canonical asset) |

An unregistered `data_id` lands as `raw:<64-hex data_id>` and bumps
`stellarindex_source_unknown_symbols_total{source="chainlink-stellar"}`.

## Aggregator treatment — reported, not counted

Class `Oracle` / `IncludeInVWAP=false`. Staleness uses the heartbeat budget (24h, the slowest
feed's heartbeat, plus grace), since feeds publish on deviation or heartbeat.

## Backfill safety

`BackfillPerWASM`: both WASM hashes the cache has run are audited and emit the same
`FeedUpdated` shape. Catch-up is `stellarindex-ops projector-replay` from ledger 64,442,968.

## References

- https://docs.chain.link/data-feeds/stellar
- https://github.com/smartcontractkit/chainlink-stellar (`contracts/data-feeds-cache`)
