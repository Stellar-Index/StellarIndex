# Blend Emitter connector

Ingests protocol-emissions events from Blend's **Emitter** contract —
the piece of the Blend money-market ecosystem that mints and
distributes BLND to the backstop pools. This is a **separate event
surface** from both the pool/pool-factory decoder
([`internal/sources/blend`](../blend/)) and the Backstop decoder
([`internal/sources/blend_backstop`](../blend_backstop/)) — it shares
neither contract address nor full event vocabulary with either
(though its `distribute` topic COLLIDES with the Backstop's; see
"Gating" below).

## Scope

Single canonical mainnet instance, spanning Blend V1→V2:

| Contract | Address |
| --- | --- |
| Emitter | `CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR` |

Verified directly against the certified ClickHouse raw lake
(2026-07-09, ADR-0034 — CH HTTP `:8123`, never MinIO's `:9000`): 469
total events across the contract's whole mainnet history, 4 distinct
topics, **every one single-topic** (`topic_count = 1` uniformly — no
event carries additional topic-encoded fields).

## Events we handle

| Event | Count | Ledger range | Body | Output |
| --- | ---: | --- | --- | --- |
| `distribute` | 465 | 51,524,666 → 63,380,088 | `Vec[Address backstop_id, i128 amount]` | `DistributeEvent` → `blend_emitter_events` |
| `drop` | 2 | 51,499,914, 57,467,292 | `Vec[Vec[Address recipient, i128 amount], ...]` (variable length — observed arities 13 and 3) | `DropEvent` (one per recipient, fanned out) → `blend_emitter_events` |
| `q_swap` | 1 | 56,992,670 | `Map{new_backstop: Address, new_backstop_token: Address, unlock_time: u64}` | `SwapConfigEvent{Kind: q_swap}` → `blend_emitter_events` |
| `swap` | 1 | 57,467,277 | Same Map shape as `q_swap` | `SwapConfigEvent{Kind: swap}` → `blend_emitter_events` |

All four schemas were confirmed by decoding the REAL base64 XDR
bytes for at least one instance of each kind (never assumed from
docs) — see `source_test.go`'s golden fixtures for the exact
ledger / tx_hash / topics_xdr / data_xdr per kind.

### `distribute` — recurring BLND emission

One emission distributed to a backstop pool. `backstop_id` names the
target (both the V1 `CAO3AGAM…` and V2 `CAQQR5SW…` backstops have
been observed as the recipient across the 465 events). The dominant
event by volume — one per emission cycle.

### `drop` — one-shot airdrop, VARIABLE-LENGTH recipient list

Only 2 lifetime occurrences: the genesis-era airdrop (ledger
51,499,914 — 13 recipients, including both backstop contracts) and a
later one (ledger 57,467,292 — 3 recipients). The outer `Vec` length
is **not fixed** — decode a variable-length vec, never a hard-coded
arity. Each contract event is ONE Soroban event carrying ALL
recipients in its body; `DropEvent.Recipients` holds the whole slice,
and the storage writer fans it out **one row per recipient** with a
`recipient_index` discriminator in the PK (the same "coarse PK
collapses a multi-row emission" lesson Phoenix's `event_index` and
Aquarius's `token_index` already codify — see `comet_liquidity`
migration 0059's postmortem, and `aquarius_reserves` migration 0089
for the closest precedent: one event → N fanned rows).

### `q_swap` / `swap` — backstop-swap timelock lifecycle

`q_swap` QUEUES a change of which backstop (and backstop LP token)
the Emitter targets, subject to `unlock_time` (Unix seconds). `swap`
EXECUTES a previously queued change once the timelock elapses. Body
shape is byte-identical between the two; verified against the real
fixtures that the one observed `swap` executed exactly what the one
observed `q_swap` queued (same `new_backstop` / `new_backstop_token`
/ `unlock_time` values — `unlock_time` lands within seconds of the
`swap` event's own ledger-close time, consistent with a timelock that
had just elapsed).

## Gating — contract identity, NOT topic bytes (ADR-0035/0040)

**`distribute` COLLIDES with `internal/sources/blend_backstop`'s own
`distribute` event.** The Backstop's `distribute` body is a bare
`i128 amount` (see `blend_backstop/README.md`); the Emitter's is
`Vec[Address backstop_id, i128 amount]` — different shape, same
topic symbol. Routing on topic bytes alone would either misfire onto
a Backstop-emitted `distribute` or silently disagree on body shape.

`Decoder.Matches` therefore gates on **contract identity**: an event
is attributed to `blend_emitter` only when it was emitted by a
contract in the curated registry. The Emitter has **no factory
namespace** — no creation event announces a new instance — so this
is the ADR-0040 §1 curated-set mechanism (mechanism 3), the same
shape as `comet.MainnetGatedSet()`: the in-code seed (today exactly
the one known mainnet Emitter) is the trust root, and the
`protocol_contracts` DB warm (`gated[blend_emitter]` in
`internal/pipeline/gated_registry.go`) layers any operator-admitted
future instance on top — fail-closed by design (a genuine second
Emitter must be operator-admitted before its events attribute; an
unregistered look-alike surfaces in the ADR-0033 recognition audit
instead of silently attributing).

## Backfill safety — audited, `BackfillSafe = true`

`BackfillSafe = true` in `internal/sources/external/registry.go`,
audited 2026-07-10 (`docs/operations/wasm-audits/blend_emitter.md`).
The audit ran read-only against the r1 ClickHouse raw lake (no
`stellarindex-ops wasm-history` / MinIO walk) and checked EVERY one
of the contract's 469 lifetime events — not a sample — against the
decoder's expected shapes: all 465 `distribute` events match
exhaustively via a single shape-check query, and both `drop` events
plus the one `q_swap` and one `swap` were individually decoded. The
contract's sole confirmed on-chain WASM hash
(`438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`)
was extracted from the lake and SHA256-verified byte-for-byte, and
contains every symbol the decoder relies on. The audit did **not**
corroborate an earlier "up to 3 WASM uploads" hypothesis — see the
audit doc's "WASM timeline" section for the correction.

## Files

| File | Role |
| --- | --- |
| [`events.go`](events.go) | Package doc, `SourceName`, `MainnetEmitter`, `MainnetGatedSet()`, topic constants, errors |
| [`decode.go`](decode.go) | Pure decode-from-event → typed field structs (`distributeFields`, `dropRecipient`, `swapConfigFields`) |
| [`consumer.go`](consumer.go) | `consumer.Event` types: `DistributeEvent`, `DropEvent` (+ `Recipient`), `SwapConfigEvent` (+ `SwapConfigKind`) |
| [`dispatcher_adapter.go`](dispatcher_adapter.go) | `Decoder` — the dispatcher-facing seam; contract-identity gate + topic routing |
| [`source_test.go`](source_test.go) | Real-mainnet-fixture golden tests (one per event kind), reject tests, foreign-contract gate rejection test |

## Wiring

- `internal/config/validate.go` — `KnownSources["blend_emitter"]`.
- `internal/pipeline/gated_registry.go` — `gatedSources[blend_emitter.SourceName]` (curated-set gate, `Genesis: 51_499_914` — the earliest observed Emitter event, the `drop` airdrop).
- `internal/pipeline/dispatcher.go` — `BuildDispatcher`: `case blend_emitter.SourceName:` registers `blend_emitter.NewDecoder(gated[blend_emitter.SourceName]...)`.
- `internal/pipeline/sink.go` — `HandleEvent` persists `DistributeEvent` / `DropEvent` / `SwapConfigEvent`; `IsProjectedEvent` claims all three (this is a projected Soroban source, ADR-0031/0032).
- `internal/projector/registry.go` — `buildSource` registers the same gated decoder for the projector's catch-up path.
- `internal/sources/external/registry.go` — `Registry["blend_emitter"]`: `Class: ClassLending` (same family as `blend` — protocol-emissions plumbing, not an independent market), `IncludeInVWAP: false` (never publishes a price), `BackfillSafe: true` (audited 2026-07-10, see above).
- `cmd/stellarindex-ops/reconciliation_catalogue.go` — a `reconSource` entry so the ADR-0033 completeness verdict covers this source (`contractIDs: []string{blend_emitter.MainnetEmitter}`, matching the CCTP/comet precedent for a small curated contract set).
- Storage: `blend_emitter_events` hypertable, migration
  [`0096_create_blend_emitter_events`](../../../migrations/0096_create_blend_emitter_events.up.sql).

## Aggregator treatment

`Class: ClassLending`, `IncludeInVWAP: false` — the Emitter publishes
no price; it moves BLND supply between the protocol's own emission
schedule and its backstops. Reported for transparency (protocol-flow
visibility), never contributes to VWAP.

## References

- Related sources: [`blend`](../blend/README.md) (pool + pool-factory),
  [`blend_backstop`](../blend_backstop/README.md) (Backstop insurance
  module — shares the colliding `distribute` topic symbol).
- Protocol page: [`docs/protocols/blend_emitter.md`](../../../docs/protocols/blend_emitter.md).
- Gating precedent: [`internal/sources/comet/README.md`](../comet/README.md)
  (`MainnetGatedSet` curated-set pattern for a factory-less source).
