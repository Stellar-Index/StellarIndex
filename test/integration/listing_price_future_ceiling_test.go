//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestListingDirectory_FuturePricedAtIsNotFresh executes the listing
// directory's price-freshness predicate on real Postgres. A `priced_at`
// in the future sits above the 24-hour floor for as long as it stays
// ahead of now(), so without the ceiling a frozen price stamped 2099 is
// served as fresh — by both readers and counted by the census.
func TestListingDirectory_FuturePricedAtIsNotFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	contractFresh := "C" + strings.Repeat("F", 55)
	contractFuture := "C" + strings.Repeat("U", 55)
	contractSkewed := "C" + strings.Repeat("S", 55)
	now := time.Now().UTC()
	entries := []timescale.ListingEntry{
		{
			Address: contractFresh, ListingID: "fresh", Symbol: "frs",
			PriceUSD: "1.25", PricedAt: now.Add(-time.Hour),
		},
		{
			Address: contractFuture, ListingID: "future", Symbol: "fut",
			PriceUSD: "9.75", PricedAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			Address: contractSkewed, ListingID: "skewed", Symbol: "skw",
			PriceUSD: "3.5", PricedAt: now.Add(time.Minute),
		},
	}
	if _, _, err := store.ReplaceListingDirectory(ctx, entries, "coingecko"); err != nil {
		t.Fatalf("ReplaceListingDirectory: %v", err)
	}

	byAddr, census, err := store.ListingDirectoryByAddress(ctx)
	if err != nil {
		t.Fatalf("ListingDirectoryByAddress: %v", err)
	}
	fut, ok := byAddr[contractFuture]
	if !ok {
		t.Fatal("the future-stamped row must stay RECOGNISED — only its price is withheld")
	}
	if fut.PriceUSD != "" || !fut.PricedAt.IsZero() {
		t.Errorf("future-stamped row served price %q at %v — a priced_at ahead of now() "+
			"must not count as fresh", fut.PriceUSD, fut.PricedAt)
	}
	if got := byAddr[contractFresh].PriceUSD; got != "1.25" {
		t.Errorf("fresh row price = %q, want %q", got, "1.25")
	}
	if got := byAddr[contractSkewed].PriceUSD; got != "3.5" {
		t.Errorf("a priced_at inside the clock-skew allowance lost its price: got %q", got)
	}
	if census.Priced != 2 {
		t.Errorf("census.Priced = %d, want 2 (fresh, skewed; future excluded)", census.Priced)
	}

	contracts, _, err := store.ListingDirectoryContracts(ctx)
	if err != nil {
		t.Fatalf("ListingDirectoryContracts: %v", err)
	}
	for _, e := range contracts {
		if e.Address == contractFuture && e.PriceUSD != "" {
			t.Errorf("contracts read served the future-stamped price %q", e.PriceUSD)
		}
	}
}
