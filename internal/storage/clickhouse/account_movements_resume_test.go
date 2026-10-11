// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"fmt"
	"testing"
)

// TestInsertAccountMovementsOrder_PartialSendLeavesNoGap pins that what a PARTIALLY sent
// batch leaves behind is sound for the max(ledger) resume every caller checkpoints on.
//
// InsertAccountMovements splits a batch into accountMovementsInsertChunk INSERTs and returns
// on the first chunk error, leaving earlier chunks durably written (ClickHouse has no
// transaction across them). For every cut point, every ledger strictly below the highest
// written ledger must be COMPLETE, so restarting at max(ledger) re-processes one ledger and
// skips nothing. An address-first order fails this: chunk 0 spans the window's whole ledger
// range, so max(ledger) is already at the top while later addresses hold nothing.
func TestInsertAccountMovementsOrder_PartialSendLeavesNoGap(t *testing.T) {
	// A window shaped like a real one: many addresses, each active
	// across the whole ledger range.
	const (
		addresses = 12
		ledgerLo  = 1000
		ledgerHi  = 1040
	)
	var rows []AccountMovementRow
	for a := 0; a < addresses; a++ {
		for l := uint32(ledgerLo); l <= ledgerHi; l++ {
			rows = append(rows, AccountMovementRow{
				Address:   fmt.Sprintf("G%02d", a),
				Ledger:    l,
				TxHash:    fmt.Sprintf("%02d-%d", a, l),
				Direction: AccountMovementSent,
			})
		}
	}

	sortAccountMovementRowsForInsert(rows)

	// total[l] is how many rows the window holds for ledger l.
	total := map[uint32]int{}
	for _, r := range rows {
		total[r.Ledger]++
	}

	// Walk every cut point a failed chunk send could leave behind.
	written := map[uint32]int{}
	for cut, r := range rows {
		written[r.Ledger]++
		highest := uint32(0)
		for l := range written {
			if l > highest {
				highest = l
			}
		}
		for l := uint32(ledgerLo); l < highest; l++ {
			if written[l] != total[l] {
				t.Fatalf("a send that stopped after row %d left ledger %d with %d of %d rows while ledger %d "+
					"was already written: -resume restarts at %d and never revisits %d — a silent gap (RLT-296)",
					cut+1, l, written[l], total[l], highest, highest, l)
			}
		}
	}
}
