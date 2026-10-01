# Comet fixtures

Real `getEvents` capture of a Comet pool `(POOL, swap)` event —
closes GH-932 ("comet is the only Soroban DEX trade decoder with
zero mainnet event bytes in its tests").

## Capture

Pulled from a public Soroban RPC endpoint's `getEvents` against the
curated allowlisted pool, `internal/sources/comet/events.go`'s
`MainnetBackstopPool` (Blend's BLND/USDC backstop,
`CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM`, WASM
`8abc28913035c07411ed5d134e6bfeab4723d97ddd4d1a22a0605d35c94d1a36`).
Comet has exactly one deployed pool (`docs/operations/wasm-audits/comet.md`),
so this single capture covers the whole allowlist per GH-932's fix
direction.

## Self-pair swaps

`<wasm>/self_pair/` holds two real self-pair swaps (`token_in ==
token_out`, `docs/operations/runbooks/amm-self-pair-swap-burst.md`) at
ledgers 64,112,340 and 64,112,891. They are older than the public RPC's
`getEvents` retention (~120k ledgers from the tip), so they were read
from the archival lake (ClickHouse `stellar.contract_events`, ADR-0034),
not stellar-rpc. In ledgers 64,111,000–64,114,000 the pool emitted 928
`POOL` events (700 swap, 156 deposit, 72 exit_pool); 36 of the swaps,
in 36 distinct transactions, are self-pair. The pool has run WASM
`8abc2891…` for its whole life (`stellar.contract_instance_changes`),
so these share the ordinary capture's directory.
`TestRealMainnetFixtures_cometSelfPair` asserts the production path
(`Matches` + `Decode`) yields zero rows and no error, via
`canonical.ErrPairMismatch`; the subdirectory keeps them out of
`TestRealMainnetFixtures_comet`, which expects an ordinary trade.

## Fixture file shape

One event per file: `swap_<ledger>_<tx_hash prefix>_op<op_index>.json`,
fields `contract_id`, `wasm_hash`, `ledger`, `tx_hash`, `op_index`,
`ledger_closed_at`, `topics` (base64 SCVal), `value` (base64 SCVal) —
the same shape `internal/events.Event` uses, so a fixture unmarshals
straight into one.
