package ingest

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
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
