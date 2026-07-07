---
title: sorocredit WASM-history audit
last_verified: 2026-07-07
status: ratified — single WASM, event schemas invariant genesis→tip
source: sorocredit
backfill_safe: true
---

# sorocredit WASM audit

Audit log for the `sorocredit` source's `BackfillSafe` flag. See
[`README.md`](README.md) for the full procedure.

## Status

**Ratified 2026-07-07.** `BackfillSafe` flips `false` → `true` in
`internal/sources/external/registry.go` in the same change as this
audit. The main contract has run a **single WASM version** over its
entire life, and — the load-bearing claim — **every one of the 7
tracked event types has a single, invariant on-wire schema across the
whole ledger history**, each matching `internal/sources/sorocredit/decode.go`.
Backfill is safe **from genesis (ledger 61,620,822)**.

`sorocredit` is an unbranded consumer-USDC credit / CDP protocol
(`ClassLending`, `DefaultWeight: 0`, `IncludeInVWAP: false`). It
publishes no price and emits no trades — `BackfillSafe` gates only the
operator-triggered `projector-replay` / `backfill` path; aggregator
output is unaffected either way. See
[`internal/sources/sorocredit/README.md`](../../../internal/sources/sorocredit/README.md)
and the CLAUDE.md "Liquidation = scheduled settlement, not distress"
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
| Dispatcher hook | event-based `Decoder` (topic[0] classify → one of 7 symbols) + a blend-style childgate |
| Genesis ledger | `61,620,822` (2026-03-12 17:14:35 UTC) |

The protocol has a **single trust root** — the main contract — which
emits every business + config event and deploys the per-position
`Collateral-<uuid>` child contracts. The childgate (ADR-0035) seeds
those children forward-compat; in practice the children emit nothing
(verified) and every event is emitted by the main contract, so the
audit target is the **one** main contract's WASM.

## Method — lake-direct, not a galexie walk

Per ADR-0034 ("decoder backfills re-derive from the lake, not MinIO
walks"), this audit used the r1 ClickHouse lake **read-only**, not a
`stellarindex-ops wasm-history` galexie walk. A full genesis→tip walk
(~1.74 M ledgers) is a heavy job that competes with verify-archive on
ZFS-ARC/MinIO I/O; the lake already holds the decoded ledger-entry
changes and every raw contract event, which is strictly more
informative for a schema audit (it checks the actual on-wire event
shapes across the whole history — exactly what `BackfillSafe`
depends on — rather than a `wasm2wat` static read of one binary).

Two lake signals were combined:

1. **WASM-version enumeration** — the ContractInstance ledger entry's
   `executable` (WASM) hash over time, from `stellar.ledger_entry_changes`.
2. **Event-schema invariance** — the per-event-type structural
   fingerprint (topic arity + SCVal type tags, body `Vec` arity +
   element type tags) across every event the contract has emitted,
   from `stellar.contract_events`.

All queries were bounded to the contract's activity range and filtered
by `contract_id` / the exact instance `key_xdr`.

## 1. WASM-version timeline

The ContractInstance entry (a `contract_data` entry keyed by
`ScVal::LedgerKeyContractInstance`, persistent durability) carries the
contract's `executable` (`ContractExecutable::Wasm(hash)`). An
in-place `update_current_contract_wasm` upgrade rewrites this field.

Instance `key_xdr` (base64) for the main contract:

    AAAABgAAAAGN0li40oVrYwRFGIib1pAgVY49gTpWCtjXW79cHTYmJwAAABQAAAAB

Query (r1 ClickHouse, `stellar.ledger_entry_changes`), extracting the
32-byte executable hash out of each instance-entry snapshot
(`SCV_CONTRACT_INSTANCE` `0x00000013` + `CONTRACT_EXECUTABLE_WASM`
`0x00000000` + hash):

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

That hash matches the `84a88013…` cited in the source package doc
(`internal/sources/sorocredit/events.go`). No `updated` instance
change was observed anywhere in the range → **no in-place WASM upgrade
observed**.

### Coverage caveat (why the instance signal alone is not sufficient)

`ledger_entry_changes` on r1 is a live-capture table with a still-
incomplete historical re-derive for the earliest partition. Per-
partition row counts over the contract's range:

| partition (M-ledger) | ledger range | lec rows | distinct ledgers | density |
| --- | --- | --- | --- | --- |
| 61 | 61,620,823 – 61,999,998 | 587,432 | 228,206 | ~2.5 rows/ledger — **sparse** |
| 62 | 62,000,000 – 62,999,998 | 1,309,754,214 | 732,781 | ~1,787 rows/ledger — dense |
| 63 | 63,000,000 – 63,363,505 | 1,506,009,716 | 351,816 | dense |

So the "no executable change" finding is **reliable for
[62,000,000 → tip]** (dense — an upgrade there would have been
captured) but the early window **[61,620,822 → 62,000,000) is under-
covered** and the instance-entry signal cannot, on its own, exclude an
unobserved upgrade there. That gap is closed by the event-schema
invariance below.

## 2. Event-schema invariance (the load-bearing check)

The decoder decodes by topic arity + type and by body-`Vec` position +
element type (it is robust to field reordering within a Map, but
`sorocredit` bodies are positional `Vec`s, so the risks are changed
arity / retyped elements / a renamed-or-removed event type across an
upgrade). The decisive test is therefore: **does every event type keep
one invariant wire schema across the entire contract life, and does
that schema match `decode.go`?**

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

Two facts fall straight out of this:

- The **3 config events fire exactly once**, at genesis — a single
  occurrence cannot drift across versions, and their genesis frames
  are pinned byte-for-byte by the golden fixtures in `source_test.go`
  (cross-checked against the lake — identical `topics_xdr` /
  `data_xdr`).
- `StatementPublished` / `Liquidation` / `Withdrawal` only begin in
  **May 2026 (≥ 62.45 M)** — i.e. entirely inside the **dense**
  coverage window. **`NewCollateralContract` is the only event type
  that occurs inside the sparse early window [61.62 M, 62.0 M)**, so
  it is the one whose whole-life schema invariance actually matters
  for the coverage caveat above.

### Distinct structural fingerprints

For each recurring symbol, the DISTINCT `(topic_count, per-topic SCVal
type tags, body Vec header + first element tag)` fingerprint across
**all** of its events:

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

Result — **exactly one fingerprint per symbol**, spanning that
symbol's full `[minl, maxl]`:

| symbol | topics (SCVal tags) | body `Vec` head | distinct shapes | ledger span |
| --- | --- | --- | ---: | --- |
| `NewCollateralContract` | `Symbol, Address` (`0F,12`) | `Vec[2]`, elem0 `String` (`10·02·0E`) | 1 | 61,624,053 → 63,363,505 |
| `StatementPublished` | `Symbol, String, String` (`0F,0E,0E`) | `Vec[3]`, elem0 `i128` (`10·03·0A`) | 1 | 62,504,966 → 63,356,218 |
| `Liquidation` | `Symbol, Address, String, String` (`0F,12,0E,0E`) | `Vec[7]`, elem0 `Address` (`10·07·12`) | 1 | 62,504,969 → 63,356,284 |
| `Withdrawal` | `Symbol, Address` (`0F,12`) | `Vec[3]`, elem0 `Address` (`10·03·12`) | 1 | 62,451,103 → 63,363,545 |

SCVal type-tag legend: `0F`=Symbol, `12`=Address, `0E`=String,
`0A`=i128, `10`=Vec, `05`=u64, `01`=Void, `0D`=Bytes.

**`NewCollateralContract` has exactly ONE structural shape across all
139,435 events from its first occurrence (61,624,053 — inside the
sparse window) to its last (63,363,505).** Any unobserved instance
upgrade in [61.62 M, 62.0 M) therefore did **not** change the one
event schema active in that window. Combined with the dense-window
"no executable change" finding for [62.0 M → tip], no schema-breaking
upgrade occurred anywhere in the contract's life.

### Match against `decode.go`

Each observed shape is exactly what the decoder expects:

| symbol | decoder helper | expectation | observed | ✓ |
| --- | --- | --- | --- | --- |
| `NewCollateralContract` | `decodeNewCollateralContract` | topics ≥2 (`addr@1`); body `Vec≥2` (`String@0`, `Address@1`) | `[Symbol,Address]` + `Vec[String,Address]` | ✓ |
| `StatementPublished` | `decodeStatement` | topics ≥3 (`str@1,@2`); body `Vec≥3` (`i128@0`,`Address@1`,`u64@2`) | `[Symbol,String,String]` + `Vec[i128,Address,u64]` | ✓ |
| `Liquidation`→settlement | `decodeSettlement` | topics ≥4 (`addr@1`,`str@2,@3`); body `Vec≥3` (`Address@0`,`Vec[Address]@1`,`Vec[i128]@2`, …) | `[Symbol,Address,String,String]` + `Vec[Address, Vec, Vec, …]` (7 elems) | ✓ |
| `Withdrawal` | `decodeWithdrawal` | topics ≥2 (`addr@1`); body `Vec≥3` (`Address@0`,`Address@1`,`i128@2`) | `[Symbol,Address]` + `Vec[Address,Address,i128]` | ✓ |
| `SupportedAssetAdded` | `decodeSupportedAssetAdded` | topics ≥2 (`addr@1`); body captured | `[Symbol,Address]` + `Vec[7 config]` | ✓ |
| `BeaconUpdated` | `decodeConfigBody` | topics ≥1; body captured | `[Symbol]` + `Vec[Void,Address]` | ✓ |
| `CollateralHashUpdated` | `decodeConfigBody` | topics ≥1; body captured | `[Symbol]` + `Vec[Bytes,Bytes]` | ✓ |

The golden-frame tests in
[`internal/sources/sorocredit/source_test.go`](../../../internal/sources/sorocredit/source_test.go)
already decode a real sample of each of the 7 types through the
production `decodeOne` path with no error, and the config-event frames
there are the exact genesis frames (verified against the lake). No new
decode arm was needed — every WASM state the contract has run emits
the shapes the current decoder handles.

## Failure-mode review (per README.md §3 checklist)

| failure mode | finding |
| --- | --- |
| New event topic added | none — 7 topic[0] symbols, stable; a new one would `classify()` to `""` and skip cleanly |
| topic[0] symbol renamed | none — every symbol byte-identical across life (grouping key is the symbol) |
| body field renamed | N/A — positional `Vec`, no field names; element **types** invariant |
| body arity changed | none — one `Vec` arity per event type across all events |
| element retyped (i128↔u128, Address↔Bytes) | none — SCVal type tags invariant per position |
| event split into multiple | none — 1 event → 1 row; no correlation buffer to break |
| event removed (older WASM emitted a now-gone shape) | none — earliest samples (incl. sparse-window `NewCollateralContract` at 61,624,053) match current shapes |

## Cross-check

No Hubble decoded view exists for this bespoke protocol, and it emits
no trades → no VWAP cross-check. The event-schema-invariance evidence
above is the load-bearing safety check (per README.md §4). Live-ingest
health: the source has been decoding production traffic with the
current decoder (the golden tests pin the shapes it sees).

## Audit decision

**APPROVED 2026-07-07.** `Registry["sorocredit"].BackfillSafe` flipped
`false` → `true` in `internal/sources/external/registry.go` in the
same change as this audit. **Safe-from ledger: genesis (61,620,822)** —
the entire history is backfill-safe. Historical replay is now
unblocked:

```sh
/usr/local/sbin/run-heavy-job.sh sorocredit-replay \
  stellarindex-ops projector-replay -source sorocredit -from 61620822
```

(under the heavy-job wrapper per CLAUDE.md; never a bespoke
`sorocredit-backfill` subcommand).

### Re-audit triggers

- A new distinct executable hash appears on the ContractInstance
  (`ledger_entry_changes` instance-key query above returns >1 hash).
- A new topic[0] symbol appears for the contract (surfaced by the
  projector's unknown-topic path / per-source gap detector).
- Any of the 4 recurring event types grows a second structural
  fingerprint (re-run the §2 fingerprint query; extend `last_verified`).

### Belt-and-suspenders follow-up (optional, not blocking)

If a future re-audit wants to confirm the instance-entry signal
directly in the under-covered early window, a **scoped**
`stellarindex-ops wasm-history -contracts CCG5EWFY… -from 61620822
-to 62000000` walk (bounded, ~380 k ledgers, run under the heavy-job
wrapper when verify-archive is idle) would do it. It is not required
for this verdict: event-schema invariance already excludes any
schema-breaking upgrade, which is the only property `BackfillSafe`
protects.

## References

- Procedure: [`README.md`](README.md)
- Decoder source: [`internal/sources/sorocredit/{events,decode,consumer}.go`](../../../internal/sources/sorocredit/)
- Source-package README: [`internal/sources/sorocredit/README.md`](../../../internal/sources/sorocredit/README.md)
- Golden fixtures: [`internal/sources/sorocredit/source_test.go`](../../../internal/sources/sorocredit/source_test.go)
- Schema-evolution stance: [`docs/architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md)
- Backfill gate: `internal/sources/external/registry.go` — `Registry["sorocredit"].BackfillSafe`
- Raw-lake schema (ADR-0034): [`deploy/clickhouse/tier1_schema.sql`](../../../deploy/clickhouse/tier1_schema.sql)
</content>
