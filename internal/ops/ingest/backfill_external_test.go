package ingest

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
	externalvendorcsv "github.com/Stellar-Index/StellarIndex/internal/sources/external/vendorcsv"
)

// fakeTradeInserter is a DB-free tradeInserter that fails on the listed TxHash keys (or every insert once infra=true,
// to exercise the abort path) and records every TxHash it was asked to insert.
type fakeTradeInserter struct {
	fail  map[string]error
	infra bool // if true, every failing insert returns an infra error

	got     []string
	batches [][]string
}

// BatchInsertTrades drops rows Validate rejects without error and fails the whole batch if any row fails.
func (f *fakeTradeInserter) BatchInsertTrades(_ context.Context, trades []canonical.Trade) error {
	var batch []string
	for _, t := range trades {
		if t.Validate() != nil {
			continue
		}
		if err, bad := f.fail[t.TxHash]; bad {
			if f.infra {
				return fmt.Errorf("sub-batch: %w", driver.ErrBadConn)
			}
			return fmt.Errorf("sub-batch: %w", err)
		}
		batch = append(batch, t.TxHash)
	}
	f.batches = append(f.batches, batch)
	f.got = append(f.got, batch...)
	return nil
}

func (f *fakeTradeInserter) InsertTrade(_ context.Context, t canonical.Trade) error {
	f.got = append(f.got, t.TxHash)
	if err, bad := f.fail[t.TxHash]; bad {
		if f.infra {
			return driver.ErrBadConn
		}
		return err
	}
	return nil
}

func tradeWithHash(hash string) canonical.Trade {
	return canonical.Trade{Source: "binance", Ledger: 1, TxHash: hash, Timestamp: time.Now()}
}

// TestInsertBackfilledTrades_SkippedRowIsError: any skipped row must surface as a non-nil error, since dropped rows have no dead-letter.
func TestInsertBackfilledTrades_SkippedRowIsError(t *testing.T) {
	trades := []canonical.Trade{tradeWithHash("a"), tradeWithHash("b"), tradeWithHash("c")}
	store := &fakeTradeInserter{fail: map[string]error{"b": errors.New("numeric overflow")}}
	var log bytes.Buffer

	err := insertBackfilledTrades(context.Background(), store, trades, 1000, &log, time.Now())
	if err == nil {
		t.Fatal("expected a non-nil error when a row failed to insert, got nil")
	}
	// The other two rows must still have been attempted (data-fault path
	// continues past the bad row).
	if len(store.got) != 3 {
		t.Fatalf("expected all 3 trades attempted despite one failure, got %v", store.got)
	}
}

// TestInsertBackfilledTrades_AllGoodIsNil: the happy path still exits 0.
func TestInsertBackfilledTrades_AllGoodIsNil(t *testing.T) {
	trades := []canonical.Trade{tradeWithHash("a"), tradeWithHash("b")}
	store := &fakeTradeInserter{}
	var log bytes.Buffer

	if err := insertBackfilledTrades(context.Background(), store, trades, 1000, &log, time.Now()); err != nil {
		t.Fatalf("expected nil error on an all-succeeded run, got %v", err)
	}
	if len(store.got) != 2 {
		t.Fatalf("expected both trades attempted, got %v", store.got)
	}
}

// TestInsertBackfilledTrades_InfraFaultAborts: an infra fault aborts the loop at once instead of skipping each remaining trade.
func TestInsertBackfilledTrades_InfraFaultAborts(t *testing.T) {
	trades := []canonical.Trade{tradeWithHash("a"), tradeWithHash("b"), tradeWithHash("c")}
	store := &fakeTradeInserter{fail: map[string]error{"b": driver.ErrBadConn}, infra: true}
	var log bytes.Buffer

	err := insertBackfilledTrades(context.Background(), store, trades, 1000, &log, time.Now())
	if err == nil {
		t.Fatal("expected a non-nil error on infra fault, got nil")
	}
	// Must abort BEFORE attempting the third trade — an infra fault
	// affects every remaining insert identically, so there is no value
	// in ploughing through the rest.
	if len(store.got) != 2 {
		t.Fatalf("expected abort after 2 attempts (a, b) on infra fault, got %v", store.got)
	}
}

// storableTrade is a fill that passes canonical.Trade.Validate, so it
// takes the batch path.
func storableTrade(t *testing.T, i int) canonical.Trade {
	t.Helper()
	one := canonical.NewAmount(big.NewInt(1))
	return canonical.Trade{
		Source: "kraken", TxHash: fmt.Sprintf("%064x", i), Timestamp: time.Unix(int64(1_600_000_000+i), 0),
		Pair: xlmUSD(t), BaseAmount: one, QuoteAmount: one,
	}
}

func storableTrades(t *testing.T, n int) []canonical.Trade {
	t.Helper()
	out := make([]canonical.Trade, n)
	for i := range out {
		out[i] = storableTrade(t, i)
	}
	return out
}

// TestInsertBackfilledTrades_Batches: valid fills are written in backfillInsertChunk-sized batches.
func TestInsertBackfilledTrades_Batches(t *testing.T) {
	trades := storableTrades(t, backfillInsertChunk+5)
	store := &fakeTradeInserter{}

	if err := insertBackfilledTrades(context.Background(), store, trades, 0, io.Discard, time.Now()); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(store.batches) != 2 || len(store.batches[0]) != backfillInsertChunk || len(store.batches[1]) != 5 {
		t.Fatalf("batch sizes = %v, want [%d 5]", batchSizes(store.batches), backfillInsertChunk)
	}
	if len(store.got) != len(trades) {
		t.Fatalf("%d written, want %d", len(store.got), len(trades))
	}
}

// TestInsertBackfilledTrades_InvalidRowIsNotSilentlyDropped: a row the batch path drops must still count as skipped and exit non-zero.
func TestInsertBackfilledTrades_InvalidRowIsNotSilentlyDropped(t *testing.T) {
	trades := storableTrades(t, 3)
	// One zero leg is an admitted fill since 0191; both zero is still invalid.
	trades[1].BaseAmount = canonical.NewAmount(big.NewInt(0))
	trades[1].QuoteAmount = canonical.NewAmount(big.NewInt(0))
	store := &fakeTradeInserter{fail: map[string]error{trades[1].TxHash: canonical.ErrInvalidTrade}}

	err := insertBackfilledTrades(context.Background(), store, trades, 0, io.Discard, time.Now())
	if err == nil {
		t.Fatal("expected a non-nil error when a row failed validation, got nil")
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 {
		t.Fatalf("batches = %v, want one batch of the 2 valid rows", store.batches)
	}
}

// TestInsertBackfilledTrades_BatchDataFaultIsolatesRow: a failed batch is retried row by row so only the bad row is skipped.
func TestInsertBackfilledTrades_BatchDataFaultIsolatesRow(t *testing.T) {
	trades := storableTrades(t, 3)
	store := &fakeTradeInserter{fail: map[string]error{trades[1].TxHash: errors.New("check violation")}}
	var log bytes.Buffer

	err := insertBackfilledTrades(context.Background(), store, trades, 0, &log, time.Now())
	if err == nil {
		t.Fatal("expected a non-nil error when a row failed to insert, got nil")
	}
	if len(store.got) != 3 {
		t.Fatalf("expected all 3 rows retried row by row, got %v", store.got)
	}
	if !strings.Contains(log.String(), "1 skipped") || !strings.Contains(log.String(), "2 inserted") {
		t.Fatalf("log = %q, want 2 inserted and 1 skipped", log.String())
	}
}

// TestInsertBackfilledTrades_BatchInfraFaultAborts: an infra fault on a batch aborts with no row-by-row retry.
func TestInsertBackfilledTrades_BatchInfraFaultAborts(t *testing.T) {
	trades := storableTrades(t, 3)
	store := &fakeTradeInserter{fail: map[string]error{trades[1].TxHash: driver.ErrBadConn}, infra: true}

	err := insertBackfilledTrades(context.Background(), store, trades, 0, io.Discard, time.Now())
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("err = %v, want ErrBadConn", err)
	}
	if len(store.got) != 0 {
		t.Fatalf("expected no row-by-row retry after an infra fault, got %v", store.got)
	}
}

func batchSizes(batches [][]string) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}

// tradeAt is a fill with an explicit venue timestamp — the field the
// resume cursor is derived from.
func tradeAt(hash string, ts time.Time) canonical.Trade {
	return canonical.Trade{Source: "kraken", Ledger: 0, TxHash: hash, Timestamp: ts}
}

// TestPartialFetchResume_SalvagesExpiredWalk: a walk that runs out of budget hands back a resume cursor at the high-water venue timestamp, since venue pages are not ordered within themselves.
func TestPartialFetchResume_SalvagesExpiredWalk(t *testing.T) {
	base := time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC)
	trades := []canonical.Trade{
		tradeAt("a", base),
		tradeAt("c", base.Add(2*time.Hour)),
		tradeAt("b", base.Add(time.Hour)),
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadline", context.DeadlineExceeded},
		{"cancel", context.Canceled},
		{"wrapped", fmt.Errorf("kraken.BackfillTrades: %w", context.DeadlineExceeded)},
		// A depth-exceeded truncation (kraken /OHLC returns its latest window once `since` is too old, err=nil) must also salvage fetched trades, not report success on an incomplete range.
		{"depth exceeded", fmt.Errorf("kraken.Backfill: %w", externalkraken.ErrDepthExceeded)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := partialFetchResume(trades, tc.err)
			if !ok {
				t.Fatalf("expected salvage for %v, got ok=false", tc.err)
			}
			if want := base.Add(2 * time.Hour); !at.Equal(want) {
				t.Fatalf("resume cursor = %v, want the high-water fill %v", at, want)
			}
		})
	}
}

// TestPartialFetchResume_RefusesRealFaults: only a context expiry is a stopping point; a venue 500, decode fault or nil error must never look resumable.
func TestPartialFetchResume_RefusesRealFaults(t *testing.T) {
	trades := []canonical.Trade{tradeAt("a", time.Now())}
	for _, tc := range []struct {
		name   string
		trades []canonical.Trade
		err    error
	}{
		{"no error", trades, nil},
		{"no trades", nil, context.DeadlineExceeded},
		{"venue fault", trades, errors.New("kraken: HTTP 500")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := partialFetchResume(tc.trades, tc.err); ok {
				t.Fatalf("expected ok=false for %s, got a salvage", tc.name)
			}
		})
	}
}

// TestPartialWalkError_NeverExitsZero: a truncated range is a non-nil error carrying the resume cursor.
func TestPartialWalkError_NeverExitsZero(t *testing.T) {
	at := time.Date(2023, 6, 1, 12, 0, 0, 0, time.UTC)

	clean := partialWalkError(42, at, nil)
	if clean == nil {
		t.Fatal("expected a non-nil error for a truncated range with clean writes")
	}
	if !strings.Contains(clean.Error(), "2023-06-01T12:00:00Z") {
		t.Fatalf("resume cursor missing from error: %v", clean)
	}

	insErr := errors.New("3 of 42 trade(s) failed to insert")
	both := partialWalkError(42, at, insErr)
	if !errors.Is(both, insErr) {
		t.Fatalf("insert fault must stay unwrappable, got %v", both)
	}
	if !strings.Contains(both.Error(), "2023-06-01T12:00:00Z") {
		t.Fatalf("resume cursor missing from combined error: %v", both)
	}
}

type fakeTradeProbe struct {
	first  time.Time
	found  bool
	err    error
	source string
	pair   canonical.Pair
	from   time.Time
	to     time.Time
}

func (f *fakeTradeProbe) EarliestTradeInWindow(_ context.Context, source string, pair canonical.Pair, from, to time.Time) (time.Time, bool, error) {
	f.source, f.pair, f.from, f.to = source, pair, from, to
	return f.first, f.found, f.err
}

// TestRefuseStoredOverlap_RefusesWindowWithStoredRows: a window the live streamer already wrote is refused, naming the earliest stored row so the operator can set -to.
func TestRefuseStoredOverlap_RefusesWindowWithStoredRows(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	from := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	live := time.Date(2026, 4, 2, 9, 30, 0, 0, time.UTC)

	probe := &fakeTradeProbe{first: live, found: true}
	err = refuseStoredOverlap(context.Background(), probe, "kraken", pair, from, to)
	if !errors.Is(err, errBackfillOverlap) {
		t.Fatalf("err = %v, want errBackfillOverlap", err)
	}
	if !strings.Contains(err.Error(), "-to 2026-04-02T09:30:00Z") {
		t.Fatalf("refusal does not name the earliest stored row as the -to bound: %v", err)
	}
	if probe.source != "kraken" || !probe.pair.Equal(pair) || !probe.from.Equal(from) || !probe.to.Equal(to) {
		t.Fatalf("probed (%s, %s, %v, %v), want (kraken, %s, %v, %v)",
			probe.source, probe.pair, probe.from, probe.to, pair, from, to)
	}

	if err := refuseStoredOverlap(context.Background(), &fakeTradeProbe{}, "kraken", pair, from, to); err != nil {
		t.Fatalf("empty window refused: %v", err)
	}

	probeErr := errors.New("connection refused")
	err = refuseStoredOverlap(context.Background(), &fakeTradeProbe{err: probeErr}, "kraken", pair, from, to)
	if !errors.Is(err, probeErr) || errors.Is(err, errBackfillOverlap) {
		t.Fatalf("probe failure must fail the run as a probe error, got %v", err)
	}
}

type fakeForeignProbe struct {
	n     int64
	first time.Time
	err   error
	to    time.Time
	keep  []canonical.Trade
}

func (f *fakeForeignProbe) TradesInWindowOutside(_ context.Context, _ string, _ canonical.Pair, _, to time.Time, keep []canonical.Trade) (int64, time.Time, error) {
	f.to, f.keep = to, keep
	return f.n, f.first, f.err
}

type fakeSourceProbe struct {
	sources  []string
	err      error
	from, to time.Time
}

func (f *fakeSourceProbe) TradeSourcesInWindow(_ context.Context, _ canonical.Pair, from, to time.Time) ([]string, error) {
	f.from, f.to = from, to
	return f.sources, f.err
}

// TestRefuseMixedBuckets: a vendor series never shares a pair window with a VWAP-eligible series, whichever one is being written.
func TestRefuseMixedBuckets(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	from := time.Date(2014, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2017, 1, 18, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	cases := []struct {
		name   string
		run    string
		stored []string
		want   string // substring of the refusal; "" means allowed
	}{
		{"vendor into empty window", externalvendorcsv.SourceName, nil, ""},
		{"vendor re-run over itself", externalvendorcsv.SourceName, []string{externalvendorcsv.SourceName}, ""},
		{"vendor beside poloniex (both non-VWAP)", externalvendorcsv.SourceName, []string{"poloniex_via_btc"}, ""},
		{"vendor into kraken rows", externalvendorcsv.SourceName, []string{"kraken"}, "already has kraken rows"},
		{"kraken into vendor rows", "kraken", []string{"coingecko_daily"}, "already has coingecko_daily rows"},
		{"kraken over kraken", "kraken", []string{"kraken"}, ""},
	}
	// The probe spans every week and month bucket the run touches, not the run itself.
	wantLo := time.Date(2014, 7, 28, 0, 0, 0, 0, time.UTC)
	wantHi := time.Date(2017, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		probe := &fakeSourceProbe{sources: tc.stored}
		err := refuseMixedBuckets(ctx, probe, tc.run, pair, from, to)
		if !probe.from.Equal(wantLo) || !probe.to.Equal(wantHi) {
			t.Errorf("%s: probe window = [%s, %s); want [%s, %s)", tc.name, probe.from, probe.to, wantLo, wantHi)
		}
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		case tc.want != "" && !errors.Is(err, errBackfillMixedBuckets):
			t.Errorf("%s: err = %v, want errBackfillMixedBuckets", tc.name, err)
		case tc.want != "" && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: refusal does not name the stored series: %v", tc.name, err)
		}
	}
	boom := errors.New("boom")
	if err := refuseMixedBuckets(ctx, &fakeSourceProbe{err: boom}, "kraken", pair, from, to); !errors.Is(err, boom) {
		t.Errorf("probe error not wrapped: %v", err)
	}
}

// TestBucketSpan: the widened window covers the Monday week and the calendar month of both ends.
func TestBucketSpan(t *testing.T) {
	d := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ from, to, lo, hi string }{
		// A Friday start; a Wednesday end: week floors on both sides, month floor only on the end.
		{"2014-08-01T00:00:00Z", "2017-01-18T00:00:00Z", "2014-07-28T00:00:00Z", "2017-02-01T00:00:00Z"},
		// -to on a month boundary: the last row (2016-12-31T23:59:59Z) is in week [12-26, 01-02) and December.
		{"2014-08-05T23:59:59Z", "2017-01-01T00:00:00Z", "2014-08-01T00:00:00Z", "2017-01-02T00:00:00Z"},
		// A Monday start on the 1st, a window inside one week: still the whole month.
		{"2016-08-01T00:00:00Z", "2016-08-03T00:00:00Z", "2016-08-01T00:00:00Z", "2016-09-01T00:00:00Z"},
		// A Sunday: week floor is the previous Monday.
		{"2017-01-15T12:00:00Z", "2017-01-15T13:00:00Z", "2017-01-01T00:00:00Z", "2017-02-01T00:00:00Z"},
	}
	for _, tc := range cases {
		lo, hi := bucketSpan(d(tc.from), d(tc.to))
		if !lo.Equal(d(tc.lo)) || !hi.Equal(d(tc.hi)) {
			t.Errorf("bucketSpan(%s, %s) = [%s, %s); want [%s, %s)", tc.from, tc.to, lo.Format(time.RFC3339), hi.Format(time.RFC3339), tc.lo, tc.hi)
		}
	}
}

type fakePreWalkProbe struct {
	fakeTradeProbe
	fakeSourceProbe
}

// TestRefuseStoredRows_AllowOverlapWaivesOnlyTheOverlapCheck: a vendor re-run over Kraken buckets is refused
// even with -allow-overlap, which exists only so a window this command wrote can be rewritten.
func TestRefuseStoredRows_AllowOverlapWaivesOnlyTheOverlapCheck(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	from := time.Date(2014, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2017, 1, 18, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()
	vendor := externalvendorcsv.SourceName

	probe := &fakePreWalkProbe{
		fakeTradeProbe:  fakeTradeProbe{first: from.Add(time.Hour), found: true},
		fakeSourceProbe: fakeSourceProbe{sources: []string{vendor, "kraken"}},
	}
	if err := refuseStoredRows(ctx, probe, vendor, pair, from, to, false); !errors.Is(err, errBackfillOverlap) {
		t.Errorf("without -allow-overlap: err = %v, want errBackfillOverlap first", err)
	}
	if err := refuseStoredRows(ctx, probe, vendor, pair, from, to, true); !errors.Is(err, errBackfillMixedBuckets) {
		t.Errorf("with -allow-overlap: err = %v, want errBackfillMixedBuckets", err)
	}
	probe.fakeSourceProbe.sources = []string{vendor}
	if err := refuseStoredRows(ctx, probe, vendor, pair, from, to, true); err != nil {
		t.Errorf("re-run over own rows with -allow-overlap: err = %v, want nil", err)
	}
}

// TestBuildBackfiller_VendorCSV: -file is the vendor source's only input and is refused elsewhere.
func TestBuildBackfiller_VendorCSV(t *testing.T) {
	if _, _, err := buildBackfiller("kraken", "XLM/USD", "x.csv"); err == nil || !strings.Contains(err.Error(), "-file applies only") {
		t.Errorf("kraken with -file: err = %v", err)
	}
	if _, _, err := buildBackfiller(externalvendorcsv.SourceName, "XLM/USD", ""); err == nil || !strings.Contains(err.Error(), "requires -file") {
		t.Errorf("vendor without -file: err = %v", err)
	}
	if _, _, err := buildBackfiller(externalvendorcsv.SourceName, "XLM/EUR", "x.csv"); err == nil || !strings.Contains(err.Error(), "only -pair XLM/USD") {
		t.Errorf("vendor with XLM/EUR: err = %v", err)
	}
	b, pair, err := buildBackfiller(externalvendorcsv.SourceName, "XLM/USD", "x.csv")
	if err != nil {
		t.Fatal(err)
	}
	v, ok := b.(*externalvendorcsv.Backfiller)
	if !ok || v.Path != "x.csv" || pair.String() != "crypto:XLM/fiat:USD" {
		t.Errorf("got %T %+v pair %s; want *vendorcsv.Backfiller{Path: x.csv} crypto:XLM/fiat:USD", b, b, pair)
	}
}

// TestRefuseForeignRows_RefusesRowsTheRunWouldNotRewrite: -allow-overlap re-runs at the granularity that wrote the rows; a coarser rewrite is refused, naming the first such row.
func TestRefuseForeignRows_RefusesRowsTheRunWouldNotRewrite(t *testing.T) {
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	pair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	from := time.Date(2025, 4, 18, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	minute := from.Add(59 * time.Second)
	batch := []canonical.Trade{tradeAt("h", from.Add(time.Hour-time.Second))}

	probe := &fakeForeignProbe{n: 1416, first: minute}
	err = refuseForeignRows(context.Background(), probe, "kraken", pair, from, to, batch)
	if !errors.Is(err, errBackfillForeignRows) {
		t.Fatalf("err = %v, want errBackfillForeignRows", err)
	}
	if !strings.Contains(err.Error(), "1416 stored row(s) from 2025-04-18T00:00:59Z") {
		t.Fatalf("refusal does not name the count and first foreign row: %v", err)
	}
	if len(probe.keep) != 1 || probe.keep[0].TxHash != "h" || !probe.to.Equal(to) {
		t.Fatalf("probe got keep=%v to=%v, want the run's batch and -to", probe.keep, probe.to)
	}

	if err := refuseForeignRows(context.Background(), &fakeForeignProbe{}, "kraken", pair, from, to, batch); err != nil {
		t.Fatalf("same-granularity re-run refused: %v", err)
	}

	probeErr := errors.New("connection refused")
	err = refuseForeignRows(context.Background(), &fakeForeignProbe{err: probeErr}, "kraken", pair, from, to, batch)
	if !errors.Is(err, probeErr) || errors.Is(err, errBackfillForeignRows) {
		t.Fatalf("probe failure must fail the run as a probe error, got %v", err)
	}
}

// TestWalkedTo: a walk that ended early covers only up to its high-water trade; later rows are resume rows, not foreign.
func TestWalkedTo(t *testing.T) {
	to := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	high := to.Add(-72 * time.Hour)
	if got := walkedTo(to, time.Time{}, false); !got.Equal(to) {
		t.Errorf("complete walk: %v, want %v", got, to)
	}
	if got, want := walkedTo(to, high, true), high.Add(time.Microsecond); !got.Equal(want) {
		t.Errorf("partial walk: %v, want %v", got, want)
	}
}

// TestDropUnsettledCandles: only bars that ended by min(-to, now) are written; the open bar and binance's inclusive-endTime bar never reach trades.
func TestDropUnsettledCandles(t *testing.T) {
	to := time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC)
	barEnd := func(open time.Time, res time.Duration) time.Time { return open.Add(time.Hour - res) }

	for _, res := range []time.Duration{time.Second, time.Millisecond} {
		t.Run(res.String(), func(t *testing.T) {
			historical := []canonical.Trade{
				tradeAt("04:00", barEnd(to.Add(-2*time.Hour), res)),
				tradeAt("05:00", barEnd(to.Add(-time.Hour), res)),
				tradeAt("06:00-open-eq-to", barEnd(to, res)),
			}
			got, dropped := dropUnsettledCandles(historical, to, to.Add(24*time.Hour))
			if dropped != 1 || len(got) != 2 || got[1].TxHash != "05:00" {
				t.Fatalf("historical -to: kept %v dropped %d, want [04:00 05:00] and 1 dropped", txHashes(got), dropped)
			}

			// -to beyond now: the bar that opened at `to` is still in progress.
			live := []canonical.Trade{
				tradeAt("05:00", barEnd(to.Add(-time.Hour), res)),
				tradeAt("06:00-in-progress", barEnd(to, res)),
			}
			got, dropped = dropUnsettledCandles(live, to.Add(48*time.Hour), to.Add(30*time.Minute))
			if dropped != 1 || len(got) != 1 || got[0].TxHash != "05:00" {
				t.Fatalf("-to past now: kept %v dropped %d, want [05:00]", txHashes(got), dropped)
			}

			// A bar ending exactly at now has closed; 1ms earlier it has not.
			bar := []canonical.Trade{tradeAt("05:00", barEnd(to.Add(-time.Hour), res))}
			if got, _ := dropUnsettledCandles(bar, to.Add(time.Hour), to); len(got) != 1 {
				t.Fatalf("bar ending at now was dropped")
			}
			bar = []canonical.Trade{tradeAt("05:00", barEnd(to.Add(-time.Hour), res))}
			if got, _ := dropUnsettledCandles(bar, to.Add(time.Hour), to.Add(-time.Millisecond)); len(got) != 0 {
				t.Fatalf("bar kept %v before it closed", txHashes(got))
			}
		})
	}
}

func txHashes(trades []canonical.Trade) []string {
	out := make([]string, 0, len(trades))
	for _, tr := range trades {
		out = append(out, tr.TxHash)
	}
	return out
}
