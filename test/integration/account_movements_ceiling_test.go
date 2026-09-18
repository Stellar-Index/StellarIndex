//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestAccountMovements_LedgerCeilingBoundsTheQuery executes F055's fixed
// SQL against a real ClickHouse: the /movements merge ceiling travels in
// AccountMovementFilter.MaxLedger/HasMaxLedger and is applied as a WHERE
// predicate, so a bounded read returns a FULL page of servable rows.
//
// The un-fixed reader emitted no ledger bound at all and the handler
// dropped the over-ceiling rows afterwards: with every one of the `limit`
// newest rows above the ceiling the page collapsed to zero, next_cursor
// was suppressed, and the account's pre-watermark history was unreachable.
// Here the same shape must yield `limit` rows, the newest of them exactly
// at the ceiling.
func TestAccountMovements_LedgerCeilingBoundsTheQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}

	g := gAccountFromSeed(t, 0x41)
	counterparty := gAccountFromSeed(t, 0x42)
	const (
		firstLedger = 40_000_000
		rowCount    = 40
		ceiling     = firstLedger + 19 // 20 servable ledgers, 20 above the ceiling
		limit       = 10
	)

	movements := make([]clickhouse.AccountMovement, 0, rowCount)
	for i := 0; i < rowCount; i++ {
		movements = append(movements, clickhouse.AccountMovement{
			MovementKind:    "payment",
			Provenance:      "classic_derived",
			Ledger:          uint32(firstLedger + i),
			LedgerCloseTime: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute),
			TxHash:          fmt.Sprintf("%064x", i),
			OpIndex:         0,
			LegIndex:        0,
			Asset:           "native",
			Amount:          big.NewInt(int64(1_000_000 + i)),
			FromAddress:     g,
			ToAddress:       counterparty,
		})
	}
	if _, err := clickhouse.InsertAccountMovements(ctx, chAddr, movements); err != nil {
		t.Fatalf("InsertAccountMovements: %v", err)
	}

	er, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.AccountMovements(ctx, g, limit, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{HasMaxLedger: true, MaxLedger: ceiling})
	if err != nil {
		t.Fatalf("AccountMovements (bounded): %v", err)
	}
	if len(rows) != limit {
		t.Fatalf("bounded read returned %d rows, want a full page of %d — the ceiling did not bound the "+
			"query, so the newest (unservable) rows ate the LIMIT (F055)", len(rows), limit)
	}
	if rows[0].Ledger != ceiling {
		t.Errorf("newest bounded row is ledger %d, want %d (the ceiling itself is inclusive)", rows[0].Ledger, ceiling)
	}
	for _, r := range rows {
		if r.Ledger > ceiling {
			t.Fatalf("row at ledger %d exceeds the ceiling %d", r.Ledger, ceiling)
		}
	}

	// A ceiling of 0 is a real ceiling (an installed genesis movements
	// floor with no cap67 watermark), not "unset": it must serve nothing.
	zeroRows, err := er.AccountMovements(ctx, g, limit, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{HasMaxLedger: true, MaxLedger: 0})
	if err != nil {
		t.Fatalf("AccountMovements (ceiling 0): %v", err)
	}
	if len(zeroRows) != 0 {
		t.Fatalf("ceiling 0 served %d rows, want 0 — the arm must fail closed at an installed genesis floor", len(zeroRows))
	}

	// No ceiling = the whole archive, unchanged from before the fix.
	allRows, err := er.AccountMovements(ctx, g, rowCount, clickhouse.AccountMovementCursor{},
		clickhouse.AccountMovementFilter{})
	if err != nil {
		t.Fatalf("AccountMovements (unbounded): %v", err)
	}
	if len(allRows) != rowCount {
		t.Fatalf("unbounded read returned %d rows, want %d", len(allRows), rowCount)
	}
}
