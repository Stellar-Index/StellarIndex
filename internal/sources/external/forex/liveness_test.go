package forex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeFXWriter is a controllable FXQuoteWriter for the liveness tests.
// It records the batch size it was handed and returns a configurable
// error so we can exercise the success / persist-failure / empty-batch
// paths of persistSnapshot independently.
type fakeFXWriter struct {
	err     error
	gotRows int
	calls   int
}

func (f *fakeFXWriter) InsertFXQuoteBatch(_ context.Context, quotes []FXQuote) error {
	f.calls++
	f.gotRows = len(quotes)
	return f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fxGauge() float64 {
	return testutil.ToFloat64(obs.ExternalFXLastQuoteUnix.WithLabelValues("massive"))
}

// TestPersistSnapshot_stampsLivenessGaugeOnCommittedWrite is the core
// regression: a successful non-empty fx_quotes write advances
// stellarindex_external_fx_last_quote_unix{source="massive"} to ~now().
// This is the liveness signal the stellarindex_external_fx_feed_stale
// alert keys off; without the stamp a dead FX feed is invisible until
// the 7-day forex-snap lookback expires and fiat pairs silently break.
//
// NOTE: these subtests share the process-global obs gauge, so they run
// sequentially (no t.Parallel) to avoid cross-mutating the "massive"
// child.
func TestPersistSnapshot_stampsLivenessGaugeOnCommittedWrite(t *testing.T) {
	w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
		History7d:   map[string][]HistoryPoint{},
	}

	before := time.Now().Unix()
	w.persistSnapshot(context.Background(), snap)

	got := fxGauge()
	if got < float64(before) {
		t.Fatalf("liveness gauge = %v, want >= %d (a committed write must stamp now())", got, before)
	}
}

// TestPersistSnapshot_failedWriteLeavesGaugeUntouched confirms a
// persist error does NOT advance the gauge — a wedged-but-erroring
// worker must not keep the feed looking fresh. We seed the gauge to a
// sentinel far below now() and assert it is unchanged after the failed
// write.
func TestPersistSnapshot_failedWriteLeavesGaugeUntouched(t *testing.T) {
	const sentinel = 42.0
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(sentinel)

	w := &Worker{writer: &fakeFXWriter{err: errors.New("db down")}, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
		History7d:   map[string][]HistoryPoint{},
	}

	w.persistSnapshot(context.Background(), snap)

	if got := fxGauge(); got != sentinel {
		t.Fatalf("liveness gauge = %v, want %v unchanged (failed write must not stamp)", got, sentinel)
	}
}

// TestPersistSnapshot_emptyBatchLeavesGaugeUntouched confirms that a
// "successful" write of an EMPTY batch (upstream returned no usable
// rates) does not stamp the gauge. Only a committed NON-EMPTY write is
// evidence the feed is live.
func TestPersistSnapshot_emptyBatchLeavesGaugeUntouched(t *testing.T) {
	const sentinel = 43.0
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(sentinel)

	fake := &fakeFXWriter{} // succeeds, but batch will be empty
	w := &Worker{writer: fake, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		// RateUSD <= 0 is skipped by persistSnapshot → empty batch.
		Currencies: []Currency{{Ticker: "EUR", RateUSD: 0}},
		History7d:  map[string][]HistoryPoint{},
	}

	w.persistSnapshot(context.Background(), snap)

	if fake.gotRows != 0 {
		t.Fatalf("expected an empty batch, writer saw %d rows", fake.gotRows)
	}
	if got := fxGauge(); got != sentinel {
		t.Fatalf("liveness gauge = %v, want %v unchanged (empty batch must not stamp)", got, sentinel)
	}
}

// TestPersistSnapshot_nilWriterIsNoop guards the cache-only mode: a nil
// writer must not panic and must not stamp the gauge (there was no
// write to be live about).
func TestPersistSnapshot_nilWriterIsNoop(t *testing.T) {
	const sentinel = 44.0
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(sentinel)

	w := &Worker{writer: nil, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
		History7d:   map[string][]HistoryPoint{},
	}

	w.persistSnapshot(context.Background(), snap)

	if got := fxGauge(); got != sentinel {
		t.Fatalf("liveness gauge = %v, want %v unchanged (nil writer must not stamp)", got, sentinel)
	}
}
