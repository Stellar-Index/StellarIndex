---
adr: 0001
title: Horizon is not in the Stellar Index architecture
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0001: Horizon is not in the Stellar Index architecture

## Context

Horizon was the canonical Stellar client API, but `stellar/go` was archived on 2025-12-16 and the
Stellar Foundation now steers new builds to Galexie, the Ingest SDK and stellar-rpc.
Project directive of 2026-04-22: "Horizon is deprecated and we will not be using it."

## Decision

Horizon is not a component of Stellar Index: we do not run it, ingest from it, or proxy to it
(including SDF's hosted instance). A third-party integration whose only path to us is Horizon is
declined until it supports a supported Stellar data path.

Data comes from the ledger lake: Galexie writes to our S3-compatible store and the dispatcher decodes
it. `stellar-core` survives only as Galexie's captive-core subprocess and `stellar-rpc` only as the
`rpc-probe` diagnostic (see AGENTS.md invariant 6). History seeding uses `rs-stellar-archivist`
mirrors; oracle and protocol data are read directly from Soroban contracts.

## Invariant

No Horizon binary or database is run, nothing ingests from or proxies to Horizon, and no Horizon
client is imported (AGENTS.md invariant 2; lint rule `C/no-horizon` in `scripts/ci/lint-imports.sh`).
One-off operator verifiers that query Horizon, such as `reconcile-balances`, are outside production
ingest and exempt.

Trades derive from `xdr.LedgerCloseMeta` ledger parsing; Horizon effect ids are not part of the
data model.

## Consequences

We follow the supported ecosystem path and keep one ingestion path and a smaller operator footprint.
We forgo Horizon-compatible semantics and decode trades, effects and operations ourselves; the
OpenAPI spec is the only contract and we do not market as a Horizon replacement.

## Evidence

Ingest path: [docs/architecture/ingest-pipeline.md](../architecture/ingest-pipeline.md). Rule
cited in AGENTS.md invariant 2.
