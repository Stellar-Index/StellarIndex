//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A projection-rostered source's contract_count is the DISTINCT count of its projected table, not the length of the
// capped roster; once protocol_contracts holds the source, the registry is the roster and no count is served.
func TestCountSourceContracts_ProjectionRosterIsCounted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const contracts = 3
	for i := range contracts {
		for j := range 2 { // two events per contract: the count is DISTINCT, not rows
			_, err := db.ExecContext(ctx, `INSERT INTO rozo_events
				(contract_id, ledger, tx_hash, op_index, ts, event_type, amount, destination)
				VALUES ($1, $2, $3, 0, now(), 'flush', 1, 'GDEST')`,
				fmt.Sprintf("CROZO%d", i), 100+j, fmt.Sprintf("%064d", i*10+j))
			if err != nil {
				t.Fatalf("seed rozo_events: %v", err)
			}
		}
	}

	n, ok, err := store.CountSourceContracts(ctx, "rozo")
	if err != nil || !ok || n != contracts {
		t.Fatalf("CountSourceContracts(rozo) = (%d, %v, %v), want (%d, true, nil)", n, ok, err, contracts)
	}
	if n, ok, err := store.CountSourceContracts(ctx, "reflector-dex"); err != nil || !ok || n != 0 {
		t.Fatalf("CountSourceContracts(reflector-dex) = (%d, %v, %v), want (0, true, nil)", n, ok, err)
	}

	if err := store.UpsertProtocolContract(ctx, "rozo", "CROZO0", "curated", 1); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if n, ok, err := store.CountSourceContracts(ctx, "rozo"); err != nil || ok {
		t.Fatalf("CountSourceContracts(rozo) with a registry = (%d, %v, %v), want ok=false", n, ok, err)
	}
}
