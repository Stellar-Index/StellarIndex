package chops

import (
	"context"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chIndexProof proves stellar.ops_by_source holds every sourced tx and op key of stellar.transactions and
// stellar.operations, window by window, and on a clean genesis-to-tip run records the proof watermark the account
// pages report (stellarindex_derived_index_watermark_ledger{index="ops_by_source"}). Read-only except that one row.
// About 70 s per million ledgers on r1; run under run-heavy-job.sh.
func chIndexProof(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("ch-index-proof")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 1, "first ledger (a run that does not start at genesis checks but never records)")
	to := fs.Uint("to", 0, "last ledger (0 = the contiguous lake tip; an explicit -to checks but never records)")
	window := fs.Uint("window", 1_000_000, "ledgers per proof window")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == 0 || *window == 0 {
		return fmt.Errorf("-from and -window must be > 0")
	}
	if err := gate.RequireStatedMode(); err != nil {
		return fmt.Errorf("ch-index-proof: %w", err)
	}
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "ch-index-proof: "+format+"\n", a...) }
	return runOpsBySourceProof(ctx, *chAddr, uint32(*from), uint32(*to), uint32(*window), gate.Banner(), logf)
}

// opsProofGenesisFrom is the highest -from that still counts as a genesis start; lakes begin at ledger 2.
const opsProofGenesisFrom = 2

// Seams so the record decision is testable without a live lake.
var (
	proveOpsWindow     = clickhouse.ProveOpsBySourceWindow
	opsMVsExist        = clickhouse.OpsBySourceMVsExist
	setOpsProofMark    = clickhouse.SetOpsBySourceProofWatermark
	resolveOpsProofTop = resolveBackfillTop
)

// runOpsBySourceProof checks [from, top] and stops at the first mismatching window. It records the watermark only
// when every window matched from genesis to the contiguous tip, and both feeding views existed before and after, so
// ledgers past the proof are covered as they land. write=false checks and reports without recording.
func runOpsBySourceProof(ctx context.Context, addr string, from, toFlag, window uint32, write bool, logf func(string, ...any)) error {
	mvBefore, err := opsMVsExist(ctx, addr)
	if err != nil {
		return err
	}
	top, err := resolveOpsProofTop(ctx, addr, from, toFlag)
	if err != nil {
		return err
	}
	if top < from {
		return fmt.Errorf("-to (%d) is below -from (%d)", top, from)
	}
	for lo := from; lo <= top; {
		hi := top
		if top-lo >= window {
			hi = lo + window - 1
		}
		w, err := proveOpsWindow(ctx, addr, lo, hi)
		if err != nil {
			return err
		}
		if !w.Complete() {
			return fmt.Errorf("ops_by_source misses keys in [%d,%d]: txs %d vs %d projected, ops %d vs %d projected; refill that window (deploy/clickhouse/ops_by_source.sql Step 2) and re-run from genesis",
				lo, hi, w.Txs, w.ProjTxs, w.Ops, w.ProjOps)
		}
		logf("[%d,%d] ok: %d txs, %d ops", lo, hi, w.Txs, w.Ops)
		if hi == top {
			break
		}
		lo = hi + 1
	}
	mvAfter, err := opsMVsExist(ctx, addr)
	if err != nil {
		return err
	}
	switch {
	case from > opsProofGenesisFrom:
		logf("proved [%d,%d]; no watermark: run started at %d, not genesis", from, top, from)
	case toFlag != 0:
		logf("proved [%d,%d]; no watermark: explicit -to", from, top)
	case !mvBefore || !mvAfter:
		logf("proved [%d,%d]; no watermark: a feeding view was missing during the run, so later ledgers are uncovered", from, top)
	case !write:
		logf("proved [%d,%d]; dry run, would record the %s watermark at %d", from, top, clickhouse.OpsBySourceProofName, top)
	default:
		if err := setOpsProofMark(ctx, addr, top); err != nil {
			return err
		}
		logf("proved [%d,%d]; recorded the %s watermark at %d", from, top, clickhouse.OpsBySourceProofName, top)
	}
	return nil
}
