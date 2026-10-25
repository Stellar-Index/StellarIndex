# Chainlink on Stellar

Ingests Chainlink Data Feeds published natively on Stellar mainnet (live since 2026-10):
the Data Streams DON writes reports to one `DataFeedsCache` Soroban contract, and every
feed's history is in that contract's events.

## What this ingests

The cache emits `FeedUpdated` for each accepted report:

| Field | Where | Decoded as |
| --- | --- | --- |
| `data_id` | topic[1], `BytesN<32>` | Feed identity; `feeds.go` maps it to a pair |
| `answer` | body map, `I256` | Price at 18 decimals (every mainnet feed's `FeedConfigSet`) |
| `timestamp` | body map, `u64` seconds | Observation time, clamped to ledger close |
| `round_id`, `ledger_seq`, `primary` | body map | Not stored |

One `on_report` updates several feeds, so `op_index` is `operation_index × 1024 + event_index`.
The contract emits `FeedUpdated` only for a permitted, non-stale report; refused ones emit
`InvalidUpdatePermission` or `StaleReport`. `FeedConfigSet` (decimals and description per feed) is
the source of `feeds.go`. These, plus `FeedAdminRemoved`, `Upgraded` and the two `ownership_transfer`
events, are matched as no-ops so the completeness census recognises them; they write nothing.

| Contract | Address |
| --- | --- |
| DataFeedsCache (mainnet) | `CAAFKBE6AMTERARISMMT4TCC4UGNO35XCKQNC5XVLL5WY5GRWXH6AMIO` |

First `FeedConfigSet` ledger 64442968; first `FeedUpdated` ledger 64545407.

## Unregistered feeds

A `data_id` missing from `feeds.go` lands as `raw:<64-hex data_id>` quoted in USD and bumps
`stellarindex_source_unknown_symbols_total{source="chainlink-stellar"}`. Add the entry and
replay to promote it. FLHY and USPC (a NAV feed) are raw on purpose: no canonical asset exists for them.

## Sources

- https://docs.chain.link/data-feeds/stellar
- https://github.com/smartcontractkit/chainlink-stellar (`contracts/data-feeds-cache`)
