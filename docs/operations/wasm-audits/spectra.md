---
title: Spectra WASM audit
last_verified: 2026-10-06
status: string-checked — 8 hashes, one per contract role, no upgrade seen; lake lineage to ledger 64,716,536
source: spectra
backfill_safe: true
---

# Spectra WASM audit

Audit log for the `spectra` source ([`README.md`](README.md) = procedure). The
eight hashes below are the `spectra` entries in
`internal/wasmaudit/audited_wasm.json`, which `stellarindex-ops wasm-drift`
checks every gated contract against ([runbook](../runbooks/wasm-drift.md)).

## Status

**String-checked 2026-10-06.** Every Spectra contract ran one WASM for its
whole observed life (nothing was upgraded in place). Each hash carries the
event-kind literals its role is expected to emit, and every
`(contract, topic[0])` pair observed on chain matches the lake's own
`topics_xdr` byte for byte. On that basis the source is
`Backfill: BackfillPerWASM`: a replay passes the gate only while every
active hash is one of the eight below.

No `stellarindex-ops wasm-history` walk was run; lineage comes from the
lake's `stellar.contract_instance_changes`.

## Contracts under audit

Roster and discovery chain: [`../../protocols/spectra.md`](../../protocols/spectra.md).
22 contracts, the set the code gates (`MainnetContracts` plus
`MainnetInfrastructure`): registry, factory, router, 2 order engines, 7 PTs, 7 YTs, 3
wrapper IBTs. The earnXLM and earnUSDC IBTs are Upshift vaults, audited in
[`upshift.md`](upshift.md), and are not part of this set.

## Method

Read-only on r1's ClickHouse lake, 2026-10-06, ledgers 63,700,000 to
64,900,000; strkeys converted to the lower-hex 32-byte contract id locally.

```sql
-- 1. lineage (all 22 contract ids)
SELECT contract_hash, wasm_hash, min(ledger_seq), max(ledger_seq), count()
FROM stellar.contract_instance_changes
WHERE contract_hash IN (<22 hex>) AND ledger_seq BETWEEN 63700000 AND 64900000
GROUP BY contract_hash, wasm_hash ORDER BY wasm_hash, 3;

-- 2. string check, one query per hash
SELECT '<wasm_hash>', ledger_seq, length(c),
       [position(c, 'pt_minted') > 0, position(c, 'redeem') > 0, ...]
FROM (SELECT ledger_seq, base64Decode(entry_xdr) c
      FROM stellar.ledger_entry_changes
      WHERE entry_type = 'contract_code'
        AND ledger_seq BETWEEN 63700000 AND <first instance change>
        AND key_xdr = '<base64(0x00000007 || wasm_hash)>'
      LIMIT 1);

-- 3. topic byte check
SELECT contract_id, topic_0_sym, topics_xdr[1], topic_count, count(), min(ledger_seq), max(ledger_seq)
FROM stellar.contract_events
WHERE contract_id IN (<22 ids>) AND ledger_seq BETWEEN 63778000 AND 64800000
  AND in_successful_call = 1
GROUP BY contract_id, topic_0_sym, topics_xdr[1], topic_count;
```

Query 3 was compared locally with the expected `ScVal` symbol encoding of
`topic_0_sym`: 125 `(contract, topic)` groups, 0 mismatches.

### Limits of the string check

- A substring `position()` over the `contract_code` bytes: a hit shows the
  literal occurs somewhere in the module (export name, data string), not that
  the contract publishes an event under it. `wrap` / `unwrap` hits on the Blend
  wrapper are an example: no such event was ever emitted by it.
- Short literals match incidentally and standard token method names
  (`transfer`, `approve`, `mint`, `burn`) are present for reasons unrelated to
  events. The long literals are the informative ones.
- A `Symbol` of up to 9 characters can compile to a packed integer rather than
  a string, so a short-literal miss does not prove absence. The only misses
  (below) are of that kind or match what the chain shows.
- `admin_transfer_initiated` / `admin_transfer_completed` were not string
  checked; they are covered by the topic byte check only.
- Bytes were matched by `LedgerKey` hash, not re-hashed with SHA-256.

## Per-hash findings

| wasm_hash | role | contracts | first seen | last instance change | instance changes | literals |
| --- | --- | --- | --- | --- | --- | --- |
| `3bcf316f76a9c5db718ccd6e2292cad70d8e8ce4371dfa0a2a7fdd86b8749ffd` | pt | 7 PTs | 63,782,624 | 64,547,666 | 83 | all 16 present: `pt_minted`, `redeem`, `yield_updated`, `yt_deployed`, `mint`, `burn`, `transfer`, `approve`, `yield_claimed`, `fee_claimed`, `rates_stored_at_expiry`, `rewards_proxy_change`, `rewards_claimed`, `pt_upgraded`, `role_granted`, `role_revoked` |
| `daeb931b9507440b6dc4f73256ac33c6089964f5d035f7eec5fcedc10e732c8e` | yt | 7 YTs | 63,782,624 | 64,579,123 | 65 | `mint`, `burn`, `transfer`, `approve` present; `role_granted`, `role_revoked` absent (the YT never emitted either) |
| `54c79aac3d2a971fb8d11e7cbbab87ee2a4be10b60b48bb21e1d6254affe5bf3` | registry | `CCUGRASB…` | 63,778,088 | 64,547,666 | 19 | all 16 present: `pt_added`, `pt_removed`, `fee_reduced`, `registry_upgraded`, the three fee changes, the seven `*_change` kinds, `role_granted`, `role_revoked` |
| `763ea32c344f79bbce87d814f55b3328b831237443084e726caf8b8e4ac2b4a7` | factory | `CC4ZVRIY…` | 63,778,145 | 64,453,038 | 17 | all 4 present: `pt_deployed`, `factory_initialized`, `factory_upgraded`, `role_granted` |
| `1452052c3c9ed3297b82a02e99baafb0c9ba5209f03ad941e6427c3aae5b07f7` | router | `CB56R3NG…` | 63,778,172 | 64,547,666 | 3 | all 3 present: `execute_completed`, `role_granted`, `role_revoked`. `execute_completed` was never emitted |
| `20ec1744c3f9e2c840c714b4ad2dc42d844d2eb7c164ced8c685cf267516b0f0` | order-engine | `CC2CEV23…` (v0), `CCKNOCLH…` | 63,778,288 | 64,547,666 | 6 | all 8 present: `order_registered`, `order_filled`, `order_cancelled`, `limit_order_engine_initialized`, `limit_order_fee_change`, `registry_change`, `fee_collector_change`, `role_granted` |
| `11b4bacca38f670e792b7a1bbd6c6888f9e572c06e1def9cc5adf9294403bbb2` | blend-wrapper | `CBRT4E5A…` (sw-USDC), `CCECATRP…` (sw-EURC) | 63,780,144 | 64,547,666 | 44 | all 9 present: `deposit`, `withdraw`, `transfer`, `mint`, `burn`, `approve`, `wrap`, `unwrap`, `role_granted` |
| `0e8d68e206d66b087a2e0b64430356aec834ff906260e057180702d74d6f81f4` | deJTRSY-wrapper | `CAHPZLEH…` (sw-deJTRSY) | 64,452,971 | 64,716,536 | 71 | 9 of 10 present: `wrap`, `unwrap`, `dejtrsy_wrapper_initialized`, `transfer`, `burn`, `approve`, `deposit`, `withdraw`, `role_granted`. `mint` absent (packed symbol; the wrapper emits no `mint` on chain) |

Each contract's first instance change is the ledger of its first event
(`role_granted`, `factory_initialized` or `pt_deployed`); no second hash appears
on any of the 22 contracts. The order engine's v0 instance (`CC2CEV23…`) was
replaced at ledger 63,780,164 by `CCKNOCLH…`, which carries the same hash.

### Observed on chain, ledgers 63,778,000 to 64,800,000

Kinds seen per role (every one has a byte-checked topic, see Method):

| role | kinds |
| --- | --- |
| registry | `pt_added` (7), `factory_change`, `router_change`, `limit_order_engine_change` (2), `pt_wasm_hash_change`, `yt_wasm_hash_change`, `fee_collector_change` (2), governance |
| factory | `factory_initialized`, `pt_deployed` (7), governance |
| order engine | `order_registered` (49), `order_filled` (25), `order_cancelled` (8), `limit_order_engine_initialized` (2 incl. v0), `fee_collector_change`, governance |
| PT | `pt_minted` (26), `yt_deployed` (7), `yield_updated` (56), `redeem` (3), `mint` (26), `burn` (3), `transfer` (33), `approve` (21), governance |
| YT | `mint` (26), `burn` (3), `transfer` (25), `approve` (15) |
| Blend wrapper | `deposit` (22), `withdraw` (7), `transfer` (43), `approve` (14), governance |
| deJTRSY wrapper | `wrap` (34), `dejtrsy_wrapper_initialized`, `transfer` (21), `approve` (33), governance |
| router | governance only |

Governance = `role_granted`, `role_revoked`, `admin_transfer_initiated`,
`admin_transfer_completed`.

Literals present in the WASM but never emitted by 2026-10-06 (the expiry and
claim kinds): `unwrap`, `yield_claimed`, `fee_claimed`,
`rates_stored_at_expiry`, `pt_removed`, `fee_reduced`, `pt_upgraded`,
`execute_completed`. They gain fixtures after the first maturity (PT
`CDRK5SWZ…`, 2026-10-16).

## Decision

The eight hashes above are the audited set for `spectra` in
`audited_wasm.json`. Backfill is now per-WASM gated
(`BackfillPerWASM`): only the hashes above pass. The audited lineage is bounded
at ledger 64,900,000; no second hash was found up to there.

Re-audit trigger: `wasm-drift` reports a `spectra` contract on a hash not
listed here; the registry emits a `pt_wasm_hash_change`, `yt_wasm_hash_change`
or `limit_order_engine_change` with a new value; a PT is upgraded
(`pt_upgraded`); or a contract is added to the gate.

## Out of scope

- Running `wasm-drift` on r1 or putting it on a timer.
- A `wasm-history` walk: the lake's instance-change table is the evidence.
- The Upshift vaults that serve as two of the IBTs.

## References

- Procedure: [`README.md`](README.md)
- Decoder: `internal/sources/spectra/{events,decode}.go`
- Protocol verification: [`../../protocols/spectra.md`](../../protocols/spectra.md)
- Lake fixtures: `test/fixtures/spectra/<wasm_hash>/`, exported by
  `scripts/dev/capture-spectra-fixtures.sh` (lake mode)
- Drift check: `internal/ops/chops/wasm_drift.go`, manifest `internal/wasmaudit/audited_wasm.json`
