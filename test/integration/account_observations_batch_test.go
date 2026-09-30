//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestLatestAccountObservationsAtOrBefore_MatchesSingleRead executes the
// batch read against real Postgres: per account it must return exactly the
// row the single-account read returns, and omit accounts with no
// observation at or before the ledger bound.
func TestLatestAccountObservationsAtOrBefore_MatchesSingleRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		multi   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		single  = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
		future  = "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
		unknown = "GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55"
		asOf    = uint32(25)
	)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, o := range []domain.AccountObservation{
		{AccountID: multi, Ledger: 10, HomeDomain: "old.example"},
		{AccountID: multi, Ledger: 20, HomeDomain: "new.example"},
		{AccountID: multi, Ledger: 30, IsRemoval: true},
		{AccountID: single, Ledger: 5},
		{AccountID: future, Ledger: 40, HomeDomain: "future.example"},
	} {
		o.ObservedAt = t0.Add(time.Duration(o.Ledger) * 5 * time.Second)
		o.Balance = big.NewInt(int64(o.Ledger))
		if err := store.InsertAccountObservation(ctx, o); err != nil {
			t.Fatalf("InsertAccountObservation %s@%d: %v", o.AccountID, o.Ledger, err)
		}
	}

	got, err := store.LatestAccountObservationsAtOrBefore(ctx, []string{multi, single, future, unknown}, asOf)
	if err != nil {
		t.Fatalf("LatestAccountObservationsAtOrBefore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2 (multi, single): %+v", len(got), got)
	}
	if row := got[multi]; row.Ledger != 20 || row.HomeDomain == nil || *row.HomeDomain != "new.example" {
		t.Errorf("multi: ledger=%d home_domain=%v, want 20 new.example", row.Ledger, row.HomeDomain)
	}
	if row := got[single]; row.HomeDomain != nil {
		t.Errorf("single: home_domain=%q, want NULL", *row.HomeDomain)
	}
	for _, acc := range []string{multi, single} {
		one, err := store.LatestAccountObservationAtOrBefore(ctx, acc, asOf)
		if err != nil {
			t.Fatalf("LatestAccountObservationAtOrBefore %s: %v", acc, err)
		}
		b := got[acc]
		if b.Ledger != one.Ledger || b.IsRemoval != one.IsRemoval || b.Balance.Cmp(one.Balance) != 0 ||
			(b.HomeDomain == nil) != (one.HomeDomain == nil) {
			t.Errorf("%s: batch %+v != single %+v", acc, b, one)
		}
	}

	latest, err := store.LatestAccountObservationsAtOrBefore(ctx, []string{multi}, ^uint32(0))
	if err != nil {
		t.Fatalf("unbounded asOf must be capped to int4, got: %v", err)
	}
	if !latest[multi].IsRemoval {
		t.Errorf("unbounded read: multi=%+v, want the ledger-30 removal", latest[multi])
	}

	empty, err := store.LatestAccountObservationsAtOrBefore(ctx, nil, asOf)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty input: got %v, %v; want empty map, nil", empty, err)
	}
}
