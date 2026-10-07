// Package accounts is the AccountEntry observer (ADR-0021): a
// LedgerEntryChange decoder emitting one observation per change to a
// watched G-strkey, written to `account_observations`.
//
// The watched set is `[supply] sdf_reserve_accounts` ∪ `[metadata]
// watched_issuer_accounts` (pipeline.AccountObserverWatchSet); there is no
// `[accounts]` block. sdf_reserve_accounts is the SDF reserve list and
// nothing else: every balance in it is subtracted from XLM circulating
// supply (ADR-0011 Algorithm 1) and the list is diffed daily against SDF's.
// Accounts watched for home_domain go in watched_issuer_accounts.
// metadata.LCMHomeDomainResolver and supply.LCMReserveBalanceReader read
// the table, each ahead of its operator-static map. There is no
// watch-every-account mode (50M+ accounts); adding one needs an ADR.
package accounts
