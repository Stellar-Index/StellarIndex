package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/ops/opsutil"
	"github.com/StellarIndex/stellar-index/internal/sources/classicmovements"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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

// classicMovementsP23StartLedger is ADR-0047 D2's hard upper bound:
// the first ledger of P23 (Whisk, 2025-09-03), from which every
// classic movement already emits a unified CAP-67 event
// (internal/sources/sep41_transfers). Pre-P23 reconstruction has
// nothing to do at or beyond this ledger.
//
// docs/architecture/pre-p23-classic-movements-research.md §1's
// ledger-boundary table confirms this exact value against
// stellar.ledgers on r1 — NOT an approximation.
const classicMovementsP23StartLedger uint32 = 58_762_517

// classicMovementsDefaultWindow is the per-window ledger span this
// command streams from ClickHouse + writes to Postgres before
// checkpointing. Bounds memory (each window's decoded batch, not the
// whole invocation, is held in-process) the same way ch-rebuild's
// maxBufferedRange guard does, and gives a resume point every window
// rather than only at the end of a multi-day run.
const classicMovementsDefaultWindow = 500_000

// classicMovementsBackfill is the ADR-0047 write path for ALL FOUR
// phases: stellarindex-ops classic-movements-backfill -config PATH
// -from N -to N [-window N] [-resume] [-write]. Each window streams
// TWO decode surfaces from ClickHouse:
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
// Both surfaces write into the SAME per-window batch via
// timescale.Store.BatchInsertClassicMovements.
//
// Phase 3's ClaimableBalance claim/clawback correlation (research
// §2's "b+own-index" path) resolves in three tiers per window:
// Decoder's free in-memory BalanceId index first, a Postgres lookup
// (timescale.Store.FindClaimableBalanceCreate) second for creates
// outside this run, and an explicit unresolved count — never a
// guessed amount — for anything neither finds. See
// classicmovements/dispatcher_adapter.go's Decoder doc for the
// memory-scaling reason operators should chunk `-from`/`-to` into
// multi-million-ledger invocations once Phase 3 volume is in play.
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
// Defaults to DRY-RUN (count only); pass -write to persist. Windowed
// + resumable: checkpoints into ingestion_cursors as
// (source="classic-movements-backfill", sub_source="<from>-<to>")
// after each window's write commits, same pattern as
// `stellarindex-ops census-backfill`. Idempotent either way — the
// classic_movements PK's ON CONFLICT DO NOTHING makes re-running an
// already-written window a no-op.
//
// -to is HARD-CLAMPED below classicMovementsP23StartLedger regardless
// of what the operator passes — loudly, via a stderr warning, never
// silently. This is the one enforcement point for ADR-0047 D2's
// "historical-only" invariant; nothing upstream (the decoder, the CH
// reader) knows about the P23 boundary at all.
func classicMovementsBackfill(args []string) error { //nolint:gocognit,gocyclo,funlen // linear: parse+clamp, resume, windowed stream+decode+write loop, checkpoint, report.
	fs := flag.NewFlagSet("classic-movements-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required) — HARD-CLAMPED below the P23 boundary (58762517) regardless of what is passed here (ADR-0047 D2: this source is historical-only)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	window := fs.Uint("window", classicMovementsDefaultWindow, "ledger-window size per streamed ClickHouse read + Postgres batch commit; bounds memory and gives a resumable checkpoint every window")
	resume := fs.Bool("resume", true, "resume from the saved cursor if a checkpoint exists for this from/to pair")
	write := fs.Bool("write", false, "actually write to Postgres (default: dry-run, count only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
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

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	const cursorSrc = "classic-movements-backfill"
	cursorSub := fmt.Sprintf("%d-%d", *from, clampedTo)
	if *resume {
		prior, gerr := store.GetCursor(ctx, cursorSrc, cursorSub)
		if gerr == nil && prior.LastLedger >= startLedger {
			startLedger = prior.LastLedger + 1
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: resuming at ledger %d (prior last_ledger=%d)\n",
				startLedger, prior.LastLedger)
		} else if gerr != nil && !errors.Is(gerr, timescale.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: read prior cursor failed (%v) — starting from -from\n", gerr)
		}
	}
	if startLedger > clampedTo {
		fmt.Fprintf(os.Stderr, "classic-movements-backfill: cursor already at or past -to (%d >= %d) — nothing to do\n",
			startLedger, clampedTo)
		return nil
	}

	mode := "DRY-RUN (count only)"
	if *write {
		mode = "WRITE"
	}
	windowSize := uint32(*window) //nolint:gosec // operator-supplied window size; zero guarded below.
	if windowSize == 0 {
		windowSize = classicMovementsDefaultWindow
	}
	fmt.Fprintf(os.Stderr, "classic-movements-backfill: [%d,%d] mode=%s window=%d ch=%s\n",
		startLedger, clampedTo, mode, windowSize, *chAddr)

	dec := classicmovements.NewDecoder()
	opTypes := classicmovements.SupportedOpTypes()
	entryChangeOpTypes := classicmovements.EntryChangeOpTypes()
	counts := map[classicmovements.Kind]int64{}
	var totalRead, totalDecoded, totalLanded int64
	var totalResolvedIndex, totalResolvedPG, totalUnresolved int64
	var totalLPUnavailable, totalCAP0038Checked, totalCAP0038Skipped, totalCAP0038Liquidations int64

	for wlo := startLedger; wlo <= clampedTo; {
		whi := wlo + windowSize - 1
		if whi > clampedTo || whi < wlo { // whi<wlo guards uint32 overflow at the top of range
			whi = clampedTo
		}

		var batch []timescale.ClassicMovementRow
		var windowRead, windowDecoded int64
		werr := clickhouse.StreamClassicOps(ctx, *chAddr, wlo, whi, opTypes, func(op clickhouse.ClassicOp) error {
			windowRead++
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
				windowDecoded++
				counts[me.Movement.Kind]++
				batch = append(batch, classicMovementRowOf(me.Movement))
			}
			return nil
		})
		if werr != nil {
			if errors.Is(werr, context.Canceled) {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: cancelled mid-window [%d,%d] — resume will pick up at %d\n", wlo, whi, wlo)
				break
			}
			return fmt.Errorf("classic-movements-backfill: stream [%d,%d]: %w", wlo, whi, werr)
		}
		totalRead += windowRead
		totalDecoded += windowDecoded

		// ADR-0047 Phase 3 second pass: resolve claim/clawback rows the
		// main decode loop couldn't correlate against a create seen
		// earlier in this window (dec.decodeOp records these instead of
		// failing). Try the free in-memory re-check first (closes the
		// same-window tx_hash-ordering gap — see Decoder.ResolveBalance's
		// doc comment), then fall back to Postgres for creates outside
		// this run's range entirely. Still-unresolved entries are a
		// genuine ADR-0047 D4 recognizable-incompleteness signal: counted
		// and logged, never guessed.
		pending := dec.TakePendingClaimableBalances()
		var windowResolvedIndex, windowResolvedPG, windowUnresolved int64
		for _, ref := range pending {
			if asset, amount, createdBy, ok := dec.ResolveBalance(ref.BalanceIDHex); ok {
				windowResolvedIndex++
				windowDecoded++
				m := classicmovements.ResolvePendingClaimableBalance(ref, asset, amount, createdBy)
				counts[m.Kind]++
				batch = append(batch, classicMovementRowOf(m))
				continue
			}
			asset, amount, createdBy, found, ferr := store.FindClaimableBalanceCreate(ctx, ref.BalanceIDHex)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: FindClaimableBalanceCreate(%s) failed: %v — counting as unresolved\n",
					ref.BalanceIDHex, ferr)
				windowUnresolved++
				continue
			}
			if !found {
				windowUnresolved++
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: unresolved %s balance_id=%s ledger=%d tx=%s op=%d — no create row found (in-memory index or Postgres); skipping, not guessing\n",
					ref.Kind, ref.BalanceIDHex, ref.Ledger, ref.TxHash, ref.OpIndex)
				continue
			}
			windowResolvedPG++
			windowDecoded++
			m := classicmovements.ResolvePendingClaimableBalance(ref, asset, amount, createdBy)
			counts[m.Kind]++
			batch = append(batch, classicMovementRowOf(m))
		}
		totalResolvedIndex += windowResolvedIndex
		totalResolvedPG += windowResolvedPG
		totalUnresolved += windowUnresolved
		if len(pending) > 0 {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] claimable-balance correlation — %d resolved (index), %d resolved (postgres), %d unresolved\n",
				wlo, whi, windowResolvedIndex, windowResolvedPG, windowUnresolved)
		}

		// ADR-0047 Phase 4 entry-changes-correlated surface:
		// LiquidityPoolDeposit/Withdraw + the CAP-0038 AllowTrust/
		// SetTrustLineFlags edge case. A window-level fidelity probe
		// decides how "no correlated entry changes" is interpreted per
		// op type — see this function's doc comment.
		fidelityCount, ferr := clickhouse.CountOpScopedEntryChanges(ctx, *chAddr, wlo, whi)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: entry-changes fidelity probe failed for [%d,%d]: %v — treating as fidelity-absent for this window\n",
				wlo, whi, ferr)
			fidelityCount = 0
		}
		fidelityPresent := fidelityCount > 0

		lpChanges := map[classicMovementOpKey][]classicmovements.EntryChangeXDR{}
		if lerr := clickhouse.StreamEntryChanges(ctx, *chAddr, wlo, whi, "liquidity_pool", func(ec clickhouse.EntryChange) error {
			k := classicMovementOpKey{Ledger: ec.Ledger, TxHash: ec.TxHash, OpIndex: ec.OpIndex}
			lpChanges[k] = append(lpChanges[k], classicmovements.EntryChangeXDR{ChangeType: ec.ChangeType, Entry: ec.Entry})
			return nil
		}); lerr != nil {
			return fmt.Errorf("classic-movements-backfill: stream liquidity_pool entry changes [%d,%d]: %w", wlo, whi, lerr)
		}

		// Only bother building the claimable_balance-created index when
		// the window has fidelity at all — CAP-0038 ops are skipped
		// entirely below when it doesn't, so this would otherwise be
		// wasted work on every window until Phase 0 lands.
		cbChanges := map[classicMovementOpKey][]classicmovements.EntryChangeXDR{}
		if fidelityPresent {
			if cerr := clickhouse.StreamEntryChanges(ctx, *chAddr, wlo, whi, "claimable_balance", func(ec clickhouse.EntryChange) error {
				if ec.ChangeType != "created" {
					return nil // CAP-0038 detection only cares about newly-created escrow rows.
				}
				k := classicMovementOpKey{Ledger: ec.Ledger, TxHash: ec.TxHash, OpIndex: ec.OpIndex}
				cbChanges[k] = append(cbChanges[k], classicmovements.EntryChangeXDR{ChangeType: ec.ChangeType, Entry: ec.Entry})
				return nil
			}); cerr != nil {
				return fmt.Errorf("classic-movements-backfill: stream claimable_balance entry changes [%d,%d]: %w", wlo, whi, cerr)
			}
		}

		var windowLPUnavailable, windowCAP0038Checked, windowCAP0038Skipped, windowCAP0038Liquidations, windowEntryChangeRead, windowEntryChangeDecoded int64
		werr2 := clickhouse.StreamClassicOps(ctx, *chAddr, wlo, whi, entryChangeOpTypes, func(op clickhouse.ClassicOp) error {
			windowEntryChangeRead++
			k := classicMovementOpKey{Ledger: op.Ledger, TxHash: op.TxHash, OpIndex: int32(op.OpIndex)} //nolint:gosec // OpIndex is a non-negative XDR index.
			switch op.Op.Body.Type {
			case xdr.OperationTypeLiquidityPoolDeposit, xdr.OperationTypeLiquidityPoolWithdraw:
				movements, derr := classicmovements.DecodeLiquidityPoolOp(op.Ledger, op.ClosedAt, op.TxHash, op.OpIndex, op.Source, op.Op, op.OpResult, lpChanges[k])
				if derr != nil {
					if errors.Is(derr, classicmovements.ErrEntryChangesUnavailable) {
						windowLPUnavailable++
						if fidelityPresent {
							fmt.Fprintf(os.Stderr, "classic-movements-backfill: ANOMALY entry-changes unavailable for LP op despite window fidelity present (ledger %d tx %s op %d) — investigate\n",
								op.Ledger, op.TxHash, op.OpIndex)
						}
						return nil
					}
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: LP decode error (ledger %d tx %s op %d): %v\n",
						op.Ledger, op.TxHash, op.OpIndex, derr)
					return nil
				}
				for _, m := range movements {
					windowEntryChangeDecoded++
					counts[m.Kind]++
					batch = append(batch, classicMovementRowOf(m))
				}
			case xdr.OperationTypeAllowTrust, xdr.OperationTypeSetTrustLineFlags:
				if !fidelityPresent {
					windowCAP0038Skipped++
					return nil
				}
				windowCAP0038Checked++
				movements, derr := classicmovements.DecodeCAP0038Revocation(op.Ledger, op.ClosedAt, op.TxHash, op.OpIndex, op.Op, op.OpResult, cbChanges[k])
				if derr != nil {
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: CAP-0038 decode error (ledger %d tx %s op %d): %v\n",
						op.Ledger, op.TxHash, op.OpIndex, derr)
					return nil
				}
				if len(movements) > 0 {
					windowCAP0038Liquidations += int64(len(movements))
					fmt.Fprintf(os.Stderr, "classic-movements-backfill: CAP-0038 auto-liquidation detected (ledger %d tx %s op %d) — %d leg(s)\n",
						op.Ledger, op.TxHash, op.OpIndex, len(movements))
				}
				for _, m := range movements {
					windowEntryChangeDecoded++
					counts[m.Kind]++
					batch = append(batch, classicMovementRowOf(m))
				}
			}
			return nil
		})
		if werr2 != nil {
			if errors.Is(werr2, context.Canceled) {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: cancelled mid-window [%d,%d] (entry-changes surface) — resume will pick up at %d\n", wlo, whi, wlo)
				break
			}
			return fmt.Errorf("classic-movements-backfill: stream entry-change ops [%d,%d]: %w", wlo, whi, werr2)
		}
		totalRead += windowEntryChangeRead
		totalDecoded += windowEntryChangeDecoded
		totalLPUnavailable += windowLPUnavailable
		totalCAP0038Checked += windowCAP0038Checked
		totalCAP0038Skipped += windowCAP0038Skipped
		totalCAP0038Liquidations += windowCAP0038Liquidations
		if windowLPUnavailable > 0 || windowCAP0038Skipped > 0 || windowCAP0038Checked > 0 {
			fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] entry-changes surface — fidelity_present=%v LP_unavailable=%d CAP0038_checked=%d CAP0038_skipped=%d CAP0038_liquidations=%d\n",
				wlo, whi, fidelityPresent, windowLPUnavailable, windowCAP0038Checked, windowCAP0038Skipped, windowCAP0038Liquidations)
		}

		if *write {
			if len(batch) > 0 {
				landed, ierr := store.BatchInsertClassicMovements(ctx, batch)
				if ierr != nil {
					return fmt.Errorf("classic-movements-backfill: write [%d,%d]: %w", wlo, whi, ierr)
				}
				totalLanded += landed
			}
			if cerr := store.UpsertCursor(ctx, cursorSrc, cursorSub, whi); cerr != nil {
				fmt.Fprintf(os.Stderr, "classic-movements-backfill: checkpoint at %d failed: %v\n", whi, cerr)
			}
		}

		fmt.Fprintf(os.Stderr, "classic-movements-backfill: window [%d,%d] done — %d ops read, %d movements decoded (running totals: read=%d decoded=%d landed=%d)\n",
			wlo, whi, windowRead, windowDecoded, totalRead, totalDecoded, totalLanded)

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
	fmt.Printf("%-24s %14d\n", "CB resolved (postgres)", totalResolvedPG)
	fmt.Printf("%-24s %14d\n", "CB UNRESOLVED", totalUnresolved)
	fmt.Printf("%-24s %14d\n", "LP entry-changes N/A", totalLPUnavailable)
	fmt.Printf("%-24s %14d\n", "CAP-0038 checked", totalCAP0038Checked)
	fmt.Printf("%-24s %14d\n", "CAP-0038 skipped (fidelity)", totalCAP0038Skipped)
	fmt.Printf("%-24s %14d\n", "CAP-0038 liquidations", totalCAP0038Liquidations)
	if *write {
		fmt.Printf("%-24s %14d\n", "TOTAL landed (new)", totalLanded)
	} else {
		fmt.Println("\n(dry-run — re-run with -write to persist to Postgres)")
	}
	if totalUnresolved > 0 {
		fmt.Printf("\nNOTE: %d claim/clawback ops had no resolvable create row (recognizable ADR-0047 D4 incompleteness — see stderr for the per-op log). Re-running once the create's own range has been backfilled resolves these on a subsequent pass; the PK's ON CONFLICT DO NOTHING makes that safe.\n", totalUnresolved)
	}
	if totalLPUnavailable > 0 || totalCAP0038Skipped > 0 {
		fmt.Printf("\nNOTE: %d LiquidityPoolDeposit/Withdraw ops and %d AllowTrust/SetTrustLineFlags checks were skipped for lack of ledger_entry_changes fidelity in this range (research §3.2 — Phase 0's ch-backfill hasn't reached it yet). Re-running this same range after Phase 0 backfills it resolves these; the PK's ON CONFLICT DO NOTHING makes that safe.\n",
			totalLPUnavailable, totalCAP0038Skipped)
	}
	return nil
}

// classicMovementRowOf converts a decode-time classicmovements.Movement
// into its timescale.ClassicMovementRow storage shape. Kept local to
// this command (not internal/pipeline) since classic-movements-backfill
// is the ONLY caller — unlike SEP41TransferRowOf/SEP41SupplyRowOf,
// which pipeline.HandleEvent's live path also needs.
func classicMovementRowOf(m classicmovements.Movement) timescale.ClassicMovementRow {
	return timescale.ClassicMovementRow{
		Kind:            timescale.ClassicMovementKind(m.Kind),
		Provenance:      timescale.ClassicMovementProvenance(m.Provenance),
		Ledger:          m.Ledger,
		LedgerCloseTime: m.LedgerCloseTime,
		TxHash:          m.TxHash,
		OpIndex:         m.OpIndex,
		LegIndex:        m.LegIndex,
		Asset:           m.Asset,
		Amount:          m.Amount,
		FromAddress:     m.FromAddress,
		ToAddress:       m.ToAddress,
		Attributes:      m.Attributes,
	}
}
