//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The real rollup cycle over a lake whose server is not in UTC, as the driver then decodes every Date column at the
// server's local midnight: the covered days with interest are each stored once, the days before the first interest
// row store nothing, and every stored row is the day /revenue?interval=1d serves for it, to the cent it serves.
func TestLendingRevenueRollup_NonUTCLakeStoresWhatRevenueServes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	for _, s := range []string{`TRUNCATE TABLE stellar.lending_interest_hourly`, `TRUNCATE TABLE stellar.entry_history_watermark`} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	d0 := time.Date(2032, 7, 1, 0, 0, 0, 0, time.UTC)
	day := func(i int) time.Time { return d0.Add(time.Duration(i) * 24 * time.Hour) }
	// Far below every other test's range: stellar.ledgers is shared, and its max is the lake tip other tests pin.
	const floor, thru = 2_719_001, 2_719_100
	for seq, at := range map[uint32]time.Time{floor: d0.Add(-time.Hour), thru: day(9).Add(24*time.Hour - time.Second)} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
			VALUES (?, ?, ?, '00', 23)`, seq, at, fmt.Sprintf("%064d", seq)); err != nil {
			t.Fatal(err)
		}
	}
	contract := func(seed byte) string {
		var raw [32]byte
		raw[0], raw[1] = seed, 0x7a
		s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	pool, asset, xlm := contract(0x51), contract(0x52), c.NativeSACContractID()
	for name, v := range map[string]uint32{"lending_interest": thru, "lending_interest_floor": floor, "lending_interest:" + pool: thru} {
		if err := conn.Exec(ctx, `INSERT INTO stellar.entry_history_watermark (name, thru_ledger) VALUES (?, ?)`, name, v); err != nil {
			t.Fatal(err)
		}
	}
	// Interest only on days 3-6, as a pool whose first reserve comes after the derive floor: the fill's first days hold
	// nothing to value.
	z := func() *big.Int { return new(big.Int) }
	row := func(a string, d int, gross int64) lendinginterest.HourRow {
		g := big.NewInt(gross)
		return lendinginterest.HourRow{
			Pool: pool, Asset: a, Protocol: lendinginterest.BlendV2, Hour: day(d).Add(5 * time.Hour), FirstLedger: 1, LastLedger: 2,
			Versions: 1, AccruedVersions: 1, Gross: g, ProtocolFee: z(), Backstop: big.NewInt(gross / 5), SupplierIntended: g,
			SupplierRealised: g, BackstopCreditDelta: z(), BackstopResidual: z(), SupplyResidual: z(), DefaultLoss: z(),
			FeeCreditResidual: z(), ResidualExplained: z(), ResidualAttributed: z(), InterestDrawn: z(), Gulped: z(),
			ReconcileBackstopLoss: z(),
		}
	}
	var rows []lendinginterest.HourRow
	for d := 3; d <= 6; d++ {
		rows = append(rows, row(asset, d, int64(1000+d)), row(xlm, d, int64(1_234_567_890+d*1_000_003)))
	}
	if err := chstore.InsertLendingInterestRows(ctx, addr, rows, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	// Derived a day ago, as a lake whose derive is not running: the first cycle's days come from the self-fill alone.
	const aged = `stellar.lending_interest_hourly_tz_aged`
	for _, s := range []string{
		`DROP TABLE IF EXISTS ` + aged,
		`CREATE TABLE ` + aged + ` AS stellar.lending_interest_hourly`,
		`INSERT INTO ` + aged + ` SELECT * REPLACE (derived_at - INTERVAL 1 DAY AS derived_at) FROM stellar.lending_interest_hourly`,
		`TRUNCATE TABLE stellar.lending_interest_hourly`,
		`INSERT INTO stellar.lending_interest_hourly SELECT * FROM ` + aged,
		`DROP TABLE ` + aged,
	} {
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, db := openOrientationStore(t, ctx, dsn)
	if _, err := db.ExecContext(ctx, `INSERT INTO protocol_contracts (source, contract_id, factory_id, first_ledger, observed_at)
		VALUES ($1, $2, $3, 1, now())`, blend.SourceName, pool, blend.MainnetPoolFactory); err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{Explorer: er, Lending: store, History: rollupXLMPrice{apiHistoryAdapter{s: store}}})
	w := srv.NewLendingRevenueRollup(store, slog.Default())

	// The server's zone reaches the driver through the handshake; a user location is the same decode path.
	lake := clickhouse.Context(ctx, clickhouse.WithUserLocation(time.FixedZone("UTC+2", 2*3600)))
	st, err := w.Cycle(lake)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Held || st.Written != 8 || st.ReserveDays != 8 || st.Queued != 10 || st.Empty != 6 {
		t.Fatalf("fill over a UTC+2 lake: %+v; want 10 days read, 6 empty, days 3-6 of both reserves written", st)
	}
	covs, err := store.LendingRevenueUSDCoverages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cov := covs[pool]; !cov.FloorDay.Equal(day(0)) || !cov.ThroughDay.Equal(day(9)) {
		t.Fatalf("coverage %+v; want days 0 through 9", cov)
	}
	got, err := store.LendingRevenueUSDDays(ctx, pool, day(0), day(9))
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]timescale.LendingRevenueUSDDay{}
	for _, r := range got {
		stored[r.Asset+" "+r.Day.Format(time.DateOnly)] = r
	}
	if len(stored) != 8 {
		t.Fatalf("%d stored rows; want 8: %v", len(got), got)
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := fmt.Sprintf("%s/v1/lending/pools/%s/revenue?from=%s&to=%s&interval=1d", ts.URL, pool,
		day(0).Format(time.RFC3339), day(10).Format(time.RFC3339))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/revenue: %d", resp.StatusCode)
	}
	var env struct {
		Data v1.LendingRevenueView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	served := 0
	for _, res := range env.Data.Reserves {
		for _, p := range res.Points {
			served++
			k := res.Asset + " " + time.Time(p.T).UTC().Format(time.DateOnly)
			r, ok := stored[k]
			if !ok {
				t.Errorf("/revenue serves %s; no row stored for it", k)
				continue
			}
			u := p.USD
			for i, leg := range []*string{
				u.InterestAccruedUSD, u.SupplierShareUSD, u.SupplierCreditedUSD, u.BackstopTakeUSD,
				u.ProtocolFeeUSD, u.BackstopDrawnUSD, u.DefaultLossUSD,
			} {
				if !servedAs(leg, r.Legs[i]) {
					t.Errorf("%s leg %d: served %s, stored %s", k, i, legText(leg, "NULL"), legText(r.Legs[i], "NULL"))
				}
			}
			if u.PricedHours != r.HoursValued || u.UnpricedHours != r.HoursUnvalued {
				t.Errorf("%s hours: served %d priced %d unpriced, stored %d, %d", k, u.PricedHours, u.UnpricedHours, r.HoursValued, r.HoursUnvalued)
			}
		}
	}
	if served != len(stored) {
		t.Errorf("/revenue serves %d days; %d stored", served, len(stored))
	}

	// A re-derive of X's day 5, seen by change detection over the same lake, re-values exactly that row.
	if err := chstore.InsertLendingInterestRows(ctx, addr, []lendinginterest.HourRow{row(asset, 5, 7777)}, lendinginterest.DeriveRev); err != nil {
		t.Fatal(err)
	}
	if st, err := w.Cycle(lake); err != nil || st.Written != 1 {
		t.Fatalf("re-derive cycle: %+v, %v; want X's day 5 alone written", st, err)
	}
	again, err := store.LendingRevenueUSDDays(ctx, pool, day(0), day(9))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range again {
		k := r.Asset + " " + r.Day.Format(time.DateOnly)
		if changed := !r.InterestDerivedAt.Equal(stored[k].InterestDerivedAt); changed != (r.Asset == asset && r.Day.Equal(day(5))) {
			t.Errorf("%s: derived_at changed=%v", k, changed)
		}
	}
	if len(again) != len(stored) {
		t.Errorf("%d rows after the re-derive; want %d", len(again), len(stored))
	}
}

// servedAs reports whether served is the exact stored leg at the 2 decimal places /revenue serves: within half a cent.
func servedAs(served, stored *string) bool {
	if served == nil || stored == nil {
		return served == nil && stored == nil
	}
	x, okA := new(big.Rat).SetString(*served)
	y, okB := new(big.Rat).SetString(*stored)
	return okA && okB && new(big.Rat).Abs(x.Sub(x, y)).Cmp(big.NewRat(1, 200)) <= 0
}
