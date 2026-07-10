package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/dispatcher"
	"github.com/StellarIndex/stellar-index/internal/ops/opsutil"
	"github.com/StellarIndex/stellar-index/internal/sources/classicmovements"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

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

// classicMovementsBackfill is the ADR-0047 op-only-surface write
// path (Phases 1-3 today; Phase 4 adds AccountMerge through this SAME
// SupportedOpTypes()-driven loop — Phase 4's LiquidityPoolDeposit/
// Withdraw + the CAP-0038 edge case are a separate entry-changes-
// correlated write path, not yet wired into this command):
// stellarindex-ops classic-movements-backfill -config PATH -from N
// -to N [-window N] [-resume] [-write]. Streams
// clickhouse.StreamClassicOps over windowed ledger ranges, decodes
// via classicmovements.Decoder, and batch-writes into
// classic_movements via timescale.Store.BatchInsertClassicMovements.
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
	counts := map[classicmovements.Kind]int64{}
	var totalRead, totalDecoded, totalLanded int64
	var totalResolvedIndex, totalResolvedPG, totalUnresolved int64

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
	} {
		fmt.Printf("%-24s %14d\n", k, counts[k])
	}
	fmt.Printf("%-24s %14d\n", "TOTAL ops read", totalRead)
	fmt.Printf("%-24s %14d\n", "TOTAL decoded", totalDecoded)
	fmt.Printf("%-24s %14d\n", "CB resolved (index)", totalResolvedIndex)
	fmt.Printf("%-24s %14d\n", "CB resolved (postgres)", totalResolvedPG)
	fmt.Printf("%-24s %14d\n", "CB UNRESOLVED", totalUnresolved)
	if *write {
		fmt.Printf("%-24s %14d\n", "TOTAL landed (new)", totalLanded)
	} else {
		fmt.Println("\n(dry-run — re-run with -write to persist to Postgres)")
	}
	if totalUnresolved > 0 {
		fmt.Printf("\nNOTE: %d claim/clawback ops had no resolvable create row (recognizable ADR-0047 D4 incompleteness — see stderr for the per-op log). Re-running once the create's own range has been backfilled resolves these on a subsequent pass; the PK's ON CONFLICT DO NOTHING makes that safe.\n", totalUnresolved)
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
