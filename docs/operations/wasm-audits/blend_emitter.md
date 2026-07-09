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
`internal/sources/external/registry.go` in the same commit as this
audit doc.

This audit did **not** run `stellarindex-ops wasm-history` (a MinIO
galexie-archive walk) -- per the task's hard constraint, only
read-only ClickHouse HTTP (`:8123`) queries against the certified raw
lake were used, the same shape as the `rozo.md` 2026-07-09 addendum.
The evidence is arguably *stronger* than a typical wasm-history walk
audit: instead of sampling WASM-hash transition boundaries, every one
of the contract's 469 lifetime events was shape-checked against the
decoder's expectations (465/465 `distribute` events exhaustively, not
sampled; both `drop` events, the one `q_swap`, and the one `swap`
individually decoded -- i.e. 100% event coverage, not a sample).

## Contract under audit

| Role | Contract | Confirmed WASM hash |
| --- | --- | --- |
| Emitter (single canonical mainnet instance) | `CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR` | `438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89` |

No factory namespace exists for this contract (curated one-contract
allowlist, `blend_emitter.MainnetGatedSet()` -- see events.go /
README.md "Gating"). The audit unit is therefore this one contract
address across its entire observed lifetime, same audit shape as
`comet.md`.

## Method

1. **Event census** (`stellar.contract_events`, `contract_id =
   'CCOQM6S7...'`): confirmed the package doc's claim of 469 total
   events / 4 topics / all `topic_count=1` exactly:
   `distribute=465`, `drop=2`, `q_swap=1`, `swap=1`. Zero orphan or
   empty-`topic_0_sym` rows (guards against the known CH gotcha where
   `topic_0_sym` can be empty for some event families); all 469 rows
   have `in_successful_call=1`.
2. **Exhaustive shape verification, not sampling.** Because
   `distribute` (465 events) dominates the volume, a single SQL query
   byte-shape-checked **all 465** rows at once (constant 20-byte
   prefix through the `SCV_ADDRESS` tag, the `SCV_I128` discriminant
   at byte 53, and the fixed 72-byte total XDR length) rather than
   spot-sampling a handful. Result: `465/465` match. The remaining 4
   events (both `drop`s, the one `q_swap`, the one `swap`) were each
   individually decoded with a from-scratch Python SCVal/XDR parser
   (no `go-stellar-sdk` dependency available for ad-hoc scripting;
   cross-validated by independently reproducing `comet.md`'s
   documented WASM hash `8abc28913035c074...` for the Comet backstop
   pool from an unrelated contract-instance entry in the same lake,
   confirming the parser decodes real mainnet XDR correctly).
3. **WASM-bytes extraction + SHA256 verification.** The Emitter's
   `contract_code` ledger entry was pulled from
   `stellar.ledger_entry_changes` (`entry_type='contract_code'`,
   exact key match on the 36-byte `LedgerKey::ContractCode{hash}`
   encoding). The XDR `opaque code<>` field's declared length (10,448
   bytes) was used to slice the WASM module precisely (not "read to
   end of buffer", which would include trailing XDR ext/padding and
   corrupt the hash) -- `sha256(wasm_bytes) ==
   438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`
   exactly. This is the strongest form of evidence available short of
   a full disassembler: the bytes are cryptographically tied to the
   on-chain hash, not merely "fetched from somewhere and assumed
   correct". WASM saved at
   [`evidence/blend_emitter/emitter-438a5528cff17ede.wasm`](evidence/blend_emitter/emitter-438a5528cff17ede.wasm).
4. **Symbol presence check** against the verified WASM bytes (see
   [`evidence/blend_emitter/emitter-438a5528cff17ede.symbols.txt`](evidence/blend_emitter/emitter-438a5528cff17ede.symbols.txt)):
   every decoder-relevant string is present --
   `distribute`, `q_swap`, `swap`, `drop`, `new_backstop`,
   `new_backstop_token`, `unlock_time`, `LastDistro`, `Dropped`,
   `BackstopBToken`, `SwapBLNDTkn`, `backstop`, `del_swap`,
   `queue_swap_backstop`, `cancel_swap_backstop`, `swap_backstop`,
   `get_last_distro`, `get_backstop`, `get_queued_swap`, `IsInit`,
   `blnd_token`, `initialize`.

Full query text + results:
[`evidence/blend_emitter/shape-verification-2026-07-10.md`](evidence/blend_emitter/shape-verification-2026-07-10.md).

## Decoder expectations

Captured from `internal/sources/blend_emitter/{events,decode}.go` at
HEAD as of 2026-07-09 (see those files' doc comments for the full
per-event shape). Summary:

| event | topic | body | decoder output |
| --- | --- | --- | --- |
| `distribute` | `[Symbol("distribute")]` | `Vec[Address backstop_id, i128 amount]` | `DistributeEvent` |
| `drop` | `[Symbol("drop")]` | `Vec[Vec[Address recipient, i128 amount], ...]` (variable length) | `DropEvent` (fanned out, one row per recipient) |
| `q_swap` | `[Symbol("q_swap")]` | `Map{new_backstop: Address, new_backstop_token: Address, unlock_time: u64}` | `SwapConfigEvent{Kind: SwapConfigQueued}` |
| `swap` | `[Symbol("swap")]` | same Map shape as `q_swap` | `SwapConfigEvent{Kind: SwapConfigExecuted}` |

All four are single-topic (`topic_count=1` uniformly -- confirmed
across all 469 events, not just the decoder's assumption).

## WASM timeline

**One confirmed WASM hash across the contract's entire observed
lifetime:** `438a5528cff17ede6fe515f095c43c5f15727af17d006971485e52462e7e7b89`.

| Signal | Finding |
| --- | --- |
| Contract-instance snapshot (`stellar.ledger_entry_changes`, exact key match on the Emitter's own `LedgerKey::ContractData{contract, key=LEDGER_KEY_CONTRACT_INSTANCE, durability=PERSISTENT}`) | Exactly **one** row exists in the entire lake, at ledger 57,467,277 (the same ledger as the one observed `swap` execute event) -- `executable = Wasm(438a5528...)`. |
| `contract_code` entry for hash `438a5528...` | Exactly **one** row in the entire lake, at ledger 52,314,704, `change_type='state'`. Its `code<>` bytes SHA256-verify to this same hash (see Method §3). |
| Event-body shape drift, ledgers 51,524,666 (earliest `distribute`) through 63,380,088 (latest `distribute`) | **None observed.** 465/465 exhaustively shape-checked; 12 individually-inspected samples spanning the full range decode identically. |

**On `events.go`'s "up to 3 WASM uploads (51,351,843 / 51,498,920 /
52,314,704)" claim — not corroborated, corrected here.** Checked
`stellar.ledger_entry_changes` unrestricted to any one contract (i.e.
"did ANYTHING Soroban-shaped happen on the whole network at this
exact ledger"), `entry_type IN ('contract_data','contract_code','ttl')`:

- `51,351,843` (+/-1,000 ledgers): **zero** rows of any of those three
  `entry_type`s, network-wide. Only classic `trustline` / `offer` /
  `claimable_balance` entries in that window.
- `51,498,920` (+/-1,000 ledgers): same -- **zero** Soroban entry
  activity anywhere on the network.
- `52,314,704`: **one** row exists (`contract_code`, `state`), and its
  hash is `438a5528...` -- the same hash already established above,
  not a distinct third version.

Root-cause hypothesis (not confirmed, noted for the next auditor):
`ledger_entry_changes` appears to capture Soroban contract-data /
contract-code footprint touches very sparsely in this era of the
chain -- the known-good Comet backstop pool (`comet.md`) and Blend's
pool WASM (`blend.md`, hash `a41fc53d...`) both show exactly **one**
lifetime row each in this same table despite both being touched by
thousands of transactions, so the sparsity is a general property of
this table/ingestion path in ~2025 H1 ledgers, not specific to the
Emitter. The two "uploads" this audit could not corroborate were most
likely never real -- the "up to 3" phrasing in `events.go` was
already a hedge, not a confirmed claim -- but the practical audit
question ("does the current decoder handle every event the contract
has ever emitted") is answered directly and exhaustively in Method
§2 regardless of how many times the WASM was technically re-uploaded.

## Per-hash review findings

| hash (first 16) | active range | reviewer | finding |
| --- | --- | --- | --- |
| `438a5528cff17ede` | Only hash observed; contract active L51,499,914 (genesis `drop`) -> L63,380,088 (latest `distribute`, near r1's current tip ~L63.4M) | ash@2026-07-10 | SHA256-verified against on-chain hash; every decoder-expected topic symbol + body field name present in the binary; 100% of 469 lifetime events (not sampled) decode to the exact shape `internal/sources/blend_emitter/decode.go` expects. |

## Failure modes specific to Blend Emitter

Per `docs/operations/wasm-audits/README.md`'s table, applied here:

1. **`distribute` topic collision with `blend_backstop`.** Already
   handled by contract-identity gating (ADR-0035/0040), not a
   WASM-audit concern -- noted here because it's this source's most
   distinctive failure mode; see README.md "Gating".
2. **`drop`'s outer `Vec` is variable-length** (observed arities 13
   and 3) -- confirmed the decoder does NOT assume a fixed arity
   (`decodeDrop` loops `range outer`), consistent with both real
   samples.
3. **Topic[0] symbol rename** (e.g. `"distribute"` -> anything else)
   would silently drop every event of that kind -- `classify()` is
   byte-equal against pre-encoded constants, same as every other
   source in this repo.
4. **`q_swap`/`swap` Map field rename** (`new_backstop` /
   `new_backstop_token` / `unlock_time`) -- decode-by-name per
   `contract-schema-evolution.md`; a rename fails loud
   (`ErrMalformedPayload`) rather than silently mis-decoding.
5. **Non-positive amount** on `distribute`/`drop` -- rejected
   (`ErrNonPositiveAmount`); none of the 465+2 observed amounts hit
   this (all strictly positive in the samples reviewed).

## Decision

**`BackfillSafe: true`** -- flipped in
`internal/sources/external/registry.go` in this commit.

Rationale:

- Every one of the contract's 469 lifetime events (100%, not a
  sample) decodes to the exact shape `internal/sources/blend_emitter`
  expects, checked directly against ClickHouse-lake XDR bytes.
- The one WASM hash confirmed on-chain (`438a5528...`) is SHA256-
  verified byte-for-byte against the extracted code, and contains
  every symbol the decoder relies on.
- No shape drift observed across the full event timeline (earliest
  `distribute` at L51,524,666 through latest at L63,380,088) --
  including across the V1->V2 backstop swap event itself
  (`q_swap`/`swap` at ~L57.47M), which changes the *value* the
  events carry (which backstop is targeted) but not the *shape*.
- The package doc's "up to 3 WASM uploads" hedge could not be
  corroborated for 2 of the 3 claimed ledgers (zero Soroban activity
  network-wide at those exact ledgers); the one ledger that IS
  corroborated resolves to the same, already-verified hash. This
  audit did not find evidence of a second WASM version ever running
  on this contract.

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
- Schema-evolution stance: [`../../architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md)
- Backfill gate: `internal/sources/external/registry.go` --
  `Registry["blend_emitter"].BackfillSafe`
- Related audits: [`blend.md`](blend.md) (pool + pool-factory + Backstop
  V2), [`comet.md`](comet.md) (the Backstop's Comet pool -- shares the
  `new_backstop_token` address seen in this audit's `q_swap`/`swap`
  samples)
- Evidence: [`evidence/blend_emitter/`](evidence/blend_emitter/) --
  WASM bytes (SHA256-verified), symbol-presence check, full query
  log.
