---
adr: 0012
title: Quorum-set composition (placeholder)
status: Proposed
date: 2026-05-12
supersedes: []
superseded_by: null
---

# ADR-0012: Quorum-set composition

## Context

Planned stub; the full ADR is written when the Tier-1 validator work (ADR-0004 Phase 3) begins.
This number is reserved so ADR-0004's reference to "the future quorum-set ADR" resolves.

## Decision

None yet. The full ADR will cover:

- which third-party validators are in our quorum set (shortlist: SDF, LOBSTR, Satoshipay, Whalestack, Franklin Templeton);
- the HALT-LIVE-DROP scoring we apply when a validator behaves poorly;
- whether R1's quorum set must differ from R2's and R3's (likely no);
- the thresholds and majorities in stellar-core's `[QUORUM_SET]` block.

## Invariant

The future ADR must preserve:

- Tier-1 status per ADR-0004: three independent regions, three validator keys, three history archives.
- The quorum set never includes a validator we operate, which would void the independence claim.
- No validator has more than 33% effective weight, Stellar's consensus safety threshold.

## Consequences

Until the ADR lands, nothing may assume a quorum-set design beyond these constraints.
Related: ADR-0004 (parent commitment), ADR-0008 (HA shape).

## Evidence

None; no code implements this yet. The ADR README index lists this number as Planned.
