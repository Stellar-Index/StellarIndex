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
	// Stroops is argMax(balance, (ledger_seq, intra_ledger_seq)) — the
	// balance from the LAST change in canonical intra-ledger walk order. The
	// intra_ledger_seq tie-break matters: one ledger can hold several changes
	// to an account, and ledger_seq alone ties them, picking an ARBITRARY
	// mid-ledger balance. The column is Int64: stroops fit within int64 for
	// the whole XLM supply, unlike Soroban i128 amounts (ADR-0003).
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
//
// SHAPE NOTE: this is the ONE remaining `account_id = ?` bloom-shaped filter
// in the package, and it is deliberate: it backs ONLY the `reconcile-balances`
// operator diagnostic (internal/ops/chops), never a serving path, and the
// aggregate over the account's FULL change history is the tool's purpose.
// Every SERVING account read rides a primary-key shape. Do not copy this
// filter shape into a handler path.
// SERVING account read rides a primary-key shape: key_xdr point/prefix
// (account state), the table's own sort key (account_movements), or
// the ops_by_source / operation_participants projections (history).
// Do not copy this filter shape into a handler path.
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
		// Aggregate queries without GROUP BY always return exactly one
		// row even when zero source rows matched — argMax/max degrade
		// to their zero values in that case, which is indistinguishable
		// from a genuine 0-stroop balance without checking snapshots.
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

// SampleAccountIDs returns up to n distinct account_ids that have a
// stellar.ledger_entry_changes 'account' entry above minLedger —
// reconcile-balances' -sample source set. Restricting to ledger_seq >
// minLedger biases the sample toward recently active accounts, whose LATEST
// snapshot approximates current chain state (a stale one would show up as a
// false MISMATCH). The frame is the change log itself, not the
// ledger_entries_current projection, so an account the projection lost is
// still drawable; callers keep the scan affordable with a tip-relative
// minLedger.
//
// The order is cityHash64(account_id, seed): a seeded pseudo-shuffle, so
// each seed draws a different cohort while one seed reproduces its cohort
// exactly. account_id breaks hash ties so a seed's order is total.
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
