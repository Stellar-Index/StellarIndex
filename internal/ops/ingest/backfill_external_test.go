package ingest

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	externalkraken "github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
)

// fakeTradeInserter is a DB-free tradeInserter: it fails on the
// TxHash keys listed in fail (or on every insert once infra=true, to
// exercise the abort path), and records every TxHash it was asked to
// insert.
type fakeTradeInserter struct {
	fail  map[string]error
	infra bool // if true, every failing insert returns an infra error

	got []string
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

// TestInsertBackfilledTrades_SkippedRowIsError: REL-02 — a per-row data
// fault must not be counted as "skipped" and silently exit 0. Dropped
// rows have no dead-letter, so any skip must surface as a non-nil error.
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

// TestInsertBackfilledTrades_AllGoodIsNil: the happy path must still
// exit 0 (regression guard against over-correcting into always-error).
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

// TestInsertBackfilledTrades_InfraFaultAborts: an infra fault (DB
// unreachable) must abort the loop immediately rather than "skip" every
// remaining trade one at a time and still report a misleading per-row
// skip count.
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

// tradeAt is a fill with an explicit venue timestamp — the field the
// resume cursor is derived from.
func tradeAt(hash string, ts time.Time) canonical.Trade {
	return canonical.Trade{Source: "kraken", Ledger: 0, TxHash: hash, Timestamp: ts}
}

// TestPartialFetchResume_SalvagesExpiredWalk: a multi-hour fills walk
// that runs out of budget must hand back a resume cursor instead of
// having its work discarded. The cursor is the high-water venue
// timestamp, not the last element, because a venue page is not
// guaranteed to be ordered within itself.
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
		// A venue's OHLC horizon truncation (kraken: /OHLC ignores
		// `since` once it is older than ~720 candles back and returns
		// its most recent window instead, err=nil upstream) must also
		// salvage the trades already fetched rather than let the
		// caller silently report success on an incomplete range.
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

// TestPartialFetchResume_RefusesRealFaults: only a context expiry is a
// stopping point. A venue 500, a decode fault or a nil error must never
// be reported as a salvageable partial walk — treating a real fault as
// "resume from here" would silently skip the range that faulted.
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

// TestPartialWalkError_NeverExitsZero: a truncated range must surface as
// a non-nil error whether or not the writes themselves succeeded, and
// must carry the resume cursor so a chunked walk can be scripted.
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

// A backfill over a window the live streamer already wrote inserts every
// fill again under a different synthesised tx_hash; the run must be
// refused and name the earliest stored row so the operator can set -to.
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

// -allow-overlap exists to re-run a window at the granularity that wrote
// it. A 1m window re-run at 1h would upsert one row per hour and leave
// the other stored minute rows beside it; the write must be refused and
// name the first such row.
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

// A walk that ended early covered only up to its high-water trade; rows
// past it are ones a later resume writes, not foreign ones.
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

// TestDropUnsettledCandles: a candle trade is stamped at its bar's last
// instant, so only bars that ended by min(-to, now) may be written. The
// venue's open bar and binance's inclusive-endTime bar (open == -to) must
// not reach trades, at second (kraken/coinbase/bitstamp) or millisecond
// (binance) stamp resolution.
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
