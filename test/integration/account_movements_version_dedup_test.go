//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestAccountMovements_DedupServesNewestVersion executes the account
// movements read against two un-merged versions of one key that differ
// only in counterparty — the shape an in-place re-derive leaves behind.
// The newer ingested_at must be served whichever part was written first;
// a LIMIT 1 BY without the version in its ORDER BY keeps an arbitrary one.
func TestAccountMovements_DedupServesNewestVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	chAddr := clickhouseAddr(t)
	if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
		t.Fatalf("EnsureAccountMovementsTable: %v", err)
	}
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.account_movements"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.account_movements") })

	er, err := clickhouse.NewExplorerReader(ctx, chAddr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	stale := gAccountFromSeed(t, 0x61) // the original derive's counterparty
	fresh := gAccountFromSeed(t, 0x62) // the re-derive's counterparty
	insert := func(address, counterparty, ingestedAt string) {
		t.Helper()
		q := fmt.Sprintf(`INSERT INTO stellar.account_movements
			(address, ledger, ledger_close_time, tx_hash, op_index, leg_index, direction,
			 movement_kind, provenance, asset, counterparty, amount, ingested_at)
			VALUES ('%s', 45000000, toDateTime64('2024-01-01 00:00:00', 0, 'UTC'), '%064x', 0, 0, 'sent',
			 'payment', 'classic_derived', 'native', '%s', 1000, toDateTime('%s', 'UTC'))`,
			address, 7, counterparty, ingestedAt)
		if err := raw.Exec(ctx, q); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	for i, newerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("newer_written_first=%v", newerFirst), func(t *testing.T) {
			g := gAccountFromSeed(t, byte(0x63+i))
			if newerFirst {
				insert(g, fresh, "2026-09-02 00:00:00")
				insert(g, stale, "2026-09-01 00:00:00")
			} else {
				insert(g, stale, "2026-09-01 00:00:00")
				insert(g, fresh, "2026-09-02 00:00:00")
			}
			rows, err := er.AccountMovements(ctx, g, 10, clickhouse.AccountMovementCursor{}, clickhouse.AccountMovementFilter{})
			if err != nil {
				t.Fatalf("AccountMovements: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1 (LIMIT 1 BY dedup of the two versions)", len(rows))
			}
			if rows[0].Counterparty != fresh {
				t.Errorf("counterparty = %s, want the newer version's %s (stale %s was served)", rows[0].Counterparty, fresh, stale)
			}
		})
	}
}
