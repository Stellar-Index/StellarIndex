# DeFindex vault events — real lake captures

## `vault-admin-2026-09-30/admin_events.jsonl`

Twelve rows of r1's `stellar.contract_events` for `("DeFindexVault", X)`
admin topics on DeFindex vault contracts, captured read-only on
2026-09-30. Columns: `ledger_seq`, `tx_hash`, `op_index`, `event_index`,
`contract_id`, `topics_xdr` (base64 ScVal per topic) and `data_xdr` (base64
ScVal body). Nothing here is synthesised.

| topic[1] | rows |
| --- | --- |
| `nmanager` | 3 |
| `rescue` | 3 |
| `unpaused` | 2 |
| `paused`, `nemanager`, `rbmanager`, `nreceiver` | 1 each |

Pinned by `internal/sources/defindex/golden_admin_test.go`: every row
decodes through the production `Decoder.Decode` into one `AdminEvent`,
with the per-kind fields and the three rescue amounts asserted exactly.

## What they do NOT settle

The capture did not record each emitter's WASM hash, so these rows do
not prove the shape holds for every vault build ever deployed. The
replay gate in `docs/operations/wasm-audits/defindex.md` (decode every
admin event in the lake before replaying) covers that.
