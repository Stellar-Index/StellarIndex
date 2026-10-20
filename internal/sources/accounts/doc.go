// Package accounts is the AccountEntry observer (ADR-0021): a LedgerEntryChange decoder emitting
// one observation per change to a watched G-strkey, written to `account_observations`.
//
// The watched set is `[supply] sdf_reserve_accounts` ∪ `[metadata] watched_issuer_accounts`
// (pipeline.AccountObserverWatchSet). sdf_reserve_accounts holds SDF reserves only, because every
// balance in it is subtracted from XLM circulating supply (ADR-0011 Algorithm 1); home_domain
// watches go in watched_issuer_accounts. There is no watch-every-account mode without an ADR.
package accounts
