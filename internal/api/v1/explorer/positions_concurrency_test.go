package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// slowPositionsReader answers every fold after `delay`, counting how
// many folds are in flight at once. Two folds fail so the coverage
// merge is exercised too.
type slowPositionsReader struct {
	delay      time.Duration
	inFlight   atomic.Int32
	maxFlight  atomic.Int32
	blendFails bool
}

func (r *slowPositionsReader) enter() {
	n := r.inFlight.Add(1)
	for {
		m := r.maxFlight.Load()
		if n <= m || r.maxFlight.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(r.delay)
	r.inFlight.Add(-1)
}

func (r *slowPositionsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	r.enter()
	if r.blendFails {
		return nil, errors.New("blend down")
	}
	return nil, nil
}

func (r *slowPositionsReader) BlendBackstopSharesByUser(context.Context, string) ([]timescale.BlendBackstopFold, error) {
	r.enter()
	return nil, errors.New("backstop down")
}

func (r *slowPositionsReader) PhoenixStakeByUser(context.Context, string) ([]timescale.PhoenixStakeFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) CreditPositionsByOwner(context.Context, string) ([]timescale.CreditPositionFold, error) {
	r.enter()
	return nil, nil
}

func (r *slowPositionsReader) AquariusGaugeByUser(context.Context, string) ([]timescale.AquariusGaugeFold, error) {
	r.enter()
	return nil, errors.New("aquarius down")
}

// TestAccountPositions_FoldsRunConcurrently pins the 2026-08-13
// sub-second fix: the six protocol folds are independent reads and
// must run CONCURRENTLY (endpoint latency = max, not sum — it was the
// last warm breach at 1.99s). Serial execution would show max
// in-flight of 1 and take 6x the delay.
func TestAccountPositions_FoldsRunConcurrently(t *testing.T) {
	const delay = 60 * time.Millisecond
	reader := &slowPositionsReader{delay: delay}
	h := &Handler{Positions: reader, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	start := time.Now()
	var cov positionsCoverage
	_ = h.collectPositions(context.Background(), "GTEST", func(string) string { return "" }, &cov)
	elapsed := time.Since(start)

	if got := reader.maxFlight.Load(); got < 2 {
		t.Fatalf("max concurrent folds = %d, want >= 2 — the folds ran serially, which is the 1.99s breach", got)
	}
	// Serial would be >= 6*delay; concurrent should land near one delay.
	if elapsed > 4*delay {
		t.Errorf("elapsed %v with %v per fold — looks serial, want ~1 fold's latency", elapsed, delay)
	}
}

// TestAccountPositions_CoverageOrderIsDeterministic pins that
// parallelising did not make the degraded-protocol list order
// (rendered into the wire coverage_note) depend on goroutine
// scheduling: it must stay in the fixed fold order, run after run.
func TestAccountPositions_CoverageOrderIsDeterministic(t *testing.T) {
	var first string
	for i := 0; i < 20; i++ {
		reader := &slowPositionsReader{blendFails: true}
		h := &Handler{Positions: reader, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		var cov positionsCoverage
		_ = h.collectPositions(context.Background(), "GTEST", func(string) string { return "" }, &cov)
		note := cov.note()
		if note == "" {
			t.Fatal("expected a degraded coverage note (three folds fail in this fixture)")
		}
		if i == 0 {
			first = note
			continue
		}
		if note != first {
			t.Fatalf("coverage note is scheduling-dependent: run %d = %q, run 0 = %q", i, note, first)
		}
	}
}
