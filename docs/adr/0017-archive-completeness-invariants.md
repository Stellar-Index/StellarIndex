---
adr: 0017
title: Archive completeness invariants and dual-archive integrity model
status: Accepted
date: 2026-04-27
supersedes: []
superseded_by: null
---

# ADR-0017: Archive completeness invariants and dual-archive integrity model

## Context

Two archives back the indexer and the verifier: the primary `galexie-archive/` MinIO bucket of per-ledger XDR, and the cross-anchor `/srv/history-archive/` mirror of SDF's history archive.
On 2026-04-27 the verifier crashed on about 35,000 missing primary files and had been silently tolerating roughly 6,800 missing mirror files as "missed = skip".

## Decision

Archive completeness is a hard invariant, not a soft tolerance. Four contracts hold in steady state on R1:

1. **Primary structural completeness.** Each closed partition `<HASH>--<N>-<N+63999>/` below `network_head` holds exactly 64,000 files; the open partition holds `network_head - start + 1`.
2. **Primary chain-link integrity.** For every adjacent `(N, N+1)`, `SHA256(ledger[N].header) == ledger[N+1].previousLedgerHash`; a sequence gap is a chain break, with no tolerance.
3. **Cross-anchor structural completeness.** Every checkpoint `seq` (`seq % 64 == 63`) up to the mirror's high-water has its `ledger-*.xdr.gz` present and decodable.
4. **Cross-anchor verification.** For every checkpoint, the mirror's `LedgerHeaderHistoryEntry.Hash` equals the primary's hash for that ledger.

Contract 3 is bounded by the mirror's own coverage, which is measured before each walk, because the mirror fill job lags the live tip:
- An absence above the mirror's high-water is `unmirrored`, a delivery lag that is counted and logged and never fatal.
- An absence at or below the high-water, including below the floor, is `missed`, a hole.
- An unreadable `-archive-root` measures no span, so every absence is `missed`.
- A run that matched nothing is inconclusive and cannot exit 0.

The checkpoint tier certifies only what it anchored: its high-water is clamped to the mirror's high-water, never rewinds a persisted watermark, and freezes when a run fails.
`verify-archive` exits non-zero on a contract failure. `-fail-on-missed` defaults to `true` and is set on both tier-B units; `-fail-on-missed=false` is the explicit opt-out.

R1 is the integrity leader and runs the contracts daily. Other regions delegate contracts 3 and 4 (and R2 contract 1) to it (ADR-0016, superseded by ADR-0050) and mark themselves `ReducedRedundancy` when R1's last successful run is older than 26 hours.
Rejected: tolerating missed files, per-ingest-cycle verification, mirroring the history archive to every region, and verifying against SDF over HTTPS with no local mirror.

## Invariant

- A hole inside the mirror's coverage fails the run; only an absence above its high-water is tolerated, and only as `unmirrored`.
- The checkpoint high-water never advances past what the mirror's own coverage anchored; the `internal/ops/archive` tests enforce it.
- A tolerated count never turns a vacuous run into a pass.
- The shipped daemon currently enforces cross-anchor verification only (F-0019); the other contracts are not yet enforced, so none may be cited as a standing guarantee.

## Consequences

Gap-detection latency is one daily cron cycle instead of a manual run, and the verifier's exit code now means something.
Bootstrap is a one-shot fill of existing gaps (`docs/operations/archive-completeness.md`).
A failed tier-B unit raises `stellarindex_verify_archive_tier_b_unit_failed`; the checkpoint counters reach Prometheus only through the unscraped opt-in `-metrics-listen`, so a stalled mirror fill shows only as a journal warning and a stuck watermark.

## Evidence

`internal/ops/archive/verify_archive*.go` and tests, `internal/archivecompleteness/`, and `docs/operations/archive-completeness.md`.
