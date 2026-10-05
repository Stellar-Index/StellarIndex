---
title: Audit doc update template — v2 full per-instance WASM history
last_verified: 2026-05-03
status: template — applied to each source as v2 audit lands
---

# Per-source v2 audit update template

Structural shape of each source's audit doc once the v2 full-instance WASM
history walk lands. The v1 audits (2026-04-29 — see each source's
`wasm-audits/<source>.md` "Ratified 2026-04-29" section) captured each
factory/router/oracle's WASM history + each deployed pool/pair's *current*
WASM. v2 adds:

1. **Per-instance upgrade history.** Each pool/pair's full WASM timeline (every
   `update_current_contract_wasm` event from genesis through the audit's `to`
   ledger).
2. **Bytes for every unique hash**, including hashes evicted from current
   ledger state — fetched via `extract-wasm-from-galexie` against r1's archive.
3. **Disassembly per unique hash.** `wasm2wat` text, with the `e.0`
   (contract_event) call sites called out and the topic+body XDR construction
   traced per call.

## Template sections to add (after each source's "Per-hash review findings")

### v2 — Full per-instance WASM history

> **Walk source**: `r1:/var/log/wasm-history-full.json`
> **Walk parameters**: `-from 50457424 -to 62296694 -parallel 8 -contracts <532-contract list>`
> **Walked at**: 2026-04-29

#### Per-instance timeline matrix

Every WASM hash each instance contract ever ran, with the active ledger range:

| contract | hash range 1 | hash range 2 | … |
| --- | --- | --- | --- |
| `<addr>` | `<hash>:[from,to]` | `<hash>:[from,to]` | |

(Stable order by C-strkey for deterministic output.)

#### Unique-hash inventory across all instances

| hash (first 16) | first seen | last seen | instance count | bytes source |
| --- | --- | --- | --- | --- |
| `<hex>` | `<ledger>` | `<ledger>` | N | RPC fetch / galexie LCM extract |

### v2 — Disassembly per unique hash

For each unique hash above, a subsection:

#### `<hash-first-16>`

- **Bytes source**: `RPC fetch (mainnet.sorobanrpc.com)` or
  `galexie LCM extract (r1, install ledger N)`.
- **Size**: `N bytes`.
- **Contract spec diff vs. canonical (this source's current production WASM)**:

  ```diff
  <relevant interface diff>
  ```

- **Event-emit call sites traced**:

  ```
  func: <name>      → e.0(topic_addr, topic_len, body_addr, body_len)
                    topic prepared at <wat-line>, ScVec of:
                      [0] ScSymbol "<name>"
                      [1] ...
                    body prepared at <wat-line>, ScVec/ScMap of:
                      ...
  ```

- **Decoder-compatibility verdict**: `compatible` / `divergent
  (detail)` / `not-decoder-relevant`.

### v2 — Decision

Where every unique hash is `compatible` and the walk found no upgrade that
would have produced incompatible hashes, the v1 `BackfillSafe: true` flip is
**confirmed deterministically**. Where any hash diverges, v2 ships either:

- A decoder fix handling both shapes (gated by hash at decode time), and
  BackfillSafe stays `true`, OR
- A backfill range cutoff in the source's metadata refusing replay of
  pre-divergent ranges, and BackfillSafe stays `true` for the audited window
  only, OR
- BackfillSafe flips back to `false` until the decoder fix lands (worst case).

## Process to apply this template

1. Wait for the full walk (`r1:/var/log/wasm-history-full.json`) to complete.
2. Per source, extract the per-instance timeline rows from the walk JSON
   (filter by contract IDs from `internal/sources/<source>/events.go` + the
   pool list at `/tmp/wasm-audit/pools-<source>.txt`).
3. Compute the unique-hash inventory across each source's instances.
4. For each unique hash:
   - Fetch bytes via `stellar contract fetch --wasm-hash` first.
   - If RPC returns "Contract Code not found" (evicted), fall back to
     `stellarindex-ops extract-wasm-from-galexie` against r1.
   - Run `wasm2wat` for text form.
   - Use `stellar contract info interface` + `strings` for the interface +
     symbol-table view used in v1.
   - Trace `e.0` call sites in the WAT (`(call $e.0 …)`) and read back the
     topic/body construction.
5. Update the source's audit doc with the v2 sections.
6. Update `last_verified` in the frontmatter.
7. If any hash diverges, ship the decoder fix in the same PR.

## CI hook (optional, future)

A `scripts/ci/lint-wasm-audits.sh` could re-fetch and re-hash each unique hash
in each audit doc, re-run `stellar contract info interface` and confirm no diff
vs the recorded interface SHA, and re-disassemble and confirm no diff vs the
recorded WAT SHA. This is v3 follow-up scope.

## See also

- Schema-evolution stance:
  [`docs/architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md)
- Per-source v1 audits: this directory's `<source>.md` files.
- Subcommand:
  `cmd/stellarindex-ops/wasm_extract.go` (`extract-wasm-from-galexie`).
- Tool: `cmd/stellarindex-ops/main.go` `wasm-history` subcommand.
