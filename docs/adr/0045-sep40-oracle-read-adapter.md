---
adr: 0045
title: SEP-40 on-chain oracle read adapter — defer generic reader; serve surface already ships
status: Proposed
date: 2026-07-06
supersedes: []
superseded_by: null
---

# ADR-0045: SEP-40 on-chain oracle read adapter

Deciders: @ash (pending). Scope investigation: BACKLOG #60 (RFP §6
accommodation). Full analysis:
[docs/architecture/sep40-oracle-read-adapter.md](../architecture/sep40-oracle-read-adapter.md).

## Context

BACKLOG #60 asks whether a "SEP-40 on-chain oracle read adapter" fits
the existing oracle infrastructure. Investigation found:

1. **SEP-40 is a read interface, not an event schema.** A conformant
   oracle stores `PriceData` and answers `lastprice`/`prices`/`assets`
   reads; the standard mandates no events. So a *generic* SEP-40
   ingest adapter means **reading contract state**, not decoding
   events — architecturally distinct from every on-chain source we run.
2. **The SEP-40 *serve* surface already ships.**
   `internal/api/v1/oracle_sep40.go` exposes our aggregated prices via
   `/v1/oracle/{lastprice,prices,x_last_price,latest}` in the SEP-40
   method shape. That is the outbound half and is not what #60 needs.
3. **Every oracle we ingest is already a SEP-40 contract**, ingested
   by its native mechanism: reflector + redstone via events, band via
   its event-less `relay()` ContractCall. None needs a SEP-40 *read*
   adapter today.
4. A generic reader would need: an event-less trigger (a
   `LedgerEntryChangeDecoder` over each oracle's storage, or a periodic
   state-snapshot over the lake); a contract-identity-gated registry of
   which oracles/assets to trust (ADR-0035/0040); per-contract
   `decimals()`/`base()` reads; and a state-read completeness model
   (ADR-0033) distinct from event coverage. Its hardest parts (storage
   key layout, state-read completeness) cannot be designed honestly
   without a concrete integration target — guessing a layout would
   repeat the defindex tag-1.0.0-vs-mainnet mismatch.

## Decision

**Defer building a generic SEP-40 read adapter.** Ship the design note
+ this ADR as the record and starting point. Build it only when a
concrete SEP-40 oracle appears that we must ingest and cannot reach via
the existing event/ContractCall paths. If built, it rides the ADR-0039
Soroban contract state reader (read-time decode from the lake — no
stellar-rpc, ADR-0001), emits `canonical.OracleUpdate` (reusing the
existing oracle sink arm, no new hypertable), and is gated on contract
identity via `internal/contractid` with new oracles failing closed into
an ADR-0033 recognition gap until curated.

## Consequences

- **Positive:** No speculative infrastructure. The already-shipped
  serve surface + three native oracle ingests cover today's needs; the
  design is captured so a future build starts from a considered plan,
  not a cold read.
- **Negative:** A SEP-40 oracle that emits no events and is not band is
  not ingestible until this is built. Accepted: none is in scope now.
- **Operational impact:** None until built. When built, one new
  config-gated source + curated contract registry to operate.
- **Downstream design impact:** Commits a future adapter to the
  state-read (ADR-0039) substrate + identity gating (ADR-0035/0040) +
  one-writer-per-domain (ADR-0031/0032) rather than a bespoke ingest
  goroutine or an RPC poller.

## Alternatives considered

1. **Build the generic reader now** — rejected: its hardest parts need
   a real target to design against; building blind risks a
   wrong-schema decoder (the defindex Phase-A failure mode).
2. **Treat SEP-40 as "already covered" and close #60** — rejected: the
   *serve* surface is covered, but a generic on-chain *read* adapter is
   genuinely absent; recording that honestly (vs. silently closing) is
   the point of this ADR.
3. **Poll oracles over stellar-rpc** — rejected: violates the ADR-0001
   / CLAUDE.md invariant that stellar-rpc is not in the ingest path;
   the lake + ADR-0039 state reader is the sanctioned substrate.

## References

- Design note: [docs/architecture/sep40-oracle-read-adapter.md](../architecture/sep40-oracle-read-adapter.md)
- Related ADRs: [0039](0039-soroban-contract-state-reader.md) (state reader), [0035](0035-factory-anchored-contract-gating.md) / [0040](0040-completing-contract-gating.md) (identity gating), [0031](0031-data-derived-coverage-signal.md) / [0032](0032-per-source-tables-as-projections.md) (one writer per domain), [0001](0001-horizon-deprecated.md) (no Horizon / no rpc ingest)
- Existing code: `internal/api/v1/oracle_sep40.go`, `internal/sources/{reflector,redstone,band}`
- External spec: SEP-40 Price Feed Oracle
