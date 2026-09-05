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
