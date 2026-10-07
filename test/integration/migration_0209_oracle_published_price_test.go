//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math/big"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestMigration0209_OraclePublishedPrice pins the migration end to end: the column
// lands on a hypertable with a compressed chunk, pre-existing rows read as
// "not recorded", the writer round-trips the on-chain integer bit-for-bit
// beside an unchanged price, the derive_generation guard governs it like
// every other column, and the down migration drops it.
func TestMigration0209_OraclePublishedPrice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c.InstallAliasRegistry(nil)

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 207)
	requireSchemaVersion(t, ctx, db, 207)
	quiesceCAGGRefreshPolicies(t, ctx, db)

	mxne, err := c.NewCryptoAsset("MXNe")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-60 * 24 * time.Hour).Truncate(time.Second)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO oracle_updates (source, ledger, tx_hash, op_index, ts, asset, quote, price, decimals)
		VALUES ('redstone', 60000000, $1, 0, $2, $3, 'fiat:USD', 5747126, 8)`,
		strings.Repeat("ab", 32), old, mxne.String()); err != nil {
		t.Fatalf("insert pre-0209 row: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`SELECT compress_chunk(ch) FROM show_chunks('oracle_updates') ch`); err != nil {
		t.Fatalf("compress oracle_updates chunks: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 209)
	requireSchemaVersion(t, ctx, db, 209)

	live, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = live.Close() }()

	got, err := live.LatestOracleUpdateForAsset(ctx, "redstone", mxne)
	if err != nil {
		t.Fatalf("read pre-0209 row: %v", err)
	}
	if got.PublishedPrice != nil {
		t.Errorf("pre-0209 row published_price = %s, want nil (not recorded)", got.PublishedPrice)
	}

	usd, err := c.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	r := c.NewAmount(big.NewInt(1_740_000_001))
	u := c.OracleUpdate{
		Source: "redstone", Ledger: 61_000_000, TxHash: strings.Repeat("cd", 32),
		Timestamp: time.Now().UTC().Truncate(time.Second), Asset: mxne, Quote: usd,
		Price: c.NewAmount(big.NewInt(5_747_126)), PublishedPrice: &r, Decimals: 8,
	}
	if err := live.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("insert: %v", err)
	}
	assertPublished := func(want string) {
		t.Helper()
		got, err := live.LatestOracleUpdateForAsset(ctx, "redstone", mxne)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.Price.String() != "5747126" {
			t.Errorf("price = %s, want 5747126 (unchanged)", got.Price)
		}
		if got.PublishedPrice == nil || got.PublishedPrice.String() != want {
			t.Errorf("published_price = %v, want %s", got.PublishedPrice, want)
		}
	}
	assertPublished("1740000001")

	rederive, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer func() { _ = rederive.Close() }()
	rederive.SetDeriveGeneration(5)
	corrected := c.NewAmount(big.NewInt(1_740_000_000))
	u.PublishedPrice = &corrected
	if err := rederive.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("re-derive insert: %v", err)
	}
	assertPublished("1740000000")

	u.PublishedPrice = &r
	if err := live.InsertOracleUpdate(ctx, u); err != nil {
		t.Fatalf("gen-0 replay insert: %v", err)
	}
	assertPublished("1740000000") // a lower generation never reverts a correction

	applyMigrationsUpTo(t, dsn, 207)
	requireSchemaVersion(t, ctx, db, 207)
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'oracle_updates' AND column_name = 'published_price'`).Scan(&n); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if n != 0 {
		t.Errorf("after 0209 down published_price still present")
	}
}
