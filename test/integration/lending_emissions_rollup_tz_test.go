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
	"strconv"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, whatever the host has installed

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

const emisRollupBLND = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"

// emisRollupLake is the real lake with the pool instance read fixed: its emission token and reserves are what the
// rollup needs from it, and seeding contract-data XDR would test the instance decoder, not the day keys.
type emisRollupLake struct {
	*chstore.ExplorerReader
	state chstore.LendingEmissionsState
}

func (l emisRollupLake) LendingEmissions(context.Context, string, bool) (chstore.LendingEmissionsState, error) {
	return l.state, nil
}

// emisRollupPrice prices BLND at a constant hourly VWAP; nothing else is priced.
type emisRollupPrice struct{ apiHistoryAdapter }

func (r emisRollupPrice) HistoryPointsInRange(_ context.Context, pair c.Pair, _ string, from, to time.Time, _ int) ([]v1.HistoryPoint, error) {
	if pair.Base.Code != "BLND" || pair.Quote.Code != "USD" {
		return nil, nil
	}
	var out []v1.HistoryPoint
	for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		out = append(out, v1.HistoryPoint{Bucket: h, VWAP: "0.0625"})
	}
	return out, nil
}

// emisRollupCursors puts every writer at one ledger, whose close time the real lake resolves.
type emisRollupCursors uint32

func (emisRollupCursors) ListCursors(context.Context) ([]timescale.Cursor, error) { return nil, nil }

func (l emisRollupCursors) GetCursor(_ context.Context, source, sub string) (timescale.Cursor, error) {
	return timescale.Cursor{Source: source, Sub: sub, LastLedger: uint32(l)}, nil
}

type emisRollupWatermark struct{}

func (emisRollupWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	return 1, time.Now().UTC(), nil
}

type emisRollupDecimals struct{}

func (emisRollupDecimals) TokenDecimals(context.Context, string) (uint32, bool, error) {
	return 7, true, nil
}

type emisRollupCompleteness struct{}

func (emisRollupCompleteness) ListCompletenessSnapshots(context.Context) ([]timescale.CompletenessSnapshot, error) {
	now := time.Now().UTC()
	var out []timescale.CompletenessSnapshot
	for _, s := range []string{"blend", "blend_backstop"} {
		out = append(out, timescale.CompletenessSnapshot{
			Source: s, Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now,
		})
	}
	return out, nil
}

// The real emissions rollup over Postgres and ClickHouse whose sessions run in another zone, with both drivers decoding in
// it: claims either side of a UTC midnight land on their UTC days, a backstop allocation that has nothing but boundary
// events is stored on its two days, every stored row is the day /emissions/history?interval=1d serves, and coverage spans
// only days with a stored row or nothing to store, even over coverage an older valuation revision wrote.
func TestLendingEmissionsRollup_NonUTCStoresWhatHistoryServes(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("Europe/Berlin", func(t *testing.T) {
		emisRollupOverZone(t, berlin, today.Add(-10*24*time.Hour), 2_719_401, 0x61)
	})
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("America/Santiago gap day", func(t *testing.T) {
		for d := today.Add(-5 * 24 * time.Hour); d.After(today.AddDate(-1, 0, 0)); d = d.Add(-24 * time.Hour) {
			if y, m, n := d.Date(); time.Date(y, m, n, 0, 0, 0, 0, santiago).Day() != n {
				emisRollupOverZone(t, santiago, d.Add(-4*24*time.Hour), 2_719_601, 0x63)
				return
			}
		}
		t.Skip("no midnight DST gap in Santiago over the past year in this tzdata")
	})
}

// emisRollupOverZone seeds one Blend V2 pool from d0, with the lake and Postgres in loc; day 4 is the day whose boundary
// events are checked.
func emisRollupOverZone(t *testing.T, loc *time.Location, d0 time.Time, writer uint32, seed byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	prev := time.Local
	time.Local = loc // pgx decodes timestamptz in the process zone
	t.Cleanup(func() { time.Local = prev })

	day := func(i int) time.Time { return d0.Add(time.Duration(i) * 24 * time.Hour) }
	yesterday := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	addr := clickhouseAddr(t)
	conn := dialClickHouse(t, ctx, "stellar")
	// The writers' cursor ledger, far below every other test's range: stellar.ledgers' max is the tip other tests pin.
	if err := conn.Exec(ctx, `INSERT INTO stellar.ledgers (ledger_seq, close_time, ledger_hash, prev_hash, protocol_version)
		VALUES (?, ?, ?, '00', 23)`, writer, time.Now().UTC(), fmt.Sprintf("%064d", writer)); err != nil {
		t.Fatal(err)
	}
	contract := func(seed byte) string {
		var raw [32]byte
		raw[0], raw[1] = seed, 0x7b
		s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	pool, resA, resB := contract(seed), contract(seed+1), contract(seed+2)
	token, ok := xdrjson.SACContractID(emisRollupBLND, network.PublicNetworkPassphrase)
	if !ok {
		t.Fatal("no BLND SAC")
	}

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	// A DSN asking for loc still gets a UTC session: the store pins it, so only the Go side decodes in loc.
	store, db := openOrientationStore(t, ctx, dsn+"&timezone="+loc.String())
	var zone string
	if err := db.QueryRowContext(ctx, `SHOW TimeZone`).Scan(&zone); err != nil || zone != "UTC" {
		t.Fatalf("postgres session zone %q (%v); want the store's UTC pin", zone, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO protocol_contracts (source, contract_id, factory_id, first_ledger, observed_at)
		VALUES ($1, $2, $3, 1, now())`, blend.SourceName, pool, blend.MainnetPoolFactory); err != nil {
		t.Fatal(err)
	}
	n := 0
	claim := func(at time.Time, ids []uint32, amount int64) {
		n++
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent{
			Pool: pool, Kind: domain.BlendEventClaim, Timestamp: at, User: "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL",
			ReserveTokenIDs: ids, Amount: big.NewInt(amount), Ledger: 70_000_000 + uint32(n), TxHash: fmt.Sprintf("%064x", n),
		}); err != nil {
			t.Fatal(err)
		}
	}
	gulp := func(at time.Time, backstop, poolShare string) {
		n++
		if err := store.InsertBlendBackstopEvent(ctx, timescale.BlendBackstopEvent{
			ContractID: blnt.MainnetBackstop, Ledger: 70_000_000 + uint32(n), TxHash: fmt.Sprintf("%064x", n), ObservedAt: at,
			EventType: timescale.BackstopGulpEmissions, Pool: pool, Amount: backstop, Amount2: poolShare,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Claims at 23:30Z on day 3 and 00:30Z on day 4, which any zone east of UTC puts on one day; one naming two reserves
	// on day 6. The allocation has nothing but boundary events, 23:10Z on day 4 and 00:10Z on day 7.
	claim(day(4).Add(-30*time.Minute), []uint32{1}, 30_000_000)
	claim(day(4).Add(30*time.Minute), []uint32{1}, 50_000_000)
	claim(day(6).Add(12*time.Hour), []uint32{1, 2}, 70_000_000)
	gulp(day(5).Add(-50*time.Minute), "9000000000000000000", "11")
	gulp(day(7).Add(10*time.Minute), "13", "17")
	withRows := map[time.Time]bool{day(3): true, day(4): true, day(6): true, day(7): true}

	// Coverage an older valuation revision wrote over every day, with no row behind it: it must vouch for nothing.
	if err := store.UpsertLendingEmissionsUSDCoverage(ctx, timescale.LendingEmissionsUSDCoverage{
		Pool: pool, Protocol: "blend", FloorDay: day(3), ThroughDay: yesterday, ValuationRev: 0,
	}); err != nil {
		t.Fatal(err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	// A position lists the pool on /v1/lending/protocols, whose emissions block reads what the rollup stores.
	if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent{
		Pool: pool, Kind: domain.BlendEventSupply, Asset: resA, User: "GDEA2WMOWYQ3T56A2PZH5QYYEQBAORW2AUUJYX4FFD5GDPESAICTFTAL",
		TokenAmount: big.NewInt(1), BOrDAmount: big.NewInt(1), Ledger: 69_999_999, TxHash: fmt.Sprintf("%064x", 1<<20),
		Timestamp: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{
		ProtocolStats: store,
		Explorer: emisRollupLake{ExplorerReader: er, state: chstore.LendingEmissionsState{
			Instance: &chstore.PoolInstance{Backstop: blnt.MainnetBackstop}, EmissionToken: token, ReserveList: []string{resA, resB},
		}},
		Lending: store, History: emisRollupPrice{apiHistoryAdapter{s: store}}, VerifiedCurrencies: cat,
		TokenDecimals: emisRollupDecimals{}, LakeWatermark: emisRollupWatermark{}, Cursors: emisRollupCursors(writer),
		CompletenessReader: emisRollupCompleteness{},
		LendingBadDebtOptions: v1.LendingBadDebtOptions{LendingWriters: map[string][]v1.TVLWriterCursor{
			"blend": {{Source: "projector", Sub: "blend"}}, "blend_backstop": {{Source: "projector", Sub: "blend_backstop"}},
		}},
	})
	srv.NetworkPassphrase = network.PublicNetworkPassphrase
	w := srv.NewLendingEmissionsRollup(store, slog.Default())
	lake := clickhouse.Context(ctx, clickhouse.WithUserLocation(loc),
		clickhouse.WithSettings(clickhouse.Settings{"session_timezone": loc.String()}))

	var written int
	for cycle := 0; ; cycle++ {
		st, err := w.Cycle(lake)
		if err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		written += st.Written
		covs, err := store.LendingEmissionsUSDCoverages(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if cov := covs[pool]; cov.ValuationRev != 0 && cov.ThroughDay.Equal(yesterday) {
			if !cov.FloorDay.Equal(day(3)) {
				t.Fatalf("coverage %+v; want from day 3, the first event's", cov)
			}
			break
		}
		if cycle == 6 {
			t.Fatalf("coverage %+v after %d cycles; want through %s at the current revision", covs[pool], cycle+1, yesterday.Format(time.DateOnly))
		}
	}

	got, err := store.LendingEmissionsUSDDays(ctx, pool, day(0), yesterday)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]timescale.LendingEmissionsUSDDay{}
	days := map[time.Time]bool{}
	for _, r := range got {
		if r.Day.Location() != time.UTC || !r.Day.Equal(r.Day.Truncate(24*time.Hour)) {
			t.Fatalf("stored day %s is not a UTC midnight", r.Day)
		}
		stored[r.Stream+" "+r.Line+" "+r.Day.Format(time.DateOnly)] = r
		days[r.Day] = true
	}
	for d := day(3); !d.After(yesterday); d = d.Add(24 * time.Hour) {
		if days[d] != withRows[d] {
			t.Errorf("day %s: stored=%v; want %v", d.Format(time.DateOnly), days[d], withRows[d])
		}
	}
	for d, want := range map[int]string{3: "30000000", 4: "50000000"} { // 23:30Z is day 3's, 00:30Z day 4's
		if r := stored["pool_claims  "+day(d).Format(time.DateOnly)]; r.Amount != want {
			t.Errorf("pool claims day %d: %+v; want %s", d, r, want)
		}
	}
	for _, d := range []int{4, 7} {
		if _, ok := stored["backstop_alloc_pool  "+day(d).Format(time.DateOnly)]; !ok {
			t.Errorf("allocation day %d not stored: %v", d, days)
		}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	url := fmt.Sprintf("%s/v1/lending/pools/%s/emissions/history?interval=1d&from=%s&to=%s", ts.URL, pool,
		day(3).Format(time.RFC3339), yesterday.Add(24*time.Hour).Format(time.RFC3339))
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
		t.Fatalf("/emissions/history: %d", resp.StatusCode)
	}
	var env struct {
		Data v1.LendingEmissionsHistoryView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	served := 0
	check := func(stream, line string, at v1.WireTime, amount string, usd *string, priced, unpriced int) {
		served++
		k := stream + " " + line + " " + time.Time(at).UTC().Format(time.DateOnly)
		r, ok := stored[k]
		switch {
		case !ok:
			t.Errorf("/emissions/history serves %s; no row stored for it", k)
		case r.Amount != amount || !servedAs(usd, r.AmountUSD) || r.HoursValued != priced || r.HoursUnvalued != unpriced:
			t.Errorf("%s: served %s tokens, usd %s, %d/%d hours; stored %s, %s, %d/%d", k, amount, legText(usd, "NULL"),
				priced, unpriced, r.Amount, legText(r.AmountUSD, "NULL"), r.HoursValued, r.HoursUnvalued)
		case r.AmountUSD == nil:
			t.Errorf("%s: no USD stored; want BLND priced", k)
		}
	}
	v := env.Data
	for _, p := range v.PoolClaims.Points {
		check("pool_claims", "", p.T, p.Amount, p.USD.AmountUSD, p.USD.PricedHours, p.USD.UnpricedHours)
	}
	for _, line := range v.PoolClaims.ByReserve.Reserves {
		for _, p := range line.Points {
			check("pool_claims_reserve", strconv.FormatUint(uint64(line.TokenID), 10), p.T, p.Amount, p.USD.AmountUSD,
				p.USD.PricedHours, p.USD.UnpricedHours)
		}
	}
	for _, p := range v.BackstopAlloc.Points {
		u := p.USD
		check("backstop_alloc_backstop", "", p.T, p.BackstopEmissions, u.BackstopEmissionsUSD, u.PricedHours, u.UnpricedHours)
		check("backstop_alloc_pool", "", p.T, p.PoolEmissions, u.PoolEmissionsUSD, u.PricedHours, u.UnpricedHours)
	}
	if served != len(stored) || served == 0 {
		t.Errorf("/emissions/history serves %d lines; %d stored", served, len(stored))
	}
	if written != len(stored) {
		t.Errorf("cycles wrote %d rows; %d stored", written, len(stored))
	}
	checkProtocolEmissions(t, ctx, ts.URL, pool, token, day(3), yesterday)
}
