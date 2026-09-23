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

// TestPersistSnapshot_syntheticUSDAnchorDoesNotStamp pins that the
// liveness gauge measures the UPSTREAM, not the worker. buildSnapshot
// always prepends the USD=1.0 anchor and the band re-accepts an exact
// 1.0 forever, so a refresh whose every upstream rate was dropped
// (unnamed, non-finite, refused) still commits a one-row batch. That row
// is true by definition and says nothing about the feed.
func TestPersistSnapshot_syntheticUSDAnchorDoesNotStamp(t *testing.T) {
	const sentinel = 45.0
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(sentinel)

	fake := &fakeFXWriter{}
	w := &Worker{writer: fake, logger: discardLogger()}
	now := time.Now().UTC()
	// "eur" has no name, so buildSnapshot drops it: the snapshot is the
	// anchor alone, the shape a names/rates mismatch produces.
	snap := buildSnapshot(map[string]float64{"eur": 0.92}, map[string]string{}, now, now, nil, nil)
	if len(snap.Currencies) != 1 || snap.Currencies[0].Ticker != "USD" {
		t.Fatalf("fixture: want the USD anchor alone, got %+v", snap.Currencies)
	}

	for i := 0; i < 3; i++ {
		w.persistSnapshot(context.Background(), snap)
	}

	if fake.gotRows != 1 {
		t.Fatalf("writer saw %d rows, want the 1 anchor row still persisted", fake.gotRows)
	}
	if got := fxGauge(); got != sentinel {
		t.Fatalf("liveness gauge = %v, want %v unchanged — the synthetic USD anchor is not an upstream quote", got, sentinel)
	}
}

// TestPersistSnapshot_carriedHistoryWithoutCurrentDoesNotStamp: history
// bars are fetched once a day and re-written every refresh from the
// worker's carry-forward, so they prove nothing about the feed this
// hour. Only a committed current upstream rate stamps.
func TestPersistSnapshot_carriedHistoryWithoutCurrentDoesNotStamp(t *testing.T) {
	const sentinel = 46.0
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(sentinel)

	fake := &fakeFXWriter{}
	w := &Worker{writer: fake, logger: discardLogger()}
	now := time.Now().UTC()
	history := map[string][]HistoryPoint{"EUR": {
		{Date: now.AddDate(0, 0, -2), RateUSD: 0.92},
		{Date: now.AddDate(0, 0, -1), RateUSD: 0.921},
	}}
	snap := buildSnapshot(map[string]float64{}, map[string]string{}, now, now, history, nil)

	w.persistSnapshot(context.Background(), snap)

	if fake.gotRows < 2 {
		t.Fatalf("fixture: writer saw %d rows, want the history bars persisted", fake.gotRows)
	}
	if got := fxGauge(); got != sentinel {
		t.Fatalf("liveness gauge = %v, want %v unchanged — carried-forward history is not a fresh quote", got, sentinel)
	}
}

// TestPersistSnapshot_realQuoteBesideAnchorStamps is the positive half
// on the shipped shape: the anchor plus one named upstream rate.
func TestPersistSnapshot_realQuoteBesideAnchorStamps(t *testing.T) {
	obs.ExternalFXLastQuoteUnix.WithLabelValues("massive").Set(47)

	w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
	now := time.Now().UTC()
	snap := buildSnapshot(map[string]float64{"eur": 0.92}, map[string]string{"eur": "euro"}, now, now, nil, nil)

	before := time.Now().Unix()
	w.persistSnapshot(context.Background(), snap)

	if got := fxGauge(); got < float64(before) {
		t.Fatalf("liveness gauge = %v, want >= %d (a committed upstream quote must stamp)", got, before)
	}
}

func enabledGauge() float64 {
	return testutil.ToFloat64(obs.SourceEnabled.WithLabelValues(fxSource))
}

// TestRun_publishesSourceEnabledWhileRunning pins the second gauge this
// worker owns. stellarindex_source_enabled is what
// /v1/sources/{name}/health projects as `enabled`, and `massive` runs
// in the API binary rather than the indexer — so until Run published
// it, the live FX feed reported enabled=false alongside five figures of
// entries_24h, the one combination that surface treats as impossible
// ("enabled:false with entries_24h:0 means off, not failing").
//
// The primary is pointed at a dead endpoint so the immediate refresh
// fails fast without mocking massive's wire format. The gauge must read
// 1 regardless: enablement is about being switched on, not about the
// upstream answering.
//
// Shares the process-global obs gauges with the tests above, so no
// t.Parallel.
func TestRun_publishesSourceEnabledWhileRunning(t *testing.T) {
	obs.SourceEnabled.Reset()

	w := newTestWorker(t)
	// Long enough that the ticker never fires: the assertions are about
	// the immediate refresh and the park-on-ctx that follows it.
	w.interval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// The gauge is the first statement of Run, ahead of any network
	// call, so a red run costs seconds rather than a fixed ten.
	deadline := time.After(3 * time.Second)
	for enabledGauge() != 1 {
		select {
		case err := <-done:
			t.Fatalf("Run returned before the gauge was published: %v", err)
		case <-deadline:
			t.Fatalf("stellarindex_source_enabled{source=%q} = %v while the worker runs, want 1 — "+
				"/v1/sources/%s/health projects this gauge as `enabled`",
				fxSource, enabledGauge(), fxSource)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// And it must fall back to 0 on shutdown rather than latch at 1 — a
	// stopped feed reading "enabled" is the mirror-image lie.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if got := enabledGauge(); got != 0 {
		t.Errorf("stellarindex_source_enabled{source=%q} = %v after shutdown, want 0", fxSource, got)
	}
}
