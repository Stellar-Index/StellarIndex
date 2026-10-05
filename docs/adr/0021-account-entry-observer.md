---
adr: 0021
title: AccountEntry observer — live home-domain + reserve-balance tracking
status: Accepted
date: 2026-04-30
supersedes: []
superseded_by: null
---

# ADR-0021: AccountEntry observer — live home-domain + reserve-balance tracking

## Context

Issuer home domains and SDF reserve balances were hand-maintained config maps because ingestion did not observe `AccountEntry` changes. Every LedgerCloseMeta already carries them, but the existing dispatcher hooks (events, ops, contract calls) see only transaction artifacts, and Horizon (ADR-0001) and per-request RPC are ruled out.

## Decision

Add a dispatcher hook, `LedgerEntryChangeDecoder`, and one canonical implementation, `internal/sources/accounts.AccountEntryObserver`, writing the `account_observations` hypertable (migration 0010).

- **Hook.** `Matches(change)` is a cheap pre-filter on the entry type; `Decode(ctx)` emits zero or more events for one change, with ledger, close time, tx hash and op index (empty and -1 for fee-meta changes). The walker visits per-op changes and the tx-level fee changes. Errors are skip-and-count, as for the other hooks.
- **Not an existing hook:** an `AccountEntry` change is a side effect of any classic op (and fees), so it is neither an event nor an op-type filter.
- **Table.** One row per `(account_id, ledger)` with `balance_stroops` NUMERIC (ADR-0003), nullable `home_domain` (NULL, not empty string, when unset), `flags` and `seq_num`; hypertable on `observed_at`, 7-day chunks. Readers take the latest row by `observed_at DESC`.
- **Idempotent insert.** Identity is `(account_id, ledger, observed_at)`, because Timescale needs the partition column in the key and `observed_at` is the ledger close time. The conflict action is `DO UPDATE ... WHERE intra_ledger_seq <= EXCLUDED.intra_ledger_seq`: an account is touched several times in a ledger (fee phase, then operations) and the LAST change is the ledger-final state, so `DO NOTHING` would freeze the fee-phase balance. A re-walk re-assigns the same position and rewrites the same value.
- **Readers** over the same table: `metadata.LCMHomeDomainResolver` (latest observed `home_domain`; an observed absence of one wins, and only an unobserved issuer falls back to `[metadata.issuer_home_domains]`) and `supply.LCMReserveBalanceReader` (sum of each account's latest `balance_stroops` at or before a ledger; an error if any account has no observation by then).
- **Fallback is permanent.** The live reader is chained ahead of `supply.ConfigReserveBalanceReader` in both refresh paths (`docs/architecture/supply-pipeline.md`, "The chained-fallback reader pattern"); the static map is the bootstrap fallback, not an interim. A reserve account that never changes emits no change, so `stellarindex-ops supply seed-observations` seeds each `[supply] sdf_reserve_accounts` entry's latest `AccountEntry` from the lake's `ledger_entries_current` projection (ADR-0034). Accounts dormant since before the lake's capture window need a `state-snapshot` run first, and the seeder reports them rather than fabricating.
- **Watched set, not global.** The observer records accounts named by `[supply] sdf_reserve_accounts` and `[metadata] watched_issuer_accounts`. Watching every account (50M+ accounts times N observations) needs its own ADR.
- **One canonical decoder,** not per-source: `AccountEntry` has one shape network-wide.

## Invariant

- Observations are derived from LCM ledger-entry changes only: never Horizon, never per-request RPC.
- The conflict guard has since gained `walk_version` (migrations 0120, 0199). The published row for an `(account, ledger)` is the last intra-ledger change, and a re-walk of a range is idempotent. Enforced by the `intra_ledger_seq` guard in `internal/storage/timescale/account_observations.go`.
- Amounts are NUMERIC end to end, never a narrower integer (ADR-0003).
- The live reserve reader never replaces the config reader silently: an account with no observation is an error that falls back, not a zero.

## Consequences

- One new hypertable, one hook (`Dispatcher.AddEntryDecoder`), two readers; the aggregator can refresh supply snapshots per tick.
- The static reserve map stays valid and must be kept for bootstrap.
- The table stays small only while the watched set does.

## Evidence

- `internal/sources/accounts`, `internal/dispatcher/dispatcher.go` (`LedgerEntryChangeDecoder`), `internal/pipeline/sink.go` (writer), `internal/supply/` (`LCMReserveBalanceReader`).
- Migrations 0010 and 0111 (`intra_ledger_seq`); ADR-0001, ADR-0003, ADR-0011.
