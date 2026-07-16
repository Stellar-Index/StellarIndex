// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// AccountBalanceSnapshot is our lake's LATEST known NATIVE (XLM)
// balance for one account, folded from stellar.ledger_entry_changes.
// Backs stellarindex-ops reconcile-balances (ADR-0033-style
// verification harness) — the account-observer / classic-movements
// readers already query this table for other entry types; this is
// the account/native-balance analogue.
type AccountBalanceSnapshot struct {
	// Stroops is argMax(balance, ledger_seq) — the balance recorded
	// at the highest ledger_seq we've observed for this account.
	// stellar.ledger_entry_changes.balance is Int64 (stroops fit
	// comfortably within int64 for the whole XLM supply — unlike
	// arbitrary Soroban i128 token amounts, which is why this column
	// isn't NUMERIC/big per ADR-0003).
	Stroops int64
	// AtLedger is the ledger_seq that snapshot was recorded at.
	AtLedger uint32
	// Snapshots is the total count of account-entry change rows we
	// hold for this account in the queried range — zero means the
	// account is outside our coverage entirely (never observed).
	Snapshots uint64
}

// QueryAccountBalance returns our lake's latest recorded native
// balance for accountID (the strkey G... address), read from
// stellar.ledger_entry_changes. found is false when the account has
// zero rows — outside our coverage, not a zero balance (a real
// zero-balance account still has at least one 'created'/'updated'
// row).
func QueryAccountBalance(ctx context.Context, addr, accountID string) (snap AccountBalanceSnapshot, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return AccountBalanceSnapshot{}, false, err
	}
	defer func() { _ = conn.Close() }()

	const query = `
		SELECT argMax(balance, ledger_seq) AS bal, max(ledger_seq) AS at_ledger, count() AS snapshots
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'account' AND account_id = ?
	`
	var (
		bal       int64
		atLedger  uint32
		snapshots uint64
	)
	if err := conn.QueryRow(ctx, query, accountID).Scan(&bal, &atLedger, &snapshots); err != nil {
		return AccountBalanceSnapshot{}, false, fmt.Errorf("clickhouse: query account balance %s: %w", accountID, err)
	}
	if snapshots == 0 {
		// Aggregate queries without GROUP BY always return exactly one
		// row even when zero source rows matched — argMax/max degrade
		// to their zero values in that case, which is indistinguishable
		// from a genuine 0-stroop balance without checking snapshots.
		return AccountBalanceSnapshot{}, false, nil
	}
	return AccountBalanceSnapshot{Stroops: bal, AtLedger: atLedger, Snapshots: snapshots}, true, nil
}

// SampleAccountIDs returns up to n distinct account_ids that have a
// stellar.ledger_entry_changes 'account' entry above minLedger —
// reconcile-balances' -sample source set. Restricting to
// ledger_seq > minLedger biases the sample toward accounts active
// recently enough that their LATEST recorded snapshot approximates
// current chain state (an account untouched since minLedger could
// have changed on-chain without us knowing, which would show up as a
// false MISMATCH rather than a real one).
//
// Ordering by cityHash64(account_id) is a deterministic pseudo-shuffle
// — cheap, reproducible across runs (useful for debugging a flaky
// sample), and avoids ClickHouse's true `rand()`, which the ops
// contract for this table doesn't need.
func SampleAccountIDs(ctx context.Context, addr string, minLedger uint32, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	const query = `
		SELECT account_id
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'account' AND account_id != '' AND ledger_seq > ?
		GROUP BY account_id
		ORDER BY cityHash64(account_id)
		LIMIT ?
	`
	rows, err := conn.Query(ctx, query, minLedger, n)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: sample account ids above ledger %d: %w", minLedger, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, n)
	for rows.Next() {
		var accountID string
		if err := rows.Scan(&accountID); err != nil {
			return nil, fmt.Errorf("clickhouse: scan sampled account id: %w", err)
		}
		out = append(out, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: sample account ids rows: %w", err)
	}
	return out, nil
}
