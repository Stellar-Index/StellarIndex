---
title: Upshift WASM audit
last_verified: 2026-09-30
status: string-checked — both curated vaults, 1 hash, lake lineage to ledger 64,698,213
source: upshift
backfill_safe: false
---

# Upshift WASM audit

Audit log for the `upshift` source. See [`README.md`](README.md) for
the procedure. The hash below is the `upshift` entry in
`internal/wasmaudit/audited_wasm.json`, which `stellarindex-ops
wasm-drift` checks every gated contract against
([runbook](../runbooks/wasm-drift.md)).

## Status

**String-checked 2026-09-30.** Both curated vaults have run one WASM
for their whole observed lives, and it carries every event-kind literal
the decoder handles or recognises. `BackfillSafe` is **not** changed by
this log; it stays `false` in `internal/sources/external/registry.go`
until a separate change decides the flip.

No `stellarindex-ops wasm-history` walk was run. The lineage comes from
the certified lake's `stellar.contract_instance_changes`, which records
the executable on every instance change.

## Contracts under audit

The curated set, `upshift.MainnetGatedSet()`
(`internal/sources/upshift/events.go`). No factory: the set is a
hand-maintained allow-list.

| role | contract | vault |
| --- | --- | --- |
| vault | `CCL3WITWFFXIHV2I52ECV5DPIEOFSTU3PBPR53ILPLF2IP5KHECXRUTY` | earnUSDC |
| vault | `CC6TRAPQD3NK7THUKWPV5SL2JHKQGNXZVB6S6MVYFSLRWAKEFUWZKZ7J` | earnXLM |

## Method

All queries ran read-only on r1's ClickHouse lake on 2026-09-30.
Contract strkeys were converted to the lower-hex 32-byte contract id
locally.

```sql
-- 1. vault lineage
SELECT contract_hash, wasm_hash, min(ledger_seq), max(ledger_seq), count()
FROM stellar.contract_instance_changes
WHERE contract_hash IN (<2 vault hex>)
GROUP BY contract_hash, wasm_hash ORDER BY contract_hash, 3;

-- 2. string check
SELECT '4b3d9f6b09f7127b0ce81b0ce9d8428f3f960e4e3ce13c11c1cbb446cccc9e73',
       ledger_seq, length(c),
       [position(c, 'deposit') > 0, position(c, 'withdraw') > 0, ...]
FROM (SELECT ledger_seq, base64Decode(entry_xdr) c
      FROM stellar.ledger_entry_changes
      WHERE entry_type = 'contract_code'
        AND ledger_seq BETWEEN 40000000 AND 64700000
        AND key_xdr = 'AAAAB0s9n2sJ9xJ7DOgbDOnYQo8/lg5OPOE8EcHLtEbMzJ5z'
      LIMIT 1);
```

Literals checked are the event-kind constants in
`internal/sources/upshift/events.go`:

- decoded: `deposit`, `withdraw`, `transfer`, `deployed_assets_changed`
- recognised, not decoded: `approve`, `deposit_to_subaccount`,
  `withdraw_from_subaccount`, `wallet_deployed_updated`,
  `wallet_net_deployed_seeded`, `subaccount_added`, `admin_set`,
  `operator_set`

### Limits of the string check

- It is a substring `position()` over the `contract_code` ledger
  entry's bytes. A hit shows the literal occurs somewhere in the module
  (an export name, a data-section string), not that the contract
  publishes an event under it.
- Short literals match incidentally: `deposit` and `withdraw` are
  substrings of `deposit_to_subaccount` and `withdraw_from_subaccount`,
  and `transfer` / `approve` are standard token method names. Hits on
  those four prove little on their own; the long literals are the
  informative ones.
- A Soroban `Symbol` of up to 9 characters can be compiled into a
  packed integer rather than stored as a string, so a miss on a short
  literal would not prove absence either. There were no misses.
- The bytes were matched by `LedgerKey` hash, not re-hashed with
  SHA-256.

The runtime evidence is separate: the topic constants in
`internal/sources/upshift/events.go` were checked byte-for-byte against
the lake's own `topics_xdr`.

## Per-hash findings

| wasm_hash | role | contract | first seen | last instance change | instance changes | literals |
| --- | --- | --- | --- | --- | --- | --- |
| `4b3d9f6b09f7127b0ce81b0ce9d8428f3f960e4e3ce13c11c1cbb446cccc9e73` | vault | `CCL3WITW…` (earnUSDC) | 62,623,313 | 64,695,270 | 612 | all 12 present |
| `4b3d9f6b09f7127b0ce81b0ce9d8428f3f960e4e3ce13c11c1cbb446cccc9e73` | vault | `CC6TRAPQ…` (earnXLM) | 62,623,319 | 64,698,213 | 362 | all 12 present |

Each vault's first instance change is the ledger of its first event
(`admin_set`), and no other hash appears on either vault.

## Decision

`4b3d9f6b…` is the audited set for `upshift` in `audited_wasm.json`.
`BackfillSafe` is unchanged (`false`).

Re-audit trigger: `wasm-drift` reports an `upshift` vault on a hash not
listed here, or a vault is added to `MainnetGatedSet()`.

## Out of scope

- Running `wasm-drift` on r1 or putting it on a timer.
- A `wasm-history` walk: the lake's instance-change table is the
  evidence here.

## References

- Procedure: [`README.md`](README.md)
- Decoder: `internal/sources/upshift/{events,decode}.go`
- Protocol verification: [`../../protocols/upshift.md`](../../protocols/upshift.md)
- Drift check: `internal/ops/chops/wasm_drift.go`, manifest `internal/wasmaudit/audited_wasm.json`
