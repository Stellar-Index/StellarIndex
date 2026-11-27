//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The emissions rollup's tables on real Postgres: the pool list starts at each pool's first emission event; a replace
// makes a day's rows its whole state (a line that left the day is deleted) and keeps the exact USD byte for byte; a NULL
// USD needs its reason and an approximate row its reason; coverage and stale revisions read back as written. Its own
// container: no lake row is written.
func TestLendingEmissionsUSD_StoreRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const pool = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
	t0 := time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC)
	for i, e := range []domain.BlendEmissionEvent{
		{Pool: pool, Kind: domain.BlendEventClaim, Timestamp: t0, Amount: big.NewInt(10), User: "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL"},
		{Pool: pool, Kind: domain.BlendEventReserveEmissions, Timestamp: t0.Add(-26 * time.Hour), ResTokenID: 1, EmissionsPerSec: 1, Expiration: 1},
	} {
		e.Ledger, e.TxHash = uint32(61_000_000+i), fmt.Sprintf("%064x", i+1)
		if err := store.InsertBlendEmissionEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := store.LendingEmissionPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := pools[pool]; !got.Equal(t0.Add(-26 * time.Hour)) {
		t.Fatalf("pools %v, want %s from its first schedule update", pools, t0.Add(-26*time.Hour))
	}

	day := t0.Truncate(24 * time.Hour)
	str := func(s string) *string { return &s }
	events := int64(2)
	row := func(stream, line, amountUSD string) timescale.LendingEmissionsUSDDay {
		return timescale.LendingEmissionsUSDDay{
			Pool: pool, Day: day, Stream: stream, Line: line, Protocol: "blend", EmissionToken: str("CTOKEN"), Events: &events,
			Amount: "120000", AmountUSD: str(amountUSD), HoursValued: 3, BasisHours: map[string]int{"stablecoin_proxy": 1},
			LowerBound: true, StreamLowerBound: true, Excluded: []string{"claim_names_several_reserves"}, ValuationRev: 1,
		}
	}
	const exact = "0.012345678901234567890123456789"
	if err := store.ReplaceLendingEmissionsUSDDays(ctx, pool, []time.Time{day}, []timescale.LendingEmissionsUSDDay{
		row("pool_claims", "", exact), row("pool_claims_reserve", "1", "0.5"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.LendingEmissionsUSDDays(ctx, pool, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || *got[0].AmountUSD != exact || got[0].Stream != "pool_claims" || !got[0].LowerBound ||
		got[0].BasisHours["stablecoin_proxy"] != 1 || len(got[0].Excluded) != 1 || *got[0].Events != 2 {
		t.Fatalf("read back %+v", got)
	}

	unread := row("pool_claims", "", "")
	unread.AmountUSD, unread.USDReason = nil, str("amount_unread")
	if err := store.ReplaceLendingEmissionsUSDDays(ctx, pool, []time.Time{day}, []timescale.LendingEmissionsUSDDay{unread}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.LendingEmissionsUSDDays(ctx, pool, day, day)
	if len(got) != 1 || got[0].AmountUSD != nil || *got[0].USDReason != "amount_unread" {
		t.Fatalf("after the replace %+v, want the one unpriced pool line and the reserve line gone", got)
	}

	noReason := row("pool_claims", "", "")
	noReason.AmountUSD = nil
	approx := row("reserve_schedule", "1", "1")
	approx.Approximate = true
	for name, bad := range map[string]timescale.LendingEmissionsUSDDay{"null usd without a reason": noReason, "approximate without a reason": approx} {
		if err := store.ReplaceLendingEmissionsUSDDays(ctx, pool, []time.Time{day}, []timescale.LendingEmissionsUSDDay{bad}); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	if got, _ = store.LendingEmissionsUSDDays(ctx, pool, day, day); len(got) != 1 {
		t.Fatalf("a refused replace changed the day: %+v", got)
	}

	c := timescale.LendingEmissionsUSDCoverage{
		Pool: pool, Protocol: "blend", FloorDay: day.Add(-24 * time.Hour), ThroughDay: day,
		Unavailable: map[string]string{"backstop_claims": "backstop_claim_names_no_pool"}, ValuationRev: 1,
	}
	if err := store.UpsertLendingEmissionsUSDCoverage(ctx, c); err != nil {
		t.Fatal(err)
	}
	covs, err := store.LendingEmissionsUSDCoverages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := covs[pool]; !g.ThroughDay.Equal(day) || g.Unavailable["backstop_claims"] != "backstop_claim_names_no_pool" {
		t.Fatalf("coverage %+v", g)
	}
	stale, err := store.LendingEmissionsUSDStaleRevDays(ctx, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale[pool]) != 1 || !stale[pool][0].Equal(day) {
		t.Fatalf("stale days %v, want the one revision-1 day", stale)
	}
}
