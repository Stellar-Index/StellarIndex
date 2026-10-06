---
adr: 0016
title: Per-region storage strategies for archival nodes (Hetzner full / AWS hybrid / Vultr hybrid)
status: Superseded
date: 2026-04-27
supersedes: []
superseded_by: [0050]
---

# ADR-0016: Per-region storage strategies for archival nodes

## Context

Regions differ in where cheap, low-latency galexie bytes live, so one identical storage stack per region wastes money.
ADR-0015 makes served output byte-equivalent across regions, which leaves each region free to get its input differently.

## Decision

Superseded by ADR-0050 (2026-08-21); `docs/architecture/ha-plan.md` is authoritative and no region is implemented from this ADR.
Rejected by ADR-0050: Model A (Postgres replication from R1 as the canonical history), the R2-on-AWS shape, and ClickHouse-blind per-region sizing.
What survives is one principle, carried into ADR-0050 section 4: regions may use different storage plumbing but must serve byte-identical closed-bucket output.
The archive verification tiers it defined still apply:

- Tier A checks chain integrity of ingested ledgers; Tier B verifies a region's own bytes against its local `/srv/history-archive` mirror and is R1-only.
- Tier D (`verifyArchivePeers`, `internal/ops/archive/verify_archive.go`) is pure peer-vs-peer consensus: it fetches each peer's `history-XXXXXXXX.json` and cross-compares against one reference peer, with no local ledger hash. R2 and R3 have no local mirror to hold one.
- Tier E (`stellar-archivist scan --verify`) needs transaction, result and bucket files, which R1's trimmed mirror (`history/` + `ledger/`, about 21 GB since 2026-05-21) no longer has. It is operator-run against a peer archive and has no cron.

## Invariant

- R1's scheduled tiers are now Tier A + B + D; every tier named in that sentence must be run by a unit or task in the archival-node ansible role, and `TestVerifyArchiveTiers_ADRIntegrityLeaderTiersAreScheduled` in `internal/ops/archive/verify_archive_tier_e_test.go` enforces it.
- Tier D is never described as comparing against a local ledger hash; `TestDoc_TierDDescribedAsPeerOnly_RLT304` in `internal/ops/archive/verify_archive_doc_truth_test.go` enforces it for the architecture doc.
- Storage shape never changes served closed-bucket output (ADR-0015).

## Consequences

R2 and R3 cannot verify their bytes against SDF's signed archive on their own and rely on R1's Tier B.
R1's data pool is raidz1 (single parity, about 18.3 TB usable) rather than the raidz2 first decided; `configs/ansible/inventory/r1.example.yml` is the authority that `scripts/ci/lint-docs.sh` section 18 lints against.
Details: `docs/architecture/storage-considerations.md` and `docs/operations/archive-completeness.md`.

## Evidence

`internal/ops/archive/verify_archive.go` and its tests, and the archival-node role under `configs/ansible/roles/archival-node/`.
