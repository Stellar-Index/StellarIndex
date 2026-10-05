---
adr: 0004
title: Tier-1 three-validator aspiration (post-launch)
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0004: Tier-1 three-validator aspiration (post-launch)

## Context

Indexing and pricing need only non-validating watcher nodes. The project's long-term posture is to
become a Tier-1 Stellar organisation, so interim infrastructure, procurement and security choices
must not close that path off.

## Decision

The long-term posture is three geographically separated full validators: one in our Vancouver colo,
two in other regions chosen during post-launch procurement. Each has its own independent public
history archive (MinIO behind nginx, own DNS hostname). Our stellar.toml publishes three
`[[VALIDATORS]]` entries (SEP-20 self-verification). Seed keys are HSM-protected.

Tier-1 inclusion is emergent, not granted by SDF: we earn it through uptime and quorum-set
inclusion, and aim to apply within 12 months of launch. The phasing is directional: testnet
validator in months 1-3, first pubnet validator in months 3-6, two more in months 6-9, promotion to
full validators and application in months 9-12. The validator track is a separate workstream with
its own runbooks, SLOs and on-call.

## Invariant

No validator key material lives on disk unencrypted (AGENTS.md invariant 5, enforced in review).

Validator and API identity share the `stellarindex.io` domain, so the stellar.toml publication path
is protected as a production secret.

## Consequences

We own the data path from the network tip and gain standing and narrative as an infrastructure
operator. It costs three machines, bandwidth, HSMs (2-6 weeks lead time) and 24/7 operations,
including prompt stellar-core upgrades and correct protocol votes. Archive servers need high public
egress, and once validators run we publish history archives instead of only consuming them.

## Evidence

Not implemented: pre-launch scope is watcher nodes only. The key rule is cited in AGENTS.md
invariant 5.
