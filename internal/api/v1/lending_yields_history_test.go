package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/network"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// yieldHistoryExplorer adds the reserve day states to yieldRatesExplorer; with block set a read waits until release closes
// or its context ends.
type yieldHistoryExplorer struct {
	*yieldRatesExplorer
	days      map[string]clickhouse.LendingRateDays // by reserve asset
	block     bool
	covErr    error // the coverage read fails with it when set
	cancelLag time.Duration
}

func (e *yieldHistoryExplorer) LendingHistoryCoverage(ctx context.Context) (clickhouse.LendingHistoryCoverage, error) {
	if e.covErr != nil {
		return clickhouse.LendingHistoryCoverage{}, e.covErr
	}
	return e.yieldRatesExplorer.LendingHistoryCoverage(ctx)
}

func (e *yieldHistoryExplorer) LendingRateDays(ctx context.Context, _, asset string, _, _ time.Time, _ clickhouse.LendingHistoryCoverage) (clickhouse.LendingRateDays, error) {
	e.mu.Lock()
	e.reads++
	e.mu.Unlock()
	if e.block {
		select {
		case <-e.release:
		case <-ctx.Done():
			<-time.After(e.cancelLag) // a lake query returns some time after its context is cancelled
			return clickhouse.LendingRateDays{}, ctx.Err()
		}
	}
	return e.days[asset], nil
}

// yieldHistoryLending knows only the fixture's pools, so any other pool id has no lineage.
type yieldHistoryLending struct {
	*ratesLendingReader
	known map[string]bool
}

func (l *yieldHistoryLending) BlendPoolVersion(ctx context.Context, pool string) (blend.PoolVersion, error) {
	if !l.known[pool] {
		return blend.PoolVersionUnknown, nil
	}
	return l.ratesLendingReader.BlendPoolVersion(ctx, pool)
}

func (l *yieldHistoryLending) BLNTPoolAssets(ctx context.Context, pool string) ([]string, error) {
	if pool != l.blntPool {
		return nil, nil
	}
	return l.ratesLendingReader.BLNTPoolAssets(ctx, pool)
}

// yieldEmisStore is the emissions rollup's stored days for the history.
type yieldEmisStore struct {
	covs map[string]timescale.LendingEmissionsUSDCoverage
	rows []timescale.LendingEmissionsUSDDay
}

func (*yieldEmisStore) CountRecentEventsBySource(context.Context) (map[string]int64, error) {
	return nil, nil
}

func (s *yieldEmisStore) LendingEmissionsUSDCoverages(context.Context) (map[string]timescale.LendingEmissionsUSDCoverage, error) {
	return s.covs, nil
}

func (s *yieldEmisStore) LendingEmissionsUSDDays(_ context.Context, pool string, from, through time.Time) ([]timescale.LendingEmissionsUSDDay, error) {
	var out []timescale.LendingEmissionsUSDDay
	for _, r := range s.rows {
		if r.Pool == pool && !r.Day.Before(from) && !r.Day.After(through) {
			out = append(out, r)
		}
	}
	return out, nil
}

// yhD0 is the golden day; the fixture's states and rollup rows sit on yhD0-1 .. yhD0+3.
var yhD0 = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -10)

func yhDay(n int) time.Time { return yhD0.AddDate(0, 0, n) }

// yhState is an XLM reserve state with 100 b-tokens and 50 d-tokens (7 dp).
func yhState(ledger uint32, bRate, dRate int64, at time.Time) *clickhouse.LendingRateState {
	return &clickhouse.LendingRateState{
		Ledger: ledger, CloseTime: at, LastTime: at, BRate: big.NewInt(bRate), DRate: big.NewInt(dRate),
		BSupply: big.NewInt(1_000_000_000), DSupply: big.NewInt(500_000_000), Protocol: clickhouse.LendingBlendV2,
	}
}

func yhRow(pool, asset string, day time.Time, amount string, usd, reason *string) timescale.LendingEmissionsUSDDay {
	token, decimals, side := emisBLND, 7, "supply"
	return timescale.LendingEmissionsUSDDay{
		Pool: pool, Day: day, Stream: "reserve_schedule", Line: "1", Protocol: "blend", EmissionToken: &token,
		EmissionTokenDecimals: &decimals, Asset: &asset, Side: &side, Amount: amount, AmountUSD: usd, USDReason: reason,
		Excluded: []string{}, ValuationRev: v1.LendingEmissionsValuationRevForTest, ValuedAt: day.Add(25 * time.Hour),
	}
}

type yieldHistoryFixture struct {
	ratesFixture
	explorer *yieldHistoryExplorer
	base     string
}

// newYieldHistoryFixture serves the active pool's XLM reserve over yhD0-1 .. yhD0+2, its rollup stored through yhD0+2 and
// XLM at $0.30 every day.
func newYieldHistoryFixture(t *testing.T) yieldHistoryFixture {
	t.Helper()
	return newYieldHistoryFixtureExcluding(t, nil)
}

// newYieldHistoryFixtureExcluding is newYieldHistoryFixture behind the source-exclusion gate excl.
func newYieldHistoryFixtureExcluding(t *testing.T, excl v1.PriceExclusionGate) yieldHistoryFixture {
	t.Helper()
	f, e := newYieldRatesFixture(t)
	hx := &yieldHistoryExplorer{yieldRatesExplorer: e, days: map[string]clickhouse.LendingRateDays{f.xlmSAC: {Days: []clickhouse.LendingRateDay{
		{Day: yhDay(-1), First: yhState(10, 1e12, 1e12, yhD0.Add(-30*time.Minute)), Last: yhState(10, 1e12, 1e12, yhD0.Add(-30*time.Minute))},
		{
			Day: yhD0, First: yhState(11, 1_000_050_000_000, 1_000_100_000_000, yhD0.Add(12*time.Hour)),
			Last: yhState(12, 1_000_100_000_000, 1_000_300_000_000, yhD0.Add(23*time.Hour+30*time.Minute)),
		},
		{
			Day: yhDay(2), First: yhState(20, 1_000_200_000_000, 1_000_400_000_000, yhDay(2).Add(6*time.Hour)),
			Last: yhState(21, 1_000_250_000_000, 1_000_500_000_000, yhDay(2).Add(18*time.Hour)),
		},
	}}}}
	withheld, d0USD := "price_withheld", "0.30003"
	store := &yieldEmisStore{
		covs: map[string]timescale.LendingEmissionsUSDCoverage{f.activePool: {
			Pool: f.activePool, Protocol: "blend", FloorDay: yhDay(-5), ThroughDay: yhDay(2), Unavailable: map[string]string{},
			ValuationRev: v1.LendingEmissionsValuationRevForTest,
		}},
		rows: []timescale.LendingEmissionsUSDDay{
			yhRow(f.activePool, f.xlmSAC, yhD0, "3000300", &d0USD, nil),
			yhRow(f.activePool, f.xlmSAC, yhDay(1), "5000000000", nil, &withheld), // the token's market was thin that day
		},
	}
	prices := map[time.Time]string{}
	for i := -3; i <= 4; i++ {
		prices[yhDay(i)] = "0.3"
	}
	lend := &yieldHistoryLending{ratesLendingReader: f.lending, known: map[string]bool{f.activePool: true, f.frozenPool: true, f.blntPool: true}}
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	f.srv = v1.New(v1.Options{
		VerifiedCurrencies: cat, Exclusions: excl,
		Explorer: hx, Lending: lend, Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3"}},
		History: xlmUSDPrices(prices), ProtocolStats: store, CompletenessReader: verifiedHistory("blend"),
	})
	f.srv.NetworkPassphrase = network.PublicNetworkPassphrase
	return yieldHistoryFixture{ratesFixture: f, explorer: hx, base: httpTestServer(t, f.srv).URL}
}

type yieldHistoryBody struct {
	Data  v1.LendingYieldHistoryView `json:"data"`
	Flags v1.Flags                   `json:"flags"`
}

func (f yieldHistoryFixture) get(t *testing.T, query string) (yieldHistoryBody, *http.Response) {
	t.Helper()
	resp := mustGet(t, f.base+"/v1/lending/yields/history?"+query)
	var body yieldHistoryBody
	if resp.StatusCode == http.StatusOK {
		mustDecode(t, resp, &body)
	}
	return body, resp
}

func (f yieldHistoryFixture) rangeQuery(asset string) string {
	return "pool=" + f.activePool + "&asset=" + asset + "&from=" + yhD0.Format(time.RFC3339) + "&to=" + yhDay(4).Format(time.RFC3339)
}

func str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestLendingYieldsHistory_GoldenDayAndEveryNullReason pins one hand-derived day and each way a day is not computable.
//
// Day 0 (supply): the base is the 23:30 state of day -1 (within the hour before the day), the end the 23:30 state of
// day 0, so the span is 86400 s. b_rate 1.000000000000 -> 1.000100000000: (r1/r0 - 1) x 31536000/86400 = 0.0001 x 365 =
// 0.0365. At the close 100 b-tokens x 1.0001 = 100.01 XLM supplied, 50 d-tokens x 1.0003 = 50.015 XLM borrowed:
// U = ceil(50.015/100.01 x 1e7) = ceil(5000999.90) = 5001000 -> 0.5001. The rollup stored $0.30003 of BLND for the side;
// the side is 100.01 XLM x $0.30 = $30.003, so the emissions APR is 0.30003 x 365 / 30.003 = 3.6500.
func TestLendingYieldsHistory_GoldenDayAndEveryNullReason(t *testing.T) {
	f := newYieldHistoryFixture(t)
	body, resp := f.get(t, f.rangeQuery("native"))
	if resp.StatusCode != http.StatusOK || len(body.Data.Points) != 4 || body.Data.Side != "supply" || body.Data.Interval != "day" ||
		body.Data.ServedThrough == nil || !time.Time(*body.Data.ServedThrough).Equal(yhDay(4)) || str(body.Data.EmissionsThroughDay) != yhDay(2).Format(time.DateOnly) {
		t.Fatalf("status %d body %+v", resp.StatusCode, body.Data)
	}
	p := body.Data.Points
	d0 := p[0]
	if str(d0.RealisedAPR) != "0.0365" || d0.RealisedSpanSeconds == nil || *d0.RealisedSpanSeconds != 86400 ||
		!time.Time(*d0.RealisedFrom).Equal(yhD0.Add(-30*time.Minute)) || str(d0.Utilization) != "0.5001" ||
		str(d0.EmissionsTokens) != "3000300" || str(d0.EmissionsUSD) != "0.30" || str(d0.EmissionsAPR) != "3.6500" ||
		d0.EmissionsValuedAt == nil || !time.Time(*d0.EmissionsValuedAt).Equal(yhD0.Add(25*time.Hour)) {
		t.Errorf("golden day 0: realised %s span %v util %s tokens %s usd %s apr %s (%s)", str(d0.RealisedAPR), d0.RealisedSpanSeconds,
			str(d0.Utilization), str(d0.EmissionsTokens), str(d0.EmissionsUSD), str(d0.EmissionsAPR), str(d0.EmissionsAPRReason))
	}
	// Day 1: no state inside it; the state in force is day 0's, so nothing accrued. Never day 0's rate carried forward.
	if d1 := p[1]; d1.RealisedAPR != nil || str(d1.RealisedAPRReason) != "no_accrual_in_window" ||
		str(d1.EmissionsTokens) != "5000000000" || d1.EmissionsUSD != nil || d1.EmissionsAPR != nil || str(d1.EmissionsAPRReason) != "price_withheld" {
		t.Errorf("day 1 (no accrual, thin emission price) = %+v", d1)
	}
	// Day 2: day 1 left no base and day 0's 23:30 state is older than the slack, so the base is day 2's first state.
	if d2 := p[2]; d2.RealisedFrom == nil || !time.Time(*d2.RealisedFrom).Equal(yhDay(2).Add(6*time.Hour)) ||
		d2.RealisedSpanSeconds == nil || *d2.RealisedSpanSeconds != 43200 || d2.RealisedAPR == nil ||
		str(d2.EmissionsTokens) != "0" || str(d2.EmissionsAPR) != "0.0000" {
		t.Errorf("day 2 (base inside the day, a stored day with no row) = %+v", d2)
	}
	// Day 3: past the rollup's stored days: unknown, never zero.
	if d3 := p[3]; d3.EmissionsTokens != nil || d3.EmissionsAPR != nil || str(d3.EmissionsTokensReason) != "days_not_valued" ||
		str(d3.RealisedAPRReason) != "no_accrual_in_window" || d3.Utilization != nil {
		t.Errorf("day 3 (rollup not caught up) = %+v", d3)
	}
	borrow, _ := f.get(t, f.rangeQuery("native")+"&side=borrow")
	// Borrow, day 0: d_rate 1.0000 -> 1.0003 over a day is 0.0003 x 365 = 0.1095; a stored day with no borrow row is zero.
	if str(borrow.Data.Points[0].RealisedAPR) != "0.1095" || str(borrow.Data.Points[0].EmissionsTokens) != "0" {
		t.Errorf("borrow day 0 = %+v", borrow.Data.Points[0])
	}
}

// TestLendingYieldsHistory_EveryXLMSpellingReadsTheSACReserve: native, crypto:XLM and the SAC address read one series.
func TestLendingYieldsHistory_EveryXLMSpellingReadsTheSACReserve(t *testing.T) {
	f := newYieldHistoryFixture(t)
	for _, a := range []string{"native", "crypto:XLM", f.xlmSAC} {
		body, resp := f.get(t, f.rangeQuery(a))
		if resp.StatusCode != http.StatusOK || body.Data.Asset != f.xlmSAC || len(body.Data.Points) != 4 || str(body.Data.Points[0].RealisedAPR) != "0.0365" {
			t.Errorf("%s: status %d asset %s points %d", a, resp.StatusCode, body.Data.Asset, len(body.Data.Points))
		}
	}
}

// TestLendingYieldsHistory_RefusesUnknownAndBadInput: an unknown pool or asset is a 404 problem; a bad side, interval or a
// range past the cap is a 400.
func TestLendingYieldsHistory_RefusesUnknownAndBadInput(t *testing.T) {
	f := newYieldHistoryFixture(t)
	cases := map[string]int{
		"pool=" + mkCStrkey(t, 77) + "&asset=native":          http.StatusNotFound,
		"pool=" + f.activePool + "&asset=" + f.usdc:           http.StatusNotFound,
		"pool=" + f.activePool + "&asset=native&side=lend":    http.StatusBadRequest,
		"pool=" + f.activePool + "&asset=native&interval=1h":  http.StatusBadRequest,
		"pool=" + f.activePool + "&asset=native&window=2000d": http.StatusBadRequest,
		"pool=nope&asset=native":                              http.StatusBadRequest,
		"pool=" + f.activePool + "&asset=not-an-asset":        http.StatusBadRequest,
	}
	for q, want := range cases {
		resp := mustGet(t, f.base+"/v1/lending/yields/history?"+q)
		if resp.StatusCode != want || resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s: status %d (%s), want %d problem", q, resp.StatusCode, resp.Header.Get("Content-Type"), want)
		}
		_ = resp.Body.Close()
	}
}

// TestLendingYieldsHistory_ExcludedSourceDoesNotValueTheSide: with the XLM/USD source excluded over day 0 the side has no
// price that day, so its emissions APR is null with the walk's reason and the response says fills were withheld; the
// realised rate, which needs no price, is unchanged.
func TestLendingYieldsHistory_ExcludedSourceDoesNotValueTheSide(t *testing.T) {
	excluded := assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "soroswap", Base: "native", Quote: "fiat:USD", From: yhDay(-3), Until: yhDay(1), // past the 48h carry

	}}})
	f := newYieldHistoryFixtureExcluding(t, excluded)
	body, resp := f.get(t, f.rangeQuery("native"))
	if resp.StatusCode != http.StatusOK || len(body.Data.Points) != 4 {
		t.Fatalf("status %d points %d", resp.StatusCode, len(body.Data.Points))
	}
	d0 := body.Data.Points[0]
	if d0.EmissionsAPR != nil || str(d0.EmissionsAPRReason) != "no_price_within_bound" || str(d0.RealisedAPR) != "0.0365" {
		t.Errorf("day 0 under an exclusion: realised %s apr %s (%s)", str(d0.RealisedAPR), str(d0.EmissionsAPR), str(d0.EmissionsAPRReason))
	}
	if body.Flags.FillsWithheldReason != v1.PriceWithheldSourceExcluded {
		t.Errorf("flags = %+v; want fills withheld for the source exclusion", body.Flags)
	}
}

// TestLendingYieldsHistory_SlowLakeDegrades: a day read that never returns costs one budget; the series is served with every
// realised rate null and timed out, the emissions still read, and the response degraded.
func TestLendingYieldsHistory_SlowLakeDegrades(t *testing.T) {
	const budget = 200 * time.Millisecond
	defer v1.SetLendingYieldReadBudgetForTest(budget)()
	f := newYieldHistoryFixtureExcluding(t, nil)
	f.explorer.block, f.explorer.cancelLag = true, 100*time.Millisecond
	defer close(f.explorer.release)
	start := time.Now()
	body, resp := f.get(t, f.rangeQuery("native"))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("a blocked lake held the response %s", took)
	}
	if resp.StatusCode != http.StatusOK || len(body.Data.Points) != 4 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d points %d cache %q", resp.StatusCode, len(body.Data.Points), resp.Header.Get("Cache-Control"))
	}
	for _, p := range body.Data.Points {
		if p.RealisedAPR != nil || str(p.RealisedAPRReason) != "rate_history_timed_out" {
			t.Errorf("point %v = %+v, want a timed-out realised rate", time.Time(p.T), p)
		}
	}
	// The emissions read did not wait on the lake: day 0's stored tokens and USD are served; with no state the side has
	// no value, so the APR alone is null.
	if d0 := body.Data.Points[0]; str(d0.EmissionsTokens) != "3000300" || str(d0.EmissionsUSD) != "0.30" || d0.EmissionsAPR != nil ||
		str(d0.EmissionsAPRReason) != "reserve_unread" {
		t.Errorf("day 0 emissions under a slow lake: tokens %s usd %s apr %s (%s)", str(d0.EmissionsTokens),
			str(d0.EmissionsUSD), str(d0.EmissionsAPR), str(d0.EmissionsAPRReason))
	}
}

// TestLendingYieldsHistory_FailedCoverageStillStopsAtToday: with the coverage read failing, a range reaching past today is
// served only through today's UTC midnight, every realised rate null with the read's reason.
func TestLendingYieldsHistory_FailedCoverageStillStopsAtToday(t *testing.T) {
	f := newYieldHistoryFixture(t)
	f.explorer.covErr = errors.New("lake down")
	today := time.Now().UTC().Truncate(24 * time.Hour)
	body, resp := f.get(t, "pool="+f.activePool+"&asset=native&from="+yhD0.Format(time.RFC3339)+"&to="+today.AddDate(0, 0, 3).Format(time.RFC3339))
	if resp.StatusCode != http.StatusOK || body.Data.ServedThrough == nil || !time.Time(*body.Data.ServedThrough).Equal(today) ||
		len(body.Data.Points) != 10 || !time.Time(body.Data.Points[9].T).Equal(today.Add(-24*time.Hour)) {
		t.Fatalf("status %d served through %v points %d, want through %v", resp.StatusCode, body.Data.ServedThrough, len(body.Data.Points), today)
	}
	if p := body.Data.Points[0]; p.RealisedAPR != nil || str(p.RealisedAPRReason) != "rate_history_read_failed" {
		t.Errorf("day 0 = %s (%s), want the coverage read's reason", str(p.RealisedAPR), str(p.RealisedAPRReason))
	}
}
