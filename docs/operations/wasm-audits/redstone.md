---
title: Redstone WASM-history audit
last_verified: 2026-09-30
status: ratified — archived hashes are code-identical and emit no events; the event-emitting successor hash is not yet archived
source: redstone
backfill_safe: true
---

# Redstone WASM audit

Audit log for the `redstone` source's `BackfillSafe` flag. See
`README.md` for the full procedure.

> **2026-09-30 update — byte-level comparison of the archived hashes.**
> Offline disassembly of both archived WASMs (`wasm-objdump -h/-x/-d`,
> `stellar contract info interface|meta --wasm`, a per-section byte
> compare) shows:
>
> 1. **`b400f7a8…` and `5e93d22c…` are the same program.** Every
>    section except `contractspecv0` is byte-identical — Code (34,400
>    bytes), Data (4,449 bytes), imports, exports, `contractmetav0`
>    (rustc 1.85.0, soroban-sdk 22.0.8). The only spec difference is
>    `upgrade`'s argument type: the `WasmHash` alias (an undeclared UDT
>    name) in `b400f7a8…`, `BytesN<32>` in `5e93d22c…` — the whole 8-byte
>    size delta. `write_prices(updater: Address, feed_ids: Vec<String>,
>    payload: Bytes)` and `PriceData {package_timestamp: u64, price: U256,
>    write_timestamp: u64}` are identical in both.
> 2. **Neither hash can emit the REDSTONE event.** Both lack the
>    `contract_event` host import (`x.1`) — which 47 of the 52 archived
>    WASMs import, and which Band's `6cdb9a3c…` (no events, per
>    [band.md](band.md)) also lacks — and neither contains the
>    `updated_feeds` body key anywhere in its bytes. The `REDSTONE`
>    topic is an 8-char small symbol, so its absence from the ASCII
>    strings is expected and proves nothing either way.
>
> Consequences: the decoder is event-driven (`classify` requires
> topic[0] `REDSTONE`), so no redstone row can come from either hash
> and the first-deploy window is empty by construction. The walk JSONs'
> timeline is wrong past that point: `internal/sources/redstone/subset_test.go`
> is a real lake REDSTONE event from the same `CA526Y2N…` address at
> L59,258,375 — inside the range the walk JSONs give `5e93d22c…`
> (ending L59,336,871 / L59,301,651) — so an event-emitting WASM was
> already active there and the walk missed at least one upgrade. The
> 2026-05-03 "no further upgrades through L62,249,727" below is
> contradicted by the same evidence. That successor hash is not in the
> byte archive and still needs a per-hash entry (see [Caveats](#caveats)).
>
> **2026-05-03 update — v2 walk confirms two-hash inventory.**
> The 2026-04-30 wide-net r1 walk re-observed the 35-min
> first-deploy hotfix (`b400f7a8…` at L58,758,722-L58,759,141)
> followed by the production hash (`5e93d22c…` from L58,759,142).
> No further upgrades observed through the walk's upper bound
> (L62,249,727) — contradicted since; see the 2026-09-30 update. Bytes preserved + SHA-256-verified for both
> hashes at `evidence/r1-walk-2026-05-01/wasm-bytes/`. With both
> hashes' bytes now archived, the prior caveat about not having
> bytes for `b400f7a8…` is closed: a future deeper audit can
> compare its event-publish + write_prices signature against the
> production hash without re-fetching from a public RPC.
>
> **2026-05-01 update.** Hash citations in this file have been
> cross-checked against the 2026-04-30 r1 walk; see
> [r1-walk-2026-05-01.md](r1-walk-2026-05-01.md) for the
> consolidated cross-source picture and current contract+WASM
> inventory.

## Status

**Ratified 2026-04-29.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same PR as this
audit. Two WASM hashes observed across the post-Soroban scan
window — a 420-ledger (~35 min) hotfix immediately after first
deploy, then `5e93d22c…`, which the walk records through scan-end.
A real REDSTONE event at L59,258,375 shows the walk missed an
upgrade to a later event-emitting hash inside that range (see
Caveats). Per-hash review + hotfix-window analysis below.

## Contracts under audit

| role | mainnet contract |
| --- | --- |
| Adapter | `CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG` |

The address is configured via `cfg.Oracle.Redstone.AdapterContract`
in `stellarindex.toml`; the value above is the published mainnet
contract. Redstone uses a
**single Adapter contract** that owns price storage for every feed
(plus thin per-feed proxies that read from the adapter — proxies
emit no events and are not in scope for this audit).

## Decoder expectations

Captured from `internal/sources/redstone/{events,decode}.go` at HEAD
as of 2026-04-29. Re-verified 2026-04-23 against the upstream
`redstone-public-contracts` repo.

### Topic structure

    topic[0] = ScvSymbol("REDSTONE")
    body     = ScvMap {
        "updater":       ScvAddress,
        "updated_feeds": ScvVec<PriceData>,
    }
    PriceData = ScvMap {
        "price":             ScvU256,    // U256 — this is unique to Redstone
        "package_timestamp": ScvU64,
        "write_timestamp":   ScvU64,
    }

Single-element topic — only `topic[0] = "REDSTONE"`. No second
slot; classification is byte-equal against `TopicSymbolRedstone`
alone.

### The "feed IDs are in op args, not event body" trap

AGENTS.md flags this:

> **Redstone's event body carries no feed_id.** `WritePrices
> { updater, updated_feeds: Vec<PriceData> }` gives prices +
> timestamps, not which feed each entry is. Feed IDs live in the
> tx's `write_prices(updater, feed_ids, payload)` InvokeContract
> op args — plumbed through `events.Event.OpArgs`.

The decoder zips `feed_ids` (from op args) against `updated_feeds`
(from event body) one-to-one. If the adapter's freshness verifier
rejects a feed, it skips that entry in `updated_feeds` WITHOUT
skipping in `feed_ids` — we guard with a strict length check
(`ErrFeedIDCountMismatch`) and skip the whole event rather than
risk attributing a BTC price to ETH.

### Body extraction

Decoder pulls **by name** (Map-keyed) — same robust pattern as
Soroswap and Comet:

| field | extracted by | invariant |
| --- | --- | --- |
| `updater` | `scval.AsAddressStrkey` | valid Soroban Address |
| `updated_feeds` | iterated as Vec | each entry is a PriceData Map |
| `PriceData.price` | `scval.AsU256ToBigInt` | **U256** (not i128 like every other source) |
| `PriceData.package_timestamp` | `scval.AsU64` | seconds |
| `PriceData.write_timestamp` | same | same |

### Function-call gating

The decoder only trusts op args from calls to `write_prices`. Any
other function call (e.g. a composed tx that also calls a different
Redstone method) is rejected with `ErrWrongFunctionCall`. This
defends against decoding an unrelated call's args as feed IDs.

### Known-feeds allow-list

Per AGENTS.md, Redstone has **19 mainnet feeds** (at audit time —
the relayer expanded to 30 feeds on 2026-07-24, then to 32 (Upshift,
earnUSDC); the live list is `internal/sources/redstone/feeds.go`,
per-feed evidence in `docs/protocols/redstone.md`). Feed IDs from op
args that aren't on the known-feeds allow-list are skipped per-entry
with `ErrUnknownFeedID` (other feeds in the same event still land).
A new feed listed on Redstone's mainnet adapter without our
allow-list update is silently dropped — the list lives in the
package's known-feeds allow-list. That exact failure mode played
out in the 2026-07-24 expansion (~5,600 events dropped fail-closed
until the registry caught up on 2026-07-27).

## Failure modes specific to Redstone

1. **Topic[0] symbol change** — `"REDSTONE"` to anything else
   silently drops every event.
2. **Body field rename** (`updated_feeds` → `feed_updates`,
   `updater` → `caller`, etc.) — by-name extraction errors per
   event; every event dropped under that WASM.
3. **PriceData field rename** — same as #2 but for the inner Map.
4. **`price` type change U256 → i128** — strict
   `AsU256ToBigInt` errors per entry. Fail-loud, every entry
   dropped. **Unique to Redstone** — every other on-chain price
   source uses i128.
5. **Decimals scale change** — Redstone documents 8 decimals
   universally. A switch to 18 decimals (matching Band's E18)
   silently mis-reports every price. **No automated detection** —
   caught only by cross-source divergence vs Reflector / Band.
6. **`write_prices` function signature change** (renamed,
   reordered args) — the decoder reads `feed_ids` from op args
   by position. A reorder would zip the wrong identifiers against
   prices. Per-WASM source review must verify the function
   signature is unchanged.
7. **Feed-ID encoding change** (String → Symbol → Bytes) — strict
   extraction errors per entry. Every entry dropped, but the
   error tells the operator what changed.
8. **Adapter and proxy split** — if Redstone refactored to publish
   events from per-feed proxies instead of the central adapter,
   our decoder would still match the topic but the op-args plumbing
   breaks (write_prices no longer the producing call).
9. **Heartbeat / freshness change** — the documented `0.2%
   deviation OR 24h heartbeat` rule could change. We expose
   `DefaultResolutionSeconds = 24h` for the staleness alert. If
   the heartbeat shortens, no decoder issue but the alert tuning
   is wrong.

## WASM timeline

Output from `stellarindex-ops wasm-history` over the post-Soroban
window — full archive on r1, walked 2026-04-29:

```json
[
  {
    "contract": "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG",
    "ranges": [
      { "wasm_hash": "b400f7a8ac121022955be1bd2468fcb99f126d2aa2fcc185a6abba36e83a3ef2",
        "from_ledger": 58758722, "to_ledger": 58759141 },
      { "wasm_hash": "5e93d22c9e19b254dae5474aebbb65a39f2f53b3b1d4371c58281987e1e29945",
        "from_ledger": 58759142, "to_ledger": 59301651 }
    ]
  }
]
```

Two distinct hashes:

- **`b400f7a8…`** active for **420 ledgers (~35 min)** —
  L58,758,722 → L58,759,141. This is the first observed event for
  the contract anywhere in the post-Soroban scan window, so it
  represents the **first-deploy** WASM. The 35-min lifetime is the
  unmistakable signature of a deploy-then-hotfix pattern (Redstone
  dev pushed v1, found a bug within 35 min, pushed v2).
- **`5e93d22c…`** recorded from L58,759,142 → L59,336,871
  (walk-end in `per-source-final/redstone.json`). That end is wrong:
  this hash emits no events, yet a real REDSTONE event from this
  address exists at L59,258,375, so the walk missed an upgrade to an
  event-emitting, unarchived WASM somewhere in L58,759,142 →
  L59,258,375. `5e93d22c…`'s true end is unknown (see Caveats).

The contract did not exist on mainnet before L58,758,722
(2025-08-29 ± a day): no `CreateContract` or
`update_current_contract_wasm` events for this address in any
worker chunk before that ledger.

## Per-hash review findings

| hash (first 16) | role | active range | reviewer | finding |
| --- | --- | --- | --- | --- |
| `b400f7a8ac121022` | Adapter (first-deploy hotfix) | L58,758,722 → L58,759,141 (420 ledgers, ~35 min) | maintainer@2026-04-29; bytes 2026-09-30 | safe — code-identical to `5e93d22c…`; emits no events |
| `5e93d22c9e19b254` | Adapter | from L58,759,142; true end unknown, before L59,258,375 (the walk JSONs' L59,336,871 / L59,301,651 end is contradicted by a real REDSTONE event at L59,258,375) | maintainer@2026-04-29; bytes 2026-09-30 | safe — emits no events; NOT the decoder's event source |

### `5e93d22c9e19b254` — second deploy, no events

- The bytes contradict the original reading of this hash as the
  decoder target: it has no `contract_event` import and no
  `updated_feeds` key (see the 2026-09-30 update above). The decoder
  fixtures in `internal/sources/redstone/*_test.go` (L59,258,375 and
  later) were emitted by a successor WASM on the same address.
- `write_prices(updater, feed_ids, payload)` op-args signature
  matches the positional reader in `decode.go` (from the embedded
  `contractspecv0`).
- U256 price type matches `scval.AsU256ToBigInt`.
- **Live ingest health**: 0 `ErrFeedIDCountMismatch` /
  `ErrWrongFunctionCall` / `ErrUnknownFeedID` rate spikes since
  the ContractCallDecoder hook landed (commit `ee0360da4`, "wire
  band + comet + redstone decoders") — a property of the successor
  WASM's events, not of this hash.
- The adapter was upgraded at least once after this hash: an
  event-emitting WASM was live no later than L59,258,375 (the real
  event in `subset_test.go`), inside the range the walk JSONs
  attribute to `5e93d22c…`. The walk missed that upgrade.

### `b400f7a8ac121022` — first-deploy hotfix, 35-min lifetime

This hash was on chain for 420 ledgers (~35 min) before being
replaced by `5e93d22c…`. The pattern is
unambiguous:

- Brand-new contract address (no prior deploy in the entire
  post-Soroban scan window — 8.3M ledgers / ~18 months of
  pre-deploy emptiness).
- 35-min lifetime to the next `update_current_contract_wasm`.
- Replaced by `5e93d22c…`, itself later replaced by an unrecorded
  event-emitting hash (see Caveats).

The bytes settle what the hotfix was: a **spec-only redeploy**. The
Code and Data sections are byte-identical to `5e93d22c…`; only
`upgrade`'s spec argument type changed (`WasmHash` → `BytesN<32>`).
Neither hash imports `contract_event`, so neither emits the REDSTONE
event, and the event-driven decoder yields no rows for this window
regardless of wire format.

**Database check (sanity, pre-backfill)**: redstone rows land in
`oracle_updates`, not `trades`. Before a replay overlapping
L58,758,722 → L58,759,141 the count is expected to be `0`:

    psql -h localhost stellarindex -c "
      SELECT count(*) FROM oracle_updates
       WHERE source = 'redstone'
         AND ledger BETWEEN 58758722 AND 58759141"

A non-zero count would mean the WASM timeline for this address is
wrong (an event-emitting hash active inside the window), not that
`b400f7a8…` decodes differently — re-walk the address before the
replay proceeds.

## Caveats

- **Walk timeline wrong; successor adapter WASM not archived or
  audited.** The events the decoder consumes come from a hash newer
  than `5e93d22c…`, live no later than L59,258,375 — inside the range
  the walk JSONs give `5e93d22c…`, so the recorded end (L59,336,871 /
  L59,301,651) and the "no upgrades through L62,249,727" claim are
  both wrong. A `wasm-history` walk of `CA526Y2N…` from L58,759,142
  (`5e93d22c…`'s first ledger) to the archive tip must record the real
  upgrade ledger(s), hash(es) and ranges and archive the bytes, and
  each needs a per-hash entry above. Until then the decoder's match to
  that WASM rests on the real-event fixtures in
  `internal/sources/redstone/*_test.go`.
- **Database emptiness check is point-in-time.** If a future
  Redstone backdated correction lands events into the
  L58,758,722 → L58,759,141 range, they cannot come from
  `b400f7a8…` (it emits no events). Re-verify the database-emptiness invariant
  before any backfill that targets that exact window.

## Decision

**`BackfillSafe: true`** — flipped in
`internal/sources/external/registry.go` in this PR.

Rationale:

- Both archived hashes (`b400f7a8…`, `5e93d22c…`) are the same
  code and emit no events, so a replay yields no redstone rows for
  `b400f7a8…`'s window or for the part of `5e93d22c…`'s range before
  the real (unrecorded) upgrade, and cannot mis-attribute any there.
  The first-deploy window needs no wire-format argument. The rest of
  `5e93d22c…`'s recorded range contains real REDSTONE events from the
  successor WASM and is covered by the point below, not this one.
- The event-emitting successor WASM (see Caveats) is the one the
  decoder targets; the real-event fixtures pass against it and live
  ingest is healthy, but it still owes a per-hash entry.

If a future Redstone upgrade lands, the audit gets a per-hash
entry + decoder verification and the flag stays at `true` (or
flips to `false` if the new WASM diverges and the decoder fix
isn't shipped yet).

## References

- Procedure: `docs/operations/wasm-audits/README.md`
- Decoder source: `internal/sources/redstone/{events,decode}.go`
- Schema-evolution stance: `docs/architecture/contract-schema-evolution.md`
- Backfill gate: `internal/sources/external/registry.go` —
  `Registry["redstone"].BackfillSafe`
- Upstream contract source: pinned in `VERSIONS.md`
- WASM-history walk JSON (full): `r1:/var/log/wasm-history-all.json`
