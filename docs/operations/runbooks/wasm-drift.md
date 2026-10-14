---
title: Runbook — wasm-drift
last_verified: 2026-09-30
status: draft
severity: P3
---

# Runbook — `stellarindex_wasm_drift` / `stellarindex_wasm_drift_stale`

## At a glance

WASM hash drift.

P3 (`severity: ticket`). Rules: `configs/prometheus/rules.r1/storage.yml` (twin `deploy/monitoring/rules/storage.yml`). Impact: a gated contract upgraded in place to bytes nobody has read; its events may decode wrong and a replay over its ledgers is not backed by an audit. ~30 min per new hash.

`stellarindex-ops wasm-drift -config PATH [-source NAME] [-textfile PATH]` reads ClickHouse only. For each gated source (`pipeline.GatedSourceNames()`) with entries in `internal/wasmaudit/audited_wasm.json` it resolves every contract's current WASM hash (`stellar.contract_instance_changes`, falling back to `ledger_entries_current`; same hop as `GET /v1/contracts/{id}/wasm`) across curated set, factories and announced children, and compares with the manifest. Exit code = drifting contracts (capped at 255). An erroring run (lake down, factory walk finds no children) writes no textfile.

| Verdict | Metric |
| --- | --- |
| hash not in the manifest | `stellarindex_wasm_drift{source,contract,wasm_hash} 1` |
| SAC or no instance entry in the lake | `stellarindex_wasm_drift_contracts_unverifiable{source,reason}` |
| source has no audit log (not checked) | `stellarindex_wasm_drift_source_unaudited{source} 1` |

## `stellarindex_wasm_drift` — a contract runs an unaudited hash

1. Take `wasm_hash` from the alert (or the `DRIFT` lines). Fetch the bytes: `GET /v1/contracts/{contract}/wasm`, or `stellar.ledger_entries_current` with `entry_type = 'contract_code'` keyed on the hash.
2. String-check them against the decoder's literals ([protocol-epochs.md §Method](../wasm-audits/protocol-epochs.md#method)): byte-search every topic and field name `internal/sources/<source>/{events,decode}.go` watches for the contract's role.
3. Record the verdict in `docs/operations/wasm-audits/<source>.md` (full hash, or first 8 hex chars + `…`).
4. Only if the check passed, add the full hash to `audited_wasm.json` (`source`, `role`, `audited` date, `doc`); `TestAuditedWasmManifestMatchesAuditLogs` fails if the doc does not name it.
5. Literals missing: the decoder is unsafe on this contract; open an issue and run no replay over its ledgers. A contract that is not the protocol's belongs out of the curated set.

## `stellarindex_wasm_drift_stale` — no completed run in 2 days

Fires only after a run has written the textfile. Read the scheduling unit's journal for the `wasm-drift:` error line (ClickHouse unreachable, or a factory walk returning zero children).

## Promoting an unaudited source

For `stellarindex_wasm_drift_source_unaudited`: write its audit log in `docs/operations/wasm-audits/<source>.md` per the [procedure](../wasm-audits/README.md), string-check every hash its contracts run, then seed them into `audited_wasm.json`.

## Discover what a contract really emits

Read-only dump of a bounded range's on-chain events (topics, body field types):

```sh
stellarindex-ops scan-soroban-events -config /etc/stellarindex.toml -from <ledger> -to <ledger> -contract <C...> -limit 50
```

## Related

`internal/ops/chops/wasm_drift.go`, `internal/pipeline/gated_registry.go`, [wasm-audits](../wasm-audits/README.md), [protocol-epochs](../wasm-audits/protocol-epochs.md#decoder-wasm-matrix), [alerts-catalog](../alerts-catalog.md)
