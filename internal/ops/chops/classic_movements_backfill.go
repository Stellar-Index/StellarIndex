package chops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// classicMovementOpKey identifies one classic operation for
// correlating clickhouse.StreamEntryChanges output against
// clickhouse.StreamClassicOps output within a single window — the
// ADR-0047 Phase 4 entry-changes-correlated surface's join key.
type classicMovementOpKey struct {
	Ledger  uint32
	TxHash  string
	OpIndex int32
}

// classicMovementsP23StartLedger aliases classicmovements.P23StartLedger (ADR-0048 D5).
const classicMovementsP23StartLedger uint32 = classicmovements.P23StartLedger

// classicMovementsDefaultWindow is the ledger span streamed and written per window. It bounds
// memory and gives a resume point every window.
const classicMovementsDefaultWindow = 500_000

// classicMovementsWindowDeadline bounds one window's decode, write and verify. A half-dead
// ClickHouse connection can block a network read for hours, and the driver ReadTimeout alone
// does not unwedge it. A window that times out is retried once on fresh connections.
const classicMovementsWindowDeadline = 20 * time.Minute

// classicMovementsBackfill writes ADR-0047 movements into stellar.account_movements (ADR-0048 D2).
// It is its own writer because MovementEvent is historical-only and has no persist arm.
//
// Claim/clawback correlation resolves in memory, then by one batched lookup, else counts as
// unresolved; an amount is never guessed. AllowTrust/SetTrustLineFlags are skipped in a
// zero-fidelity window, where their empty result would under-report CAP-0038 liquidations.
// Resume is trusted only when data starts exactly at -from, and -to is hard-clamped below
// classicMovementsP23StartLedger, the one enforcement point of ADR-0047 D2.
func classicMovementsBackfill(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse+clamp, resume, windowed stream+decode+write+verify loop, report.
	fs, gate := opsutil.NewMutatingFlagSet("classic-movements-backfill")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required) — HARD-CLAMPED below the P23 boundary (58762517) regardless of what is passed here (ADR-0047 D2: this source is historical-only)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address — both the read (lake) and write (stellar.account_movements, ADR-0048 D2) target; no Postgres connection is opened by this command")
	window := fs.Uint("window", classicMovementsDefaultWindow, "ledger-window size per streamed ClickHouse read + ClickHouse batch write; bounds memory and gives a resumable checkpoint every window")
	resume := fs.Bool("resume", true, "resume from the highest ledger already written to stellar.account_movements in [-from,-to], if any (data-derived — see doc comment)")
	verify := fs.Bool("verify", false, "after each window, recount stellar.account_movements and compare against this run's decode-time per-kind counts (cheap reconciliation, not full ADR-0033 machinery)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := gate.Enabled()
	if *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-from, -to are required; -to must be >= -from")
	}

	clampedTo := uint32(*to) //nolint:gosec // flag.Uint values here are ledger sequences, always in uint32 range for real usage.
	if clampedTo >= classicMovementsP23StartLedger {
		fmt.Fprintf(os.Stderr,
			"classic-movements-backfill: WARNING -to=%d is at/past the P23 boundary (ledger %d, 2025-09-03, Whisk) — classic-movement reconstruction is HISTORICAL-ONLY per ADR-0047 D2 (every ledger from P23 onward already emits a unified CAP-67 event via sep41_transfers); clamping -to to %d\n",
			*to, classicMovementsP23StartLedger, classicMovementsP23StartLedger-1)
		clampedTo = classicMovementsP23StartLedger - 1
	}
	startLedger := uint32(*from) //nolint:gosec // see above
	if startLedger > clampedTo {
		return fmt.Errorf("classic-movements-backfill: -from=%d is at/past the P23 boundary (ledger %d) after clamping -to to %d — nothing to do; this source is historical-only",
			*from, classicMovementsP23StartLedger, clampedTo)
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	if err := clickhouse.EnsureAccountMovementsTable(ctx, *chAddr); err != nil {
		return fmt.Errorf("classic-movements-backfill: %w", err)
	}

	if *resume {
		maxLedger, maxFound, merr := clickhouse.MaxAccountMovementLedger(ctx, *chAddr, startLedger, clampedTo)
		if merr != nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: resume lookup failed (%v) — starting from -from\n", merr)
		} else if maxFound && maxLedger >= startLedger {
			// max(ledger) alone cannot tell a fully processed range from a narrower prior run
			// that left data only at the top, so confirm the data starts at -from first.
			minLedger, minFound, minErr := clickhouse.MinAccountMovementLedger(ctx, *chAddr, startLedger, clampedTo)
			if minErr != nil {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: resume contiguity check failed (%v) — starting from -from, not trusting the resume jump\n", minErr)
			} else {
				resumeAt, jumped := classicMovementsResumeStart(startLedger, maxLedger, minLedger, minFound)
				if jumped {
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: resuming at ledger %d (highest ledger already in stellar.account_movements for this range; that ledger is deliberately re-processed, not skipped)\n", resumeAt)
					startLedger = resumeAt
				} else {
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: resume skipped — data in [%d,%d] starts at ledger %d, not -from=%d (likely a narrower prior run); starting from -from so that gap is revisited\n", startLedger, clampedTo, minLedger, startLedger)
				}
			}
		}
	}

	mode := "DRY-RUN (count only)"
	if write {
		mode = "WRITE"
	}
	windowSize := uint32(*window) //nolint:gosec // operator-supplied window size; zero guarded below.
	if windowSize == 0 {
		windowSize = classicMovementsDefaultWindow
	}
	fmt.Fprintf(os.Stderr, "classic-movements-backfill: [%d,%d] mode=%s window=%d ch=%s verify=%v\n",
		startLedger, clampedTo, mode, windowSize, *chAddr, *verify)

	dec := classicmovements.NewDecoder()
	opTypes := classicmovements.SupportedOpTypes()
	entryChangeOpTypes := classicmovements.EntryChangeOpTypes()
	counts := map[classicmovements.Kind]int64{}
	var totalRead, totalDecoded, totalWritten int64
	var totalResolvedIndex, totalResolvedCH, totalUnresolved int64
	var totalLPUnavailable, totalCAP0038Checked, totalCAP0038Skipped, totalCAP0038Liquidations int64
	var totalVerifyMismatches int64

	for wlo := startLedger; wlo <= clampedTo; {
		whi := wlo + windowSize - 1
		if whi > clampedTo || whi < wlo { // whi<wlo guards uint32 overflow at the top of range
			whi = clampedTo
		}

		windowCtx, windowCancel := context.WithTimeout(ctx, classicMovementsWindowDeadline)
		res, werr := classicMovementsAttemptWindow(windowCtx, *chAddr, dec, opTypes, entryChangeOpTypes, wlo, whi, write, *verify)
		windowCancel()
		if werr != nil && errors.Is(werr, context.DeadlineExceeded) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] hit its %s deadline (2026-07-12 half-dead-connection stall class) — retrying once on fresh connections\n",
				wlo, whi, classicMovementsWindowDeadline)
			// Drop pending claim refs from the failed attempt; the retry re-decodes the window
			// and would otherwise double them.
			dec.TakePendingClaimableBalances()
			windowCtx2, cancel2 := context.WithTimeout(ctx, classicMovementsWindowDeadline)
			res, werr = classicMovementsAttemptWindow(windowCtx2, *chAddr, dec, opTypes, entryChangeOpTypes, wlo, whi, write, *verify)
			cancel2()
		}
		if werr != nil {
			if errors.Is(werr, context.Canceled) || ctx.Err() != nil {
				// -resume is data-derived (MaxAccountMovementLedger), so
				// it restarts at the highest ledger this window managed to
				// write, NOT at the window's start — saying otherwise sends
				// operators looking for a gap in the wrong place.
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: cancelled mid-window [%d,%d] — a -resume run restarts from the highest ledger already written to stellar.account_movements in [-from,-to], which for a partially-written window is above %d; inserts are ledger-ordered, so every ledger below that point is complete and that ledger itself is re-processed\n", wlo, whi, wlo)
				break
			}
			return fmt.Errorf("classic-movements-backfill: window [%d,%d]: %w", wlo, whi, werr)
		}

		// Merge into the run totals — reached only after a successful
		// attempt (first try or the one retry), so a window that failed
		// both tries never contributes partial counts or a partial
		// batch to the run (it already returned above instead).
		for k, v := range res.windowCounts {
			counts[k] += v
		}
		totalRead += res.windowRead + res.windowEntryChangeRead
		totalDecoded += res.windowDecoded + res.windowEntryChangeDecoded
		totalResolvedIndex += res.windowResolvedIndex
		totalResolvedCH += res.windowResolvedCH
		totalUnresolved += res.windowUnresolved
		totalLPUnavailable += res.windowLPUnavailable
		totalCAP0038Checked += res.windowCAP0038Checked
		totalCAP0038Skipped += res.windowCAP0038Skipped
		totalCAP0038Liquidations += res.windowCAP0038Liquidations
		totalWritten += res.windowWritten
		totalVerifyMismatches += res.verifyMismatches

		fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] done — %d ops read, %d movements decoded (running totals: read=%d decoded=%d written=%d) — resume point -from %d\n",
			wlo, whi, res.windowRead, res.windowDecoded, totalRead, totalDecoded, totalWritten, whi)

		if whi == clampedTo {
			break
		}
		wlo = whi + 1
	}

	fmt.Printf("\n=== classic-movements-backfill [%d,%d] %s ===\n", startLedger, clampedTo, mode)
	fmt.Printf("%-24s %14s\n", "movement_kind", "count")
	for _, k := range []classicmovements.Kind{
		classicmovements.KindPayment, classicmovements.KindCreateAccount,
		classicmovements.KindPathPayment,
		classicmovements.KindClaimableBalanceCreate, classicmovements.KindClaimableBalanceClaim,
		classicmovements.KindClaimableBalanceClawback, classicmovements.KindClawback,
		classicmovements.KindAccountMerge,
		classicmovements.KindLiquidityPoolDeposit, classicmovements.KindLiquidityPoolWithdraw,
	} {
		fmt.Printf("%-24s %14d\n", k, counts[k])
	}
	fmt.Printf("%-24s %14d\n", "TOTAL ops read", totalRead)
	fmt.Printf("%-24s %14d\n", "TOTAL decoded", totalDecoded)
	fmt.Printf("%-24s %14d\n", "CB resolved (index)", totalResolvedIndex)
	fmt.Printf("%-24s %14d\n", "CB resolved (clickhouse)", totalResolvedCH)
	fmt.Printf("%-24s %14d\n", "CB UNRESOLVED", totalUnresolved)
	fmt.Printf("%-24s %14d\n", "LP entry-changes N/A", totalLPUnavailable)
	fmt.Printf("%-24s %14d\n", "CAP-0038 checked", totalCAP0038Checked)
	fmt.Printf("%-24s %14d\n", "CAP-0038 skipped (fidelity)", totalCAP0038Skipped)
	fmt.Printf("%-24s %14d\n", "CAP-0038 liquidations", totalCAP0038Liquidations)
	if write {
		fmt.Printf("%-24s %14d\n", "TOTAL rows written", totalWritten)
		fmt.Println("(rows, post-fan-out — 1-2 rows per movement; not deduped, ReplacingMergeTree resolves at merge time)")
	} else {
		fmt.Println("\n(dry-run — re-run with -write to persist to ClickHouse)")
	}
	if *verify {
		fmt.Printf("%-24s %14d\n", "verify mismatches", totalVerifyMismatches)
	}
	if totalUnresolved > 0 {
		fmt.Printf("\nNOTE: %d claim/clawback ops had no resolvable create row (recognizable ADR-0047 D4 incompleteness — see stderr for the per-op log). Re-running once the create's own range has been backfilled resolves these on a subsequent pass; ClickHouse's ReplacingMergeTree makes that safe.\n", totalUnresolved)
	}
	if totalLPUnavailable > 0 || totalCAP0038Skipped > 0 {
		fmt.Printf("\nNOTE: %d LiquidityPoolDeposit/Withdraw ops and %d AllowTrust/SetTrustLineFlags checks were skipped for lack of ledger_entry_changes fidelity in this range (see internal/sources/classicmovements/README.md, Protocol boundaries). Re-running this same range once ledger_entry_changes covers it resolves these; ClickHouse's ReplacingMergeTree makes that safe.\n",
			totalLPUnavailable, totalCAP0038Skipped)
	}
	if totalVerifyMismatches > 0 {
		fmt.Printf("\nWARNING: %d window(s) had a movement_kind count mismatch between this run's decode and stellar.account_movements — see stderr for the per-window detail. Common benign causes: a concurrent write to the SAME range from another invocation, or un-merged ReplacingMergeTree parts still settling; a persistent mismatch after a quiet re-run warrants investigation.\n", totalVerifyMismatches)
	}
	return nil
}

// classicMovementsResumeStart is the pure -resume decision: jump to maxLedger only when
// minLedger == startLedger, proving the data starts exactly there (see
// sortAccountMovementRowsForInsert for why maxLedger is then a sound checkpoint). Data starting
// ABOVE startLedger is a widened -from, and jumping would skip the gap silently.
func classicMovementsResumeStart(startLedger, maxLedger, minLedger uint32, minFound bool) (resumeAt uint32, jumped bool) {
	if !minFound || minLedger != startLedger {
		return startLedger, false
	}
	return maxLedger, true
}

// windowResult holds one window's batch and counters. It holds no run-level state, and the
// caller merges it only after a successful attempt, so a failed attempt is discarded whole
// and retrying is safe.
type windowResult struct {
	batch        []clickhouse.AccountMovement
	windowCounts map[classicmovements.Kind]int64

	windowRead, windowDecoded                                             int64
	windowResolvedIndex, windowResolvedCH, windowUnresolved               int64
	windowLPUnavailable                                                   int64
	windowCAP0038Checked, windowCAP0038Skipped, windowCAP0038Liquidations int64
	windowEntryChangeRead, windowEntryChangeDecoded                       int64
	windowWritten                                                         int64
	verifyMismatches                                                      int64
}

// classicMovementsAttemptWindow makes one decode, write and verify attempt on [wlo,whi].
// dec is shared across windows and retries; its create index may span windows, but dec.pending
// must be discarded by the caller before a retry.
func classicMovementsAttemptWindow(
	winCtx context.Context,
	chAddr string,
	dec *classicmovements.Decoder,
	opTypes, entryChangeOpTypes []string,
	wlo, whi uint32,
	write, verify bool,
) (windowResult, error) {
	res := windowResult{windowCounts: map[classicmovements.Kind]int64{}}

	if err := classicMovementsDecodeOpsSurface(winCtx, chAddr, dec, opTypes, wlo, whi, &res); err != nil {
		return res, err
	}
	// Entry changes must run before pending-claim resolution. A CAP-0038 revocation create
	// bypasses dec.Decode, so only res.batch holds it, and a same-window claim would otherwise
	// resolve as unresolved.
	if err := classicMovementsDecodeEntryChangesSurface(winCtx, chAddr, entryChangeOpTypes, wlo, whi, &res); err != nil {
		return res, err
	}
	classicMovementsResolvePendingClaimableBalances(winCtx, chAddr, dec, wlo, whi, &res)
	if err := classicMovementsWriteAndVerify(winCtx, chAddr, wlo, whi, write, verify, &res); err != nil {
		return res, err
	}
	return res, nil
}

// classicMovementsDecodeOpsSurface streams and decodes the op-only surface for one window into
// res. dec.Decode also feeds dec's shared claimable-balance create index.
func classicMovementsDecodeOpsSurface(winCtx context.Context, chAddr string, dec *classicmovements.Decoder, opTypes []string, wlo, whi uint32, res *windowResult) error {
	// The query joins ReplacingMergeTree tables without FINAL, so one op can arrive as k*m
	// identical rows; seen collapses the stream to one op per (ledger, tx_hash, op_index).
	seen := make(map[classicMovementOpKey]struct{})
	werr := clickhouse.StreamClassicOps(winCtx, chAddr, wlo, whi, opTypes, func(op clickhouse.ClassicOp) error {
		classicMovementsDecodeOp(dec, seen, op, res)
		return nil
	})
	if werr != nil {
		return fmt.Errorf("stream classic ops [%d,%d]: %w", wlo, whi, werr)
	}
	return nil
}

// classicMovementsDecodeOp dedupes one ClassicOp against seen, decodes it and accumulates into
// res. It is split out so the dedup is testable without a live ClickHouse.
func classicMovementsDecodeOp(dec *classicmovements.Decoder, seen map[classicMovementOpKey]struct{}, op clickhouse.ClassicOp, res *windowResult) {
	res.windowRead++
	key := classicMovementOpKey{Ledger: op.Ledger, TxHash: op.TxHash, OpIndex: int32(op.OpIndex)} //nolint:gosec // OpIndex is a non-negative XDR index.
	if _, dup := seen[key]; dup {
		return
	}
	seen[key] = struct{}{}
	outs, derr := dec.Decode(dispatcher.OpContext{
		Ledger:   op.Ledger,
		ClosedAt: op.ClosedAt,
		TxHash:   op.TxHash,
		TxSource: op.Source,
		OpIndex:  int(op.OpIndex),
		Op:       op.Op,
		OpResult: op.OpResult,
	})
	if derr != nil {
		// Non-fatal per the OpDecoder contract: count and skip.
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: decode error (ledger %d tx %s op %d): %v\n",
			op.Ledger, op.TxHash, op.OpIndex, derr)
		return
	}
	for _, ev := range outs {
		me, ok := ev.(classicmovements.MovementEvent)
		if !ok {
			continue
		}
		res.windowDecoded++
		res.windowCounts[me.Movement.Kind]++
		res.batch = append(res.batch, accountMovementOf(me.Movement))
	}
}

// classicMovementsResolvePendingClaimableBalances is the ADR-0047 second pass over claim/clawback
// rows the decode loop could not correlate with a create:
//
//  1. an in-memory re-check, closing the same-window tx_hash ordering gap;
//  2. a re-check against res.batch, because CAP-0038 revocation creates bypass dec.Decode and are
//     not in ClickHouse until this window is written;
//  3. one batched clickhouse.FindClaimableBalanceCreates call for creates outside this run's range.
//
// Still-unresolved rows are counted and logged, never guessed; a lookup failure degrades the miss-set
// to unresolved rather than failing the window.
func classicMovementsResolvePendingClaimableBalances(winCtx context.Context, chAddr string, dec *classicmovements.Decoder, wlo, whi uint32, res *windowResult) {
	pending := dec.TakePendingClaimableBalances()
	if len(pending) == 0 {
		return
	}

	batchIdx := classicMovementsBatchClaimableBalanceIndex(res.batch)

	var misses []classicmovements.PendingClaimableBalanceRef
	for _, ref := range pending {
		if asset, amount, createdBy, ok := dec.ResolveBalance(ref.BalanceIDHex); ok {
			res.windowResolvedIndex++
			res.windowDecoded++
			m := classicmovements.ResolvePendingClaimableBalance(ref, asset, amount, createdBy)
			res.windowCounts[m.Kind]++
			res.batch = append(res.batch, accountMovementOf(m))
			continue
		}
		if create, ok := batchIdx[ref.BalanceIDHex]; ok {
			res.windowResolvedIndex++
			res.windowDecoded++
			m := classicmovements.ResolvePendingClaimableBalance(ref, create.Asset, canonical.NewAmount(create.Amount), create.FromAddress)
			res.windowCounts[m.Kind]++
			res.batch = append(res.batch, accountMovementOf(m))
			continue
		}
		misses = append(misses, ref)
	}

	if len(misses) > 0 {
		ids := make([]string, len(misses))
		for i, ref := range misses {
			ids[i] = ref.BalanceIDHex
		}
		found, ferr := clickhouse.FindClaimableBalanceCreates(winCtx, chAddr, ids)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: FindClaimableBalanceCreates(%d ids) failed: %v — counting whole miss-set as unresolved\n",
				len(ids), ferr)
			res.windowUnresolved += int64(len(misses))
		} else {
			for _, ref := range misses {
				row, ok := found[ref.BalanceIDHex]
				if !ok {
					res.windowUnresolved++
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: unresolved %s balance_id=%s ledger=%d tx=%s op=%d — no create row found (in-memory index or ClickHouse); skipping, not guessing\n",
						ref.Kind, ref.BalanceIDHex, ref.Ledger, ref.TxHash, ref.OpIndex)
					continue
				}
				res.windowResolvedCH++
				res.windowDecoded++
				m := classicmovements.ResolvePendingClaimableBalance(ref, row.Asset, canonical.NewAmount(row.Amount), row.CreatedBy)
				res.windowCounts[m.Kind]++
				res.batch = append(res.batch, accountMovementOf(m))
			}
		}
	}

	fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] claimable-balance correlation — %d resolved (index), %d resolved (clickhouse), %d unresolved\n",
		wlo, whi, res.windowResolvedIndex, res.windowResolvedCH, res.windowUnresolved)
}

// classicMovementsBatchClaimableBalanceIndex keys this window's create movements by balance_id.
// A CAP-0038 revocation create never passes through dec.Decode, so dec never indexes it. A
// malformed balance_id is skipped and that ref falls through to ClickHouse.
func classicMovementsBatchClaimableBalanceIndex(batch []clickhouse.AccountMovement) map[string]clickhouse.AccountMovement {
	idx := make(map[string]clickhouse.AccountMovement)
	for _, m := range batch {
		if m.MovementKind != string(classicmovements.KindClaimableBalanceCreate) {
			continue
		}
		id, ok := m.Attributes["balance_id"].(string)
		if !ok || id == "" {
			continue
		}
		idx[id] = m
	}
	return idx
}

// classicMovementsDecodeEntryChangesSurface decodes the ADR-0047 entry-changes-correlated ops:
// liquidity pool deposit/withdraw and the CAP-0038 revocation case. A window-level fidelity
// probe decides how an absence of correlated entry changes is read.
func classicMovementsDecodeEntryChangesSurface(winCtx context.Context, chAddr string, entryChangeOpTypes []string, wlo, whi uint32, res *windowResult) error {
	fidelityCount, ferr := clickhouse.CountOpScopedEntryChanges(winCtx, chAddr, wlo, whi)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: entry-changes fidelity probe failed for [%d,%d]: %v — treating as fidelity-absent for this window\n",
			wlo, whi, ferr)
		fidelityCount = 0
	}
	fidelityPresent := fidelityCount > 0

	lpChanges := map[classicMovementOpKey][]classicmovements.EntryChangeXDR{}
	if lerr := clickhouse.StreamEntryChanges(winCtx, chAddr, wlo, whi, "liquidity_pool", func(ec clickhouse.EntryChange) error {
		k := classicMovementOpKey{Ledger: ec.Ledger, TxHash: ec.TxHash, OpIndex: ec.OpIndex}
		lpChanges[k] = append(lpChanges[k], classicmovements.EntryChangeXDR{ChangeType: ec.ChangeType, Entry: ec.Entry})
		return nil
	}); lerr != nil {
		return fmt.Errorf("stream liquidity_pool entry changes [%d,%d]: %w", wlo, whi, lerr)
	}

	// Skipped without fidelity, since CAP-0038 ops are skipped entirely then.
	cbChanges := map[classicMovementOpKey][]classicmovements.EntryChangeXDR{}
	if fidelityPresent {
		if cerr := clickhouse.StreamEntryChanges(winCtx, chAddr, wlo, whi, "claimable_balance", func(ec clickhouse.EntryChange) error {
			if ec.ChangeType != "created" {
				return nil // CAP-0038 detection only cares about newly-created escrow rows.
			}
			k := classicMovementOpKey{Ledger: ec.Ledger, TxHash: ec.TxHash, OpIndex: ec.OpIndex}
			cbChanges[k] = append(cbChanges[k], classicmovements.EntryChangeXDR{ChangeType: ec.ChangeType, Entry: ec.Entry})
			return nil
		}); cerr != nil {
			return fmt.Errorf("stream claimable_balance entry changes [%d,%d]: %w", wlo, whi, cerr)
		}
	}

	seenEC := make(map[classicMovementOpKey]struct{})
	werr2 := clickhouse.StreamClassicOps(winCtx, chAddr, wlo, whi, entryChangeOpTypes, func(op clickhouse.ClassicOp) error {
		res.windowEntryChangeRead++
		k := classicMovementOpKey{Ledger: op.Ledger, TxHash: op.TxHash, OpIndex: int32(op.OpIndex)} //nolint:gosec // OpIndex is a non-negative XDR index.
		// Same duplicate rows as in the ops surface; collapse before handling.
		if _, dup := seenEC[k]; dup {
			return nil
		}
		seenEC[k] = struct{}{}
		switch op.Op.Body.Type {
		case xdr.OperationTypeLiquidityPoolDeposit, xdr.OperationTypeLiquidityPoolWithdraw:
			classicMovementsHandleLiquidityPoolOp(op, lpChanges[k], fidelityPresent, res)
		case xdr.OperationTypeAllowTrust, xdr.OperationTypeSetTrustLineFlags:
			classicMovementsHandleCAP0038Op(op, cbChanges[k], fidelityPresent, res)
		}
		return nil
	})
	if werr2 != nil {
		return fmt.Errorf("stream entry-change ops [%d,%d]: %w", wlo, whi, werr2)
	}
	if res.windowLPUnavailable > 0 || res.windowCAP0038Skipped > 0 || res.windowCAP0038Checked > 0 {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] entry-changes surface — fidelity_present=%v LP_unavailable=%d CAP0038_checked=%d CAP0038_skipped=%d CAP0038_liquidations=%d\n",
			wlo, whi, fidelityPresent, res.windowLPUnavailable, res.windowCAP0038Checked, res.windowCAP0038Skipped, res.windowCAP0038Liquidations)
	}
	return nil
}

// classicMovementsHandleLiquidityPoolOp decodes one LP deposit/withdraw op against its
// correlated entry changes and accumulates the result into res.
func classicMovementsHandleLiquidityPoolOp(op clickhouse.ClassicOp, entryChanges []classicmovements.EntryChangeXDR, fidelityPresent bool, res *windowResult) {
	movements, derr := classicmovements.DecodeLiquidityPoolOp(op.Ledger, op.ClosedAt, op.TxHash, op.OpIndex, op.Source, op.Op, op.OpResult, entryChanges)
	if derr != nil {
		if errors.Is(derr, classicmovements.ErrEntryChangesUnavailable) {
			res.windowLPUnavailable++
			if fidelityPresent {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: ANOMALY entry-changes unavailable for LP op despite window fidelity present (ledger %d tx %s op %d) — investigate\n",
					op.Ledger, op.TxHash, op.OpIndex)
			}
			return
		}
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: LP decode error (ledger %d tx %s op %d): %v\n",
			op.Ledger, op.TxHash, op.OpIndex, derr)
		return
	}
	for _, m := range movements {
		res.windowEntryChangeDecoded++
		res.windowCounts[m.Kind]++
		res.batch = append(res.batch, accountMovementOf(m))
	}
}

// classicMovementsHandleCAP0038Op decodes one AllowTrust/SetTrustLineFlags op for a possible
// CAP-0038 auto-liquidation against its created claimable-balance entry changes.
func classicMovementsHandleCAP0038Op(op clickhouse.ClassicOp, entryChanges []classicmovements.EntryChangeXDR, fidelityPresent bool, res *windowResult) {
	if !fidelityPresent {
		res.windowCAP0038Skipped++
		return
	}
	res.windowCAP0038Checked++
	movements, derr := classicmovements.DecodeCAP0038Revocation(op.Ledger, op.ClosedAt, op.TxHash, op.OpIndex, op.Op, op.OpResult, entryChanges)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: CAP-0038 decode error (ledger %d tx %s op %d): %v\n",
			op.Ledger, op.TxHash, op.OpIndex, derr)
		return
	}
	if len(movements) > 0 {
		res.windowCAP0038Liquidations += int64(len(movements))
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: CAP-0038 auto-liquidation detected (ledger %d tx %s op %d) — %d leg(s)\n",
			op.Ledger, op.TxHash, op.OpIndex, len(movements))
	}
	for _, m := range movements {
		res.windowEntryChangeDecoded++
		res.windowCounts[m.Kind]++
		res.batch = append(res.batch, accountMovementOf(m))
	}
}

// classicMovementsWriteAndVerify persists res.batch (if -write) and recounts the window (if
// -verify), both under winCtx so either can trigger the per-window retry.
func classicMovementsWriteAndVerify(winCtx context.Context, chAddr string, wlo, whi uint32, write, verify bool, res *windowResult) error {
	if write && len(res.batch) > 0 {
		written, ierr := clickhouse.InsertAccountMovements(winCtx, chAddr, res.batch)
		if ierr != nil {
			return fmt.Errorf("write [%d,%d]: %w", wlo, whi, ierr)
		}
		res.windowWritten = written
	}

	if verify {
		res.verifyMismatches = verifyWindowCounts(wlo, whi, res.windowCounts, func() (clickhouse.AccountMovementVerifyCounts, error) {
			return clickhouse.VerifyAccountMovementsWindow(winCtx, chAddr, wlo, whi)
		})
	}
	return nil
}

// verifyWindowCounts compares per-kind counts from query against decoded, logs each mismatch
// and returns how many kinds disagreed. Never fatal.
func verifyWindowCounts(wlo, whi uint32, decoded map[classicmovements.Kind]int64, query func() (clickhouse.AccountMovementVerifyCounts, error)) int64 {
	observed, err := query()
	if err != nil {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: verify window [%d,%d] failed: %v\n", wlo, whi, err)
		return 1
	}
	var mismatches int64
	seen := map[string]bool{}
	for kind, want := range decoded {
		seen[string(kind)] = true
		got := int64(observed[string(kind)]) //nolint:gosec // movement counts are always small relative to int64
		if got != want {
			mismatches++
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: VERIFY MISMATCH window [%d,%d] kind=%s decoded=%d clickhouse=%d\n",
				wlo, whi, kind, want, got)
		}
	}
	for kind, got := range observed {
		if seen[kind] {
			continue
		}
		if got > 0 {
			mismatches++
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: VERIFY MISMATCH window [%d,%d] kind=%s decoded=0 clickhouse=%d (unexpected kind in ClickHouse for this window)\n",
				wlo, whi, kind, got)
		}
	}
	return mismatches
}

// accountMovementOf converts a decoded Movement into its clickhouse.AccountMovement shape.
func accountMovementOf(m classicmovements.Movement) clickhouse.AccountMovement {
	return clickhouse.AccountMovement{
		MovementKind:    string(m.Kind),
		Provenance:      string(m.Provenance),
		Ledger:          m.Ledger,
		LedgerCloseTime: m.LedgerCloseTime,
		TxHash:          m.TxHash,
		OpIndex:         m.OpIndex,
		LegIndex:        m.LegIndex,
		Asset:           m.Asset,
		Amount:          m.Amount.BigInt(),
		FromAddress:     m.FromAddress,
		ToAddress:       m.ToAddress,
		Attributes:      m.Attributes,
	}
}
