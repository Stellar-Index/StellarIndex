---
title: BLNT V3 WASM audit
last_verified: 2026-11-15
status: source-checked — 6 immutable WASM lineages; Galexie fixtures cover every contract role
source: blnt
backfill_safe: true
---

# BLNT V3 WASM audit

Audit log for the independent `blnt` source ([`README.md`](README.md) =
procedure). The six hashes below are the `blnt` entries in
`internal/wasmaudit/audited_wasm.json`. The replay gate also resolves every
factory child and refuses a range if its lake history contains any other hash.

## Status

**Source-checked and replay-fixtured 2026-11-15.** The exact release artifacts,
deployment ledgers and source commits are recorded by the deployer in the
public [core deployment record](https://github.com/BLNT-Protocol/blnt-contracts-v3/blob/main/deployments/mainnet-v3-core.json),
[Fixed Pool V3 record](https://github.com/BLNT-Protocol/blnt-contracts-v3/blob/main/deployments/mainnet-v3-fixed-pool.json),
[YieldBlent record](https://github.com/BLNT-Protocol/blnt-contracts-v3/blob/main/deployments/mainnet-v3-yieldblent-pool.json),
and [lockup-backfill record](https://github.com/BLNT-Protocol/blnt-contracts-v3/blob/main/deployments/mainnet-v3-lockup-backfill.json).
The release and deployment checks recorded there bind the hashes to the named
source commits and attestations.

The core contracts and pools have no contract-WASM update entrypoint. The
factory stores the pool hash in immutable constructor state and every `deploy`
uses that stored hash, so later factory children inherit the audited pool
lineage. The supplemental lockup-backfill contract is likewise immutable.
The replay gate remains load-bearing: a future unexpected lineage in lake
history fails closed even if a contract is present in the curated registry.

## Contracts and lineages

The machine-readable roster is
[`evidence/blnt/deployed-lineages.json`](evidence/blnt/deployed-lineages.json).

| WASM hash | role | deployed contracts | first ledger | source |
| --- | --- | --- | --- | --- |
| `4fa7d99dee6fe99cad5d07a46ee39e437dc287e53e3ab1f3ccc0aacf7e6cd6c5` | emitter | `CDSO6F…` | 65,134,154 | [`5d055ca`](https://github.com/BLNT-Protocol/blnt-contracts-v3/tree/5d055ca89afc9ed443d36b7ad34e3de59b487764/emitter) |
| `985d7eda4b610f28103edcb28faa52408169f455ac9a880c4a39f58ecc0c2135` | pool factory | `CAKLFW…` | 65,134,160 | [`5d055ca`](https://github.com/BLNT-Protocol/blnt-contracts-v3/tree/5d055ca89afc9ed443d36b7ad34e3de59b487764/pool-factory) |
| `dcdc0b1860799629c7c88804955dd7373cb6febaf069e2ba863e98ebbaa95c27` | primary backfill | `CANIE6…` | 65,134,164 | [`fe366da`](https://github.com/BLNT-Protocol/blnt-backfill-contract/tree/fe366da68da7f24ef20063c43aaf0b13ea752257) |
| `0469cfb445bedd87478274f265c8776b32cde1e8840fca4684ffa1dc4d5a750f` | tiered backstop | `CAHCUK…` | 65,134,174 | [`5d055ca`](https://github.com/BLNT-Protocol/blnt-contracts-v3/tree/5d055ca89afc9ed443d36b7ad34e3de59b487764/backstop) |
| `0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356` | pool | Fixed Pool V3 `CACSQW…`, YieldBlent `CAUTPS…`, and later factory children | 65,134,362 | [`5d055ca`](https://github.com/BLNT-Protocol/blnt-contracts-v3/tree/5d055ca89afc9ed443d36b7ad34e3de59b487764/pool) |
| `7c3d3591b6227827c21a0fe0da5df402bd55394fa5ed38c0bbace0400a823ddb` | supplemental lockup backfill | `CBNRUF…` | 65,369,820 | [`2835707`](https://github.com/BLNT-Protocol/blnt-lockup-backfill-contract/tree/283570797ce1f73d2f219e0f47771f5854fed2c1) |

## Decoder review

The decoder stores a lossless envelope: exact topic and value XDR plus contract,
ledger, transaction, operation, event index, close time, role and topic-0 event
name. It does not interpret amounts or project these events into Blend V2
tables. Consequently, the per-hash review is limited to identity gating and
the event-name catalogue; body-shape changes remain preserved rather than
silently reinterpreted.

The source at each deployed commit was checked against `roleEvents`:

| role | accepted topic-0 symbols |
| --- | --- |
| pool | `set_admin`, `update_pool`, `queue_set_reserve`, `cancel_set_reserve`, `set_reserve`, `set_status`, `reserve_emission_update`, `gulp_emissions`, `claim`, `bad_debt`, `defaulted_debt`, `supply`, `withdraw`, `supply_collateral`, `withdraw_collateral`, `borrow`, `repay`, `flash_loan`, `clawback`, `gulp`, `reconcile_loss`, `protocol_credit_loss`, `new_auction`, `fill_auction`, `delete_auction` |
| factory | `deploy` |
| backstop | `migration_activated`, `backfill_funded`, `deposit`, `queue_withdrawal`, `dequeue_withdrawal`, `withdraw`, `distribute`, `gulp_emissions`, `rw_zone_add`, `rw_zone_remove`, `claim`, `draw`, `donate` |
| emitter | `distribute`, `q_swap`, `del_swap`, `swap`, `drop` |
| primary backfill | `claim`, `claim_grant`, `swap_blnd`, `burn_expired` |
| lockup backfill | `fund`, `claim`, `claim_fund`, `expire` |

Factory `deploy` is additionally shape-checked as a single address body before
the announced child is admitted. All other accepted events parse every topic
and body SCVal, then preserve the original bytes.

## Galexie replay fixtures

`test/fixtures/blnt/<wasm_hash>/` contains raw successful-event XDR exported
from the AWS public Galexie archive. The set covers all six hashes and roles,
including a factory deployment, tiered backstop deposit, emitter distribution,
primary-backfill claim and swap, lockup funding and claim-funding, and pool
supply/flash-loan/repay events. `TestRealFixturesDecodeEveryAuditedRole`
requires every fixture directory to be audited for `blnt`, verifies topic-0
bytes and lossless identity, and fails if any role loses real-lake coverage.

Observed fixture range: ledgers 65,134,362 through 65,387,356. Fixtures are
regression evidence, not the lineage authority; the production replay gate
queries contract instance history for the requested range.

## Decision

All six deployed lineages match the role-specific decoder and have real-lake
coverage. `blnt` is therefore `BackfillPerWASM`: a replay is admitted only
when every contract the decoder can accept—including factory-discovered pools
and registry-warmed contracts—ran a hash attested here.

Re-audit trigger: `wasm-drift` reports an unlisted hash, the deployment roster
or protocol registry adds a non-factory contract, the factory deploys a pool
whose history does not resolve to the audited pool hash, or a source release
changes any emitted topic.

## References

- Procedure: [`README.md`](README.md)
- Protocol roster: [`../../protocols/blnt.md`](../../protocols/blnt.md)
- Decoder: `internal/sources/blnt/`
- Fixtures: `test/fixtures/blnt/<wasm_hash>/`
- Evidence: [`evidence/blnt/deployed-lineages.json`](evidence/blnt/deployed-lineages.json)
- Drift gate: `internal/ops/chops/wasm_drift.go`
