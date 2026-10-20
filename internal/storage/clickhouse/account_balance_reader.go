// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// AccountBalanceSnapshot is the lake's LATEST known native (XLM) balance for one account, folded from stellar.ledger_entry_changes.
type AccountBalanceSnapshot struct {
	// Stroops is argMax(balance, (ledger_seq, intra_ledger_seq)): the balance from the LAST change
	// in intra-ledger walk order. The tie-break matters because one ledger can hold several
	// changes to an account and ledger_seq alone would pick an ARBITRARY mid-ledger balance.
	// Int64 suffices: stroops fit it for the whole XLM supply, unlike Soroban i128 (ADR-0003).
	Stroops int64
	// AtLedger is the ledger_seq that snapshot was recorded at.
	AtLedger uint32
	// Snapshots is the total count of account-entry change rows we
	// hold for this account in the queried range — zero means the
	// account is outside our coverage entirely (never observed).
	Snapshots uint64
}

// QueryAccountBalance returns the latest native balance for accountID; found=false means no rows (outside coverage, not zero); backs only the reconcile-balances diagnostic, do not copy its `account_id = ?` filter into a handler.
func QueryAccountBalance(ctx context.Context, addr, accountID string) (snap AccountBalanceSnapshot, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return AccountBalanceSnapshot{}, false, err
	}
	defer func() { _ = conn.Close() }()

	const query = `
		SELECT argMax(balance, (ledger_seq, intra_ledger_seq)) AS bal, max(ledger_seq) AS at_ledger, count() AS snapshots
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
		// Aggregate queries without GROUP BY always return exactly one row even when zero source rows matched — argMax/max
		// degrade to their zero values in that case, which is indistinguishable from a genuine 0-stroop balance without
		// checking snapshots.
		return AccountBalanceSnapshot{}, false, nil
	}
	return AccountBalanceSnapshot{Stroops: bal, AtLedger: atLedger, Snapshots: snapshots}, true, nil
}

// sampleAccountIDsQuery is [SampleAccountIDs]' frame: args are
// (minLedger, seed, n).
const sampleAccountIDsQuery = `
	SELECT account_id
	FROM stellar.ledger_entry_changes
	WHERE entry_type = 'account' AND account_id != '' AND ledger_seq > ?
	GROUP BY account_id
	ORDER BY cityHash64(account_id, ?), account_id
	LIMIT ?
`

// SampleAccountIDs returns up to n distinct account_ids with an 'account' change above minLedger, in a seeded cityHash64(account_id, seed) order (same seed, same cohort).
func SampleAccountIDs(ctx context.Context, addr string, minLedger uint32, seed uint64, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, sampleAccountIDsQuery, minLedger, seed, n)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: sample account ids above ledger %d (seed %d): %w", minLedger, seed, err)
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
