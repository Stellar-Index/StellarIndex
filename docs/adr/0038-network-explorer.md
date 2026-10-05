---
adr: 0038
title: Network explorer (full Stellar + Soroban) over the certified lake
status: Accepted
date: 2026-06-14
supersedes: []
superseded_by: null
---

# ADR-0038: Network explorer (full Stellar + Soroban)

## Context

The ClickHouse lake (ADR-0034) already holds the whole chain to genesis, contiguous and hash-chain-verified, which is the most expensive part of an explorer.
What remained was serving it, deriving account state, and rendering it.

## Decision

Build a network explorer as a read layer over the lake. Postgres stays the pricing served tier, and chain history is never put in it.

- **Phase A, read API.** `/v1/ledgers`, `/v1/ledgers/{seq}` (and `/transactions`, `/operations`), `/v1/operations`, `/v1/tx/{hash}`, `/v1/contracts/{id}` and `/v1/search` (dispatch by strkey, hash or ledger shape). Handlers live in `internal/api/v1/explorer`, reading through `clickhouse.ExplorerReader`.
- **Phase B, account history.** `/v1/accounts/{g}/transactions` and `/operations`. An account's non-source activity (a payment to it, a crossed offer, a claimant) comes from the `stellar.operation_participants` index, derived at extract time by `xdrjson.ParticipantAccounts`. Responses stamp `scope: "all"`.
- **Phase C, account state.** `stellar.ledger_entry_changes` is populated by `extractLedgerEntryChanges`. Current state is a `ReplacingMergeTree` projection keyed on entry key and served as `GET /v1/accounts/{g}`.
- **Phase D, UI.** Explorer routes under `web/explorer` are static shells that fetch the API client-side, so the static-export model is kept (ADR-0044 covers the planned move off it).
- Both operator-gated data jobs have run. Measured on r1 2026-09-30, `ledger_entry_changes` (163,658,626,082 rows) and `operation_participants` (4,513,850,669 rows) are each populated in every 1M-ledger partition 0..64 (tip about 64.69M). That is a per-partition population check, not a row-level completeness proof.

## Invariant

- Money is never truncated: op and entry amounts are `*big.Int` on the way in and decimal strings on the wire (ADR-0003).
- The explorer reads ClickHouse, not Postgres, for chain history.
- XDR to JSON decode is centralised in `internal/xdrjson`, and handlers do no ad-hoc decode. `internal/xdrjson/operation_type_parity_test.go` guards operation-type coverage.
- No Horizon (ADR-0001): decode comes from our own raw XDR.
- A closed ledger is final, so explorer responses are cacheable by ledger sequence or tx hash. The closed-bucket serving rules of ADR-0015 do not apply to them.

## Consequences

- A full explorer costs a fraction of building one from scratch because the verified substrate exists.
- Storage is large (the entry-change and participant tables sit on top of the operation history).
- Participants from operation results, such as path-payment intermediaries, are not indexed. Only operation-body participants are.
- Decode must stay exhaustive and i128-correct, and account-balance exactness (reserves, liabilities, sponsorship) is the subtle part.

## Evidence

`internal/api/v1/server.go` (explorer routes), `internal/storage/clickhouse/explorer_reader.go`, `internal/storage/clickhouse/extract.go`, `internal/storage/clickhouse/extract_entry_changes.go`, `internal/xdrjson/`, `deploy/clickhouse/tier1_schema.sql`, `web/explorer/src/app/`.
