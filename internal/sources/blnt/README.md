# BLNT V3 event adapter

This package indexes the independently deployed
[BLNT V3](../../../docs/protocols/blnt.md) lending protocol under the distinct
source name `blnt`. It does not reuse the official Blend source or its V1/V2
tables.

## Scope

The decoder recognizes every event topic in the frozen V3 pool, pool factory,
backstop and emitter source, plus the immutable BLNT backfill and lockup-backfill
contracts. Every accepted event becomes one `blnt_events` row carrying:

- canonical ledger, transaction, operation and event identity;
- the emitting contract's verified role;
- decoded topic-zero event type; and
- the complete topic and value SCVal XDR, losslessly preserved as base64.

The projection is an event catalogue, not a spot-price source and not yet a
derived lending-position index. It never emits `canonical.Trade` and never
enters VWAP. Keeping the complete XDR makes future typed position, tier and
auction projections reproducible without incorrectly reusing Blend V2 schemas.

## Identity gate

The symbols are generic (`claim`, `withdraw`, `distribute`, and others), so
topic-only matching would ingest unrelated contracts. `Decoder.Matches`
requires the canonical factory/module identity or a pool admitted by the
factory's canonical `deploy` event. The two existing mainnet pools are seeded
for cold starts; later factory children are registered in-stream and persisted
through the standard `contractid` hook.

## Replay status

The source is enabled from `GenesisLedger`. All six immutable WASM lineages are
listed in the [per-WASM audit](../../../docs/operations/wasm-audits/blnt.md),
and hash-scoped fixtures from the public Galexie archive cover every contract
role. Replays use the standard projector path and fail closed if any admitted
contract ran an unaudited hash in the requested range.
