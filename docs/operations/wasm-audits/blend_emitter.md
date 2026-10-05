---
title: Blend Emitter WASM-history audit
last_verified: 2026-07-10
status: complete -- BackfillSafe=true (2026-07-10, ClickHouse-lake-only audit, no wasm-history walk)
source: blend_emitter
backfill_safe: true
---

# Blend Emitter WASM audit

Audit log for the `blend_emitter` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

## Status

**APPROVED 2026-07-10.** `BackfillSafe` flipped `false` -> `true` in
`internal/sources/external/registry.go` in the same commit as this doc.

No `stellarindex-ops wasm-history` run (MinIO galexie-archive walk): only
read-only ClickHouse HTTP (`:8123`) queries against the certified raw lake, the
same shape as the `rozo.md` 2026-07-09 addendum. Evidence is arguably stronger
than a typical walk: all 469 lifetime events were shape-checked (465/465
`distribute` exhaustively; both `drop` events, the one `q_swap`, the one `swap`
individually decoded), 100% coverage, not a sample.

## Contract under audit

| Role | Contract | Confirmed WASM hash |
| --- | --- | --- |
| Emitter (single canonical mainnet instance) | `CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR` | `438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89` |

No factory namespace exists (curated one-contract allowlist,
`blend_emitter.MainnetGatedSet()`; see events.go / README.md "Gating"). The
audit unit is this one address across its observed lifetime, same shape as
`comet.md`.

## Method

1. **Event census** (`stellar.contract_events`, `contract_id =
   'CCOQM6S7...'`): 469 total events / 4 topics / all `topic_count=1`:
   `distribute=465`, `drop=2`, `q_swap=1`, `swap=1`. Zero orphan or
   empty-`topic_0_sym` rows (guards the CH gotcha where `topic_0_sym` can be
   empty); all 469 rows `in_successful_call=1`.
2. **Exhaustive shape verification.** One SQL query byte-shape-checked **all
   465** `distribute` rows (constant 20-byte prefix through the `SCV_ADDRESS`
   tag, `SCV_I128` discriminant at byte 53, fixed 72-byte total XDR length):
   `465/465` match. The other 4 events (both `drop`s, `q_swap`, `swap`) were
   individually decoded with a from-scratch Python SCVal/XDR parser (no
   `go-stellar-sdk` for ad-hoc scripting; validated by reproducing `comet.md`'s
   WASM hash `8abc28913035c074...` for the Comet backstop pool from an
   unrelated contract-instance entry in the same lake).
3. **WASM-bytes extraction + SHA256 verification.** The `contract_code` entry
   came from `stellar.ledger_entry_changes` (`entry_type='contract_code'`,
   exact key match on the 36-byte `LedgerKey::ContractCode{hash}`). The XDR
   `opaque code<>` declared length (10,448 bytes) sliced the module precisely
   (not to end of buffer, which would include XDR ext/padding) --
   `sha256(wasm_bytes) ==
   438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`
   exactly -- the strongest form of evidence available short of a full
   disassembler. WASM saved at
   [`evidence/blend_emitter/emitter-438a5528cff17ede.wasm`](evidence/blend_emitter/emitter-438a5528cff17ede.wasm).
4. **Symbol presence check** against the verified bytes (see
   [`evidence/blend_emitter/emitter-438a5528cff17ede.symbols.txt`](evidence/blend_emitter/emitter-438a5528cff17ede.symbols.txt)):
   present: `distribute`, `q_swap`, `swap`, `drop`, `new_backstop`,
   `new_backstop_token`, `unlock_time`, `LastDistro`, `Dropped`,
   `BackstopBToken`, `SwapBLNDTkn`, `backstop`, `del_swap`,
   `queue_swap_backstop`, `cancel_swap_backstop`, `swap_backstop`,
   `get_last_distro`, `get_backstop`, `get_queued_swap`, `IsInit`,
   `blnd_token`, `initialize`.

Full query text + results:
[`evidence/blend_emitter/shape-verification-2026-07-10.md`](evidence/blend_emitter/shape-verification-2026-07-10.md).

## Decoder expectations

From `internal/sources/blend_emitter/{events,decode}.go` at HEAD 2026-07-09 (per-event detail in their doc comments):

| event | topic | body | decoder output |
| --- | --- | --- | --- |
| `distribute` | `[Symbol("distribute")]` | `Vec[Address backstop_id, i128 amount]` | `DistributeEvent` |
| `drop` | `[Symbol("drop")]` | `Vec[Vec[Address recipient, i128 amount], ...]` (variable length) | `DropEvent` (fanned out, one row per recipient) |
| `q_swap` | `[Symbol("q_swap")]` | `Map{new_backstop: Address, new_backstop_token: Address, unlock_time: u64}` | `SwapConfigEvent{Kind: SwapConfigQueued}` |
| `swap` | `[Symbol("swap")]` | same Map shape as `q_swap` | `SwapConfigEvent{Kind: SwapConfigExecuted}` |

All four are single-topic (`topic_count=1` across all 469 events, confirmed).

## WASM timeline

**One confirmed WASM hash across the entire observed lifetime:** `438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`.

| Signal | Finding |
| --- | --- |
| Contract-instance snapshot (`stellar.ledger_entry_changes`, exact key match on the Emitter's own `LedgerKey::ContractData{contract, key=LEDGER_KEY_CONTRACT_INSTANCE, durability=PERSISTENT}`) | Exactly **one** row exists in the entire lake, at ledger 57,467,277 (the same ledger as the one observed `swap` execute event) -- `executable = Wasm(438a5528...)`. |
| `contract_code` entry for hash `438a5528...` | Exactly **one** row in the entire lake, at ledger 52,314,704, `change_type='state'`. Its `code<>` bytes SHA256-verify to this same hash (see Method §3). |
| Event-body shape drift, ledgers 51,524,666 (earliest `distribute`) through 63,380,088 (latest `distribute`) | **None observed.** 465/465 exhaustively shape-checked; 12 individually-inspected samples spanning the full range decode identically. |

**`events.go`'s "up to 3 WASM uploads (51,351,843 / 51,498,920 /
52,314,704)" claim: not corroborated, corrected here.** Checked
`stellar.ledger_entry_changes` unrestricted to any one contract, `entry_type IN ('contract_data','contract_code','ttl')`:

- `51,351,843` (+/-1,000 ledgers): **zero** rows of those three `entry_type`s network-wide (only classic `trustline` / `offer` / `claimable_balance`).
- `51,498,920` (+/-1,000 ledgers): same, **zero** Soroban entry activity.
- `52,314,704`: **one** row (`contract_code`, `state`), hash `438a5528...`, the already-established hash, not a third version.

Hypothesis (unconfirmed, for the next auditor): `ledger_entry_changes` captures
Soroban contract-data / contract-code touches very sparsely in this era; the
Comet backstop pool (`comet.md`) and Blend's pool WASM (`blend.md`, hash
`a41fc53d...`) each show exactly **one** lifetime row despite thousands of
transactions, so it is a general property of this table in ~2025 H1, not
Emitter-specific. The two uncorroborated "uploads" were most likely never real
(the "up to 3" was already a hedge); the practical question (does the decoder
handle every event ever emitted) is answered exhaustively in Method §2.

## Per-hash review findings

| hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- |
| `438a5528cff17ede` | Only hash observed; contract active L51,499,914 (genesis `drop`) -> L63,380,088 (latest `distribute`, near r1's current tip ~L63.4M) | maintainer@2026-07-10 | SHA256-verified against on-chain hash; every decoder-expected topic symbol + body field name present in the binary; 100% of 469 lifetime events (not sampled) decode to the exact shape `internal/sources/blend_emitter/decode.go` expects. |

## Failure modes specific to Blend Emitter

Per `docs/operations/wasm-audits/README.md`'s table:

1. **`distribute` topic collision with `blend_backstop`.** Handled by contract-identity gating (ADR-0035/0040), not a WASM-audit concern; see README.md "Gating".
2. **`drop`'s outer `Vec` is variable-length** (observed arities 13 and 3); the decoder does NOT assume fixed arity (`decodeDrop` loops `range outer`).
3. **Topic[0] symbol rename** (e.g. `"distribute"`) would silently drop every event of that kind; `classify()` is byte-equal against pre-encoded constants.
4. **`q_swap`/`swap` Map field rename** (`new_backstop` / `new_backstop_token` / `unlock_time`): decode-by-name per `ingest-pipeline.md#contract-schema-evolution`; fails loud (`ErrMalformedPayload`).
5. **Non-positive amount** on `distribute`/`drop`: rejected (`ErrNonPositiveAmount`); none of the 465+2 observed amounts hit this (all strictly positive in samples reviewed).

## Decision

**`BackfillSafe: true`** -- flipped in
`internal/sources/external/registry.go` in this commit.

Rationale:

- All 469 lifetime events (100%) decode to the shape `internal/sources/blend_emitter` expects, checked against ClickHouse-lake XDR bytes.
- The one on-chain WASM hash (`438a5528...`) is SHA256-verified byte-for-byte against the extracted code and contains every symbol the decoder relies on.
- No shape drift from earliest `distribute` (L51,524,666) through latest (L63,380,088), including across the V1->V2 backstop swap (`q_swap`/`swap` at ~L57.47M), which changes the *value* (targeted backstop), not the *shape*.
- The "up to 3 WASM uploads" hedge was uncorroborated for 2 of 3 ledgers (zero Soroban activity network-wide); the corroborated one resolves to the same verified hash. No evidence of a second WASM version.

Re-audit trigger: a new WASM hash ever appears for
`CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR`'s
contract-instance entry (operators can spot-check via the same exact-
key lookup this audit used), OR the orphan-event counter
(`stellarindex_source_orphan_events_total{source="blend_emitter"}`)
shows a sustained non-zero rate (a new, undecoded topic).

## References

- Procedure: [`README.md`](README.md)
- Decoder source: `internal/sources/blend_emitter/{events,decode}.go`
- Package README: `internal/sources/blend_emitter/README.md`
- Schema-evolution stance: [`../../architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution)
- Backfill gate: `internal/sources/external/registry.go` --
  `Registry["blend_emitter"].BackfillSafe`
- Related audits: [`blend.md`](blend.md) (pool + pool-factory + Backstop
  V2), [`comet.md`](comet.md) (the Backstop's Comet pool -- shares the
  `new_backstop_token` address seen in this audit's `q_swap`/`swap`
  samples)
- Evidence: [`evidence/blend_emitter/`](evidence/blend_emitter/) --
  WASM bytes (SHA256-verified), symbol-presence check, full query
  log.
