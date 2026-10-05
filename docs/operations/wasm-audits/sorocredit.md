---
title: sorocredit WASM-history audit
last_verified: 2026-09-30
status: "ratified 2026-07-07 — 8th symbol admitted (see 2026-09-22 addendum)"
source: sorocredit
backfill_safe: true
---

# sorocredit WASM audit

Audit log for the `sorocredit` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

## Status
**Ratified 2026-07-07.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same change as this
audit. One WASM version over the contract's whole life, and (load-bearing)
**each of the 7 tracked event types has one invariant on-wire schema
across the whole history**, matching `internal/sources/sorocredit/decode.go`.
Backfill is safe **from genesis (ledger 61,620,822)**. An 8th type,
`TreasuryUpdated`, was admitted later (**2026-09-22 addendum**); verdict unchanged.

`sorocredit`: unbranded consumer-USDC credit / CDP protocol
(`ClassLending`, `DefaultWeight: 0`, `IncludeInVWAP: false`). No price,
no trades; `BackfillSafe` gates only operator-triggered
`projector-replay` / `backfill`. See
[`internal/sources/sorocredit/README.md`](../../../internal/sources/sorocredit/README.md)
and the AGENTS.md "Liquidation = scheduled settlement, not distress"
note.

## Source identity

| field | value |
| --- | --- |
| Source name (registry key) | `sorocredit` |
| Registry class | `ClassLending` (DefaultWeight 0, IncludeInVWAP false) |
| Trust root (main contract) | `CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR` |
| Contract-ID (hex) | `8dd258b8d2856b63044518889bd69020558e3d813a560ad8d75bbf5c1d362627` |
| Creator | `GADI6FHS…` |
| Decoder files | [`internal/sources/sorocredit/{events,decode,consumer}.go`](../../../internal/sources/sorocredit/) |
| Dispatcher hook | event-based `Decoder` (topic[0] classify → one of 8 symbols) + a blend-style childgate |
| Genesis ledger | `61,620,822` (2026-03-12 17:14:35 UTC) |

Single **trust root**: the main contract emits every business + config
event and deploys per-position `Collateral-<uuid>` children. The childgate
(ADR-0035) seeds the children forward-compat, but they emit nothing
(verified), so the audit target is the **one** main contract's WASM.

## Method — lake-direct, not a galexie walk
Per ADR-0034 (decoder backfills re-derive from the lake), this audit
used the r1 ClickHouse lake **read-only**, not a `stellarindex-ops wasm-history`
galexie walk (~1.74 M ledgers, heavy, competes with verify-archive on
ZFS-ARC/MinIO I/O). The lake holds the ledger-entry changes and every raw
contract event, which checks actual on-wire shapes, what `BackfillSafe`
depends on, rather than a `wasm2wat` read of one binary.

1. **WASM-version enumeration**: ContractInstance `executable` (WASM) hash
   over time, from `stellar.ledger_entry_changes`.
2. **Event-schema invariance**: per-event-type structural fingerprint
   (topic arity + SCVal type tags, body `Vec` arity + element type tags)
   across every event, from `stellar.contract_events`.

Queries bounded to the contract's activity range, filtered by
`contract_id` / the exact instance `key_xdr`.

## 1. WASM-version timeline
The ContractInstance entry (`contract_data`, key
`ScVal::LedgerKeyContractInstance`, persistent) carries `executable`
(`ContractExecutable::Wasm(hash)`); an in-place
`update_current_contract_wasm` upgrade rewrites it.
Instance `key_xdr` (base64) for the main contract:

    AAAABgAAAAGN0li40oVrYwRFGIib1pAgVY49gTpWCtjXW79cHTYmJwAAABQAAAAB

Query (r1 ClickHouse, `stellar.ledger_entry_changes`), extracting the
32-byte hash from each snapshot (`SCV_CONTRACT_INSTANCE` `0x00000013` +
`CONTRACT_EXECUTABLE_WASM` `0x00000000` + hash):

```sql
SELECT hex(substring(base64Decode(entry_xdr),
        position(base64Decode(entry_xdr), unhex('0000001300000000')) + 8, 32)) AS wasm_hash,
       count() AS changes, min(ledger_seq) AS first_ledger, max(ledger_seq) AS last_ledger
FROM stellar.ledger_entry_changes
WHERE ledger_seq >= 61620000 AND entry_type = 'contract_data'
  AND key_xdr = :instance_key    -- the base64 LedgerKey shown above
  AND position(base64Decode(entry_xdr), unhex('0000001300000000')) > 0
GROUP BY wasm_hash ORDER BY first_ledger;
```

Result — **one** executable hash, one snapshot, at the deploy ledger:

| WASM hash | changes | first ledger | last ledger |
| --- | --- | --- | --- |
| `84a88013828d4c4f4e4f5d0fa2f686050d69889384a044eab3ec4b1169f810ea` | 1 | 61,620,824 | 61,620,824 |

Matches `84a88013…` in `internal/sources/sorocredit/events.go`. No
`updated` instance change anywhere in the range → **no in-place WASM
upgrade observed**.

### Coverage caveat (why the instance signal alone is not sufficient)
`ledger_entry_changes` on r1 is live-capture with a still-incomplete
historical re-derive for the earliest partition. Per-partition counts:

| partition (M-ledger) | ledger range | lec rows | distinct ledgers | density |
| --- | --- | --- | --- | --- |
| 61 | 61,620,823 – 61,999,998 | 587,432 | 228,206 | ~2.5 rows/ledger — **sparse** |
| 62 | 62,000,000 – 62,999,998 | 1,309,754,214 | 732,781 | ~1,787 rows/ledger — dense |
| 63 | 63,000,000 – 63,363,505 | 1,506,009,716 | 351,816 | dense |

"No executable change" is **reliable for [62,000,000 → tip]** (dense) but
the early window **[61,620,822 → 62,000,000) is under-covered**; the
instance signal alone cannot exclude an unobserved upgrade there. The
event-schema invariance below closes that gap.

## 2. Event-schema invariance (the load-bearing check)
The decoder keys on topic arity + type and body-`Vec` position + element
type; `sorocredit` bodies are positional `Vec`s, so the risks are changed
arity / retyped elements / renamed-or-removed event across an upgrade.
Test: **does every event type keep one wire schema across the contract's
life, matching `decode.go`?**

### Per-symbol volumes + life span (`stellar.contract_events`)

| topic[0] symbol | events | first ledger (date) | last ledger (date) |
| --- | ---: | --- | --- |
| `NewCollateralContract` | 139,435 | 61,624,053 (2026-03-12) | 63,363,505 (2026-07-07) |
| `StatementPublished` | 187,926 | 62,504,966 (2026-05-10) | 63,356,218 (2026-07-06) |
| `Liquidation` (→ settlement) | 187,718 | 62,504,969 (2026-05-10) | 63,356,284 (2026-07-06) |
| `Withdrawal` | 19,807 | 62,451,103 (2026-05-06) | 63,363,496 (2026-07-07) |
| `SupportedAssetAdded` | 1 | 61,620,825 (2026-03-12) | — |
| `BeaconUpdated` | 1 | 61,620,822 (2026-03-12) | — |
| `CollateralHashUpdated` | 1 | 61,620,824 (2026-03-12) | — |
| `TreasuryUpdated` | 1 (known) | 63,847,367 (2026-08-18 discovery) | — |

`TreasuryUpdated` was not in this 2026-07-07 sweep: found later by the
ADR-0033 recognition audit, not covered by the fingerprint table below.
Safety argument (code-structural, not lake-derived): 2026-09-22 addendum.

- The **3 config events fire once**, at genesis; they cannot drift, and
  their frames are pinned byte-for-byte by the golden fixtures in
  `source_test.go` (cross-checked against the lake: identical
  `topics_xdr` / `data_xdr`).
- `StatementPublished` / `Liquidation` / `Withdrawal` start in **May 2026
  (≥ 62.45 M)**, inside the **dense** window. **`NewCollateralContract`
  is the only type in the sparse early window [61.62 M, 62.0 M)**, so
  its whole-life invariance is what matters for the coverage caveat.

### Distinct structural fingerprints
DISTINCT `(topic_count, per-topic SCVal type tags, body Vec header +
first element tag)` fingerprint across **all** events of each recurring symbol:
```sql
SELECT topic_0_sym, topic_count,
       arrayStringConcat(arrayMap(x -> substring(hex(base64Decode(x)),1,8), topics_xdr),',') AS topic_type_tags,
       substring(hex(base64Decode(data_xdr)),1,32) AS data_prefix,
       count() AS n, min(ledger_seq) AS minl, max(ledger_seq) AS maxl
FROM stellar.contract_events
WHERE contract_id='CCG5EWFY2KCWWYYEIUMIRG6WSAQFLDR5QE5FMCWY25N36XA5GYTCPQWR'
  AND topic_0_sym IN ('NewCollateralContract','StatementPublished','Liquidation','Withdrawal')
GROUP BY topic_0_sym, topic_count, topic_type_tags, data_prefix;
```

Result: **one fingerprint per symbol**, spanning its full `[minl, maxl]`:

| symbol | topics (SCVal tags) | body `Vec` head | distinct shapes | ledger span |
| --- | --- | --- | ---: | --- |
| `NewCollateralContract` | `Symbol, Address` (`0F,12`) | `Vec[2]`, elem0 `String` (`10·02·0E`) | 1 | 61,624,053 → 63,363,505 |
| `StatementPublished` | `Symbol, String, String` (`0F,0E,0E`) | `Vec[3]`, elem0 `i128` (`10·03·0A`) | 1 | 62,504,966 → 63,356,218 |
| `Liquidation` | `Symbol, Address, String, String` (`0F,12,0E,0E`) | `Vec[7]`, elem0 `Address` (`10·07·12`) | 1 | 62,504,969 → 63,356,284 |
| `Withdrawal` | `Symbol, Address` (`0F,12`) | `Vec[3]`, elem0 `Address` (`10·03·12`) | 1 | 62,451,103 → 63,363,545 |

SCVal tag legend: `0F`=Symbol, `12`=Address, `0E`=String,
`0A`=i128, `10`=Vec, `05`=u64, `01`=Void, `0D`=Bytes.

**`NewCollateralContract` has ONE shape across all 139,435 events, from
61,624,053 (inside the sparse window) to 63,363,505.** Any unobserved
upgrade in [61.62 M, 62.0 M) did **not** change the schema active there;
with the dense-window "no executable change" finding for [62.0 M → tip],
no schema-breaking upgrade occurred in the contract's life.

### Match against `decode.go`
Each observed shape is what the decoder expects:

| symbol | decoder helper | expectation | observed | ✓ |
| --- | --- | --- | --- | --- |
| `NewCollateralContract` | `decodeNewCollateralContract` | topics ≥2 (`addr@1`); body `Vec≥2` (`String@0`, `Address@1`) | `[Symbol,Address]` + `Vec[String,Address]` | ✓ |
| `StatementPublished` | `decodeStatement` | topics ≥3 (`str@1,@2`); body `Vec≥3` (`i128@0`,`Address@1`,`u64@2`) | `[Symbol,String,String]` + `Vec[i128,Address,u64]` | ✓ |
| `Liquidation`→settlement | `decodeSettlement` | topics ≥4 (`addr@1`,`str@2,@3`); body exactly `Vec[7]` (`Address@0`,`Vec[Address]@1`,`Vec[i128]@2` parallel, …) | `[Symbol,Address,String,String]` + `Vec[Address, Vec, Vec, …]` (7 elems) | ✓ |
| `Withdrawal` | `decodeWithdrawal` | topics ≥2 (`addr@1`); body `Vec≥3` (`Address@0`,`Address@1`,`i128@2`) | `[Symbol,Address]` + `Vec[Address,Address,i128]` | ✓ |
| `SupportedAssetAdded` | `decodeSupportedAssetAdded` | topics ≥2 (`addr@1`); body captured | `[Symbol,Address]` + `Vec[7 config]` | ✓ |
| `BeaconUpdated` | `decodeConfigBody` | topics ≥1; body captured | `[Symbol]` + `Vec[Void,Address]` | ✓ |
| `CollateralHashUpdated` | `decodeConfigBody` | topics ≥1; body captured | `[Symbol]` + `Vec[Bytes,Bytes]` | ✓ |
| `TreasuryUpdated` | `decodeConfigBody` | topics ≥1; body captured | `[Symbol]` + `Vec[Address,Address]` (1 known occurrence, ledger 63,847,367) | ✓ |

Golden-frame tests in
[`internal/sources/sorocredit/source_test.go`](../../../internal/sources/sorocredit/source_test.go)
decode a real sample of each of the 8 types via the production `decodeOne`
path without error; the config frames are the exact genesis frames
(verified against the lake). No new decode arm needed.

## Failure-mode review (per README.md §3 checklist)

| failure mode | finding |
| --- | --- |
| New event topic added | 8 topic[0] symbols tracked (was 7 at ratification — `TreasuryUpdated` admitted, see 2026-09-22 addendum); a still-unmapped topic classifies to `""` and skips cleanly, but is caught by the ADR-0033 recognition audit (`recognition_ok=FALSE`) rather than silently accepted forever |
| topic[0] symbol renamed | none — every symbol byte-identical across life (grouping key is the symbol) |
| body field renamed | N/A — positional `Vec`, no field names; element **types** invariant |
| body arity changed | none — one `Vec` arity per event type across all events |
| element retyped (i128↔u128, Address↔Bytes) | none — SCVal type tags invariant per position |
| event split into multiple | none — 1 event → 1 row; no correlation buffer to break |
| event removed (older WASM emitted a now-gone shape) | none — earliest samples (incl. sparse-window `NewCollateralContract` at 61,624,053) match current shapes |

## Cross-check
No Hubble decoded view exists for this bespoke protocol; no trades → no
VWAP cross-check. Event-schema invariance is the load-bearing check (per
README.md §4). Live ingest decodes production traffic with the current
decoder (golden tests pin the shapes).

## Audit decision
**APPROVED 2026-07-07.** `Registry["sorocredit"].BackfillSafe` flipped
`false` → `true` in `internal/sources/external/registry.go` in the same
change as this audit. **Safe-from ledger: genesis (61,620,822)**. Replay:

```sh
/usr/local/sbin/run-heavy-job.sh sorocredit-replay \
  stellarindex-ops projector-replay -source sorocredit -from 61620822
```

(heavy-job wrapper per AGENTS.md; never a bespoke `sorocredit-backfill` subcommand).

### Re-audit triggers

- A new distinct executable hash appears on the ContractInstance
  (`ledger_entry_changes` instance-key query above returns >1 hash).
- A new topic[0] symbol appears for the contract (surfaced by the
  projector's unknown-topic path / per-source gap detector). **Already
  fired once** — see the 2026-09-22 addendum below.
- Any of the 4 recurring event types grows a second structural
  fingerprint (re-run the §2 fingerprint query; extend `last_verified`).

### Belt-and-suspenders follow-up — done 2026-09-30
Early window walked from the archive: `stellarindex-ops wasm-history
-bucket galexie-archive -contracts CCG5EWFY… -from 61620822 -to 62000000
-parallel 4` under the heavy-job wrapper on r1 (379,179 ledgers, 15m25s).
Result: one range, one hash,
`84a88013828d4c4f4e4f5d0fa2f686050d69889384a044eab3ec4b1169f810ea`, over
[61,620,822 → 62,000,000]. Instance signal now confirmed for the whole
life; verdict unchanged.

## 2026-09-22 addendum — TreasuryUpdated (8th symbol) admitted
`TreasuryUpdated` (topic `Symbol("TreasuryUpdated")`, body
`Vec[Address old, Address new]`) was found by the ADR-0033 recognition
audit (2026-08-18): one lake event on the main contract at ledger
63,847,367 that `classify()` dropped, tripping `recognition_ok=FALSE`.
`internal/sources/sorocredit/decode.go` was updated (commit 8f4569d94) to
route it to `TypeTreasuryUpdated` via the existing `decodeConfigBody`
(also covering `BeaconUpdated` / `CollateralHashUpdated`). This doc's
counts (source-identity table, §2 per-symbol tables, failure-mode review)
were not updated then; this addendum corrects the drift.

No new lake fingerprint sweep was run, none needed: `decodeConfigBody`
does **zero** typed field extraction, storing `e.Value` (raw base64 body)
verbatim in `Attributes["body"]` and always returning nil error, so
`TreasuryUpdated`'s schema-invariance risk is nil by construction (same
argument accepted in 2026-07-07 for the other two config events).
Unlike those (once, at genesis), its one known occurrence is after
genesis (63,847,367, past the audit's last-observed ledger 63,363,505):
a treasury-pointer rotation is an admin action and may recur, still
through the same zero-assertion decoder.

`Registry["sorocredit"].BackfillSafe` stays `true`.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: [`internal/sources/sorocredit/{events,decode,consumer}.go`](../../../internal/sources/sorocredit/)
- Source-package README: [`internal/sources/sorocredit/README.md`](../../../internal/sources/sorocredit/README.md)
- Golden fixtures: [`internal/sources/sorocredit/source_test.go`](../../../internal/sources/sorocredit/source_test.go)
- Schema-evolution stance: [`docs/architecture/ingest-pipeline.md#contract-schema-evolution`](../../architecture/ingest-pipeline.md#contract-schema-evolution)
- Backfill gate: `internal/sources/external/registry.go` — `Registry["sorocredit"].BackfillSafe`
- Raw-lake schema (ADR-0034): [`deploy/clickhouse/tier1_schema.sql`](../../../deploy/clickhouse/tier1_schema.sql)
</content>
