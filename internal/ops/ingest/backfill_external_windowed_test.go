package ingest

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestWalkWindowed_VenueErrorKeepsEarlierWindows: a venue fault partway
// through a deep -raw-trades walk must leave every earlier window written
// (each fill exactly once, the on-boundary fill included) and report the
// failing window's start as the resume point.
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

// TestWalkWindowed_WriteFaults: an infra write fault stops the walk at
// that window's start; a per-row data fault keeps walking but must never
// let the command exit 0.
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
