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
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always CLAMPED DOWN to the contiguous tip: a -to above a near-tip lake hole would derive past it and stamp the watermark beyond it, losing those ledgers' movements permanently.")
	window := fs.Uint("window", 50_000, "ledgers per derive window")
	follow := fs.Bool("follow", false, "run continuously as a daemon: after each catch-up, sleep -follow-interval and derive again, following the lake tip. The movement feed's real-time mechanism (a user watches their transactions land). Always resumes from the watermark to the CONTIGUOUS tip — ignores -from/-to.")
	followInterval := fs.Duration("follow-interval", 1*time.Second, "sleep between catch-ups in -follow mode. Kept ≥~0.5s: each tick re-scans the derive window, and sub-second ticks add ClickHouse read + small-write pressure for a latency gain the ~5s ledger cadence + upstream ingest already dominate.")
	floorLedger := fs.Uint("floor-ledger", uint(timescale.SEP41MovementsFloorLedger), "first-run watermark floor — the P23/CAP-67 boundary this derive starts from BEFORE any watermark exists. Defaults to the pubnet P23 boundary; set to the chain's start on testnet/futurenet, where the whole chain is post-P23 (otherwise the derive floors ABOVE every ledger the net has and produces nothing). A first run clamps the floor UP to the lake's first ledger (no lake holds genesis=1), so 1 and 2 behave alike.")
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

	if *follow {
		if *from != 0 || *to != 0 {
			return fmt.Errorf("-follow always resumes from the watermark to the contiguous tip; do not combine with -from/-to")
		}
		if dryRun {
			// A dry-run never advances the watermark, so -follow would re-stream
			// the ENTIRE backlog on every tick, forever. The daemon must write.
			return fmt.Errorf("-follow requires -write: a dry-run never advances the watermark and would re-derive the whole backlog each tick")
		}
		return runCap67Follow(ctx, *chAddr, uint32(*window), dryRun, *followInterval, uint32(*floorLedger)) //nolint:gosec // window/floor fit uint32
	}

	res, err := cap67CatchUpOnce(ctx, *chAddr, uint32(*from), uint32(*to), uint32(*window), dryRun, uint32(*floorLedger)) //nolint:gosec // ledger sequences fit uint32
	if err != nil {
		return err
	}
	return enforceDecodeBudget("ch-cap67-movements", res.skipped, *maxDecodeErrs)
}

// cap67CatchUpOnce is the one-shot derive, a var so the exit-status contract
// is testable without a live ClickHouse. -follow keeps the per-window log
// only: a daemon cannot exit on a deterministic skip without crash-looping.
var cap67CatchUpOnce = runCap67CatchUp

// cap67CatchUp is what one catch-up did: the range it resolved and the
// movement rows it derived. Idle (nothing to do) is a property of the
// RANGE, not of the row count — a window whose ledgers carry no transfer
// events derives 0 rows yet still advances the watermark, which is work.
// The follow loop keys its idle accounting off this distinction; a bare
// row count would read an event-free stretch as a stall.
type cap67CatchUp struct {
	start, last uint32
	rows        int64
	skipped     uint64 // transfer events dropped as undecodable
}

// idle reports that the resolved range was empty: the watermark already
// sits at (or above) the contiguous tip, or `start` is a ledger the lake
// does not hold.
func (c cap67CatchUp) idle() bool { return c.last < c.start }

// runCap67CatchUp resolves the derive range [from|watermark, to|contiguous-tip]
// and streams every window into account_movements, advancing the watermark
// after each window. Returns what the run did (see cap67CatchUp); an idle
// result when already at/past the contiguous tip. Gated on the contiguous
// watermark twice over, so it never steps past a lake hole: Cap67Range
// clamps the range (including an operator-supplied -to), and each advance
// re-proves its own window hole-free before the watermark moves.
func runCap67CatchUp(ctx context.Context, chAddr string, from, to, window uint32, dryRun bool, floorLedger uint32) (cap67CatchUp, error) {
	start, last, err := Cap67Range(ctx, chAddr, from, to, floorLedger)
	if err != nil {
		return cap67CatchUp{}, err
	}
	res := cap67CatchUp{start: start, last: last}
	if res.idle() {
		return res, nil // already at/past the contiguous tip — nothing to do
	}

	runStart := time.Now()
	for lo := start; ; {
		hi := last
		if rem := last - lo; rem >= window {
			hi = lo + window - 1
		}
		n, skipped, err := deriveCap67MovementsWindow(ctx, chAddr, lo, hi, dryRun)
		if err != nil {
			return res, fmt.Errorf("window [%d,%d]: %w — resume with -from %d (or no -from: the watermark holds)", lo, hi, err, lo)
		}
		res.rows += n
		res.skipped += skipped
		if !dryRun {
			// The advance re-proves [lo,hi] hole-free at the write and
			// REFUSES otherwise (ErrCap67MovementsHole): the range was
			// resolved once, up front, and the watermark is the only record
			// of what has been derived. A refusal ends the run with the
			// watermark where it was — in -follow mode the next tick retries
			// once ch-live-catchup has healed the hole.
			if err := clickhouse.SetCap67MovementsWatermark(ctx, chAddr, lo, hi); err != nil {
				return res, fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
		}
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d] done — %d movement rows (total %d, elapsed %s)\n",
			lo, hi, n, res.rows, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return res, nil
		}
		lo = hi + 1
	}
}

// Idle-tick policy for -follow mode. An idle tick is one whose catch-up
// resolved an empty range: the watermark is at the contiguous tip, or the
// resume point is a ledger the lake does not (yet) hold. A healthy feed
// idles a few ticks per ledger (5 s cadence against a 1 s tick) and never
// many in a row, so a long idle run is either upstream ingest stalled, a
// hole ch-live-catchup has yet to heal, or a resume point the lake will
// never reach — the silent-forever shape the test nets sat in for months
// (start=1 against a lake that begins at 2, 2026-09-17). Two responses:
// say so in the journal at a bounded rate, and stop paying the
// ContiguousWatermark window-function scan every second for nothing.
const (
	// cap67IdleLogEvery is the consecutive-idle-tick period of the
	// "idle: start=… contiguous tip=… min_present=…" journal line.
	cap67IdleLogEvery = 30
	// cap67IdleBackoffAfter is how many consecutive idle ticks run at the
	// base interval before the tick starts stretching. 30 at the default
	// 1 s tick is six ledger cadences — a healthy feed never idles that
	// long — so pubnet's real-time latency is untouched.
	cap67IdleBackoffAfter = 30
	// cap67IdleMaxInterval caps the stretched tick. A stall that heals
	// (the catch-up timer fills the hole) is noticed within this bound.
	cap67IdleMaxInterval = 30 * time.Second
)

// followTick is the sleep before the next catch-up after `idle`
// consecutive ticks found nothing to derive: the base interval through
// cap67IdleBackoffAfter idle ticks, then doubling per further idle tick
// up to cap67IdleMaxInterval. Never shorter than the base — an operator
// who asked for a slow tick keeps it — and back to the base the moment a
// tick does work.
func followTick(base time.Duration, idle int) time.Duration {
	ceiling := max(cap67IdleMaxInterval, base)
	d := base
	for i := cap67IdleBackoffAfter; i < idle && d < ceiling; i++ {
		d *= 2
	}
	return min(d, ceiling)
}

// runCap67Follow is the persistent-daemon loop that makes the movement feed
// real-time: catch up to the contiguous tip, sleep `interval`, repeat, until
// ctx is cancelled (SIGTERM ⟹ graceful shutdown). A transient catch-up error
// is logged and retried on the next tick — the watermark holds, so NO ledger
// is skipped — while a ctx-cancel ends the loop cleanly. Crash-safe: on
// restart it resumes from the persisted watermark.
func runCap67Follow(ctx context.Context, chAddr string, window uint32, dryRun bool, interval time.Duration, floorLedger uint32) error {
	fmt.Fprintf(os.Stderr, "ch-cap67-movements: FOLLOW mode — catch-up every %s, gated on the contiguous watermark, on %s\n", interval, chAddr)
	return followLoop(ctx, interval, func(ctx context.Context, idle int) (bool, error) {
		res, err := runCap67CatchUp(ctx, chAddr, 0, 0, window, dryRun, floorLedger)
		if err != nil {
			return false, err
		}
		if !res.idle() {
			if res.rows > 0 {
				fmt.Fprintf(os.Stderr, "ch-cap67-movements: follow tick derived %d movement rows\n", res.rows)
			}
			return true, nil
		}
		// This tick is the (idle+1)th consecutive idle one. Every
		// cap67IdleLogEvery of them, name the stall's shape: the resume
		// point, the contiguous tip it is waiting on (start-1 when the
		// resume point itself is missing) and the lowest ledger the lake
		// holds — the one figure that separates "the lake begins above
		// the resume point, so this never resolves" from "a hole or a
		// lagging ingest, so it will".
		if n := idle + 1; n%cap67IdleLogEvery == 0 {
			cap67IdleLogTick(ctx, chAddr, res, n, interval, clickhouse.LakeMinLedger)
		}
		return false, nil
	})
}

// cap67IdleLogTick emits the periodic idle-tick diagnostic line named in
// runCap67Follow's doc comment. The min-present read is diagnostic only —
// nothing derives from it — so its failure is logged inline (as
// "<unavailable: …>") rather than returned: this function's caller reports
// every tick as idle-with-no-error regardless, because a returned error
// would surface as a catchUp error to followLoop, which retries it on the
// SAME idle count forever (an error is "neither work nor idleness"), so a
// persistently-failing diagnostic read would hammer LakeMinLedger every
// tick instead of once per cap67IdleLogEvery and this line would never
// advance past it. Extracted from runCap67Follow, like followLoop itself,
// so the failure path is unit-testable without a live ClickHouse.
func cap67IdleLogTick(ctx context.Context, chAddr string, res cap67CatchUp, n int, interval time.Duration, minLedger func(context.Context, string) (uint32, error)) {
	minPresent, merr := minLedger(ctx, chAddr)
	if merr != nil {
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: idle for %d ticks — idle: start=%d contiguous tip=%d min_present=<unavailable: %v> (next tick in %s)\n",
			n, res.start, res.last, merr, followTick(interval, n))
		return
	}
	fmt.Fprintf(os.Stderr, "ch-cap67-movements: idle for %d ticks — idle: start=%d contiguous tip=%d min_present=%d (next tick in %s)\n",
		n, res.start, res.last, minPresent, followTick(interval, n))
}

// followLoop runs catchUp immediately and then again after each sleep until
// ctx is cancelled. catchUp receives the number of consecutive idle ticks so
// far and reports whether THIS tick did work; the sleep before the next tick
// is followTick of that count — the base `interval` while the feed is live,
// stretching only through a long idle run. A catchUp error is logged and
// RETRIED on the next tick — the derive advances its watermark only after a
// clean window, so a failed tick skips NO ledger — and leaves the idle count
// as it was (an error is neither work nor idleness). A ctx-cancel (mid-derive
// or between ticks) ends the loop cleanly (SIGTERM ⟹ graceful shutdown; on
// restart the daemon resumes from the persisted watermark). Extracted from
// runCap67Follow so the loop's shutdown + error-resilience + idle-accounting
// contract is unit-testable without a live ClickHouse.
func followLoop(ctx context.Context, interval time.Duration, catchUp func(ctx context.Context, idle int) (worked bool, err error)) error {
	idle := 0
	for {
		worked, err := catchUp(ctx, idle)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				// A ctx-cancel mid-derive IS the clean shutdown path: the
				// caller reads nil as "loop ended by request" and the
				// watermark already holds the last clean window.
				return nil //nolint:nilerr // shutdown by request, not a failure
			}
			fmt.Fprintf(os.Stderr, "ch-cap67-movements: catch-up error (watermark holds, retrying next tick): %v\n", err)
		case worked:
			idle = 0
		default:
			idle++
		}
		wait := time.NewTimer(followTick(interval, idle))
		select {
		case <-ctx.Done():
			wait.Stop()
			fmt.Fprintln(os.Stderr, "ch-cap67-movements: follow mode stopping (context cancelled)")
			return nil
		case <-wait.C:
		}
	}
}

// resolveStart is the first ledger a watermark-driven run derives. A
// resumed run (wm > 0) continues at wm+1. A first run starts at the floor
// — the P23 boundary on pubnet, genesis on a test net — clamped UP to the
// lake's lowest present ledger: ContiguousWatermark treats a `from` the
// lake does not hold as a boundary hole and answers from-1, so a floor
// below the lake's first ledger (genesis=1 against a lake that begins at
// 2, which is every net's lake) would idle the daemon forever without
// deriving a row — the test nets' empty account_movements archive
// (2026-09-17). A floor above the lake's start (pubnet's) is kept as is;
// an empty lake (lakeMin == 0) leaves the floor alone and the run idles
// until the lake reaches it.
func resolveStart(wm, floor, lakeMin uint32) uint32 {
	if wm > 0 {
		return wm + 1
	}
	return max(floor, lakeMin)
}

// Cap67Range resolves the derive range: from=0 resumes from the
// watermark (floorLedger on first run — the P23 boundary on pubnet, or
// genesis=1 on a test net — clamped up to the lake's first ledger, see
// resolveStart); the upper bound is the CONTIGUOUS lake tip from `start`,
// with a non-zero `to` min()'d against it (see the contiguity gate below).
func Cap67Range(ctx context.Context, chAddr string, from, to, floorLedger uint32) (uint32, uint32, error) {
	if floorLedger == 0 {
		// Defensive: a first run starts AT the floor, and genesis is ledger
		// 1 — a floor of 0 is not a ledger. The CLI already rejects
		// -floor-ledger 0; this guards other callers.
		floorLedger = 1
	}
	start := from
	if start == 0 {
		wm, err := clickhouse.Cap67MovementsWatermark(ctx, chAddr)
		if err != nil {
			return 0, 0, fmt.Errorf("read watermark: %w", err)
		}
		var lakeMin uint32
		if wm == 0 {
			// First run only: the one time the floor, rather than the
			// watermark, picks the resume point.
			lakeMin, err = clickhouse.LakeMinLedger(ctx, chAddr)
			if err != nil {
				return 0, 0, fmt.Errorf("read lake min ledger: %w", err)
			}
		}
		start = resolveStart(wm, floorLedger, lakeMin)
	}
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
	//
	// The clamp is UNCONDITIONAL: an operator-supplied -to is min()'d against
	// the tip rather than trusted. The gate used to sit inside the to == 0
	// branch, so `-to N` walked straight past a hole below N and stamped the
	// watermark at every window top on the way — the loss above, on the one
	// invocation shape an operator reaches for after an incident.
	tip, err := clickhouse.ContiguousWatermark(ctx, chAddr, start)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve contiguous lake tip: %w", err)
	}
	last := tip
	if to != 0 && to < tip {
		last = to
	}
	if to > tip {
		// Two reasons land here and the operator cannot tell them apart from
		// the bound alone: an unhealed hole below -to, or a lake that simply
		// has not reached it yet. Either way the derive is delayed, not short.
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: -to %d is above the contiguous lake tip %d — deriving through %d only; re-run once the lake is contiguous through %d (an unhealed hole below it, or ingest has yet to reach it)\n",
			to, tip, last, to)
	}
	return start, last, nil
}

// cap67InsertBatch bounds one InsertAccountMovements flush.
const cap67InsertBatch = 50_000

// deriveCap67MovementsWindow streams one ledger window's transfer events
// and writes the fanned-out movement rows, returning rows written and events
// skipped as undecodable.
func deriveCap67MovementsWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool) (int64, uint64, error) {
	// Decoder with an empty watched set: Decode() classifies by topic
	// alone; Matches() (the watched-set gate) is deliberately NOT
	// consulted — this job's whole point is covering the unwatched
	// contracts (native XLM above all).
	dec := sep41.NewUngatedDecoder()

	var (
		batch   []clickhouse.AccountMovement
		written int64
		decErrs uint64
	)
	flush := func() error {
		if len(batch) == 0 || dryRun {
			written += cap67FannedOutRows(batch)
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

	err := streamCap67TransferEvents(ctx, addr, lo, hi,
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
		return written, decErrs, err
	}
	if err := flush(); err != nil {
		return written, decErrs, err
	}
	if decErrs > 0 {
		// Not fatal per window: a deterministically undecodable transfer
		// event re-fails on every retry; the raw event stays in
		// contract_events for audit. The one-shot run's exit status
		// accounts for it via enforceDecodeBudget.
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d]: %d events skipped (decode)\n", lo, hi, decErrs)
	}
	return written, decErrs, nil
}

// streamCap67TransferEvents is clickhouse.StreamContractEventsFiltered, a
// var so a window's derive is testable without a live lake.
var streamCap67TransferEvents = clickhouse.StreamContractEventsFiltered

// cap67FannedOutRows is the row count InsertAccountMovements writes for
// batch — one or two rows per movement — so a dry run reports the same unit
// a write run does.
func cap67FannedOutRows(batch []clickhouse.AccountMovement) int64 {
	var n int64
	for _, m := range batch {
		n += int64(len(clickhouse.FanOutAccountMovement(m)))
	}
	return n
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
