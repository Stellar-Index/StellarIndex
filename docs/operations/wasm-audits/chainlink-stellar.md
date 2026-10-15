---
title: Chainlink on Stellar WASM-history audit
last_verified: 2026-10-15
status: ratified — two WASM hashes, one FeedUpdated shape
source: chainlink-stellar
backfill_safe: true
---

# Chainlink on Stellar WASM audit

Audit log for the `chainlink-stellar` source's `BackfillPerWASM` policy. See `README.md` for the
procedure.

## Contracts under audit

| role | mainnet contract |
| --- | --- |
| DataFeedsCache | `CAAFKBE6AMTERARISMMT4TCC4UGNO35XCKQNC5XVLL5WY5GRWXH6AMIO` |

Configured via `cfg.Oracle.ChainlinkStellar.CacheContract`.

## WASM timeline

From `stellar.contract_instance_changes` (contract_hash `0055049e…b5cfe0`, r1 lake, 2026-10-15):

| WASM hash | from ledger | how |
| --- | --- | --- |
| `4d2e0f84bc7a61cbeee446f51ec542585e4c54b3d0a2adcce59afea40191b9eb` | 64,427,336 | deploy |
| `59ccbf5a2ef2abbb6449b73448d5559549b2c20ea3f9d8d665a25d24fdc9bdbf` | 64,668,750 | `Upgraded` event |

## Decoder review

The decoder reads only `FeedUpdated`: topics `[Symbol("FeedUpdated"), BytesN<32> data_id]`, body
map `{answer: I256, ledger_seq: u32, primary: bool, round_id: u64, timestamp: u64}` read by field
name. Per the source (smartcontractkit/chainlink-stellar `contracts/data-feeds-cache`,
`on_report`), it is emitted only for a permitted, non-stale report; refusals emit
`InvalidUpdatePermission` / `StaleReport`.

Both hashes emit the same shape. Lake census of the contract's events by era, 2026-10-15:

| era | FeedUpdated rows | topics | body bytes (base64) |
| --- | --- | --- | --- |
| `4d2e0f84…` (64,545,407–64,668,705) | 2,192 | 2 | 236 |
| `59ccbf5a…` (64,668,776–64,957,898) | 4,030 | 2 | 236 |

`decode_test.go` decodes a real event from each era (BTC at 64,545,407; XAUm and LINK after the
upgrade). Every feed's `FeedConfigSet` declares 18 decimals.
