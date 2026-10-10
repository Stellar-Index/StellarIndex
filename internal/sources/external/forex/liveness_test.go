package forex

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
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

// TestPersistSnapshot_BrokenCurrentFeedNeverRepoisonsPostHeal is the
// Massive UZS incident's SECOND act, end to end. The
// restart-heal fixed the poisoned bootstrap baseline, but the current
// feed KEPT serving the broken ~1820 bar (true level ≈ 11,800): the
// deviation arm rejected it into pending, and the next fetch of the
// same broken value pending-CONFIRMED it — two repeats of one broken
// endpoint counted as two independent witnesses — re-poisoning the
// baseline the heal cannot re-fix (a confirm clears
// bootstrapUnconfirmed, deliberately). Net: one wrong UZS row per day
// while the upstream stayed broken.
//
// With the confirm veto: the ticker's heal-grade history majority
// refutes the candidate every refresh, so the confirm is refused
// (reason "deviation_history_conflict") across arbitrarily many cycles
// — the broken value NEVER reaches fx_quotes and the baseline never
// moves. Red-proof: reverting the vetoConfirmByHistory call in
// acceptRate turns the cycle-3 assertions red (the confirm arm accepts
// 1817 and writes it).
func TestPersistSnapshot_BrokenCurrentFeedNeverRepoisonsPostHeal(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := uzsBars(today)
	const healedMedian = 11817.69

	// Cycle 1 — restart: broken bootstrap, then the history-majority heal.
	snap := snapshotWithHistory(
		map[string]float64{"UZS": 1820},
		map[string][]HistoryPoint{"UZS": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)
	if got := w.guards["UZS"].lastAccepted; got != healedMedian {
		t.Fatalf("heal precondition: lastAccepted = %v, want %v", got, healedMedian)
	}

	// Cycles 2..4 — the upstream keeps serving the broken level with its
	// live jitter. Cycle 2 re-arms pending; cycles 3 and 4 hit the
	// two-fetch confirm arm, which the history majority must veto.
	for i, rate := range []float64{1820, 1817, 1822} {
		cycle := snapshotWithHistory(
			map[string]float64{"UZS": rate},
			map[string][]HistoryPoint{"UZS": bars},
		)
		cycle.PublishedAt = today
		w.persistSnapshot(ctx, cycle)

		batch := cw.batches[len(cw.batches)-1]
		if got := currentDayRate(batch, "UZS", today); got != -1 {
			t.Errorf("cycle %d: broken current bar %v reached fx_quotes as today's row (%v) — "+
				"the pending-confirm arm re-poisoned the healed baseline", i+2, rate, got)
		}
		if got := w.guards["UZS"].lastAccepted; got != healedMedian {
			t.Errorf("cycle %d: baseline moved to %v, want %v held — history must keep "+
				"refuting the repeating broken value", i+2, got, healedMedian)
		}
	}

	// The heal must not have re-fired (MR-1: one heal per poisoning, and
	// nothing here re-poisoned).
	if w.guards["UZS"].bootstrapUnconfirmed {
		t.Error("bootstrapUnconfirmed re-set — the veto must refuse the confirm, not re-open the bootstrap")
	}
	// A recovered upstream at the true level is accepted first try.
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("recovered current rate at the true level rejected; baseline=%v",
			w.guards["UZS"].lastAccepted)
	}
}

// TestPersistSnapshot_GenuineDevaluationConfirms_HistoryFollowed pins
// the veto's release path: a REAL devaluation the upstream keeps
// reporting is still pending-confirmed once the trailing-7d majority
// follows the move (the EGP ~38%-in-a-day shape). History that AGREES
// with the candidate has no power to refuse it — the veto only fires on
// a majority that REFUTES. Red-proof: replacing the veto's
// withinBand(rate, med) release with an unconditional refusal (or
// vetoing on any median != candidate) turns the confirm assertion red.
func TestPersistSnapshot_GenuineDevaluationConfirms_HistoryFollowed(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Confirmed baseline at the old level.
	for _, r := range []float64{30.9, 30.95} {
		s := snapshotOf(map[string]float64{"EGP": r})
		s.PublishedAt = today
		w.persistSnapshot(ctx, s)
	}

	// Devaluation day: first sighting of the ~60% move is held (the
	// two-strike shape, unchanged).
	first := snapshotOf(map[string]float64{"EGP": 49.5})
	first.PublishedAt = today
	w.persistSnapshot(ctx, first)
	if got := currentDayRate(cw.batches[len(cw.batches)-1], "EGP", today); got != -1 {
		t.Fatalf("first sighting of the move persisted at %v, want held for confirmation", got)
	}

	// Next refresh: the upstream keeps reporting it AND the trailing-7d
	// history has moved to the new level — the majority now agrees with
	// the candidate, so the confirm must go through.
	moved := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 49.9},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 50.0},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 50.05},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 49.95},
	}
	second := snapshotWithHistory(
		map[string]float64{"EGP": 50.1},
		map[string][]HistoryPoint{"EGP": moved},
	)
	second.PublishedAt = today
	w.persistSnapshot(ctx, second)

	if got := currentDayRate(cw.batches[len(cw.batches)-1], "EGP", today); got != 50.1 {
		t.Errorf("confirmed devaluation persisted at %v, want 50.1 — an AGREEING history "+
			"majority must not veto; only a refuting one may", got)
	}
	if got := w.guards["EGP"].lastAccepted; got != 50.1 {
		t.Errorf("baseline = %v after a confirmed genuine devaluation, want 50.1", got)
	}
}

// TestPersistSnapshot_GenuineDevaluationConfirms_SplitWindowNoVeto pins
// the veto's OTHER release path — the mid-transition days. While the
// trailing-7d window still spans both levels (old-level bars plus
// new-level bars), there is no heal-grade mutually-agreeing majority at
// all ([historyMajority] fails the agreement test by construction), so
// the veto stands down and the two-fetch confirmation behaves exactly
// as without the veto: a real devaluation costs one refresh interval of
// lag, never a wedge. Red-proof: a naive veto that compares against the
// plain median of the full series (skipping the heal-grade agreement
// test) computes median ≈ 49.9 here — but a naive veto against, say,
// the majority-side median of a 4-of-7 split (30.8..30.9 majority in a
// [30.8 30.85 30.9 30.88 49.9 50.0 49.95] window) refuses this confirm
// and turns the assertion red.
func TestPersistSnapshot_GenuineDevaluationConfirms_SplitWindowNoVeto(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	for _, r := range []float64{30.9, 30.95} {
		s := snapshotOf(map[string]float64{"NGN": r})
		s.PublishedAt = today
		w.persistSnapshot(ctx, s)
	}

	hold := snapshotOf(map[string]float64{"NGN": 49.5})
	hold.PublishedAt = today
	w.persistSnapshot(ctx, hold)

	// Mid-transition window: four old-level bars, three new-level bars.
	// >10% apart, so there is no mutually-agreeing majority to consult.
	split := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 30.8},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 30.85},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 30.9},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 30.88},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 49.9},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 50.0},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 49.95},
	}
	confirm := snapshotWithHistory(
		map[string]float64{"NGN": 50.1},
		map[string][]HistoryPoint{"NGN": split},
	)
	confirm.PublishedAt = today
	w.persistSnapshot(ctx, confirm)

	if got := currentDayRate(cw.batches[len(cw.batches)-1], "NGN", today); got != 50.1 {
		t.Errorf("mid-transition confirm persisted at %v, want 50.1 — a split window has "+
			"no heal-grade majority, so the veto must stand down", got)
	}
	if got := w.guards["NGN"].lastAccepted; got != 50.1 {
		t.Errorf("baseline = %v, want 50.1 — the veto must not wedge a genuine move "+
			"behind a non-majority window", got)
	}
}

// TestPersistSnapshot_countsRowsAsSourceEntries is the headline: a
// committed non-empty write must advance the universal per-source entry
// counter by the number of rows written.
func TestPersistSnapshot_countsRowsAsSourceEntries(t *testing.T) {
	obs.SourceEventsTotal.Reset()

	w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies: []Currency{
			{Ticker: "EUR", RateUSD: 1.08},
			{Ticker: "BRL", RateUSD: 5.18},
			{Ticker: "JPY", RateUSD: 147.2},
		},
		History7d: map[string][]HistoryPoint{},
	}

	w.persistSnapshot(context.Background(), snap)

	got := entriesFor("massive")
	if got == 0 {
		t.Fatal("massive recorded 0 entries after a committed write — this is the " +
			"defect: the status page's entries column reads this counter, so the " +
			"active FX feed displayed as dead")
	}
	if got != 3 {
		t.Errorf("entries = %v, want 3 (one per row written) — the count must be the "+
			"number of DATA POINTS recorded, not one per batch", got)
	}
}

// TestPersistSnapshot_doesNotCountFailedOrEmptyWrites is the honesty
// half, and it matters more than the headline. A counter that advanced
// on a failed insert would make a wedged feed look productive — the
// exact failure the liveness gauge beside it is documented to avoid.
func TestPersistSnapshot_doesNotCountFailedOrEmptyWrites(t *testing.T) {
	t.Run("failed insert", func(t *testing.T) {
		obs.SourceEventsTotal.Reset()
		w := &Worker{
			writer: &fakeFXWriter{err: errors.New("db down")},
			logger: discardLogger(),
		}
		w.persistSnapshot(context.Background(), &Snapshot{
			PublishedAt: time.Now().UTC(),
			Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
			History7d:   map[string][]HistoryPoint{},
		})
		if got := entriesFor("massive"); got != 0 {
			t.Errorf("entries = %v after a FAILED insert, want 0 — counting rows we "+
				"did not persist reports a broken feed as healthy", got)
		}
	})

	t.Run("empty batch", func(t *testing.T) {
		obs.SourceEventsTotal.Reset()
		w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
		w.persistSnapshot(context.Background(), &Snapshot{
			PublishedAt: time.Now().UTC(),
			Currencies:  []Currency{},
			History7d:   map[string][]HistoryPoint{},
		})
		if got := entriesFor("massive"); got != 0 {
			t.Errorf("entries = %v for an EMPTY batch, want 0 — an upstream that "+
				"returned no usable rates has produced no data points", got)
		}
	})
}

// TestPersistSnapshot_attributesEntriesToTheProviderThatAnswered — when
// a fallback serves the round, its rows belong to the fallback, not to
// massive. Attributing them to the primary would report a feed as
// healthy while it was in fact down, which is the same class of lie as
// counting a failed write.
func TestPersistSnapshot_attributesEntriesToTheProviderThatAnswered(t *testing.T) {
	obs.SourceEventsTotal.Reset()

	w := &Worker{
		writer:       &fakeFXWriter{},
		logger:       discardLogger(),
		activeSource: "exchangeratesapi",
	}
	w.persistSnapshot(context.Background(), &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
		History7d:   map[string][]HistoryPoint{},
	})

	if got := entriesFor("exchangeratesapi"); got != 1 {
		t.Errorf("entries{exchangeratesapi} = %v, want 1 — a fallback's rows must be "+
			"attributed to the provider that actually answered", got)
	}
	if got := entriesFor("massive"); got != 0 {
		t.Errorf("entries{massive} = %v, want 0 — attributing a fallback's rows to "+
			"the primary reports a down feed as healthy", got)
	}
}

// A batch holding only the synthetic USD anchor and carried-forward
// history bars carries no upstream rate; it must not count as entries.
func TestPersistSnapshot_doesNotCountAnchorOrCarriedHistory(t *testing.T) {
	obs.SourceEventsTotal.Reset()

	w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
	now := time.Now().UTC()
	w.persistSnapshot(context.Background(), &Snapshot{
		PublishedAt: now,
		Currencies:  []Currency{{Ticker: anchorTicker, RateUSD: 1}},
		History7d: map[string][]HistoryPoint{
			"EUR": {{Date: now.AddDate(0, 0, -1), RateUSD: 1.08}},
		},
	})

	if got := entriesFor("massive"); got != 0 {
		t.Errorf("entries = %v for an anchor+history-only batch, want 0", got)
	}
}

// TestPersistSnapshot_HealedBaselineIsHealedAgainWhenHistoryCorrects
// is the mirror image of the UZS incident: the CURRENT feed is right
// and the HISTORY endpoint is the broken side. The bootstrap sample is
// refuted by seven mutually-agreeing wrong bars, so the heal re-points
// the baseline at the wrong level (that inversion is indistinguishable
// from inside the worker and is not what this test objects to). What
// must NOT happen is the wedge: once the history endpoint is fixed and
// its majority refutes the healed baseline, the baseline must be healed
// again. A heal that cleared the ticker to "confirmed" made the first
// heal final — the correct current rate stayed rejected and the confirm
// veto refused it against the same broken bars, permanently.
func TestPersistSnapshot_HealedBaselineIsHealedAgainWhenHistoryCorrects(t *testing.T) {
	w, _ := bandTestWorker(io.Discard)
	ctx := context.Background()
	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	bars := func(level float64) []HistoryPoint {
		out := make([]HistoryPoint, 0, 7)
		for i := 7; i >= 1; i-- {
			out = append(out, HistoryPoint{Date: today.Add(-time.Duration(i) * 24 * time.Hour), RateUSD: level})
		}
		return out
	}
	const trueLevel, brokenLevel = 11800.0, 1820.0

	// Day 1: correct bootstrap, broken history → inverted heal.
	snap := snapshotWithHistory(map[string]float64{"UZS": trueLevel}, map[string][]HistoryPoint{"UZS": bars(brokenLevel)})
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)
	if g := w.guards["UZS"]; g == nil || !withinBand(g.lastAccepted, brokenLevel) {
		t.Fatalf("precondition: the history majority did not heal the bootstrap baseline; guard=%+v", w.guards["UZS"])
	}

	// Day 2: history endpoint fixed; the current feed was right all along.
	snap = snapshotWithHistory(map[string]float64{"UZS": trueLevel}, map[string][]HistoryPoint{"UZS": bars(trueLevel)})
	snap.PublishedAt = today.Add(24 * time.Hour)
	w.persistSnapshot(ctx, snap)

	g := w.guards["UZS"]
	if !withinBand(g.lastAccepted, trueLevel) {
		t.Fatalf("baseline still %v after the corrected history majority (%v) refuted the healed level: "+
			"the heal is once-per-ticker, so an inverted heal wedges the ticker for good", g.lastAccepted, trueLevel)
	}
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("current rate at the true level rejected after the second heal; baseline=%v", g.lastAccepted)
	}
	if g.healedUncorroborated {
		t.Error("a current fetch agreeing with the healed baseline must clear healedUncorroborated")
	}
}

// TestPersistSnapshot_HistoryRowRejectsBadBar is the proven-red guard
// for history rows. Regression (1): trailing-7d history rows written
// with ONLY a >0/finite filter — no [maxRateDeviation] band — let a
// transiently-wrong-but-positive upstream bar for a PAST date (provider
// glitch: EUR 0.85 instead of 0.92 two days ago, but here a decimal shift
// to 9.2 to sit unambiguously outside the 50% band) overwrite the correct
// stored rate in place. fx_quotes.rate_usd is the denominator of every
// fiat-quoted usd_volume, so that one bad history bar re-scales every
// EUR-quoted trade valued off that date, with nothing to revert it.
//
// After the fix persistSnapshot bands each history point via
// acceptHistoryRate against the ticker's current accepted rate: a bar >50%
// off is dropped, an in-band bar is written.
//
// Reverting the persistSnapshot history loop to the old
// `<=0 || IsNaN || IsInf` filter turns the "bad bar dropped" assertion red.
func TestPersistSnapshot_HistoryRowRejectsBadBar(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	twoDaysAgo := today.Add(-2 * 24 * time.Hour)
	threeDaysAgo := today.Add(-3 * 24 * time.Hour)

	snap := snapshotWithHistory(
		map[string]float64{"EUR": 0.92}, // today's baseline
		map[string][]HistoryPoint{
			"EUR": {
				{Date: threeDaysAgo, RateUSD: 0.93}, // in band → written
				{Date: twoDaysAgo, RateUSD: 9.2},    // decimal-shift glitch → must be dropped
			},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	batch := cw.batches[0]

	// The good in-band history bar is written.
	if !hasHistoryRow(batch, "EUR", threeDaysAgo, 0.93) {
		t.Errorf("in-band history bar EUR@%s=0.93 was not persisted; batch=%v",
			threeDaysAgo.Format("2006-01-02"), batch)
	}
	// The decimal-shift history bar (9.2, ~10x the 0.92 baseline, far outside
	// the 50%% band) must be REJECTED — otherwise it overwrites the correct
	// stored 0.92 for that date and mis-scales every EUR-quoted usd_volume.
	if hasHistoryRow(batch, "EUR", twoDaysAgo, 9.2) {
		t.Errorf("bad history bar EUR@%s=9.2 was persisted, want it REJECTED "+
			"(baseline 0.92, band %v) — an unbanded past bar re-scales every "+
			"EUR-quoted usd_volume for that date (MR-1)",
			twoDaysAgo.Format("2006-01-02"), maxRateDeviation)
	}
}

// TestPersistSnapshot_HistoryRowNoBaselineBootstraps — with no current-rate
// baseline for the ticker (e.g. the ticker appears only in History7d), the
// history band must not refuse to bootstrap, mirroring acceptRate: dropping
// legitimate history would starve the feed.
func TestPersistSnapshot_HistoryRowNoBaselineBootstraps(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-24 * time.Hour)

	// No current rate for MXN — only a history point.
	snap := snapshotWithHistory(
		map[string]float64{},
		map[string][]HistoryPoint{
			"MXN": {{Date: yesterday, RateUSD: 18.5}},
		},
	)
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	if !hasHistoryRow(cw.batches[0], "MXN", yesterday, 18.5) {
		t.Errorf("first-sighting history bar MXN@%s=18.5 was dropped, want it "+
			"bootstrapped — no baseline must accept, not block",
			yesterday.Format("2006-01-02"))
	}
}

// TestPersistSnapshot_BootstrapPoisonHealedByHistoryMajority is the
// Massive UZS incident, end to end. At process restart the
// current feed served a broken 1820 (true level ≈ 11,800); the bootstrap
// arm accepted it sight-unseen, and the guard then rejected the ticker's
// entire CORRECT trailing-7d series against the poisoned baseline —
// today's wrong row reached fx_quotes while seven right rows were
// refused. With the heal: the agreeing history majority refutes the
// unconfirmed bootstrap, the baseline re-points at the bars' median, the
// bars are written, and the poisoned current-day row never reaches the
// writer.
func TestPersistSnapshot_BootstrapPoisonHealedByHistoryMajority(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 11860.49},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 11847},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 11791.69},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11785},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11817.69},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11817},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11838.98},
	}
	snap := snapshotWithHistory(
		map[string]float64{"UZS": 1820}, // the broken bootstrap sample
		map[string][]HistoryPoint{"UZS": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	if len(cw.batches) != 1 {
		t.Fatalf("expected 1 persisted batch, got %d", len(cw.batches))
	}
	batch := cw.batches[0]

	// The poisoned current-day row must be scrubbed.
	if hasHistoryRow(batch, "UZS", today, 1820) {
		t.Errorf("poisoned bootstrap row UZS@today=1820 reached the writer; " +
			"the ticker's own agreeing 7-day history refutes it")
	}
	// Every correct history bar must be written.
	for _, p := range bars {
		if !hasHistoryRow(batch, "UZS", p.Date, p.RateUSD) {
			t.Errorf("correct history bar UZS@%s=%v was rejected against the "+
				"poisoned baseline — evidence pointing the wrong way",
				p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
	// The baseline is healed: a follow-up current rate at the true level
	// must be accepted first try.
	if !w.acceptRate("UZS", 11795) {
		t.Errorf("post-heal current rate at the true level was rejected; "+
			"baseline=%v", w.guards["UZS"].lastAccepted)
	}
}

// TestPersistSnapshot_ConfirmedBaselineIsNeverHealed pins the heal's
// deliberate limit: a baseline corroborated by two agreeing current
// fetches is NOT flipped by an agreeing history majority — a systemically
// broken history endpoint against a healthy current endpoint is exactly
// the MR-1 poisoning, and from inside the worker the two cases are
// indistinguishable, so the confirmed baseline wins and the bars stay
// rejected.
func TestPersistSnapshot_ConfirmedBaselineIsNeverHealed(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)

	// Two agreeing snapshots confirm the (correct) baseline.
	first := snapshotOf(map[string]float64{"EUR": 0.92})
	first.PublishedAt = today
	w.persistSnapshot(ctx, first)

	// Second refresh: same current rate (confirms), but the history
	// endpoint now serves a decimal-shifted series (systemic glitch).
	bars := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 9.3},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 9.2},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 9.25},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 9.18},
	}
	second := snapshotWithHistory(
		map[string]float64{"EUR": 0.92},
		map[string][]HistoryPoint{"EUR": bars},
	)
	second.PublishedAt = today
	w.persistSnapshot(ctx, second)

	batch := cw.batches[len(cw.batches)-1]
	for _, p := range bars {
		if hasHistoryRow(batch, "EUR", p.Date, p.RateUSD) {
			t.Errorf("decimal-shifted history bar EUR@%s=%v was written — the "+
				"heal flipped a CONFIRMED baseline (MR-1 in a new coat)",
				p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
	if got := w.guards["EUR"].lastAccepted; got != 0.92 {
		t.Errorf("confirmed baseline moved to %v, want 0.92 held", got)
	}
}

// TestPersistSnapshot_RedenominationSplitSeriesDoesNotHeal — a genuine
// mid-week redenomination splits the trailing series across two levels;
// the mutual-agreement test must refuse to call either side a "majority",
// leaving genuine moves to the two-fetch pending confirmation.
func TestPersistSnapshot_RedenominationSplitSeriesDoesNotHeal(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 1800}, // old level
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 1810},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11800}, // redenominated
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11810},
	}
	snap := snapshotWithHistory(
		map[string]float64{"XXX": 1805},
		map[string][]HistoryPoint{"XXX": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	// Split series: no heal. Baseline stays at the bootstrap sample and
	// the current-day row is written.
	if got := w.guards["XXX"].lastAccepted; got != 1805 {
		t.Errorf("split (non-agreeing) series healed the baseline to %v, want 1805", got)
	}
	if !hasHistoryRow(cw.batches[0], "XXX", today, 1805) {
		t.Errorf("current-day row was scrubbed without a heal")
	}
}

// TestPersistSnapshot_SplitRejectedSeriesFailsAgreement pins the
// mutual-agreement band itself (the redenomination
// test above actually exercises the min-bars floor — its in-band old-level
// bars are accepted, so only 2 bars reach the rejected set). Here the
// bootstrap sample is far from BOTH levels, every bar lands in the
// rejected set, and that set spans two levels >10% apart: heal-grade
// count (7 ≥ 4) but no mutual agreement → no heal, baseline held.
// Red-proof: widening historyHealAgreement (or removing the agreement
// loop in historyMajority) turns this test's baseline assertion red.
func TestPersistSnapshot_SplitRejectedSeriesFailsAgreement(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	today := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	bars := []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 1800}, // old level
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 1810},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 1805},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11800}, // new level
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11810},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11790},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11805},
	}
	snap := snapshotWithHistory(
		map[string]float64{"YYY": 500}, // bootstrap far from both levels
		map[string][]HistoryPoint{"YYY": bars},
	)
	snap.PublishedAt = today
	w.persistSnapshot(ctx, snap)

	// No heal: a two-level rejected series is not a majority, whatever
	// its size — the baseline (however dubious) holds for the pending
	// confirmation to sort out.
	if got := w.guards["YYY"].lastAccepted; got != 500 {
		t.Errorf("split rejected series healed the baseline to %v, want 500 held — "+
			"the mutual-agreement band is the only guard in this corner", got)
	}
	for _, p := range bars {
		if hasHistoryRow(cw.batches[0], "YYY", p.Date, p.RateUSD) {
			t.Errorf("bar %s=%v written without a heal", p.Date.Format("2006-01-02"), p.RateUSD)
		}
	}
}

// TestPersistSnapshot_RejectsOneBadUpstreamBar pins the fx
// sanity band. fx_quotes is the denominator of every fiat-quoted
// usd_volume the X2.5 triangulation derives, so a single mis-scaled
// upstream bar — the classic case is a decimal shift, here EUR jumping
// from 0.92 to 9.2 — silently re-scales that currency's entire
// conversion if persistSnapshot writes whatever the upstream says, with
// no comparison against the last stored rate for the ticker.
func TestPersistSnapshot_RejectsOneBadUpstreamBar(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	// Refresh 1 — establishes the baseline.
	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EUR": 0.92, "JPY": 157.0}))
	// Refresh 2 — EUR arrives 10x too large (a decimal shift upstream).
	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EUR": 9.2, "JPY": 157.4}))

	if len(cw.batches) != 2 {
		t.Fatalf("expected 2 persisted batches, got %d", len(cw.batches))
	}
	if got := rateFor(cw.batches[0], "EUR"); got != 0.92 {
		t.Fatalf("baseline refresh must persist EUR 0.92, got %v", got)
	}
	if got := rateFor(cw.batches[1], "EUR"); got != -1 {
		t.Errorf("EUR persisted at %v on the bad bar, want it REJECTED (last accepted 0.92, band %v) — "+
			"one bad bar must not re-scale every EUR-quoted usd_volume", got, maxRateDeviation)
	}
	// The band is per-ticker: a healthy JPY move in the same batch is unaffected.
	if got := rateFor(cw.batches[1], "JPY"); got != 157.4 {
		t.Errorf("JPY persisted at %v, want 157.4 — an in-band ticker must not be collateral damage", got)
	}
}

// TestPersistSnapshot_ConfirmedDevaluationIsAccepted is the other half of
// the band decision: the band must not WEDGE a currency. A real
// devaluation (EGP ~38% in a day, NGN ~40%) the upstream keeps reporting
// is accepted on the next refresh, so the worst case is one refresh
// interval of lag rather than an indefinitely stale rate.
func TestPersistSnapshot_ConfirmedDevaluationIsAccepted(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EGP": 30.9}))
	// A 60% devaluation — outside the band, so held on first sighting.
	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EGP": 49.5}))
	// The upstream keeps reporting it: two independent fetches agree.
	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EGP": 50.1}))

	if got := rateFor(cw.batches[1], "EGP"); got != -1 {
		t.Errorf("EGP persisted at %v on first sighting of a 60%% move, want it held for confirmation", got)
	}
	if got := rateFor(cw.batches[2], "EGP"); got != 50.1 {
		t.Errorf("EGP persisted at %v after a CONFIRMING second fetch, want 50.1 — "+
			"a real devaluation must not wedge the ticker on a stale rate forever", got)
	}
}

// TestPersistSnapshot_RejectsNonFiniteAndNonPositive keeps the arithmetic
// guard honest: 1/rate feeds inverse_usd, so a zero, negative, NaN or Inf
// rate would poison the stored row in both directions.
func TestPersistSnapshot_RejectsNonFiniteAndNonPositive(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	ctx := context.Background()

	w.persistSnapshot(ctx, snapshotOf(map[string]float64{"EUR": 0.92}))
	w.persistSnapshot(ctx, snapshotOf(map[string]float64{
		"AAA": math.NaN(),
		"BBB": math.Inf(1),
		"CCC": 0,
		"DDD": -1.5,
		"EUR": 0.93,
	}))

	for _, ticker := range []string{"AAA", "BBB", "CCC", "DDD"} {
		if got := rateFor(cw.batches[1], ticker); got != -1 {
			t.Errorf("%s persisted at %v, want REJECTED (non-finite / non-positive)", ticker, got)
		}
	}
	if got := rateFor(cw.batches[1], "EUR"); got != 0.93 {
		t.Errorf("EUR persisted at %v, want 0.93", got)
	}
}

// TestPersistSnapshot_FirstSightingBootstraps — with no baseline there is
// nothing to compare against, so the band must not refuse to bootstrap
// (that would leave fx_quotes permanently empty after a restart).
func TestPersistSnapshot_FirstSightingBootstraps(t *testing.T) {
	w, cw := bandTestWorker(io.Discard)
	w.persistSnapshot(context.Background(), snapshotOf(map[string]float64{"KRW": 1380.0}))
	if got := rateFor(cw.batches[0], "KRW"); got != 1380.0 {
		t.Errorf("first sighting persisted %v, want 1380 — the band must bootstrap, not block", got)
	}
}

func entriesFor(source string) float64 {
	return testutil.ToFloat64(obs.SourceEventsTotal.WithLabelValues(source))
}

// uzsBars is the trailing-7d series from the Massive UZS
// incident: seven mutually-agreeing bars around the true ~11,800 level
// (median 11817.69).
func uzsBars(today time.Time) []HistoryPoint {
	return []HistoryPoint{
		{Date: today.Add(-7 * 24 * time.Hour), RateUSD: 11860.49},
		{Date: today.Add(-6 * 24 * time.Hour), RateUSD: 11847},
		{Date: today.Add(-5 * 24 * time.Hour), RateUSD: 11791.69},
		{Date: today.Add(-4 * 24 * time.Hour), RateUSD: 11785},
		{Date: today.Add(-3 * 24 * time.Hour), RateUSD: 11817.69},
		{Date: today.Add(-2 * 24 * time.Hour), RateUSD: 11817},
		{Date: today.Add(-1 * 24 * time.Hour), RateUSD: 11838.98},
	}
}

// currentDayRate returns the persisted RateUSD for ticker's CURRENT-day
// row in the batch (bucket == day), or -1 when absent. Distinct from
// rateFor, which matches any row for the ticker including history rows.
func currentDayRate(batch []FXQuote, ticker string, day time.Time) float64 {
	want := day.UTC().Truncate(24 * time.Hour)
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) {
			return q.RateUSD
		}
	}
	return -1
}

func bandTestWorker(w io.Writer) (*Worker, *captureWriter) {
	cw := &captureWriter{}
	return &Worker{
		logger: slog.New(slog.NewTextHandler(w, nil)),
		writer: cw,
		guards: map[string]*rateGuard{},
	}, cw
}

// hasHistoryRow reports whether batch carries a row for ticker at the given
// date (truncated to the day, as persistSnapshot stores it) with rate.
func hasHistoryRow(batch []FXQuote, ticker string, date time.Time, rate float64) bool {
	want := date.UTC().Truncate(24 * time.Hour)
	for _, q := range batch {
		if q.Ticker == ticker && q.Bucket.Equal(want) && q.RateUSD == rate {
			return true
		}
	}
	return false
}

// captureWriter records every batch persistSnapshot commits, so a test can
// assert on the exact rows that would reach fx_quotes.
type captureWriter struct {
	batches [][]FXQuote
}

func (c *captureWriter) InsertFXQuoteBatch(_ context.Context, quotes []FXQuote) error {
	cp := make([]FXQuote, len(quotes))
	copy(cp, quotes)
	c.batches = append(c.batches, cp)
	return nil
}

func snapshotOf(rates map[string]float64) *Snapshot {
	s := &Snapshot{PublishedAt: time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC)}
	for ticker, r := range rates {
		s.Currencies = append(s.Currencies, Currency{Ticker: ticker, RateUSD: r})
	}
	return s
}

// rateFor returns the persisted RateUSD for ticker in the given batch, or
// -1 when the ticker is absent (i.e. was rejected).
func rateFor(batch []FXQuote, ticker string) float64 {
	for _, q := range batch {
		if q.Ticker == ticker {
			return q.RateUSD
		}
	}
	return -1
}
