package chops

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	sep41 "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ch-cap67-movements — inventory item #1 (open-fixes-inventory-2026-08-08):
// derive post-P23 account movements for EVERY asset (native XLM included)
// from the lake's own CAP-67 transfer events into
// stellar.account_movements, provenance 'cap67_derived'.
//
// WHY: the Postgres sep41_transfers tail projects only WATCHED token
// contracts — native XLM's SAC is deliberately unwatched (volume), so a
// classic-payment account's /movements feed "stopped" at the P23
// boundary (the GATL report, 2026-08-08) even though the lake captures
// all of it (native SAC: 44.76M active ledgers).
//
// SHAPE: windowed + resumable via stellar.cap67_movements_watermark
// (deploy/clickhouse/cap67_movements.sql). `-follow` runs it as the
// continuous real-time daemon (5.3): each iteration catches up from the
// watermark (or the P23 boundary on first run) to the CONTIGUOUS lake tip,
// then sleeps -follow-interval and repeats — this is the movement feed a user
// watches their transactions land on. Without -follow it is a one-shot
// catch-up that exits at the tip (manual -from/-to backfills). Idempotent:
// account_movements is a ReplacingMergeTree keyed
// (address, ledger, tx_hash, op_index, leg_index, direction), so re-derives
// collapse. Scope is event_kind 'transfer' only — exact parity with the
// Postgres tail this replaces (mint/burn are supply events, served elsewhere).
//
// The API's movements handler floors its Postgres arm at this job's
// watermark, so at ANY backfill progress the two arms are gap-free and
// double-count-free.
func chCap67Movements(args []string) error {
	fs := flag.NewFlagSet("ch-cap67-movements", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or the P23 boundary on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip)")
	window := fs.Uint("window", 50_000, "ledgers per derive window")
	follow := fs.Bool("follow", false, "run continuously as a daemon: after each catch-up, sleep -follow-interval and derive again, following the lake tip. The movement feed's real-time mechanism (a user watches their transactions land). Always resumes from the watermark to the CONTIGUOUS tip — ignores -from/-to.")
	followInterval := fs.Duration("follow-interval", 2*time.Second, "sleep between catch-ups in -follow mode")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *window == 0 {
		return fmt.Errorf("-window must be > 0")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	if *follow {
		if *from != 0 || *to != 0 {
			return fmt.Errorf("-follow always resumes from the watermark to the contiguous tip; do not combine with -from/-to")
		}
		if dryRun {
			// A dry-run never advances the watermark, so -follow would re-stream
			// the ENTIRE backlog on every tick, forever. The daemon must write.
			return fmt.Errorf("-follow requires -write: a dry-run never advances the watermark and would re-derive the whole backlog each tick")
		}
		return runCap67Follow(ctx, *chAddr, uint32(*window), dryRun, *followInterval) //nolint:gosec // window fits uint32
	}

	if _, err := runCap67CatchUp(ctx, *chAddr, uint32(*from), uint32(*to), uint32(*window), dryRun); err != nil { //nolint:gosec // ledger sequences fit uint32
		return err
	}
	return nil
}

// runCap67CatchUp resolves the derive range [from|watermark, to|contiguous-tip]
// and streams every window into account_movements, advancing the watermark
// after each window. Returns the total movement rows derived; a no-op
// (0 rows, nil) when already at/past the contiguous tip. Gated on the
// contiguous watermark via Cap67Range, so it never steps past a lake hole.
func runCap67CatchUp(ctx context.Context, chAddr string, from, to, window uint32, dryRun bool) (int64, error) {
	start, last, err := Cap67Range(ctx, chAddr, from, to)
	if err != nil {
		return 0, err
	}
	if last < start {
		return 0, nil // already at/past the contiguous tip — nothing to do
	}

	runStart := time.Now()
	var totalRows int64
	for lo := start; ; {
		hi := last
		if rem := last - lo; rem >= window {
			hi = lo + window - 1
		}
		n, err := deriveCap67MovementsWindow(ctx, chAddr, lo, hi, dryRun)
		if err != nil {
			return totalRows, fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		totalRows += n
		if !dryRun {
			if err := clickhouse.SetCap67MovementsWatermark(ctx, chAddr, hi); err != nil {
				return totalRows, fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
		}
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d] done — %d movement rows (total %d, elapsed %s)\n",
			lo, hi, n, totalRows, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return totalRows, nil
		}
		lo = hi + 1
	}
}

// runCap67Follow is the persistent-daemon loop that makes the movement feed
// real-time: catch up to the contiguous tip, sleep `interval`, repeat, until
// ctx is cancelled (SIGTERM ⟹ graceful shutdown). A transient catch-up error
// is logged and retried on the next tick — the watermark holds, so NO ledger
// is skipped — while a ctx-cancel ends the loop cleanly. Crash-safe: on
// restart it resumes from the persisted watermark.
func runCap67Follow(ctx context.Context, chAddr string, window uint32, dryRun bool, interval time.Duration) error {
	fmt.Fprintf(os.Stderr, "ch-cap67-movements: FOLLOW mode — catch-up every %s, gated on the contiguous watermark, on %s\n", interval, chAddr)
	return followLoop(ctx, interval, func(ctx context.Context) error {
		n, err := runCap67CatchUp(ctx, chAddr, 0, 0, window, dryRun)
		if err == nil && n > 0 {
			fmt.Fprintf(os.Stderr, "ch-cap67-movements: follow tick derived %d movement rows\n", n)
		}
		return err
	})
}

// followLoop runs catchUp immediately and then once every `interval` until ctx
// is cancelled. A catchUp error is logged and RETRIED on the next tick — the
// derive advances its watermark only after a clean window, so a failed tick
// skips NO ledger — while a ctx-cancel (mid-derive or between ticks) ends the
// loop cleanly (SIGTERM ⟹ graceful shutdown; on restart the daemon resumes
// from the persisted watermark). Extracted from runCap67Follow so the loop's
// shutdown + error-resilience contract is unit-testable without a live
// ClickHouse.
func followLoop(ctx context.Context, interval time.Duration, catchUp func(context.Context) error) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := catchUp(ctx); err != nil {
			if ctx.Err() != nil {
				return nil // shutdown mid-derive — clean exit
			}
			fmt.Fprintf(os.Stderr, "ch-cap67-movements: catch-up error (watermark holds, retrying next tick): %v\n", err)
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "ch-cap67-movements: follow mode stopping (context cancelled)")
			return nil
		case <-ticker.C:
		}
	}
}

// Cap67Range resolves the derive range: from=0 resumes from the
// watermark (P23 boundary on first run); to=0 targets the lake tip.
func Cap67Range(ctx context.Context, chAddr string, from, to uint32) (uint32, uint32, error) {
	start := from
	if start == 0 {
		wm, err := clickhouse.Cap67MovementsWatermark(ctx, chAddr)
		if err != nil {
			return 0, 0, fmt.Errorf("read watermark: %w", err)
		}
		if wm == 0 {
			wm = timescale.SEP41MovementsFloorLedger - 1
		}
		start = wm + 1
	}
	last := to
	if last == 0 {
		// CONTIGUITY GATE (money-display correctness): the LiveSink drops
		// whole ledgers under buffer pressure, so the lake can have holes
		// near the tip. Reading to the raw max would derive PAST a hole and
		// advance the watermark past it — and since we resume from
		// watermark+1 with no trailing re-derive, that ledger's classic/native
		// movements would be LOST PERMANENTLY (the raw lake self-heals via
		// ch-live-catchup, but account_movements never revisits it). Clamp the
		// upper bound to the contiguous watermark from `start` — the same
		// guard the real-time projector uses (projector.resolveTip) — so the
		// derive STALLS at a hole (delayed, not lost) until catch-up heals it.
		// Keyed off stellar.ledgers, the per-ledger commit marker flushed LAST:
		// present-in-ledgers ⟹ that ledger's contract_events are durable.
		// Returns start-1 when start is itself a hole / the lake hasn't reached
		// it, which the caller's `last < start` guard treats as "nothing to do".
		tip, err := clickhouse.ContiguousWatermark(ctx, chAddr, start)
		if err != nil {
			return 0, 0, fmt.Errorf("resolve contiguous lake tip: %w", err)
		}
		last = tip
	}
	return start, last, nil
}

// cap67InsertBatch bounds one InsertAccountMovements flush.
const cap67InsertBatch = 50_000

// deriveCap67MovementsWindow streams one ledger window's transfer events
// and writes the fanned-out movement rows.
func deriveCap67MovementsWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool) (int64, error) {
	// Decoder with an empty watched set: Decode() classifies by topic
	// alone; Matches() (the watched-set gate) is deliberately NOT
	// consulted — this job's whole point is covering the unwatched
	// contracts (native XLM above all).
	dec := sep41.NewUngatedDecoder()

	var (
		batch   []clickhouse.AccountMovement
		written int64
		decErrs int64
	)
	flush := func() error {
		if len(batch) == 0 || dryRun {
			written += int64(len(batch))
			batch = batch[:0]
			return nil
		}
		n, ierr := clickhouse.InsertAccountMovements(ctx, addr, batch)
		if ierr != nil {
			return ierr
		}
		written += n
		batch = batch[:0]
		return nil
	}

	err := clickhouse.StreamContractEventsFiltered(ctx, addr, lo, hi,
		nil, []string{"transfer"}, nil,
		false, // no FINAL — RMT dups collapse in the idempotent target
		false, // no OpArgs
		false, // no state-write keys
		func(ev events.Event) error {
			m, ok := cap67MovementFromEvent(dec, &ev)
			if !ok {
				decErrs++
				return nil
			}
			batch = append(batch, m)
			if len(batch) >= cap67InsertBatch {
				return flush()
			}
			return nil
		})
	if err != nil {
		return written, err
	}
	if err := flush(); err != nil {
		return written, err
	}
	if decErrs > 0 {
		// Visible, not fatal: a deterministically undecodable transfer
		// event re-fails on every retry; the raw event stays in
		// contract_events for audit.
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d]: %d events skipped (decode)\n", lo, hi, decErrs)
	}
	return written, nil
}

// cap67MovementFromEvent decodes one transfer event to its movement.
// ok=false skips (non-transfer classification, decode failure, or an
// unusable close time).
func cap67MovementFromEvent(dec *sep41.Decoder, ev *events.Event) (clickhouse.AccountMovement, bool) {
	outs, err := dec.Decode(*ev)
	if err != nil || len(outs) == 0 {
		return clickhouse.AccountMovement{}, false
	}
	tr, ok := outs[0].(sep41.Event)
	if !ok || tr.Kind != "transfer" {
		return clickhouse.AccountMovement{}, false
	}
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return clickhouse.AccountMovement{}, false
	}
	return clickhouse.AccountMovement{
		MovementKind:    "transfer",
		Provenance:      clickhouse.ProvenanceCAP67Derived,
		Ledger:          ev.Ledger,
		LedgerCloseTime: closedAt.UTC(),
		TxHash:          ev.TxHash,
		OpIndex:         uint32(ev.OperationIndex), //nolint:gosec // non-negative by spec
		LegIndex:        uint32(ev.EventIndex),     //nolint:gosec // non-negative by spec
		Asset:           cap67AssetName(ev),
		Amount:          tr.Amount,
		FromAddress:     tr.FromAddr,
		ToAddress:       tr.ToAddr,
	}, true
}

// scvalText extracts the text of an ScvString or ScvSymbol — the two
// encodings the CAP-67 sep0011 topic appears with on the wire.
func scvalText(sv xdr.ScVal) (string, bool) {
	if s, err := scval.AsString(sv); err == nil {
		return s, true
	}
	if sv.Type == xdr.ScValTypeScvSymbol && sv.Sym != nil {
		return string(*sv.Sym), true
	}
	return "", false
}

// sep0011Re matches the CAP-67 4th-topic asset string: "native" or
// "CODE:GISSUER" (1-12 alphanumeric code).
var sep0011Re = regexp.MustCompile(`^[A-Za-z0-9]{1,12}:G[A-Z2-7]{55}$`)

// cap67AssetName resolves the movement's `asset` column value. 4-topic
// CAP-67 events carry the sep0011 asset name in the trailing topic —
// mapped to the archive's canonical form ("native" / "CODE-GISSUER",
// matching every classic_derived row). 3-topic events are pure Soroban
// tokens: the contract id IS the asset identity (same fallback the
// Postgres-tail mapper uses).
func cap67AssetName(ev *events.Event) string {
	if len(ev.Topic) != 4 {
		return ev.ContractID
	}
	sv, err := scval.Parse(ev.Topic[3])
	if err != nil {
		return ev.ContractID
	}
	s, ok := scvalText(sv)
	if !ok {
		return ev.ContractID
	}
	if s == "native" {
		return "native"
	}
	if sep0011Re.MatchString(s) {
		return strings.Replace(s, ":", "-", 1)
	}
	return ev.ContractID
}
