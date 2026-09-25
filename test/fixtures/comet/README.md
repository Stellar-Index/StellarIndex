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

## Known gap

The 2026-08-25 Blend/Comet exploit self-pair swaps
(`docs/operations/runbooks/amm-self-pair-swap-burst.md`, ledger
~64,112,340) are **not** captured here: the public RPC's `getEvents`
retention window only reaches back roughly 120k ledgers from the
chain tip, so that historical ledger is outside its range at capture
time. Proving the self-pair zero-rows path (`decodeSwap` →
`canonical.ErrPairMismatch`) against real exploit bytes needs the
archival lake (ClickHouse raw tier, ADR-0034), not stellar-rpc — a
follow-up for whoever has query access to it.

## Fixture file shape

One event per file: `swap_<ledger>_<tx_hash prefix>_op<op_index>.json`,
fields `contract_id`, `wasm_hash`, `ledger`, `tx_hash`, `op_index`,
`ledger_closed_at`, `topics` (base64 SCVal), `value` (base64 SCVal) —
the same shape `internal/events.Event` uses, so a fixture unmarshals
straight into one.
