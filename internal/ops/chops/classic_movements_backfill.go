package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/ops/opsutil"
	"github.com/StellarIndex/stellar-index/internal/sources/classicmovements"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
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

// classicMovementsP23StartLedger re-exports classicmovements.P23StartLedger
// under this file's existing local name (ADR-0048 D5 promoted the
// value to an exported constant so internal/api/v1/explorer's account-
// movements merge can pin against it without duplicating the literal —
// see that constant's doc comment for the full rationale). Kept as an
// alias, not a straight replace-in-place, to avoid touching every call
// site in this file for a pure rename.
const classicMovementsP23StartLedger uint32 = classicmovements.P23StartLedger

// classicMovementsDefaultWindow is the per-window ledger span this
// command streams from ClickHouse + writes to ClickHouse before
// moving on. Bounds memory (each window's decoded batch, not the
// whole invocation, is held in-process) the same way ch-rebuild's
// maxBufferedRange guard does, and gives a resume point every window
// rather than only at the end of a multi-day run.
const classicMovementsDefaultWindow = 500_000

// classicMovementsWindowDeadline bounds a single window's decode +
// write + verify attempt (2026-07-12 stall): a half-dead ClickHouse
// native connection left this loop blocked in a network read for
// hours with zero CPU — the driver-level ReadTimeout alone did not
// unwedge it. A window that cannot finish inside this deadline is
// aborted and retried exactly once on fresh connections
// (StreamClassicOps / StreamEntryChanges dial per call, so a retry
// gets new sockets); a second expiry within the same deadline is
// treated as a real error, not retried again. Generous bound: the
// densest observed window (~9M ops at 25k ledgers) completed in well
// under 10 minutes when healthy.
const classicMovementsWindowDeadline = 20 * time.Minute

// classicMovementsBackfill is the ADR-0047 write path for ALL FOUR
// phases, RETARGETED by ADR-0048 D2 to write ClickHouse's
// stellar.account_movements instead of Postgres' classic_movements
// (migration 0105 stays applied but UNPOPULATED — see
// migrations/README.md's 0105 row): stellarindex-ops
// classic-movements-backfill -ch-addr ADDR -from N -to N [-window N]
// [-resume] [-write] [-verify]. Lake-in, lake-out — no Postgres
// anywhere in this command's loop (ADR-0048 D2's explicit
// requirement), unlike the pre-0048 version this replaces.
//
// Each window streams TWO decode surfaces from ClickHouse:
//   - the op-only surface (classicmovements.SupportedOpTypes /
//     Decoder.Decode) — Phases 1-3 plus Phase 4's AccountMerge;
//   - the entry-changes-correlated surface
//     (classicmovements.EntryChangeOpTypes /
//     classicmovements.DecodeLiquidityPoolOp /
//     classicmovements.DecodeCAP0038Revocation) — Phase 4's
//     LiquidityPoolDeposit/Withdraw and the CAP-0038 AllowTrust/
//     SetTrustLineFlags edge case, correlated per-op against
//     clickhouse.StreamEntryChanges output gathered for the same
//     window (see entrychanges.go's package doc for why this can't
//     go through Decoder.Decode).
//
// Both surfaces write into the SAME per-window batch, fanned out
// (one classicmovements.Movement -> 1-2 stellar.account_movements
// rows, ADR-0048 D2's direction discriminator) and batch-inserted via
// clickhouse.InsertAccountMovements.
//
// Phase 3's ClaimableBalance claim/clawback correlation (research
// §2's "b+own-index" path) resolves in three tiers per window:
// Decoder's free in-memory BalanceId index first, a ClickHouse lookup
// (clickhouse.FindClaimableBalanceCreate, scanning what THIS command
// has itself already written to stellar.account_movements) second for
// creates outside this run, and an explicit unresolved count — never
// a guessed amount — for anything neither finds. See
// classicmovements/dispatcher_adapter.go's Decoder doc for the memory-
// scaling reason operators should chunk `-from`/`-to` into multi-
// million-ledger invocations once Phase 3 volume is in play.
//
// Phase 4's entry-changes surface runs a cheap per-window fidelity
// probe (clickhouse.CountOpScopedEntryChanges) before deciding how to
// treat "no correlated entry changes found" for each op type:
// LiquidityPoolDeposit/Withdraw treat it as
// classicmovements.ErrEntryChangesUnavailable regardless (a real
// deposit/withdraw always mutates the pool, so absence always means
// unavailable fidelity); AllowTrust/SetTrustLineFlags are SKIPPED
// entirely for the window when the probe finds zero fidelity (their
// empty-changes case is indistinguishable from "no liquidation
// happened," which is by far the common case, so treating it as
// "checked, none found" during the fidelity-absent era would
// silently under-report CAP-0038 liquidations). As of this writing,
// EVERY window this command can address (hard-clamped below P23,
// 58,762,517) predates ledger_entry_changes' current per-op fidelity
// floor (~61,996,000, research §3.2) — Phase 0's `ch-backfill` is a
// separate, operator-scheduled prerequisite that closes this gap;
// until it runs, every LP op reports unavailable and every CAP-0038
// check is skipped, both counted and logged, never guessed.
//
// Deliberately does NOT reuse ch-rebuild's generic
// pipeline.HandleEvent write path: classicmovements.MovementEvent is
// historical-only (ADR-0047 D2) and has no HandleEvent persist arm
// by design (see internal/pipeline/lockstep_ast_test.go's
// notSunkEvents entry) — this command is its own dedicated,
// self-contained writer.
//
// Defaults to DRY-RUN (count only); pass -write to persist.
// Windowed + resumable, but — per ADR-0048 D2's "no Postgres in the
// loop" — resume is now DATA-DERIVED rather than cursor-persisted:
// -resume (default true) queries clickhouse.MaxAccountMovementLedger
// for the highest ledger already written in [-from,-to] and restarts
// FROM that ledger (not past it — a deliberate one-ledger overlap so
// a crash mid-window can never silently skip a partially-written
// ledger; ReplacingMergeTree absorbs the resulting duplicate insert
// for free). This mirrors ch-participant-backfill / ch-txindex-
// backfill's "the data IS the checkpoint" convention rather than a
// separate ingestion_cursors row. Idempotent either way — ClickHouse
// re-inserting an already-written window collapses at merge time.
//
// -verify (default false) recounts each window from
// stellar.account_movements right after it's processed (whether or
// not -write persisted anything new this run) and compares per-
// movement_kind counts against what THIS run's decode produced for
// that window — a cheap, window-scoped reconciliation (ADR-0047 D4
// applied to the CH write target), not full ADR-0033 machinery.
// Mismatches are logged loudly but non-fatal; the final summary
// reports the total.
//
// -to is HARD-CLAMPED below classicMovementsP23StartLedger regardless
// of what the operator passes — loudly, via a stderr warning, never
// silently. This is the one enforcement point for ADR-0047 D2's
// "historical-only" invariant; nothing upstream (the decoder, the CH
// reader) knows about the P23 boundary at all.
func classicMovementsBackfill(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse+clamp, resume, windowed stream+decode+write+verify loop, report.
	fs := flag.NewFlagSet("classic-movements-backfill", flag.ContinueOnError)
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required) — HARD-CLAMPED below the P23 boundary (58762517) regardless of what is passed here (ADR-0047 D2: this source is historical-only)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address — both the read (lake) and write (stellar.account_movements, ADR-0048 D2) target; no Postgres connection is opened by this command")
	window := fs.Uint("window", classicMovementsDefaultWindow, "ledger-window size per streamed ClickHouse read + ClickHouse batch write; bounds memory and gives a resumable checkpoint every window")
	resume := fs.Bool("resume", true, "resume from the highest ledger already written to stellar.account_movements in [-from,-to], if any (data-derived — see doc comment)")
	write := fs.Bool("write", false, "actually write to ClickHouse (default: dry-run, count only)")
	verify := fs.Bool("verify", false, "after each window, recount stellar.account_movements and compare against this run's decode-time per-kind counts (cheap reconciliation, not full ADR-0033 machinery)")
	if err := fs.Parse(args); err != nil {
		return err
	}
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
		maxLedger, found, merr := clickhouse.MaxAccountMovementLedger(ctx, *chAddr, startLedger, clampedTo)
		if merr != nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: resume lookup failed (%v) — starting from -from\n", merr)
		} else if found && maxLedger >= startLedger {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: resuming at ledger %d (highest ledger already in stellar.account_movements for this range; that ledger is deliberately re-processed, not skipped)\n", maxLedger)
			startLedger = maxLedger
		}
	}

	mode := "DRY-RUN (count only)"
	if *write {
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
		res, werr := classicMovementsAttemptWindow(windowCtx, *chAddr, dec, opTypes, entryChangeOpTypes, wlo, whi, *write, *verify)
		windowCancel()
		if werr != nil && errors.Is(werr, context.DeadlineExceeded) && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] hit its %s deadline (2026-07-12 half-dead-connection stall class) — retrying once on fresh connections\n",
				wlo, whi, classicMovementsWindowDeadline)
			// Discard stale pending claim/clawback refs the failed
			// attempt accumulated via dec.Decode before it stalled — the
			// retry re-decodes this same window from scratch and
			// re-records whatever is still genuinely unresolved. Without
			// this, the retry's own TakePendingClaimableBalances would
			// double up on refs the failed attempt already queued.
			dec.TakePendingClaimableBalances()
			windowCtx2, cancel2 := context.WithTimeout(ctx, classicMovementsWindowDeadline)
			res, werr = classicMovementsAttemptWindow(windowCtx2, *chAddr, dec, opTypes, entryChangeOpTypes, wlo, whi, *write, *verify)
			cancel2()
		}
		if werr != nil {
			if errors.Is(werr, context.Canceled) || ctx.Err() != nil {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: cancelled mid-window [%d,%d] — resume will pick up at %d\n", wlo, whi, wlo)
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
	if *write {
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
		fmt.Printf("\nNOTE: %d LiquidityPoolDeposit/Withdraw ops and %d AllowTrust/SetTrustLineFlags checks were skipped for lack of ledger_entry_changes fidelity in this range (research §3.2 — Phase 0's ch-backfill hasn't reached it yet). Re-running this same range after Phase 0 backfills it resolves these; ClickHouse's ReplacingMergeTree makes that safe.\n",
			totalLPUnavailable, totalCAP0038Skipped)
	}
	if totalVerifyMismatches > 0 {
		fmt.Printf("\nWARNING: %d window(s) had a movement_kind count mismatch between this run's decode and stellar.account_movements — see stderr for the per-window detail. Common benign causes: a concurrent write to the SAME range from another invocation, or un-merged ReplacingMergeTree parts still settling; a persistent mismatch after a quiet re-run warrants investigation.\n", totalVerifyMismatches)
	}
	return nil
}

// windowResult holds everything one call to classicMovementsAttemptWindow
// produces for a single [wlo,whi] window: the pre-fan-out batch ready
// for clickhouse.InsertAccountMovements, the per-movement_kind counts
// for this window alone, and every per-window counter the summary
// report accumulates into its run-level totals. Deliberately holds
// NOTHING run-level (no reference to the outer counts map or the
// totalXxx accumulators) — the caller only merges a windowResult into
// run state after classicMovementsAttemptWindow returns successfully,
// which is what makes retrying a timed-out window safe: a failed
// attempt's windowResult is simply discarded, never partially merged.
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

// classicMovementsAttemptWindow runs ONE attempt at decoding, writing,
// and verifying the [wlo,whi] window — everything classicMovementsBackfill's
// loop body used to do inline before the 2026-07-12 half-dead-connection
// stall motivated a per-window deadline with a single retry (see
// classicMovementsWindowDeadline). Pulled into its own named function,
// rather than left as a closure in the loop, to keep the caller's
// gocognit/gocyclo/funlen complexity down now that it also has to
// juggle the retry-once control flow; the four phases below are
// FURTHER split into their own named functions for the same reason —
// see each one's doc comment.
//
// dec is the SAME classicmovements.Decoder instance across every
// window and every retry, by design — its in-memory claimable-balance-
// create index legitimately spans windows (ADR-0047 Phase 3), and
// re-decoding the same ops on a retry re-inserts the same map keys,
// which is idempotent. The one piece of dec's state that is NOT safe
// to carry from a failed attempt into its retry is dec.pending — the
// caller drains and discards it before retrying (see the loop body).
//
// Touches ONLY window-local state (the returned windowResult) plus
// dec's shared, cross-window index — never the caller's run-level
// counts map or totalXxx accumulators, so a failed attempt can be
// discarded cleanly by the caller without unwinding any global
// mutation. winCtx is threaded through every ClickHouse call (in every
// phase below) so the per-window deadline actually bounds the whole
// attempt, not just the first read.
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
	classicMovementsResolvePendingClaimableBalances(winCtx, chAddr, dec, wlo, whi, &res)
	if err := classicMovementsDecodeEntryChangesSurface(winCtx, chAddr, entryChangeOpTypes, wlo, whi, &res); err != nil {
		return res, err
	}
	if err := classicMovementsWriteAndVerify(winCtx, chAddr, wlo, whi, write, verify, &res); err != nil {
		return res, err
	}
	return res, nil
}

// classicMovementsDecodeOpsSurface streams + decodes the op-only
// surface (classicmovements.SupportedOpTypes / Decoder.Decode) for
// one window — Phases 1-3 plus Phase 4's AccountMerge — accumulating
// into res. dec.Decode also updates dec's shared claimable-balance-
// create index as a side effect (see classicmovements.Decoder's doc
// comment); that index feeds
// classicMovementsResolvePendingClaimableBalances, called next.
func classicMovementsDecodeOpsSurface(winCtx context.Context, chAddr string, dec *classicmovements.Decoder, opTypes []string, wlo, whi uint32, res *windowResult) error {
	werr := clickhouse.StreamClassicOps(winCtx, chAddr, wlo, whi, opTypes, func(op clickhouse.ClassicOp) error {
		res.windowRead++
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
			// Non-fatal per the OpDecoder contract (count + skip). In
			// practice this should only ever be ErrMalformedMovement —
			// StreamClassicOps already scoped the CH read to opTypes, so
			// ErrUnsupportedOpType should never fire here.
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: decode error (ledger %d tx %s op %d): %v\n",
				op.Ledger, op.TxHash, op.OpIndex, derr)
			return nil
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
		return nil
	})
	if werr != nil {
		return fmt.Errorf("stream classic ops [%d,%d]: %w", wlo, whi, werr)
	}
	return nil
}

// classicMovementsResolvePendingClaimableBalances is the ADR-0047
// Phase 3 second pass: resolve claim/clawback rows the main decode
// loop couldn't correlate against a create seen earlier in this
// window (dec.decodeOp records these instead of failing). Try the
// free in-memory re-check first (closes the same-window tx_hash-
// ordering gap — see Decoder.ResolveBalance's doc comment), then fall
// back to ClickHouse for creates outside this run's range entirely
// (ADR-0048 D2: previously a Postgres lookup). Still-unresolved
// entries are a genuine ADR-0047 D4 recognizable-incompleteness
// signal: counted and logged, never guessed. Never returns an error —
// a ClickHouse lookup failure here degrades to "counted as
// unresolved," not a window-level failure.
func classicMovementsResolvePendingClaimableBalances(winCtx context.Context, chAddr string, dec *classicmovements.Decoder, wlo, whi uint32, res *windowResult) {
	pending := dec.TakePendingClaimableBalances()
	for _, ref := range pending {
		if asset, amount, createdBy, ok := dec.ResolveBalance(ref.BalanceIDHex); ok {
			res.windowResolvedIndex++
			res.windowDecoded++
			m := classicmovements.ResolvePendingClaimableBalance(ref, asset, amount, createdBy)
			res.windowCounts[m.Kind]++
			res.batch = append(res.batch, accountMovementOf(m))
			continue
		}
		asset, amountBig, createdBy, found, ferr := clickhouse.FindClaimableBalanceCreate(winCtx, chAddr, ref.BalanceIDHex)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: FindClaimableBalanceCreate(%s) failed: %v — counting as unresolved\n",
				ref.BalanceIDHex, ferr)
			res.windowUnresolved++
			continue
		}
		if !found {
			res.windowUnresolved++
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: unresolved %s balance_id=%s ledger=%d tx=%s op=%d — no create row found (in-memory index or ClickHouse); skipping, not guessing\n",
				ref.Kind, ref.BalanceIDHex, ref.Ledger, ref.TxHash, ref.OpIndex)
			continue
		}
		res.windowResolvedCH++
		res.windowDecoded++
		m := classicmovements.ResolvePendingClaimableBalance(ref, asset, canonical.NewAmount(amountBig), createdBy)
		res.windowCounts[m.Kind]++
		res.batch = append(res.batch, accountMovementOf(m))
	}
	if len(pending) > 0 {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] claimable-balance correlation — %d resolved (index), %d resolved (clickhouse), %d unresolved\n",
			wlo, whi, res.windowResolvedIndex, res.windowResolvedCH, res.windowUnresolved)
	}
}

// classicMovementsDecodeEntryChangesSurface is the ADR-0047 Phase 4
// entry-changes-correlated surface: LiquidityPoolDeposit/Withdraw +
// the CAP-0038 AllowTrust/SetTrustLineFlags edge case. A window-level
// fidelity probe decides how "no correlated entry changes" is
// interpreted per op type — see classicMovementsBackfill's doc
// comment. Per-op handling is delegated to
// classicMovementsHandleLiquidityPoolOp /
// classicMovementsHandleCAP0038Op to keep this function's own
// complexity down.
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

	// Only bother building the claimable_balance-created index when
	// the window has fidelity at all — CAP-0038 ops are skipped
	// entirely below when it doesn't, so this would otherwise be
	// wasted work on every window until Phase 0 lands.
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

	werr2 := clickhouse.StreamClassicOps(winCtx, chAddr, wlo, whi, entryChangeOpTypes, func(op clickhouse.ClassicOp) error {
		res.windowEntryChangeRead++
		k := classicMovementOpKey{Ledger: op.Ledger, TxHash: op.TxHash, OpIndex: int32(op.OpIndex)} //nolint:gosec // OpIndex is a non-negative XDR index.
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

// classicMovementsHandleLiquidityPoolOp decodes one
// LiquidityPoolDeposit/Withdraw op against its correlated entry
// changes (lpChanges — this op's slice from
// classicMovementsDecodeEntryChangesSurface's lpChanges map, already
// keyed by the caller) and accumulates the result into res. Split out
// of the StreamClassicOps callback purely to keep
// classicMovementsDecodeEntryChangesSurface's cognitive complexity
// within the gocognit gate.
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

// classicMovementsHandleCAP0038Op decodes one AllowTrust/
// SetTrustLineFlags op for a possible CAP-0038 auto-liquidation
// against its correlated claimable_balance-created entry changes
// (cbChanges — this op's slice, already keyed by the caller) and
// accumulates the result into res. Split out of the StreamClassicOps
// callback for the same gocognit reason as
// classicMovementsHandleLiquidityPoolOp.
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

// classicMovementsWriteAndVerify persists res.batch (if -write) and
// recounts the window from ClickHouse for reconciliation (if
// -verify), both under winCtx so either can trigger the per-window
// retry same as any decode-phase call.
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

// verifyWindowCounts recounts a window from ClickHouse (via query,
// injected so this stays testable without a live connection) and
// compares per-movement_kind counts against decoded — this run's own
// windowCounts. Logs every mismatch to stderr; returns the number of
// kinds that disagreed (0 = clean). Never fatal — see the doc comment
// on classicMovementsBackfill's -verify flag.
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

// accountMovementOf converts a decode-time classicmovements.Movement
// into its clickhouse.AccountMovement storage shape — the pre-fan-out
// input to clickhouse.InsertAccountMovements. Kept local to this
// command (not internal/pipeline), like the retired
// classicMovementRowOf it replaces: classic-movements-backfill is the
// ONLY caller.
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
