//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwa_curated_directory (migration 0161) census through real Timescale.
// The curated arm serves only contract rows, so the census must split the
// recognised population by address form or the classic rows it leaves
// out vanish from the response. The fixture includes a classic code that
// begins with C, which a `LIKE 'C%'` contract test would misfile.
func TestCuratedRWADirectory_CensusSplitsAddressForms(t *testing.T) {
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

	const (
		curator        = "dune:stellar"
		pricedContract = "CBUBVYRKTQLMDRUBPP6SH4GO33KZCEEYBIWB5AWNGKODP4A6KPKM2VJ4"
		bareContract   = "CBGV2QFQBBGEQRUKUMCPO3SZOHDDYO6SCP5CH6TW7EALKVHCXTMWDDOF"
		pricedClassic  = "CETES-GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
		bareClassic    = "yUSDC-GCUG7ARUFEEUMSL56K7245YCPXPZPOXAY6TSRXZB2JZFBI4DOBVOTUSA"
		staleClassic   = "BENJI-GAXSPCTVGFIVYGHT7JLJZV57HCN5KUYDJ6DMPLNWUKL7A5A3HKCNW7JW"
	)
	priced := time.Now().UTC().Add(-time.Hour)
	entries := []timescale.CuratedRWAEntry{
		{Address: pricedContract, AssetCode: "VuMe", PriceUSD: "1.1174", PricedAt: priced},
		{Address: bareContract, AssetCode: "EUTBL"},
		{Address: pricedClassic, AssetCode: "CETES", PriceUSD: "0.057", PricedAt: priced},
		{Address: bareClassic, AssetCode: "yUSDC"},
		{Address: staleClassic, AssetCode: "BENJI", PriceUSD: "1", PricedAt: priced},
	}
	if _, _, err := store.ReplaceCuratedRWADirectory(ctx, curator, entries, "test"); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE rwa_curated_directory SET synced_at = now() - INTERVAL '72 hours' WHERE address = $1`,
		staleClassic); err != nil {
		t.Fatalf("age row: %v", err)
	}

	rows, census, err := store.CuratedRWADirectoryByAddress(ctx, curator)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := timescale.CuratedRWACensus{
		Entries: 4, Contracts: 2, Classic: 2, Priced: 1, PricedClassic: 1, Stale: 1,
	}
	if census != want {
		t.Errorf("census = %+v, want %+v", census, want)
	}
	if why := census.Check(); why != "" {
		t.Errorf("census does not balance: %s", why)
	}
	if len(rows) != census.Entries {
		t.Errorf("read returned %d rows, census.Entries = %d", len(rows), census.Entries)
	}
}
