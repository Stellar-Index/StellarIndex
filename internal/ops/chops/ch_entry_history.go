package chops

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ch-entry-history derives the account- and asset-keyed entry-change history from one decode pass of
// stellar.ledger_entry_changes, windowed and resumable via stellar.entry_history_watermark; without -write it only reports sizes.
func chEntryHistory(args []string) error {
	fs := flag.NewFlagSet("ch-entry-history", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or -floor-ledger on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always clamped down to the contiguous tip.")
	window := fs.Uint("window", 10_000, "ledgers per derive window")
	floorLedger := fs.Uint("floor-ledger", 1, "first-run start when no watermark exists; clamped up to the lake's first ledger")
	maxDecodeErrs := registerDecodeBudget(fs)
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 {
		return fmt.Errorf("-window must be > 0")
	}
	if *floorLedger == 0 {
		return fmt.Errorf("-floor-ledger must be > 0 (the genesis ledger is 1)")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	start, last, err := resolveDeriveRange(ctx, *chAddr, "ch-entry-history", uint32(*from), uint32(*to), uint32(*floorLedger), clickhouse.EntryHistoryWatermark) //nolint:gosec // ledger sequences fit uint32
	if err != nil {
		return err
	}
	if last < start {
		fmt.Fprintf(os.Stderr, "ch-entry-history: nothing to derive (start %d, contiguous tip %d)\n", start, last)
		return nil
	}
	st := newEntryHistoryStats()
	if err := runEntryHistory(ctx, *chAddr, start, last, uint32(*window), dryRun, st); err != nil { //nolint:gosec // window fits uint32
		return err
	}
	st.report(start, last)
	return enforceDecodeBudget("ch-entry-history", st.skipped, *maxDecodeErrs)
}

// runEntryHistory derives [start, last] window by window, advancing the
// watermark after each written window.
func runEntryHistory(ctx context.Context, chAddr string, start, last, window uint32, dryRun bool, st *entryHistoryStats) error {
	runStart := time.Now()
	for lo := start; ; {
		hi := last
		if last-lo >= window {
			hi = lo + window - 1
		}
		n, txLedgers, err := deriveEntryHistoryWindow(ctx, chAddr, lo, hi, dryRun, st)
		if err != nil {
			return fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		if !dryRun {
			if err := setEntryHistoryWatermark(ctx, chAddr, lo, hi, txLedgers); err != nil {
				return fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
		}
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d] done — %d rows (elapsed %s)\n",
			lo, hi, n, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// Seams so a window's derive is testable without a live lake.
var (
	streamEntryHistorySource  = clickhouse.StreamEntryHistorySource
	insertEntryHistory        = clickhouse.InsertEntryHistory
	setEntryHistoryWatermark  = clickhouse.SetEntryHistoryWatermark
	entryHistoryFlushRowCount = 50_000
)

// deriveEntryHistoryWindow projects one window, returning rows written (or that would be) and the distinct ledgers it
// read transaction-scoped changes for, the watermark's coverage proof.
func deriveEntryHistoryWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool, st *entryHistoryStats) (int64, uint64, error) {
	p := entryhistory.NewProjector()
	var (
		accounts     []clickhouse.AccountEntryChange
		assets       []clickhouse.AssetEntryChange
		sponsorships []clickhouse.SponsorshipEvent
		written      int64
		skipped      uint64
		txLedgers    uint64
		lastTxLedger uint32
	)
	flush := func() error {
		if !dryRun {
			if _, err := insertEntryHistory(ctx, addr, accounts, assets, sponsorships); err != nil {
				return err
			}
		}
		written += int64(len(accounts) + len(assets) + len(sponsorships))
		accounts, assets, sponsorships = accounts[:0], assets[:0], sponsorships[:0]
		return nil
	}
	err := streamEntryHistorySource(ctx, addr, lo, hi, func(r clickhouse.EntryHistorySourceRow) error {
		// The stream is ledger-ordered, so a ledger's tx-scoped rows are adjacent.
		if r.TxHash != "" && r.Ledger != lastTxLedger {
			txLedgers++
			lastTxLedger = r.Ledger
		}
		pr, err := p.ProjectAll(r)
		if err != nil {
			skipped++
			if skipped <= 3 {
				fmt.Fprintf(os.Stderr, "ch-entry-history: skip ledger %d tx %s op %d change %d: %v\n", r.Ledger, r.TxHash, r.OpIndex, r.ChangeIndex, err)
			}
			return nil
		}
		st.record(pr)
		accounts = append(accounts, pr.Accounts...)
		assets = append(assets, pr.Assets...)
		sponsorships = append(sponsorships, pr.Sponsorships...)
		if len(accounts)+len(assets)+len(sponsorships) >= entryHistoryFlushRowCount {
			return flush()
		}
		return nil
	})
	if err != nil {
		return written, txLedgers, err
	}
	if err := flush(); err != nil {
		return written, txLedgers, err
	}
	st.preImageMissing += p.PreImageMissing
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "ch-entry-history: window [%d,%d]: %d changes skipped (decode)\n", lo, hi, skipped)
	}
	st.skipped += skipped
	return written, txLedgers, nil
}

// feeSeqFields are the account fields a transaction's fee charge, refund and
// sequence bump touch.
var feeSeqFields = map[string]bool{"balance": true, "seq_num": true, "seq_ledger": true, "seq_time": true}

// entryHistoryStats is the sizing measurement: rows per table and entry type, decoded-field JSON bytes, and tx-level
// fee/sequence-only account updates (the candidate to leave out of the backfill).
type entryHistoryStats struct {
	accountRows, assetRows, fieldBytes map[string]int64
	sponsorships                       map[string]int64
	feeSeqOnly                         int64
	skipped, preImageMissing           uint64
}

func newEntryHistoryStats() *entryHistoryStats {
	return &entryHistoryStats{accountRows: map[string]int64{}, assetRows: map[string]int64{}, fieldBytes: map[string]int64{}, sponsorships: map[string]int64{}}
}

func (s *entryHistoryStats) record(pr entryhistory.Projection) {
	acc, ast := pr.Accounts, pr.Assets
	for _, e := range pr.Sponsorships {
		s.sponsorships[e.Event]++
	}
	for _, a := range acc {
		s.accountRows[a.EntryType]++
		s.fieldBytes[a.EntryType] += int64(len(a.Fields))
		if a.EntryType == "account" && a.Role == "owner" && a.OpIndex == -1 && isFeeSeqOnly(a.Changed) {
			s.feeSeqOnly++
		}
	}
	for _, a := range ast {
		s.assetRows[a.EntryType]++
		s.fieldBytes[a.EntryType] += int64(len(a.Fields))
	}
}

func isFeeSeqOnly(changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	for _, c := range changed {
		if !feeSeqFields[c] {
			return false
		}
	}
	return true
}

func (s *entryHistoryStats) report(from, to uint32) {
	types := map[string]bool{}
	for t := range s.accountRows {
		types[t] = true
	}
	for t := range s.assetRows {
		types[t] = true
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	var totalAcc, totalAst, totalBytes int64
	fmt.Fprintf(os.Stderr, "ch-entry-history: measured [%d,%d] (pre-merge counts; fields_bytes = uncompressed decoded-field JSON)\n", from, to)
	for _, t := range names {
		fmt.Fprintf(os.Stderr, "  entry_type=%-17s account_rows=%d asset_rows=%d fields_bytes=%d\n", t, s.accountRows[t], s.assetRows[t], s.fieldBytes[t])
		totalAcc += s.accountRows[t]
		totalAst += s.assetRows[t]
		totalBytes += s.fieldBytes[t]
	}
	fmt.Fprintf(os.Stderr, "  total account_rows=%d asset_rows=%d fields_bytes=%d\n", totalAcc, totalAst, totalBytes)
	fmt.Fprintf(os.Stderr, "  fee/seq-only account rows (tx-level, only balance/seq_* changed)=%d of %d account-entry rows\n",
		s.feeSeqOnly, s.accountRows["account"])
	fmt.Fprintf(os.Stderr, "  sponsorship events started=%d revoked=%d ended=%d; removals without a pre-image=%d\n",
		s.sponsorships["started"], s.sponsorships["revoked"], s.sponsorships["ended"], s.preImageMissing)
	if s.skipped > 0 {
		fmt.Fprintf(os.Stderr, "  skipped (undecodable)=%d\n", s.skipped)
	}
}
