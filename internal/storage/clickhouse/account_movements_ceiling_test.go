// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestAccountMovementsQuery_LedgerCeiling pins F055's storage half: the
// /movements merge boundary is a SQL predicate on this read, and its
// set-signal is HasMaxLedger — never "MaxLedger != 0".
//
// Ceiling 0 is REACHABLE, not a sentinel for "no ceiling": a deployment
// that installs the movements floor at genesis (testnet/futurenet,
// timescale.InstallMovementsFloor(1)) computes ceiling = floor-1 == 0
// whenever the cap67 watermark is absent or unreadable, and the arm must
// then serve nothing at all. A `MaxLedger > 0` guard drops the clause at
// exactly that value and serves the whole archive alongside the Postgres
// tail instead — the fail-open inverse of what the ceiling is for.
func TestAccountMovementsQuery_LedgerCeiling(t *testing.T) {
	const clause = " AND ledger <= ?"

	t.Run("ceiling zero still emits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 0}, false)
		if !strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with a SET ceiling of 0 is missing %q — the CH arm would serve "+
				"its whole archive at an installed genesis floor (F055):\n%s", clause, q)
		}
	})

	t.Run("set ceiling emits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 58_000_000}, true)
		if !strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with a set ceiling is missing %q:\n%s", clause, q)
		}
	})

	t.Run("unset ceiling omits the clause", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{}, false)
		if strings.Contains(q, clause) {
			t.Errorf("accountMovementsQuery with no ceiling emitted %q — an unbounded read must stay "+
				"unbounded:\n%s", clause, q)
		}
	})

	t.Run("clause order mirrors arg order", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{
			Kind: "payment", Direction: AccountMovementSent, Asset: "native",
			HasMaxLedger: true, MaxLedger: 58_000_000,
		}, true)
		assetIdx := strings.Index(q, "asset = ?")
		ledgerIdx := strings.Index(q, clause)
		curIdx := strings.Index(q, "(ledger, tx_hash, op_index, leg_index) < (?, ?, ?, ?)")
		if !(assetIdx >= 0 && ledgerIdx > assetIdx && curIdx > ledgerIdx) {
			t.Errorf("the ledger ceiling must bind between the asset filter and the cursor keyset "+
				"(ExplorerReader.AccountMovements appends its args in that order):\n%s", q)
		}
	})

	t.Run("the ceiling precedes the LIMIT", func(t *testing.T) {
		q := accountMovementsQuery(AccountMovementFilter{HasMaxLedger: true, MaxLedger: 58_000_000}, false)
		if strings.Index(q, clause) > strings.Index(q, "LIMIT") {
			t.Errorf("the ledger ceiling must be a WHERE predicate, applied before the LIMIT (F055):\n%s", q)
		}
	})
}
