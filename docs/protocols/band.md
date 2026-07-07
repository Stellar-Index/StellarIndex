# Band — contract & event verification

> **For the Band team:** this is how Stellar Index ingests Band's Soroban
> StandardReference oracle. Band emits **zero events** (see Q1), so we
> observe the relayer's contract calls instead. Please confirm the
> mainnet address and tell us if a second StandardReference contract is
> deployed (we pin one).
>
> - **Enumeration method:** single StandardReference contract pinned by
>   `(contract_id, function_name)` — the decoder matches the relayer's
>   `relay` / `force_relay` InvokeContract calls, not events.
> - **Last verified:** 2026-07-06 (source: `internal/sources/band`;
>   zero-events verified 2026-04-22/24 against pinned upstream source;
>   WASM audit `docs/operations/wasm-audits/band.md`, 2026-04-29).
> - **Gate status:** ✅ Gated (ADR-0035): matches ONLY the pinned
>   contract ID + the two known function names.

## What Band is

[Band Protocol](https://www.bandprotocol.com)'s Soroban
StandardReference contract publishes USD-denominated single-symbol rates
on a deviation-driven relayer cadence (BTC / ETH / common majors plus a
Band-maintained symbol set).

| Role | Mainnet address |
|---|---|
| StandardReference | `CCQXWMZVM3KRTXTUPTN53YHL272QGKF32L7XEDNZ2S6OSUFK3NFBGG5M` |

## The wire pattern — Band emits zero events (Q1)

Verified 2026-04-22 via grep across
`bandprotocol/band-std-reference-contracts-soroban`, re-confirmed
2026-04-24 against the pinned source: a conventional event-topic
`dispatcher.Decoder` would **never fire** for Band. Instead this package
plugs into `dispatcher.ContractCallDecoder` — it observes the
`InvokeContract` op itself and decodes the relayer's call args as the
authoritative payload. (Soroswap-Router uses the same hook; any future
event-less Soroban source that mutates storage on a known call slots in
the same way: match by `(contract_id, function_name)`, decode from op
args.)

## Calls decoded

Both produce one `canonical.OracleUpdate` per `(symbol, rate)` pair:

| Function | Args | Notes |
|---|---|---|
| `relay` | `(from, symbol_rates, resolve_time, request_id)` | standard relayer path |
| `force_relay` | `(symbol_rates, resolve_time, request_id)` | admin path; no `from` — attribution falls back to op/tx source |

Provenance details:

- **Scale is E9 for single-symbol rates.** The `symbol_rates` Vec we
  decode is `u64` at `E9 = 10^9` (`DefaultDecimals = 9`). Pair rates
  computed on-read via `get_reference_data` are E18 — we do **not** emit
  those (they're a function of storage state, not the wire input).
- **USD is a no-op.** USD is hardcoded to `1 @ E9` in Band's storage; the
  relayer never pushes USD updates, so a `USD` symbol in `symbol_rates`
  is skipped as expected (not a decode error).
- **`resolve_time` is `u64` UNIX seconds** (`time.Unix(…, 0)` →
  `OracleUpdate.Timestamp`); out-of-range values fall back to ledger
  close time.
- **Synthetic op-index fan-out:** one `relay` call → N updates, each
  sharing `(ledger, tx_hash, op_source)` with a unique
  `OpIndex = base + i*1024`.

## Aggregator treatment — reported, not counted

Class `Oracle` / `IncludeInVWAP=false` (`external.Registry`). Surfaced on
`/v1/sources` for transparency, excluded from VWAP.

**Metric caveat:** because Band emits no events,
`stellarindex_source_events_total{source="band"}` reads **zero** by
design — confirm the ContractCallDecoder is firing via the cursor-advance
metric and op-args ingestion counters, not the events counter.

## Backfill safety

`BackfillSafe = true` (audited 2026-04-29). ContractCall observation works
identically over historical ledgers — the dispatcher routes InvokeContract
ops to Band's adapter whether the ledger is live or replayed.

## References

- Source package: `internal/sources/band/README.md`
- ADR-0014 (crypto-ticker representation)
- Sibling oracles: [reflector.md](reflector.md), [redstone.md](redstone.md)
