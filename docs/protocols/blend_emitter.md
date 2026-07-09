# Blend Emitter — contract & event verification

> **For the Blend team:** this documents how Stellar Index attributes
> events from the Emitter contract — the protocol-emissions plumbing
> that mints and distributes BLND to the backstop pools. This page
> answers the open question `docs/protocols/blend.md` (Backstop
> section) raised: **yes, an Emitter-contract event surface exists**,
> and this page + `internal/sources/blend_emitter` now cover it.
> Please confirm the single mainnet address, the four event schemas,
> and whether any additional Emitter events exist beyond the ones
> below.
>
> - **Enumeration method:** direct ClickHouse raw-lake read (ADR-0034)
>   — every event `topic_0_sym` the Emitter contract has EVER emitted
>   on mainnet, grouped and counted.
> - **Last verified:** 2026-07-09 (r1 lake, CH HTTP `:8123`).
> - **Gate status:** ✅ **GATED (curated one-contract allowlist,
>   shipped alongside this page — no ungated window on this source).**

## What the Emitter is

Blend's Emitter contract mints BLND on a schedule and distributes it
to the protocol's backstop pools (see `docs/protocols/blend.md` for
the pool/pool-factory and Backstop decoders). It is a **separate
event surface** from both — it shares neither contract address nor
full event vocabulary with either.

| Contract | Address |
| --- | --- |
| Emitter (single canonical mainnet instance, spans Blend V1→V2) | `CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR` |

## Events decoded

Verified 2026-07-09 directly against the certified ClickHouse raw
lake: 469 total events across the contract's whole mainnet history,
4 distinct topics, **every one single-topic**.

| Event (topic) | Count | Ledger range | Body | Where it lands |
| --- | ---: | --- | --- | --- |
| `distribute` | 465 | 51,524,666 → 63,380,088 | `Vec[Address backstop_id, i128 amount]` | `blend_emitter_events` (event_kind=distribute) |
| `drop` | 2 | 51,499,914, 57,467,292 | `Vec[Vec[Address recipient, i128 amount], ...]` (variable length — observed arities 13 and 3) | `blend_emitter_events` (event_kind=drop, one row per recipient) |
| `q_swap` | 1 | 56,992,670 | `Map{new_backstop: Address, new_backstop_token: Address, unlock_time: u64}` | `blend_emitter_events` (event_kind=q_swap) |
| `swap` | 1 | 57,467,277 | Same Map shape as `q_swap` | `blend_emitter_events` (event_kind=swap) |

Notes:

- **`distribute`** is the dominant event by volume — one per emission
  cycle. Both the V1 backstop (`CAO3AGAM…`) and V2 backstop
  (`CAQQR5SW…`) have been observed as the `backstop_id` across the
  465 events.
- **`drop`** carries a VARIABLE-LENGTH recipient list, not a fixed
  arity — the two lifetime occurrences have 13 and 3 recipients
  respectively. Decode it as a variable-length outer `Vec`, never a
  hard-coded shape. One contract event → N stored rows
  (`recipient_index` discriminator), same "coarse PK collapses a
  multi-row emission" pattern as `aquarius_reserves`' `token_index`
  (migration 0089).
- **`q_swap` / `swap`** share an identical body shape — verified
  byte-identical on the real fixtures. `q_swap` QUEUES a change of
  target backstop (+ backstop token) subject to `unlock_time`
  (Unix seconds); `swap` EXECUTES it once the timelock elapses. The
  one observed `swap` executed exactly what the one observed `q_swap`
  queued (same `new_backstop` / `new_backstop_token` / `unlock_time`).

## Gate status — GATED (curated allowlist, contract identity)

**`distribute` COLLIDES with `blend_backstop`'s own `distribute`
event** (see `docs/protocols/blend.md`'s Backstop section — its
`distribute` body is a bare `i128 amount`, no `backstop_id`). Routing
on topic bytes alone would either misfire onto a Backstop-emitted
`distribute` or silently disagree on body shape.

`Decoder.Matches` therefore gates on **contract identity**
(ADR-0035/0040): an event is attributed to `blend_emitter` only when
emitted by a contract in the curated registry — the in-code
`blend_emitter.MainnetGatedSet()` (today exactly the one known
mainnet Emitter) plus the `protocol_contracts` DB warm. The Emitter
has **no factory namespace** to anchor a deploy-graph gate on (same
situation as Comet), so this is the ADR-0040 §1 curated-set
mechanism.

**Admitting a future instance:** fail-closed by design. A genuinely
new Emitter deployment must be operator-admitted before its events
attribute — `stellarindex-ops seed-protocol-contracts -source
blend_emitter` (after adding it to the curated set) or a direct
`protocol_contracts` row.

## Backfill safety — OPEN wasm-audit item

`BackfillSafe = false` in `internal/sources/external/registry.go`.
The Emitter's single mainnet address shows **up to 3 WASM uploads**
(ledgers 51,351,843 / 51,498,920 / 52,314,704) spanning Blend V1→V2.
Event-schema stability ACROSS those versions has **not yet** been
confirmed by a `stellarindex-ops wasm-history` audit — live ingest
works without it; `stellarindex-ops backfill` refuses to run this
source against a historical range until the audit lands and flips
the flag.

## Storage

| Event | Hypertable | Migration |
| --- | --- | --- |
| `distribute` / `drop` / `q_swap` / `swap` | `blend_emitter_events` | 0095 |

## Aggregator treatment

Class `Lending` (same family as `blend`) / `IncludeInVWAP=false`
(`external.Registry`) — the Emitter publishes no price; it moves BLND
supply between the protocol's own emission schedule and its
backstops. Reported for protocol-flow transparency, never contributes
to VWAP.

## References

- Related sources: [`blend`](../../internal/sources/blend/README.md)
  (pool + pool-factory), [`blend_backstop`](../../internal/sources/blend_backstop/README.md)
  (Backstop insurance module — shares the colliding `distribute`
  topic symbol), [`internal/sources/blend_emitter`](../../internal/sources/blend_emitter/README.md).
- Gating precedent: [`docs/protocols/comet.md`](comet.md) (curated
  one-contract allowlist for a factory-less source).
- [ADR-0035](../adr/0035-factory-anchored-contract-gating.md),
  [ADR-0040](../adr/0040-completing-contract-gating.md).
