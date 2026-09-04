//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestClosedVWAP1mAtOrBefore_ExecutesAgainstTimescale runs the statement
// behind /v1/assets/{id}'s change_24h_pct against the real schema. The
// scripted-driver tests pin its shape but never parse it; a parameter
// bound untyped beside `- INTERVAL '1 minute'` is resolved by Postgres
// as an interval, and the statement then fails with 42883 on every
// call while the caller turns the error into an absent field. An empty
// table must therefore yield sql.ErrNoRows — not a planning error.
func TestClosedVWAP1mAtOrBefore_ExecutesAgainstTimescale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = store.Close() }()

	pair := canonical.Pair{
		Base:  canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"},
		Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"},
	}
	_, err = store.ClosedVWAP1mAtOrBefore(ctx, pair, time.Now().Add(-24*time.Hour))
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ClosedVWAP1mAtOrBefore on an empty table: err = %v, want sql.ErrNoRows — the statement does not execute against the real schema", err)
	}
}
