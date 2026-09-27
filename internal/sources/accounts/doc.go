// Package accounts is the canonical AccountEntry observer per
// ADR-0021. It plugs into the dispatcher's fourth hook
// (LedgerEntryChangeDecoder) and emits one observation per
// LedgerEntryChange touching an operator-watched G-strkey.
//
// Operator usage: the watched set is the union of
// `[supply] sdf_reserve_accounts` and `[metadata] watched_issuer_accounts`
// (pipeline.AccountObserverWatchSet); there is no `[accounts]` config
// block. sdf_reserve_accounts is the SDF reserve list and nothing else:
// every account in it has its balance subtracted from XLM circulating
// supply (ADR-0011 Algorithm 1) and is diffed daily against the list
// SDF publishes. Issuer accounts observed for their home_domain go in
// watched_issuer_accounts, never in the reserve list.
//
// Output: [Observation] events flowing through the standard
// dispatcher → consumer pipeline. The indexer-side sink writes
// each observation to the `account_observations` hypertable
// (introduced in Task #60); two readers consume that table, each by
// account id — metadata.LCMHomeDomainResolver (latest home_domain,
// ahead of the operator-static `[metadata.issuer_home_domains]` map)
// and supply.LCMReserveBalanceReader (ahead of the operator-static
// `[supply.reserve_balances_stroops]` map). See ADR-0021.
//
// The observer is operator-watched-set driven by default — no
// "watch every account" mode at v1. Switching to that mode would
// require a separate ADR; the table-size implications (50M+
// network accounts) are non-trivial and bear further design
// before being on by default.
package accounts
