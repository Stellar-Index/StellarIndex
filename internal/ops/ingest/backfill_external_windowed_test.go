package ingest

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
)

var windowDay1 = time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)

type fakeKrakenFill struct {
	at time.Time
	id int64
}

// fakeKrakenFills spans three days: one fill half a second before the
// day-2 boundary (repeated by the next window's overlap), and one exactly
// ON it, which an exclusive `since` cursor would drop without the overlap.
var fakeKrakenFills = []fakeKrakenFill{
	{windowDay1.Add(time.Hour), 1},
	{windowDay1.Add(24*time.Hour - 500*time.Millisecond), 2},
	{windowDay1.Add(24 * time.Hour), 3},
	{windowDay1.Add(29 * time.Hour), 4},
	{windowDay1.Add(49 * time.Hour), 5},
	{windowDay1.Add(50 * time.Hour), 6},
	{windowDay1.Add(80 * time.Hour), 7},
}

// fakeKrakenPage stands in for Kraken's 1000-fill page so the walk paginates.
const fakeKrakenPage = 3

// newFakeKrakenTrades serves /0/public/Trades from fakeKrakenFills with a
// strictly-exclusive `since`, and answers HTTP 502 for any page starting
// at or after failFrom.
func newFakeKrakenTrades(t *testing.T, failFrom time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if err != nil {
			t.Errorf("since: %v", err)
		}
		if since >= failFrom.UnixNano() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		rows := [][]any{}
		last := since
		for _, f := range fakeKrakenFills {
			if f.at.UnixNano() > since && len(rows) < fakeKrakenPage {
				rows = append(rows, []any{"0.30000000", "100.00000000", float64(f.at.UnixNano()) / 1e9, "b", "l", "", f.id})
				last = f.at.UnixNano()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  []string{},
			"result": map[string]any{"XXLMZUSD": rows, "last": strconv.FormatInt(last, 10)},
		})
	}))
}

func xlmUSD(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// TestWalkWindowed_VenueErrorKeepsEarlierWindows: a mid-walk venue fault keeps earlier windows written exactly once and reports the failing window's start as the resume point.
func TestWalkWindowed_VenueErrorKeepsEarlierWindows(t *testing.T) {
	day3 := windowDay1.Add(48 * time.Hour)
	srv := newFakeKrakenTrades(t, day3.Add(-time.Minute))
	defer srv.Close()

	pair := xlmUSD(t)
	kr := &externalkraken.Streamer{Endpoint: srv.URL, PairMap: map[string]canonical.Pair{"XXLMZUSD": pair}}
	fetch := func(ctx context.Context, f, to time.Time) ([]canonical.Trade, error) {
		return kr.BackfillTrades(ctx, pair, f, to)
	}
	store := &fakeTradeInserter{}
	var batches [][]canonical.Trade
	sink := func(trades []canonical.Trade) error {
		batches = append(batches, append([]canonical.Trade(nil), trades...))
		return insertBackfilledTrades(context.Background(), store, trades, 0, io.Discard, time.Now())
	}
	plan := windowPlan{size: 24 * time.Hour, overlap: time.Second}

	err := walkWindowed(context.Background(), plan, windowDay1, windowDay1.Add(72*time.Hour), fetch, sink, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("err = %v, want the venue's HTTP 502", err)
	}
	if want := "resume with -from 2021-03-03T00:00:00Z"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}

	written := map[string]int{}
	for _, b := range batches {
		for _, tr := range b {
			written[tr.Timestamp.UTC().Format(time.RFC3339Nano)]++
		}
	}
	for _, f := range fakeKrakenFills[:4] {
		if n := written[f.at.Format(time.RFC3339Nano)]; n != 1 {
			t.Errorf("fill %d at %s written %d time(s), want exactly 1", f.id, f.at.Format(time.RFC3339Nano), n)
		}
	}
	if len(store.got) != 4 {
		t.Errorf("inserted %d trade(s), want the 4 from the two completed windows", len(store.got))
	}
	for i, b := range batches {
		if first, last := b[0].Timestamp, b[len(b)-1].Timestamp; last.Sub(first) >= plan.size {
			t.Errorf("batch %d spans %v, want it bounded by one window", i, last.Sub(first))
		}
	}
}

func windowTrades(from, to time.Time) []canonical.Trade {
	var out []canonical.Trade
	for at := from; at.Before(to); at = at.Add(6 * time.Hour) {
		out = append(out, tradeAt(at.Format(time.RFC3339), at))
	}
	return out
}

// candleUnderFillsProbe reports one stored kraken candle row (a foreign identity to the fills) in any window containing at.
type candleUnderFillsProbe struct {
	at    time.Time
	calls int
}

func (p *candleUnderFillsProbe) TradesInWindowOutside(_ context.Context, _ string, _ canonical.Pair, from, to time.Time, _ []canonical.Trade) (int64, time.Time, error) {
	p.calls++
	if !p.at.Before(from) && p.at.Before(to) {
		return 1, p.at, nil
	}
	return 0, time.Time{}, nil
}

// TestRefusingForeignFetch_StopsBeforeWritingOverCandles: an -allow-overlap fills walk must not write a window that holds rows its
// fills would not overwrite (candles), or that window's volume is counted twice.
func TestRefusingForeignFetch_StopsBeforeWritingOverCandles(t *testing.T) {
	fetch := func(_ context.Context, f, to time.Time) ([]canonical.Trade, error) { return windowTrades(f, to), nil }
	plan := windowPlan{size: 24 * time.Hour}
	day2 := windowDay1.Add(24 * time.Hour)
	probe := &candleUnderFillsProbe{at: day2.Add(time.Hour)}
	store := &fakeTradeInserter{}

	err := walkWindowed(context.Background(), plan, windowDay1, windowDay1.Add(72*time.Hour),
		refusingForeignFetch(fetch, probe, "kraken", xlmUSD(t), true),
		func(tr []canonical.Trade) error {
			return insertBackfilledTrades(context.Background(), store, tr, 0, io.Discard, time.Now())
		}, io.Discard)
	if !errors.Is(err, errBackfillForeignRows) || !strings.Contains(err.Error(), "resume with -from "+day2.Format(time.RFC3339)) {
		t.Fatalf("err = %v, want errBackfillForeignRows stopping at %s", err, day2.Format(time.RFC3339))
	}
	if len(store.got) != 4 {
		t.Fatalf("wrote %d trade(s), want only day 1's 4: nothing from the window holding candles", len(store.got))
	}

	quiet := &candleUnderFillsProbe{at: day2}
	if _, err := refusingForeignFetch(fetch, quiet, "kraken", xlmUSD(t), false)(context.Background(), windowDay1, day2); err != nil || quiet.calls != 0 {
		t.Fatalf("without -allow-overlap the pre-walk overlap refusal already applies: err=%v probe calls=%d, want nil and 0", err, quiet.calls)
	}
}

// TestWalkWindowed_WriteFaults: an infra write fault stops the walk; a per-row fault keeps walking but never exits 0.
func TestWalkWindowed_WriteFaults(t *testing.T) {
	fetch := func(_ context.Context, f, to time.Time) ([]canonical.Trade, error) { return windowTrades(f, to), nil }
	plan := windowPlan{size: 24 * time.Hour}
	to := windowDay1.Add(72 * time.Hour)
	day2 := windowDay1.Add(24 * time.Hour).Format(time.RFC3339)

	infra := &fakeTradeInserter{fail: map[string]error{day2: driver.ErrBadConn}, infra: true}
	err := walkWindowed(context.Background(), plan, windowDay1, to, fetch, func(tr []canonical.Trade) error {
		return insertBackfilledTrades(context.Background(), infra, tr, 0, io.Discard, time.Now())
	}, io.Discard)
	if !errors.Is(err, driver.ErrBadConn) || !strings.Contains(err.Error(), "resume with -from "+day2) {
		t.Fatalf("infra fault: err = %v, want ErrBadConn and a resume at %s", err, day2)
	}
	if len(infra.got) != 5 {
		t.Fatalf("infra fault: %d insert(s) attempted, want 4 from day 1 plus the failing one", len(infra.got))
	}

	data := &fakeTradeInserter{fail: map[string]error{day2: errors.New("check violation")}}
	err = walkWindowed(context.Background(), plan, windowDay1, to, fetch, func(tr []canonical.Trade) error {
		return insertBackfilledTrades(context.Background(), data, tr, 0, io.Discard, time.Now())
	}, io.Discard)
	if err == nil {
		t.Fatal("data fault: walk exited nil after dropping a row")
	}
	if len(data.got) != 12 {
		t.Fatalf("data fault: %d insert(s) attempted, want all 12 across three windows", len(data.got))
	}
}

// storedRowsProbe is TradesInWindowOutside over an in-memory trades table: rows in [from, to) whose (tx_hash, ts) is not in keep.
type storedRowsProbe struct{ rows []canonical.Trade }

func (p *storedRowsProbe) TradesInWindowOutside(_ context.Context, _ string, _ canonical.Pair, from, to time.Time, keep []canonical.Trade) (int64, time.Time, error) {
	id := func(tr canonical.Trade) string { return tr.TxHash + "@" + tr.Timestamp.UTC().Format(time.RFC3339Nano) }
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[id(k)] = true
	}
	var (
		n     int64
		first time.Time
	)
	for _, r := range p.rows {
		if r.Timestamp.Before(from) || !r.Timestamp.Before(to) || kept[id(r)] {
			continue
		}
		if n == 0 || r.Timestamp.Before(first) {
			first = r.Timestamp
		}
		n++
	}
	return n, first, nil
}

// rawWalkStore is a trades store already holding rows, for walking fills through walkRawTrades.
type rawWalkStore struct {
	fakeBackfillTradeStore
	storedRowsProbe
}

func fillsIn(fills []canonical.Trade) windowFetch {
	return func(_ context.Context, f, to time.Time) ([]canonical.Trade, error) {
		var out []canonical.Trade
		for _, tr := range fills {
			if !tr.Timestamp.Before(f) && tr.Timestamp.Before(to) {
				out = append(out, tr)
			}
		}
		return out, nil
	}
}

var testRawPlan = windowPlan{size: 24 * time.Hour, overlap: time.Second}

// TestWalkRawTrades_RefusesFillsInsideAStoredCandle: a candle is stamped at its bucket's last second, so a fills run ending inside the
// bucket never sees it in [-from, -to), yet its fills would sit beside the candle's whole-bucket volume.
func TestWalkRawTrades_RefusesFillsInsideAStoredCandle(t *testing.T) {
	cases := []struct {
		name      string
		candle    time.Time
		from, to  time.Time
		modes     []bool
		resume    string
		advisedTo time.Time // the -to the refusal names with -from kept; zero when it says none exists
	}{
		{
			name: "1d candle, half-day fills", candle: windowDay1.Add(24*time.Hour - time.Second),
			from: windowDay1, to: windowDay1.Add(12 * time.Hour), modes: []bool{false, true},
			resume: "No -to admits this -from: nothing in [2021-03-01T00:00:00Z, 2021-03-02T00:00:00Z)",
		},
		{
			name: "1h candle, half-hour fills", candle: windowDay1.Add(11*time.Hour - time.Second),
			from: windowDay1.Add(10 * time.Hour), to: windowDay1.Add(10*time.Hour + 30*time.Minute), modes: []bool{false, true},
			resume: "No -to admits this -from: nothing in [2021-03-01T10:00:00Z, 2021-03-01T11:00:00Z)",
		},
		// Without -allow-overlap the pre-walk [-from, -to) probe refuses this candle; with it, only the window-end probe can.
		{
			name: "1d candle straddling a window end", candle: windowDay1.Add(48*time.Hour - time.Second),
			from: windowDay1.Add(12 * time.Hour), to: windowDay1.Add(60 * time.Hour), modes: []bool{true},
			resume: "resume with -from 2021-03-01T12:00:00Z -to 2021-03-02T00:00:00Z", advisedTo: windowDay1.Add(24 * time.Hour),
		},
		// A coarser bucket that opened before -from: no -to after -from avoids it.
		{
			name: "1w candle opened before -from", candle: time.Date(2021, 3, 3, 23, 59, 59, 0, time.UTC),
			from: windowDay1.Add(12 * time.Hour), to: windowDay1.Add(36 * time.Hour), modes: []bool{false, true},
			resume: "No -to admits this -from: nothing in [2021-03-01T12:00:00Z, 2021-03-04T00:00:00Z)",
		},
	}
	for _, tc := range cases {
		for _, allowOverlap := range tc.modes {
			t.Run(fmt.Sprintf("%s/allow-overlap=%v", tc.name, allowOverlap), func(t *testing.T) {
				candle := []canonical.Trade{tradeAt("candle", tc.candle)}
				store := &rawWalkStore{storedRowsProbe: storedRowsProbe{rows: candle}}
				err := walkRawTrades(context.Background(), store, testRawPlan, fillsIn(windowTrades(tc.from, tc.to)), xlmUSD(t),
					tc.from, tc.to, allowOverlap, 0, io.Discard)
				if !errors.Is(err, errBackfillForeignRows) || !strings.Contains(err.Error(), tc.resume) {
					t.Fatalf("err = %v, want errBackfillForeignRows naming %q", err, tc.resume)
				}
				if len(store.got) != 0 {
					t.Fatalf("wrote %d fill(s) inside the stored candle's bucket, want none", len(store.got))
				}
				if tc.advisedTo.IsZero() {
					return
				}
				advised := &rawWalkStore{storedRowsProbe: storedRowsProbe{rows: candle}}
				if err := walkRawTrades(context.Background(), advised, testRawPlan, fillsIn(windowTrades(tc.from, tc.advisedTo)), xlmUSD(t),
					tc.from, tc.advisedTo, allowOverlap, 0, io.Discard); err != nil {
					t.Fatalf("the advised run -to %s is refused: %v", tc.advisedTo.Format(time.RFC3339), err)
				}
			})
		}
	}
}

// TestWalkRawTrades_AllowOverlapProbesEachWindow: the per-window probe is wired into the real walk, and its refusal names the
// run that is admitted — the same -from, ending at the first foreign row — rather than a -from that is refused again.
func TestWalkRawTrades_AllowOverlapProbesEachWindow(t *testing.T) {
	day2 := windowDay1.Add(24 * time.Hour)
	candle := day2.Add(time.Hour - time.Second)
	store := &rawWalkStore{storedRowsProbe: storedRowsProbe{rows: []canonical.Trade{tradeAt("candle", candle)}}}
	to := windowDay1.Add(72 * time.Hour)

	err := walkRawTrades(context.Background(), store, testRawPlan, fillsIn(windowTrades(windowDay1, to)), xlmUSD(t), windowDay1, to, true, 0, io.Discard)
	if !errors.Is(err, errBackfillForeignRows) {
		t.Fatalf("err = %v, want errBackfillForeignRows", err)
	}
	// The stored row is the stamp of a 1h candle opened at 00:00, so no fills run from the stopping window's start avoids it.
	if want := "nothing in [2021-03-02T00:00:00Z, 2021-03-02T01:00:00Z) can be written as fills"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}
	if !strings.Contains(err.Error(), "Keep -from and end the run with -to 2021-03-02T00:00:00Z") {
		t.Fatalf("inner refusal does not end the run at the candle's open: %v", err)
	}
	if len(store.got) != 4 {
		t.Fatalf("wrote %d fill(s), want only day 1's 4", len(store.got))
	}

	// The advised run: the same -from, ending at the candle's open, over the day-1 fills the walk wrote.
	written := append([]canonical.Trade{tradeAt("candle", candle)}, windowTrades(windowDay1, day2)...)
	advised := &rawWalkStore{storedRowsProbe: storedRowsProbe{rows: written}}
	if err := walkRawTrades(context.Background(), advised, testRawPlan, fillsIn(windowTrades(windowDay1, to)), xlmUSD(t), windowDay1, day2, true, 0, io.Discard); err != nil {
		t.Fatalf("the advised run -from %s -to %s is refused: %v", windowDay1.Format(time.RFC3339), day2.Format(time.RFC3339), err)
	}
}

// candleRunStore is a trades store for the candle path's pre-walk and post-walk refusals.
type candleRunStore struct{ storedRowsProbe }

func (s *candleRunStore) EarliestTradeInWindow(ctx context.Context, source string, pair canonical.Pair, from, to time.Time) (time.Time, bool, error) {
	n, first, err := s.TradesInWindowOutside(ctx, source, pair, from, to, nil)
	return first, n > 0, err
}

func (s *candleRunStore) TradeSourcesInWindow(context.Context, canonical.Pair, time.Time, time.Time, []string) ([]string, error) {
	return nil, nil
}

func (s *candleRunStore) Prices1mRetentionArmed(context.Context) (bool, error) { return false, nil }

// refuseCandleRun runs the candle path's refusals in order: pre-walk, then post-walk over the fetched candles.
func refuseCandleRun(t *testing.T, store *candleRunStore, from, to time.Time, granularity time.Duration, candles []canonical.Trade, allowOverlap bool) error {
	t.Helper()
	if err := refuseStoredRows(context.Background(), store, "kraken", xlmUSD(t), from, to, allowOverlap); err != nil {
		return err
	}
	return refuseCandleRunRows(context.Background(), store, "kraken", "XLM/USD", xlmUSD(t), from, to, granularity, candles)
}

// hourCandles are kraken XLM/USD 1h candles, with Backfill's identities, over [from, to).
func hourCandles(t *testing.T, from, to time.Time) []canonical.Trade {
	t.Helper()
	var out []canonical.Trade
	for open := from; open.Before(to); open = open.Add(time.Hour) {
		stamp := open.Add(time.Hour - time.Second)
		hash, err := externalkraken.CandleTxHash("XLM/USD", stamp, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tradeAt(hash, stamp))
	}
	return out
}

// TestRefuseCandleRunRows_InsideStoredCoarserCandle: 1h candles written inside a stored 1d candle's bucket count that span's
// volume twice in every rollup at or above 1d; neither the pre-walk [-from, -to) probe nor the walked window sees the 1d stamp.
func TestRefuseCandleRunRows_InsideStoredCoarserCandle(t *testing.T) {
	day := &candleRunStore{storedRowsProbe{rows: []canonical.Trade{tradeAt("1d", windowDay1.Add(24*time.Hour-time.Second))}}}
	nextDay := &candleRunStore{storedRowsProbe{rows: []canonical.Trade{tradeAt("1d", windowDay1.Add(48*time.Hour-time.Second))}}}
	day2 := windowDay1.Add(24 * time.Hour)
	for _, allowOverlap := range []bool{false, true} {
		to := windowDay1.Add(12 * time.Hour)
		err := refuseCandleRun(t, day, windowDay1, to, time.Hour, hourCandles(t, windowDay1, to), allowOverlap)
		if want := "No -to admits this -from: nothing in [2021-03-01T00:00:00Z, 2021-03-02T00:00:00Z)"; !errors.Is(err, errBackfillForeignRows) || !strings.Contains(err.Error(), want) {
			t.Errorf("allow-overlap=%v, 1h inside a stored 1d bucket: err = %v, want errBackfillForeignRows naming %q", allowOverlap, err, want)
		}

		to = day2.Add(12 * time.Hour)
		err = refuseCandleRun(t, nextDay, windowDay1, to, time.Hour, hourCandles(t, windowDay1, to), allowOverlap)
		if want := "Keep -from and end the run with -to 2021-03-02T00:00:00Z"; !errors.Is(err, errBackfillForeignRows) || !strings.Contains(err.Error(), want) {
			t.Fatalf("allow-overlap=%v, 1h run ending inside a stored 1d bucket: err = %v, want errBackfillForeignRows naming %q", allowOverlap, err, want)
		}
		if err := refuseCandleRun(t, nextDay, windowDay1, day2, time.Hour, hourCandles(t, windowDay1, day2), allowOverlap); err != nil {
			t.Errorf("allow-overlap=%v: the advised run -to %s is refused: %v", allowOverlap, day2.Format(time.RFC3339), err)
		}
	}
}

// TestRefuseCandleRunRows_SameGranularityReRunAdmitted: a re-run over candles this command wrote is admitted, including one that
// ends inside a coarser bucket whose stamp holds the run's own later candle (a 1h candle at 07:59:59 is also a 4h stamp).
func TestRefuseCandleRunRows_SameGranularityReRunAdmitted(t *testing.T) {
	to := windowDay1.Add(12 * time.Hour)
	own := hourCandles(t, windowDay1, to)
	store := &candleRunStore{storedRowsProbe{rows: own}}
	if err := refuseCandleRun(t, store, windowDay1, to, time.Hour, own, true); err != nil {
		t.Fatalf("complete re-run refused: %v", err)
	}
	partial := own[:6]
	end := walkedTo(to, partial[len(partial)-1].Timestamp, true)
	if err := refuseCandleRunRows(context.Background(), store, "kraken", "XLM/USD", xlmUSD(t), windowDay1, end, time.Hour, partial); err != nil {
		t.Fatalf("partial re-run refused: %v", err)
	}
	if err := refuseCandleRun(t, store, windowDay1, windowDay1.Add(6*time.Hour+30*time.Minute), time.Hour, partial, true); err != nil {
		t.Fatalf("re-run with -to inside its own stored candle refused: %v", err)
	}
	// The same stamp holding a 4h candle is foreign.
	fourHour := &candleRunStore{storedRowsProbe{rows: append(slices.Clone(partial), tradeAt("4h", windowDay1.Add(8*time.Hour-time.Second)))}}
	if err := refuseCandleRunRows(context.Background(), fourHour, "kraken", "XLM/USD", xlmUSD(t), windowDay1, end, time.Hour, partial); !errors.Is(err, errBackfillForeignRows) {
		t.Fatalf("err = %v, want errBackfillForeignRows for a stored 4h candle straddling the walked end", err)
	}
}

// TestRefuseCandleRunRows_PartialWalkCoversLastCandle: a walk that ended on a candle still writes that candle's whole bucket, so a
// stored fill in its final second is foreign.
func TestRefuseCandleRunRows_PartialWalkCoversLastCandle(t *testing.T) {
	candles := hourCandles(t, windowDay1, windowDay1.Add(6*time.Hour))
	last := candles[len(candles)-1].Timestamp
	store := &candleRunStore{storedRowsProbe{rows: []canonical.Trade{tradeAt("fill", last.Add(500*time.Millisecond))}}}
	end := walkedTo(windowDay1.Add(24*time.Hour), last, true)
	err := refuseCandleRunRows(context.Background(), store, "kraken", "XLM/USD", xlmUSD(t), windowDay1, end, time.Hour, candles)
	if !errors.Is(err, errBackfillForeignRows) {
		t.Fatalf("err = %v, want errBackfillForeignRows for the fill inside the last candle's final second", err)
	}
}

// TestWalkRawTrades_ReRunOfOwnFillsAdmitted: re-running a range whose fills this command already wrote — one inside the 1 s overlap
// the next window re-reads — writes every fill once and refuses nothing.
func TestWalkRawTrades_ReRunOfOwnFillsAdmitted(t *testing.T) {
	to := windowDay1.Add(72 * time.Hour)
	fills := append(windowTrades(windowDay1, to), tradeAt("overlap", windowDay1.Add(24*time.Hour-500*time.Millisecond)))
	store := &rawWalkStore{storedRowsProbe: storedRowsProbe{rows: fills}}

	if err := walkRawTrades(context.Background(), store, testRawPlan, fillsIn(fills), xlmUSD(t), windowDay1, to, true, 0, io.Discard); err != nil {
		t.Fatalf("re-run of own fills refused: %v", err)
	}
	if len(store.got) != len(fills) {
		t.Fatalf("wrote %d fill(s), want each of the %d exactly once", len(store.got), len(fills))
	}
}

// TestCandleCoverStart: a candle stamped after -from whose bucket opened before it widens the probe to that open.
func TestCandleCoverStart(t *testing.T) {
	from := windowDay1.Add(10*time.Hour + 30*time.Minute)
	for _, res := range []time.Duration{time.Second, time.Millisecond} {
		trades := []canonical.Trade{tradeAt("a", windowDay1.Add(11*time.Hour-res)), tradeAt("b", windowDay1.Add(12*time.Hour-res))}
		if got, want := candleCoverStart(trades, from, time.Hour), windowDay1.Add(10*time.Hour); !got.Equal(want) {
			t.Errorf("stamp at close-%v: %v, want the first bucket's open %v", res, got, want)
		}
	}
	if got := candleCoverStart([]canonical.Trade{tradeAt("a", windowDay1.Add(11*time.Hour-time.Second))}, windowDay1.Add(10*time.Hour), time.Hour); !got.Equal(windowDay1.Add(10 * time.Hour)) {
		t.Errorf("aligned -from widened to %v", got)
	}
}
