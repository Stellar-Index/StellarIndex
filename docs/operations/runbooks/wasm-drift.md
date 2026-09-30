---
title: Runbook — wasm-drift
last_verified: 2026-09-30
status: draft
severity: P3
---

# Runbook — `stellarindex_wasm_drift` / `stellarindex_wasm_drift_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_wasm_drift`, `stellarindex_wasm_drift_stale` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml`; multi-host twin in `deploy/monitoring/rules/storage.yml`. |
| Typical MTTR | 30 min per new hash (fetch bytes, string-check, record) |
| Impact | None visible yet. A gated contract upgraded in place to bytes nobody has read: its events may decode wrong or not at all, and a replay over its ledgers is not backed by an audit. |

`stellarindex-ops wasm-drift -config PATH [-source NAME] [-textfile PATH]`
reads ClickHouse only. For every gated source (`pipeline.GatedSourceNames()`)
that has entries in `internal/ops/chops/audited_wasm.json`, it builds the
contract set — curated set ∪ factories ∪ every child the factories announced,
walked from the lake's creation events — resolves each contract's **current**
WASM hash (`stellar.contract_instance_changes`, falling back to
`ledger_entries_current` — the same hop `GET /v1/contracts/{id}/wasm` uses),
and compares it with the manifest.

| Verdict | Meaning | Metric |
| --- | --- | --- |
| drift | hash is not in the manifest for this source | `stellarindex_wasm_drift{source,contract,wasm_hash} 1` |
| sac / unresolved | a Stellar Asset Contract, or no instance entry in the lake — nothing to compare | `stellarindex_wasm_drift_contracts_unverifiable{source,reason}` |
| unaudited source | the source has no audit log in the manifest; its contracts are not checked | `stellarindex_wasm_drift_source_unaudited{source} 1` |

Exit code = drifting contracts (capped at 255). A run that errors (lake down,
a factory walk that finds no children or cannot decode a creation event)
writes no textfile, so `stellarindex_wasm_drift_stale` covers it.

## `stellarindex_wasm_drift` — a contract runs an unaudited hash

1. Take `wasm_hash` from the alert (or the run's `DRIFT` lines). Fetch the
   bytes from the lake: `GET /v1/contracts/{contract}/wasm`, or
   `stellar.ledger_entries_current` with `entry_type = 'contract_code'`
   keyed on the hash.
2. String-check them against the decoder's literals per
   [decoder-wasm-matrix.md §Method](../wasm-audits/decoder-wasm-matrix.md#method):
   byte-search every topic and field name
   `internal/sources/<source>/{events,decode}.go` watches, for the
   contract's role.
3. Record the verdict in `docs/operations/wasm-audits/<source>.md` — the
   hash in full, or as its first 8 hex chars + `…`.
4. Only if the check passed: add the full hash to `audited_wasm.json`
   (`source`, `role`, `audited` date, `doc`).
   `TestAuditedWasmManifestMatchesAuditLogs` fails if the doc does not name it.
5. If literals are missing, the decoder is not safe on this contract: open an
   issue, and do not run a replay over the contract's ledgers until the
   decoder handles the new bytes. A contract that is not the protocol's at
   all belongs out of the curated set, not in the manifest.

## `stellarindex_wasm_drift_stale` — no completed run in 2 days

Only fires once a run has written the textfile. Check the scheduling unit's
journal for the `wasm-drift:` error line; the usual causes are ClickHouse
being unreachable or a factory walk returning zero children.

## Promoting an unaudited source

A gated source with no manifest entry (`stellarindex_wasm_drift_source_unaudited`)
is not checked at all. To bring it under the check: write its audit log in
`docs/operations/wasm-audits/<source>.md` following the
[procedure](../wasm-audits/README.md), string-check every hash its contracts
currently run, then seed those hashes into `audited_wasm.json`. The next run
checks it.

## Related

- `internal/ops/chops/wasm_drift.go` — the check; `internal/pipeline/gated_registry.go` — the gated sources it walks.
- [wasm-audits](../wasm-audits/README.md) — the audit logs the manifest is checked against.
- [decoder-wasm-matrix](../wasm-audits/decoder-wasm-matrix.md) — the string-check method.
- [alerts-catalog](../alerts-catalog.md)
