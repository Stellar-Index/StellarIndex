package forex

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"
)

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

func bandTestWorker(w io.Writer) (*Worker, *captureWriter) {
	cw := &captureWriter{}
	return &Worker{
		logger: slog.New(slog.NewTextHandler(w, nil)),
		writer: cw,
		guards: map[string]*rateGuard{},
	}, cw
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

// TestPersistSnapshot_RejectsOneBadUpstreamBar pins C2-030
// (audit-2026-07-23). fx_quotes is the denominator of every fiat-quoted
// usd_volume the X2.5 triangulation derives, so a single mis-scaled
// upstream bar — the classic case is a decimal shift, here EUR jumping
// from 0.92 to 9.2 — silently re-scales that currency's entire
// conversion. persistSnapshot wrote whatever the upstream said, with no
// comparison against the last stored rate for the ticker.
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
// the C2-030 decision: the band must not WEDGE a currency. A real
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
// guard honest: 1/rate feeds InverseUSD, so a zero, negative, NaN or Inf
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
