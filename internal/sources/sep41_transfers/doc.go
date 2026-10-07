// Package sep41_transfers decodes SEP-41 transfer, approve, set_admin and
// set_authorized events into a queryable hypertable for per-account
// net-position; mint, burn and clawback belong to [sep41_supply].
//
// Topic shapes (SEP-41 v0.4.1, CAP-67):
//
//	transfer        ("transfer", from, to[, sep0011_asset])  data: i128 OR map{amount, to_muxed_id}
//	approve         ("approve", from, spender)               data: [i128 amount, u32 live_until_ledger]
//	set_admin       ("set_admin"[, admin])                   data: Address(new_admin)
//	set_authorized  ("set_authorized", id[, sep0011_asset])  data: bool authorize
package sep41_transfers
