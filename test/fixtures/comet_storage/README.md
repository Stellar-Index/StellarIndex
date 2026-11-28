# Comet pool storage fixtures

`pool_entries.json` holds real `contract_data` ledger entries of the three curated Comet pools (Blend BLND/USDC
`CAS3FL6T…`, WASM `8abc2891…`; BLNT/USDC `CDQMIKIH…` and BLNT/XLM `CA5HX3D4…`, WASM `d735c339…`), read from the
archival lake's `stellar.ledger_entries_current` (ADR-0034), not stellar-rpc. All rows were read together, so each
entry is the state at the lake tip and unchanged since its `ledger_seq` (the entry's last-modified ledger).

Per pool: the contract instance (Controller, SwapFee, no Freeze key), `AllRecordData`, `TotalShares`, and the LP
`Balance(holder)` of each backstop that holds the pool's shares (Blend v2 `CAQQR5…` and v1 `CAO3AG…`; BLNT `CAHCUK…`).

Fields: `pool`, `kind` (`Instance`, `AllRecordData`, `TotalShares`, `Balance`), `holder` (`Balance` only),
`ledger_seq`, `key_xdr` and `entry_xdr` (base64 XDR, as the lake stores them).

`internal/sources/comet/storage_test.go` decodes them and `valuation_test.go` derives the golden LP values from them.
The Blend pool's state is after the pool was drained: 0.4351707 USDC and 427.4375264 BLND against
186,208,326,131,163,363,413.5993981 LP shares.
