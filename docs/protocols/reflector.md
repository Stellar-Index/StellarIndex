# Reflector — contract & event verification

> **For the Reflector team:** this is the set of Reflector oracle
> contracts Stellar Index ingests and how we attribute their price
> updates. Please confirm the three mainnet addresses below are current
> (a v4 spawn that rotates any address needs a config update on our side,
> or we silently stop ingesting that feed).
>
> - **Enumeration method:** operator-pinned contract IDs — each of the
>   three Reflector contracts is configured explicitly in
>   `[oracle.reflector]` and the decoder only attributes events from that
>   exact contract.
> - **Last verified:** 2026-07-06 (source: `internal/sources/reflector`;
>   real-mainnet event fixtures `test/fixtures/reflector/v6-2026-04-23/`;
>   WASM audit `docs/operations/wasm-audits/reflector.md`, 2026-04-29).
> - **Gate status:** ✅ Gated (ADR-0035): each source instance matches
>   ONLY its one configured contract ID.

## What Reflector is

Reflector is a decentralised, SEP-40-compliant oracle network native to
Stellar/Soroban. It is **three separate contracts**, not one — each a
different upstream data source, each attributed under its own source name
so alerts, cursors, and divergence checks break out cleanly:

| Source name | Contract | Mainnet address | Feed | Asset shape |
|---|---|---|---|---|
| `reflector-dex` | Reflector DEX | `CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M` | on-chain Stellar DEX prices | `Asset::Stellar(Address)` |
| `reflector-cex` | Reflector CEX | `CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN` | aggregated CEX prices | `Asset::Other(Symbol)` (e.g. `BTC`) |
| `reflector-fx` | Reflector FX | `CBKGPWGKSKZF52CFHMTRR23TBWTPMRDIYZ4O2P5VS65BMHYH4DXMCJZC` | fiat + commodity FX | `Asset::Other(Symbol)` (e.g. `EURUSD`) |

Verify addresses via stellar.expert before pasting into config — the DAO
can rotate them on a v4 spawn.

## Event decoded — one event, N updates

Verified against `reflector-contract/oracle/src/events.rs`. Each contract
emits the **same event shape** per price update:

```
topic:  ["REFLECTOR", "update", <timestamp: u64 ms>]
body:   Map{ "update_data": Vec<(Val, i128)> }        // [(asset, price), ...]
```

Decoding one event produces **one `canonical.OracleUpdate` per
(asset, price) pair** in the `update_data` vector (a CEX event typically
carries 30–50 prices; fewer on DEX / FX). Rows land in `oracle_updates`.

Provenance details that matter for correctness:

- **The timestamp is in `topic[2]`** as a `u64` in **milliseconds** (the
  contract divides by 1000 to expose seconds via `last_timestamp`), NOT
  in the body. Earlier drafts of the package README described a
  `body: Map{prices, timestamp}` shape — that was never the wire form.
- **Price is an `i128` at contract-declared decimals** (the SEP-40
  `decimals()` method, typically 14). We store the raw `i128` plus the
  decimals; the display layer scales on read (never float — ADR-0003).
- **Relayer identity** = the update tx's `source_account`, stashed on
  `OracleUpdate.Observer` so divergence analysis can detect a single
  relayer compromise.

## What Reflector does NOT expose

- **No on-chain `twap` / `x_twap` / `x_last_price` methods.** Reflector
  v3 does not have them, despite some docs implying otherwise. We compute
  TWAP and cross-pair locally in `internal/aggregate`.

## Aggregator treatment — reported, not counted

Class `Oracle` / `IncludeInVWAP=false` (`external.Registry`). Reflector
publishes already-aggregated derived prices with its own governance and
methodology, so it is surfaced on `/v1/sources` and used as a divergence
cross-check, but it does **not** contribute to our VWAP. An operator can
opt a single oracle into VWAP per-source via config, but the default is
excluded (mixing an oracle's methodology into our market-trade average
would double-impose their aggregation on our output).

## Topic census re-confirmation (ROADMAP #89, 2026-07-10)

A read-only lake topic census against all three configured contracts
found only the `REFLECTOR` topic[0] (46,045 events across the three)
— consistent with "one event, N updates" above. `topic[1]` breakdown
(always `update`) isn't visible from the fast pre-aggregated census
path for Symbol-namespaced protocols; no ambiguity given there is
only one documented event shape.

## Backfill safety

`BackfillSafe = true` (audited 2026-04-29; v2 disassembly confirms
decoder compatibility across the WASM versions that ran). Reflector's
events are durable Soroban events, so backfill from a galexie archive
decodes identically to live ingest.

## Update cadence / staleness

Each contract updates on a uniform ~5-min cadence on mainnet. The
`oracle-stale` alert fires at > 10× the declared resolution (= 50 min
without an update), which flags an upstream halt (e.g. a CEX-aggregator
outage) per contract independently.

## References

- Source package: `internal/sources/reflector/README.md`
- SEP-40 (oracle interface); ADR-0013 (SCVal decoding)
- Sibling oracles: [redstone.md](redstone.md), [band.md](band.md)
