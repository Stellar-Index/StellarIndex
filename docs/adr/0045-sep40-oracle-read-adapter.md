---
adr: 0045
title: SEP-40 on-chain oracle read adapter — defer generic reader; serve surface already ships
status: Accepted
date: 2026-07-06
supersedes: []
superseded_by: null
---

# ADR-0045: SEP-40 on-chain oracle read adapter

## Context

SEP-40 is a read interface, not an event schema, so a generic ingest adapter would read contract state, unlike every event-decoding source we run.
Every oracle we ingest already arrives by its own path (reflector and redstone by events, band by its `relay()` ContractCall), and the outbound SEP-40 surface already ships.

## Decision

Do not build a generic SEP-40 read adapter until a concrete SEP-40 oracle appears that cannot be reached by the event or ContractCall paths, because its storage layout and state-read completeness cannot be designed without a real target.
If built, it uses the Soroban contract state reader (ADR-0039, read-time decode from the lake, no stellar-rpc), emits `canonical.OracleUpdate` through the existing oracle sink arm with no new hypertable, and is gated on contract identity through `internal/contractid`, with unrecognised oracles failing closed into a recognition gap (ADR-0033) until curated.

## Invariant

- No production ingest imports `internal/stellarrpc` (AGENTS.md invariant [6]): `scripts/ci/lint-imports.sh` rule A.
- `/v1/oracle/{lastprice,prices,x_last_price,latest}` keep the SEP-40 method shape: `internal/api/v1/oracle_sep40.go`.

## Consequences

- No speculative infrastructure; the design note `docs/architecture/oracle-manipulation-defense.md` §"SEP-40" is the starting point for a future build.
- A SEP-40 oracle that emits no events and is not band cannot be ingested until the adapter exists.
- Rejected: building the reader now (a wrong-schema decoder risk), closing the item as covered (the serve surface is covered, the read adapter is absent), and polling over stellar-rpc.

## Evidence

`internal/api/v1/oracle_sep40.go`, `internal/sources/{reflector,redstone,band}`.
