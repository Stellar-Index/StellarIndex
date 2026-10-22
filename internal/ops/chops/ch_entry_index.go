package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// entryIndexLockPath serialises ch-entry-index runs; the unit lists its directory under ReadWritePaths.
const entryIndexLockPath = "/var/lib/stellarindex/ch-entry-index.lock"

// entryIndexSignersLockPath is -signers-only's own lock: that walk runs for hours beside the timer's windows.
const entryIndexSignersLockPath = "/var/lib/stellarindex/ch-entry-index-signers.lock"

// entryIndexClaimablesLockPath is -claimables-only's own lock, for the same reason.
const entryIndexClaimablesLockPath = "/var/lib/stellarindex/ch-entry-index-claimables.lock"

// entryIndexFlushRowCount bounds the rows held in memory before an INSERT.
const entryIndexFlushRowCount = 500_000

// Seams for tests.
var (
	streamEntryIndexOwners     = clickhouse.StreamEntryIndexOwners
	streamEntryIndexAssets     = clickhouse.StreamEntryIndexAssets
	streamEntryIndexDecodeRows = clickhouse.StreamEntryIndexDecodeRows
	streamEntryIndexSignerOps  = clickhouse.StreamEntryIndexSignerOps
	streamClaimableRemovals    = clickhouse.StreamEntryIndexClaimableRemovals
	insertEntryIndex           = clickhouse.InsertEntryIndex
	insertClaimableIndex       = clickhouse.InsertClaimableIndex
	setEntryIndexWatermark     = clickhouse.SetEntryIndexWatermark
	setSignerIndexWatermark    = clickhouse.SetSignerIndexWatermark
	setClaimableIndexWatermark = clickhouse.SetClaimableIndexWatermark
	entryIndexWatermark        = clickhouse.EntryIndexWatermark
	signerIndexWatermark       = clickhouse.SignerIndexWatermark
	claimableIndexWatermark    = clickhouse.ClaimableIndexWatermark
	resolveEntryIndexRange     = resolveDeriveRange
)

// ch-entry-index builds the ledger index of classic-entry activity (stellar.account_entry_ledgers by account and role,
// stellar.asset_entry_ledgers by asset) from stellar.ledger_entry_changes, plus the signer role from set_options operations,
// windowed and resumable via the entry_index row of stellar.entry_history_watermark; without -write it only counts.
func chEntryIndex(args []string) error {
	fs := flag.NewFlagSet("ch-entry-index", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or -floor-ledger on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always clamped down to the contiguous tip.")
	window := fs.Uint("window", 10_000, "ledgers per build window (progress is reported once per window)")
	floorLedger := fs.Uint("floor-ledger", 1, "first-run start when no watermark exists; clamped up to the lake's first ledger")
	lockFile := fs.String("lock-file", entryIndexLockPath, "flock serialising concurrent -write runs (-signers-only defaults to "+entryIndexSignersLockPath+")")
	signersOnly := fs.Bool("signers-only", false, "build only the signer role, from the signer watermark (the lake floor on first run) up to the entry_index watermark; sets only the signer watermark")
	claimablesOnly := fs.Bool("claimables-only", false, "build only account_claimables and claimable_removals, from the claimable watermark (the lake floor on first run) up to the entry_index watermark; sets only the claimable watermark")
	heartbeat := fs.String("heartbeat", "", "ops-job heartbeat textfile path. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_ch-entry-index.prom when that directory exists")
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
	if *signersOnly && *claimablesOnly {
		return fmt.Errorf("-signers-only and -claimables-only are separate walks; run one at a time")
	}
	gate.Banner()
	run := &entryIndexRun{chAddr: *chAddr, from: uint32(*from), to: uint32(*to), window: uint32(*window), floor: uint32(*floorLedger), dryRun: gate.DryRun(), maxDecodeErrs: *maxDecodeErrs, signersOnly: *signersOnly, claimablesOnly: *claimablesOnly} //nolint:gosec // ledger sequences fit uint32
	if run.dryRun {
		return run.exec()
	}
	// The lock comes before any network call so a second timer firing exits at once.
	job := "ch-entry-index"
	switch {
	case run.signersOnly:
		job = "ch-entry-index-signers"
		if *lockFile == entryIndexLockPath {
			*lockFile = entryIndexSignersLockPath
		}
	case run.claimablesOnly:
		job = "ch-entry-index-claimables"
		if *lockFile == entryIndexLockPath {
			*lockFile = entryIndexClaimablesLockPath
		}
	}
	unlock, err := acquireRollupLock(job, *lockFile)
	if err != nil {
		return err
	}
	defer unlock()
	run.hb = opsutil.NewJobHeartbeat(job, *heartbeat, nil)
	if run.hb.Enabled() {
		fmt.Fprintf(os.Stderr, "%s: heartbeat -> %s\n", job, run.hb.Path())
	}
	run.hb.Start()
	err = run.exec()
	run.hb.Stop(err == nil)
	return err
}

type entryIndexRun struct {
	chAddr                  string
	from, to, window, floor uint32
	dryRun, signersOnly     bool
	claimablesOnly          bool
	maxDecodeErrs           uint64
	hb                      *opsutil.JobHeartbeat // nil in a dry run
}

// progress reports windows completed and the ledger built through; the lagging alert reads the cursor.
func (r *entryIndexRun) progress(windows uint64, cursor uint32) {
	if r.hb != nil {
		r.hb.Progress(windows, uint64(cursor))
	}
}

func (r *entryIndexRun) exec() error {
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	switch {
	case r.signersOnly:
		return r.execRole(ctx, "signer role", signerIndexWatermark)
	case r.claimablesOnly:
		return r.execRole(ctx, "claimable index", claimableIndexWatermark)
	}

	start, last, err := resolveEntryIndexRange(ctx, r.chAddr, "ch-entry-index", r.from, r.to, r.floor, clickhouse.EntryIndexWatermark)
	if err != nil {
		return err
	}
	r.progress(0, start-1)
	if last < start {
		fmt.Fprintf(os.Stderr, "ch-entry-index: nothing to build (start %d, contiguous tip %d)\n", start, last)
		return nil
	}
	st := &entryIndexStats{}
	if err := runEntryIndex(ctx, r, start, last, st); err != nil {
		return err
	}
	st.report(start, last)
	return enforceDecodeBudget("ch-entry-index", st.skipped, r.maxDecodeErrs)
}

// execRole walks a floor-anchored role up to the entry_index watermark, re-reading it until the two meet: from there each
// timer window advances both. An explicit -from or a dry run (which moves no watermark) makes one pass.
func (r *entryIndexRun) execRole(ctx context.Context, what string, watermark func(context.Context, string) (uint32, error)) error {
	st := &entryIndexStats{}
	var first, through uint32
	for {
		start, last, err := resolveEntryIndexRange(ctx, r.chAddr, "ch-entry-index", r.from, r.to, r.floor, watermark)
		if err != nil {
			return err
		}
		entryWM, err := entryIndexWatermark(ctx, r.chAddr)
		if err != nil {
			return fmt.Errorf("read entry_index watermark: %w", err)
		}
		last = min(last, entryWM)
		if last < start {
			break
		}
		if through != 0 && start <= through {
			return fmt.Errorf("%s watermark did not advance past %d after a written pass", what, through)
		}
		if first == 0 {
			first = start
		}
		if err := runEntryIndex(ctx, r, start, last, st); err != nil {
			return err
		}
		through = last
		if r.from != 0 || r.dryRun {
			break
		}
	}
	if first == 0 {
		fmt.Fprintf(os.Stderr, "ch-entry-index: %s already caught up with the entry_index watermark\n", what)
		return nil
	}
	st.report(first, through)
	return enforceDecodeBudget("ch-entry-index", st.skipped, r.maxDecodeErrs)
}

type entryIndexStats struct {
	accountRows, assetRows, decoded, skipped uint64
	claimableRows, removalRows               uint64
	logged                                   int
}

func (st *entryIndexStats) skip(ledger uint32, what string, err error) {
	st.skipped++
	if st.logged < 3 {
		st.logged++
		fmt.Fprintf(os.Stderr, "ch-entry-index: ledger %d %s skipped: %v\n", ledger, what, err)
	}
}

func (st *entryIndexStats) report(start, last uint32) {
	fmt.Fprintf(os.Stderr, "ch-entry-index: [%d,%d] %d account rows, %d asset rows, %d claimable rows, %d removals, %d entries decoded, %d skipped\n",
		start, last, st.accountRows, st.assetRows, st.claimableRows, st.removalRows, st.decoded, st.skipped)
}

// runEntryIndex builds [start, last] window by window, advancing the watermark after each written window.
func runEntryIndex(ctx context.Context, r *entryIndexRun, start, last uint32, st *entryIndexStats) error {
	runStart := time.Now()
	var windows uint64
	for lo := start; ; {
		hi := last
		if last-lo >= r.window {
			hi = lo + r.window - 1
		}
		windowStart := time.Now()
		w := &entryIndexWindow{chAddr: r.chAddr, dryRun: r.dryRun, signersOnly: r.signersOnly, claimablesOnly: r.claimablesOnly, st: st}
		if err := w.build(ctx, lo, hi); err != nil {
			return fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		if err := r.advanceWatermarks(ctx, lo, hi, w.txLedgers); err != nil {
			return err
		}
		windows++
		st.accountRows += w.accountRows
		st.assetRows += w.assetRows
		st.claimableRows += w.claimableRows
		st.removalRows += w.removalRows
		r.progress(windows, hi)
		fmt.Fprintf(os.Stderr, "ch-entry-index: window [%d,%d] done — %d account rows, %d asset rows, %d tx ledgers (%s; elapsed %s)\n",
			lo, hi, w.accountRows, w.assetRows, w.txLedgers, time.Since(windowStart).Round(time.Millisecond), time.Since(runStart).Round(time.Second))
		if hi == last {
			return nil
		}
		lo = hi + 1
	}
}

// advanceWatermarks records a written window: one role's watermark alone under -signers-only or -claimables-only, else all.
func (r *entryIndexRun) advanceWatermarks(ctx context.Context, lo, hi uint32, txLedgers uint64) error {
	switch {
	case r.dryRun:
		return nil
	case r.signersOnly:
		if err := setSignerIndexWatermark(ctx, r.chAddr, lo, hi); err != nil {
			return fmt.Errorf("window [%d,%d] written but signer watermark refused: %w — resume with no -from", lo, hi, err)
		}
		return nil
	case r.claimablesOnly:
		if err := setClaimableIndexWatermark(ctx, r.chAddr, lo, hi); err != nil {
			return fmt.Errorf("window [%d,%d] written but claimable watermark refused: %w — resume with no -from", lo, hi, err)
		}
		return nil
	}
	// The role watermarks go first: a refused entry watermark re-runs the window, which re-records nothing.
	if err := setSignerIndexWatermark(ctx, r.chAddr, lo, hi); err != nil && !errors.Is(err, clickhouse.ErrEntryHistorySkippedPrefix) {
		return fmt.Errorf("window [%d,%d] written but signer watermark refused: %w — re-run it with -from %d -to %d", lo, hi, err, lo, hi)
	}
	if err := setClaimableIndexWatermark(ctx, r.chAddr, lo, hi); err != nil && !errors.Is(err, clickhouse.ErrEntryHistorySkippedPrefix) {
		return fmt.Errorf("window [%d,%d] written but claimable watermark refused: %w — re-run it with -from %d -to %d", lo, hi, err, lo, hi)
	}
	if err := setEntryIndexWatermark(ctx, r.chAddr, lo, hi, txLedgers); err != nil {
		return fmt.Errorf("window [%d,%d] written but watermark refused: %w — re-run it with -from %d -to %d", lo, hi, err, lo, hi)
	}
	return nil
}

// entryIndexWindow accumulates one window's rows; txLedgers is the distinct ledgers the owner stream covered, the
// watermark's coverage proof (every tx-bearing ledger holds a fee-phase account row).
type entryIndexWindow struct {
	chAddr                            string
	dryRun, signersOnly               bool
	claimablesOnly                    bool
	st                                *entryIndexStats
	accounts                          []clickhouse.EntryIndexAccountRow
	assets                            []clickhouse.EntryIndexAssetRow
	claimables                        []clickhouse.EntryIndexClaimableRow
	removals                          []clickhouse.EntryIndexClaimableRemoval
	accountRows, assetRows, txLedgers uint64
	claimableRows, removalRows        uint64
}

func (w *entryIndexWindow) build(ctx context.Context, lo, hi uint32) error {
	if w.signersOnly {
		if err := w.signers(ctx, lo, hi); err != nil {
			return err
		}
		return w.flush(ctx)
	}
	if w.claimablesOnly {
		if err := w.decodeRoles(ctx, lo, hi); err != nil {
			return err
		}
		if err := w.claimableRemovals(ctx, lo, hi); err != nil {
			return err
		}
		return w.flush(ctx)
	}
	if err := w.owners(ctx, lo, hi); err != nil {
		return err
	}
	if err := w.decodeRoles(ctx, lo, hi); err != nil {
		return err
	}
	if err := w.claimableRemovals(ctx, lo, hi); err != nil {
		return err
	}
	if err := w.signers(ctx, lo, hi); err != nil {
		return err
	}
	if err := w.assetCounts(ctx, lo, hi); err != nil {
		return err
	}
	return w.flush(ctx)
}

func (w *entryIndexWindow) add(ctx context.Context, row clickhouse.EntryIndexAccountRow) error {
	w.accounts = append(w.accounts, row)
	w.accountRows++
	if len(w.accounts) >= entryIndexFlushRowCount {
		return w.flush(ctx)
	}
	return nil
}

func (w *entryIndexWindow) flush(ctx context.Context) error {
	if !w.dryRun && (len(w.accounts) > 0 || len(w.assets) > 0) {
		if _, err := insertEntryIndex(ctx, w.chAddr, w.accounts, w.assets); err != nil {
			return err
		}
	}
	if !w.dryRun {
		if err := insertClaimableIndex(ctx, w.chAddr, w.claimables, w.removals); err != nil {
			return err
		}
	}
	w.accounts, w.assets = w.accounts[:0], w.assets[:0]
	w.claimables, w.removals = w.claimables[:0], w.removals[:0]
	return nil
}

func (w *entryIndexWindow) addClaimable(ctx context.Context, row clickhouse.EntryIndexClaimableRow) error {
	w.claimables = append(w.claimables, row)
	w.claimableRows++
	if len(w.claimables) >= entryIndexFlushRowCount {
		return w.flush(ctx)
	}
	return nil
}

func (w *entryIndexWindow) claimableRemovals(ctx context.Context, lo, hi uint32) error {
	return streamClaimableRemovals(ctx, w.chAddr, lo, hi, func(r clickhouse.EntryIndexClaimableRemoval) error {
		w.removals = append(w.removals, r)
		w.removalRows++
		if len(w.removals) >= entryIndexFlushRowCount {
			return w.flush(ctx)
		}
		return nil
	})
}

func (w *entryIndexWindow) owners(ctx context.Context, lo, hi uint32) error {
	var last uint32
	return streamEntryIndexOwners(ctx, w.chAddr, lo, hi, func(r clickhouse.EntryIndexOwnerRow) error {
		if r.Ledger != last {
			last = r.Ledger
			w.txLedgers++
		}
		raw, err := strkey.Decode(strkey.VersionByteAccountID, r.AccountID)
		if err != nil {
			w.st.skip(r.Ledger, "owner "+r.AccountID, err)
			return nil
		}
		return w.add(ctx, clickhouse.EntryIndexAccountRow{Account: string(raw), Role: clickhouse.EntryIndexRoleOwner, Ledger: r.Ledger})
	})
}

// decodeRoles adds claimant and sponsor rows; the stream is ledger-ordered, so one map per ledger dedups them.
func (w *entryIndexWindow) decodeRoles(ctx context.Context, lo, hi uint32) error {
	seen := map[clickhouse.EntryIndexAccountRow]struct{}{}
	var cur uint32
	return streamEntryIndexDecodeRows(ctx, w.chAddr, lo, hi, func(r clickhouse.EntryIndexDecodeRow) error {
		if r.Ledger != cur {
			cur = r.Ledger
			clear(seen)
		}
		var e xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(r.EntryXDR, &e); err != nil {
			w.st.skip(r.Ledger, r.EntryType+" entry", err)
			return nil
		}
		w.st.decoded++
		for _, c := range claimableRows(&e, r) {
			if err := w.addClaimable(ctx, c); err != nil {
				return err
			}
		}
		if w.claimablesOnly {
			return nil
		}
		for _, row := range entryIndexRoleRows(&e, r.Ledger) {
			if _, dup := seen[row]; dup {
				continue
			}
			seen[row] = struct{}{}
			if err := w.add(ctx, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// entryIndexRoleRows lists the claimant and sponsor rows one decoded entry contributes. Sponsor means the entry's own
// sponsor; signer sponsorships (AccountEntryExtensionV2) are not indexed.
func entryIndexRoleRows(e *xdr.LedgerEntry, ledger uint32) []clickhouse.EntryIndexAccountRow {
	var rows []clickhouse.EntryIndexAccountRow
	if cb, ok := e.Data.GetClaimableBalance(); ok {
		for _, c := range cb.Claimants {
			v0, ok := c.GetV0()
			if !ok {
				continue
			}
			if key, ok := v0.Destination.GetEd25519(); ok {
				rows = append(rows, clickhouse.EntryIndexAccountRow{Account: string(key[:]), Role: clickhouse.EntryIndexRoleClaimant, Ledger: ledger})
			}
		}
	}
	if sp := e.SponsoringID(); sp != nil {
		if key, ok := sp.GetEd25519(); ok {
			rows = append(rows, clickhouse.EntryIndexAccountRow{Account: string(key[:]), Role: clickhouse.EntryIndexRoleSponsor, Ledger: ledger})
		}
	}
	return rows
}

// claimableRows lists one account_claimables row per ed25519 claimant of a decoded claimable balance.
func claimableRows(e *xdr.LedgerEntry, r clickhouse.EntryIndexDecodeRow) []clickhouse.EntryIndexClaimableRow {
	cb, ok := e.Data.GetClaimableBalance()
	if !ok {
		return nil
	}
	var rows []clickhouse.EntryIndexClaimableRow
	for _, c := range cb.Claimants {
		v0, ok := c.GetV0()
		if !ok {
			continue
		}
		if key, ok := v0.Destination.GetEd25519(); ok {
			rows = append(rows, clickhouse.EntryIndexClaimableRow{Account: string(key[:]), KeyXDR: r.KeyXDR, Ledger: r.Ledger, CloseTime: r.CloseTime, EntryXDR: r.EntryXDR})
		}
	}
	return rows
}

// signers adds a signer row for each ed25519 key a set_options operation adds or reweights; a removal (weight 0)
// adds nothing, since readers only list accounts the key currently signs for.
func (w *entryIndexWindow) signers(ctx context.Context, lo, hi uint32) error {
	seen := map[clickhouse.EntryIndexAccountRow]struct{}{}
	var cur uint32
	return streamEntryIndexSignerOps(ctx, w.chAddr, lo, hi, func(op clickhouse.EntryIndexSignerOp) error {
		if op.Ledger != cur {
			cur = op.Ledger
			clear(seen)
		}
		var body xdr.OperationBody
		if err := xdr.SafeUnmarshalBase64(op.BodyXDR, &body); err != nil {
			w.st.skip(op.Ledger, "set_options op", err)
			return nil
		}
		w.st.decoded++
		row, ok := entryIndexSignerRow(body, op.Ledger)
		if _, dup := seen[row]; !ok || dup {
			return nil
		}
		seen[row] = struct{}{}
		return w.add(ctx, row)
	})
}

func entryIndexSignerRow(body xdr.OperationBody, ledger uint32) (clickhouse.EntryIndexAccountRow, bool) {
	so, ok := body.GetSetOptionsOp()
	if !ok || so.Signer == nil || so.Signer.Weight == 0 {
		return clickhouse.EntryIndexAccountRow{}, false
	}
	key, ok := so.Signer.Key.GetEd25519()
	if !ok {
		return clickhouse.EntryIndexAccountRow{}, false
	}
	return clickhouse.EntryIndexAccountRow{Account: string(key[:]), Role: clickhouse.EntryIndexRoleSigner, Ledger: ledger}, true
}

func (w *entryIndexWindow) assetCounts(ctx context.Context, lo, hi uint32) error {
	return streamEntryIndexAssets(ctx, w.chAddr, lo, hi, func(r clickhouse.EntryIndexAssetRow) error {
		w.assets = append(w.assets, r)
		w.assetRows++
		if len(w.assets) >= entryIndexFlushRowCount {
			return w.flush(ctx)
		}
		return nil
	})
}
