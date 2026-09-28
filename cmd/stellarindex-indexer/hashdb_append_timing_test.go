package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prom_dto "github.com/prometheus/client_model/go"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/hashdb"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// bloatedLedgerCloseMeta returns a valid, XDR-ENCODABLE LCM (same
// minimal shape as validLedgerCloseMeta in main_test.go). Used below to
// exercise recordHashdb with a realistic (if not literally huge) LCM;
// the marshal's actual cost no longer matters to the test — see
// TestRecordHashdb_DurationExcludesMarshal.
func bloatedLedgerCloseMeta(seq uint32, phases int) sdkxdr.LedgerCloseMeta {
	components := []sdkxdr.TxSetComponent{}
	ps := make([]sdkxdr.TransactionPhase, phases)
	for i := range ps {
		ps[i] = sdkxdr.TransactionPhase{V: 0, V0Components: &components}
	}
	return sdkxdr.LedgerCloseMeta{
		V: 1,
		V1: &sdkxdr.LedgerCloseMetaV1{
			LedgerHeader: sdkxdr.LedgerHeaderHistoryEntry{
				Header: sdkxdr.LedgerHeader{LedgerSeq: sdkxdr.Uint32(seq)},
			},
			TxSet: sdkxdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &sdkxdr.TransactionSetV1{Phases: ps},
			},
		},
	}
}

// histogramSampleSum is the sibling of obstest.HistogramSampleCount,
// needed here to compare the recorded DURATION (not merely that a
// sample landed) against a wall-clock baseline.
func histogramSampleSum(t *testing.T, vec *prometheus.HistogramVec, labelKey, labelValue string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var total float64
	for m := range ch {
		var dto io_prom_dto.Metric
		if err := m.Write(&dto); err != nil {
			t.Fatalf("histogramSampleSum: Write: %v", err)
		}
		for _, l := range dto.GetLabel() {
			if l.GetName() == labelKey && l.GetValue() == labelValue {
				total += dto.GetHistogram().GetSampleSum()
			}
		}
	}
	return total
}

// TestRecordHashdb_DurationExcludesMarshal is the regression guard for
// T138: HashdbAppendDurationSeconds documents itself (internal/obs/
// metrics.go) as the latency of hashdb.Append's single O(1) WriteAt —
// no seek, no fsync, no network I/O — not of marshaling the ledger.
//
// Structural, not wall-clock-ratio: a prior version built an
// implausibly large LCM and compared the OBSERVED metric against
// time.Since(wallStart)/2 for the whole call. Under load a single
// scheduler/fsync stall inside the (cheap) WriteAt was enough to push
// that ratio past 50% with the marshal untouched — flaked twice on
// 2026-09-28 in CI (#1539) despite passing 5/5 in isolation. Comparing
// two real wall-clock measurements of a fast operation is inherently
// noisy on a loaded machine.
//
// Here the test controls the marshal's cost directly via
// marshalLedgerCloseMeta (a slow hook injecting a fixed, large sleep)
// instead of relying on LCM size to make marshaling slow, and never
// measures wall-clock time at all: it asserts the OBSERVED metric
// stays far below the injected delay. If recordHashdb's timer started
// before the marshal, observed would include the injected sleep and
// blow the absolute ceiling by orders of magnitude — no ratio, no
// load-sensitive baseline.
func TestRecordHashdb_DurationExcludesMarshal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 9000)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	const injectedMarshalDelay = 200 * time.Millisecond
	orig := marshalLedgerCloseMeta
	t.Cleanup(func() { marshalLedgerCloseMeta = orig })
	marshalLedgerCloseMeta = func(lcm sdkxdr.LedgerCloseMeta) ([]byte, error) {
		time.Sleep(injectedMarshalDelay)
		return lcm.MarshalBinary()
	}

	lcm := bloatedLedgerCloseMeta(9000, 200000)

	beforeSum := histogramSampleSum(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")

	var lastAppended atomic.Uint32
	recordHashdb(db, lcm, logger, &lastAppended)

	afterSum := histogramSampleSum(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")
	observed := afterSum - beforeSum

	if observed <= 0 {
		t.Fatalf("HashdbAppendDurationSeconds{ok} did not advance")
	}
	// The write itself is a handful of microseconds even on a loaded CI
	// runner. Require observed to stay a small fraction of the delay we
	// deliberately injected into the marshal — comfortable headroom
	// above realistic WriteAt jitter, nowhere near the injected 200ms a
	// timer-placement regression would leak in.
	const maxWantedObserved = 20 * time.Millisecond
	if observed > maxWantedObserved.Seconds() {
		t.Errorf("HashdbAppendDurationSeconds{ok} observed=%.6fs exceeds %.3fs — timer includes the LedgerCloseMeta marshal (which this test made take >= %.3fs), not just hashdb.Append's WriteAt",
			observed, maxWantedObserved.Seconds(), injectedMarshalDelay.Seconds())
	}
}
