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
// minimal shape as validLedgerCloseMeta in main_test.go) whose TxSet
// carries many empty phases, so MarshalBinary takes measurably longer
// than hashdb.Append's single 32-byte WriteAt. That timing skew is
// what TestRecordHashdb_DurationExcludesMarshal below relies on.
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
// A bloated LCM makes MarshalBinary take much longer than the 32-byte
// write; if recordHashdb's timer starts before the marshal, the
// observed duration tracks nearly the whole call instead of just the
// write.
func TestRecordHashdb_DurationExcludesMarshal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := hashdb.Create(path, 9000)
	if err != nil {
		t.Fatalf("hashdb.Create: %v", err)
	}
	defer func() { _ = db.Close() }()

	lcm := bloatedLedgerCloseMeta(9000, 200000)

	beforeSum := histogramSampleSum(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")

	var lastAppended atomic.Uint32
	wallStart := time.Now()
	recordHashdb(db, lcm, logger, &lastAppended)
	wallElapsed := time.Since(wallStart).Seconds()

	afterSum := histogramSampleSum(t, obs.HashdbAppendDurationSeconds, "outcome", "ok")
	observed := afterSum - beforeSum

	if observed <= 0 {
		t.Fatalf("HashdbAppendDurationSeconds{ok} did not advance")
	}
	if wallElapsed <= 0 {
		t.Fatalf("test did not measure any wall-clock time")
	}
	// The write itself is a handful of microseconds; if the marshal
	// (which this LCM was built to make large) were inside the timed
	// region, observed would track ~all of wallElapsed. Require it to
	// be a small fraction — well below half — of the whole call.
	if observed > wallElapsed/2 {
		t.Errorf("HashdbAppendDurationSeconds{ok} observed=%.6fs is %.1f%% of the whole call (wall=%.6fs) — timer includes the LedgerCloseMeta marshal, not just hashdb.Append's WriteAt",
			observed, 100*observed/wallElapsed, wallElapsed)
	}
}
