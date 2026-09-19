package timescale

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// bulkPanicTrades builds n distinct on-chain DEX trades whose quote leg has no
// USD peg, so tradeUSDVolume falls through tiers 1/2/2b and consults the FX
// resolver — the per-row call that can fail unpredictably, and so the place a
// panic inside the fan-out actually comes from.
func bulkPanicTrades(t *testing.T, n int) []canonical.Trade {
	t.Helper()
	xlm := canonical.NativeAsset()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset AQUA: %v", err)
	}
	rows := make([]canonical.Trade, 0, n)
	for i := range n {
		tr := mkClassicDEXTrade(t, "soroswap", aqua, xlm, 50_000_000_000)
		tr.Ledger = uint32(i + 1)
		rows = append(rows, tr)
	}
	return rows
}

// K012 — the bulk backfill writer's usd_volume fan-out is a bounded pool of
// detached goroutines joined by a WaitGroup. JOINED IS NOT PROTECTED: an
// unrecovered panic in any of them terminates the whole process and the
// WaitGroup does nothing about it. Without the guard this test does not fail,
// it CRASHES the test binary.
//
// Containing the panic alone would be strictly worse on this path, which is
// why the assertion is about the RETURNED VALUE and not about survival. Rows a
// dead worker never reached keep out[i]'s zero value — a NULL usd_volume — and
// errAt[i] stays nil, so BulkBackfillTrades would COPY a silently under-valued
// batch into `trades` and report it landed. A panicked resolve must fail the
// whole call and hand back no volumes at all.
func TestResolveBulkUSDVolumes_PanickingResolverFailsTheCallNotTheProcess(t *testing.T) {
	// The literal, not the constant: this label is the alert's contract, so a
	// rename of bulkResolverWorkerName must fail here rather than silently move
	// the series the page rule reads.
	const workerName = "timescale-bulk-backfill-usd-volume-resolver"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	s := &Store{usdVolumeFXResolver: panicFXResolver{}}
	rows := bulkPanicTrades(t, 64)

	type outcome struct {
		out []sql.NullString
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := s.resolveBulkUSDVolumes(context.Background(), rows, 4)
		done <- outcome{out, err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolveBulkUSDVolumes never returned: a worker that contained its panic " +
			"stopped receiving, stranding the index feeder on a send nobody will take")
	}

	if got.err == nil {
		t.Fatalf("a fan-out in which every resolver panicked returned err=nil with %d "+
			"volume slot(s): the caller would COPY those rows with NULL usd_volume and "+
			"tally them as landed", len(got.out))
	}
	if !strings.Contains(got.err.Error(), "panicked") {
		t.Errorf("err = %v, want the contained panic surfaced as the call's failure", got.err)
	}
	if got.out != nil {
		t.Errorf("out is non-nil (%d slots) — a partially-resolved volume set must never "+
			"reach the writer", len(got.out))
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after <= before {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} did not move (%v → %v) — a "+
			"recover that does not move the counter turns a loud crash into a silent "+
			"under-valued backfill", workerName, before, after)
	}
}

// K012 — the COPY fan-out is the other WaitGroup-joined pool, and its release
// is errs[i]. copyTradePartitions returns the index ranges that COMMITTED so
// the caller can keep source_entry_counts exact, and it decides "committed"
// from errs[i] == nil. So a panic that is merely CONTAINED reports the
// partition as LANDED with zero rows copied, and the caller credits
// source_entry_counts for rows that were never written — a silent tally
// corruption on the SDEX trade writer. The panic must therefore become that
// partition's error.
//
// The panic is the real one a Store with no pool produces: copyTradeRange's
// first statement is s.db.Conn(ctx).
func TestCopyTradePartitions_PanickingPartitionIsNotReportedAsLanded(t *testing.T) {
	const workerName = "timescale-bulk-backfill-copy-partition" // literal: see above
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	s := &Store{} // no *sql.DB — copyTradeRange's s.db.Conn dereferences nil
	rows := bulkPanicTrades(t, 16)
	usd := make([]sql.NullString, len(rows))

	landed, copied, err := s.copyTradePartitions(context.Background(), rows, usd, BulkBackfillOptions{})

	if err == nil {
		t.Fatalf("a partition that panicked returned err=nil (landed=%v copied=%d)", landed, copied)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("err = %v, want the contained panic surfaced as that partition's failure", err)
	}
	if len(landed) != 0 {
		t.Errorf("landed = %v, want empty — a panicked partition copied nothing, so "+
			"crediting source_entry_counts for its rows would corrupt the tally", landed)
	}
	if copied != 0 {
		t.Errorf("copied = %d, want 0", copied)
	}

	if after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName)); after <= before {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} did not move (%v → %v)",
			workerName, before, after)
	}
}

// TestResolveBulkUSDVolumes_HealthyFanOutUnchanged is the companion the
// destructive case needs: adding the guard must not change what an ordinary
// fan-out returns. Same rows, a resolver that answers, exact expected volumes
// on every row — not merely "no error".
func TestResolveBulkUSDVolumes_HealthyFanOutUnchanged(t *testing.T) {
	s := &Store{usdVolumeFXResolver: stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.10",
	}}}

	rows := bulkPanicTrades(t, 32)
	out, err := s.resolveBulkUSDVolumes(context.Background(), rows, 4)
	if err != nil {
		t.Fatalf("resolveBulkUSDVolumes: %v", err)
	}
	if len(out) != len(rows) {
		t.Fatalf("len(out) = %d, want %d", len(out), len(rows))
	}
	// Quote leg is XLM at $0.10: 50_000_000_000 stroops / 1e7 = 5_000 XLM = $500.
	for i := range out {
		if !out[i].Valid || out[i].String != "500.00000000" {
			t.Fatalf("out[%d] = %+v, want {500.00000000 true}", i, out[i])
		}
	}
}
