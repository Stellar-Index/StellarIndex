package chops

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41 "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ch-cap67-movements derives post-P23 account movements for EVERY asset (native XLM included) from the lake's CAP-67 transfer, mint, burn
// and clawback events into stellar.account_movements (provenance 'cap67_derived'); the Postgres sep41_transfers tail covers only WATCHED
// contracts. Windowed and resumable via stellar.cap67_movements_watermark, which the API's movements handler floors its Postgres arm at
// (gap- and double-count-free).
func chCap67Movements(args []string) error {
	fs := flag.NewFlagSet("ch-cap67-movements", flag.ContinueOnError)
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	from := fs.Uint("from", 0, "first ledger (0 = resume from the watermark, or the P23 boundary on first run)")
	to := fs.Uint("to", 0, "last ledger (inclusive; 0 = current contiguous lake tip). Always CLAMPED DOWN to the contiguous tip: a -to above a near-tip lake hole would derive past it and stamp the watermark beyond it, losing those ledgers' movements permanently.")
	window := fs.Uint("window", 50_000, "ledgers per derive window")
	follow := fs.Bool("follow", false, "run continuously as a daemon: after each catch-up, sleep -follow-interval and derive again, following the lake tip. The movement feed's real-time mechanism (a user watches their transactions land). Always resumes from the watermark to the CONTIGUOUS tip — ignores -from/-to.")
	followInterval := fs.Duration("follow-interval", 1*time.Second, "sleep between catch-ups in -follow mode. Kept ≥~0.5s: each tick re-scans the derive window, and sub-second ticks add ClickHouse read + small-write pressure for a latency gain the ~5s ledger cadence + upstream ingest already dominate.")
	floorLedger := fs.Uint("floor-ledger", uint(timescale.SEP41MovementsFloorLedger), "first-run watermark floor — the P23/CAP-67 boundary this derive starts from BEFORE any watermark exists. Defaults to the pubnet P23 boundary; set to the chain's start on testnet/futurenet, where the whole chain is post-P23 (otherwise the derive floors ABOVE every ledger the net has and produces nothing). A first run clamps the floor UP to the lake's first ledger (no lake holds genesis=1), so 1 and 2 behave alike.")
	network := fs.String("network", "pubnet", "Stellar network the lake belongs to (pubnet / testnet / futurenet). A CAP-67 transfer's asset label is trusted only when the emitting contract is that asset's SAC on THIS network; a wrong value labels every genuine SAC transfer by its contract id.")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges (INV-0793). Empty = "+opsutil.DefaultTextfileDir+"/ops_job_ch_cap67_movements.prom when that directory exists (r1), otherwise no heartbeat at all")
	maxDecodeErrs := registerDecodeBudget(fs)
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	passphrase, err := cap67NetworkPassphrase(*network)
	if err != nil {
		return err
	}
	canonical.InstallNetworkPassphrase(passphrase)
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

	// Without a metric on the watermark, a wedged -follow daemon looks like a healthy one.
	// record reports per window, not at run end, since a first-run backfill can take hours and
	// would leave the cursor flat, reading as hung.
	prog := newCap67Progress(*heartbeat)
	ok := false
	defer func() { prog.stop(ok) }()

	if *follow {
		if *from != 0 || *to != 0 {
			return fmt.Errorf("-follow always resumes from the watermark to the contiguous tip; do not combine with -from/-to")
		}
		if dryRun {
			// A dry-run never advances the watermark, so -follow would re-stream
			// the ENTIRE backlog on every tick, forever. The daemon must write.
			return fmt.Errorf("-follow requires -write: a dry-run never advances the watermark and would re-derive the whole backlog each tick")
		}
		err := runCap67Follow(ctx, *chAddr, uint32(*window), dryRun, *followInterval, uint32(*floorLedger), prog.record) //nolint:gosec // window/floor fit uint32
		ok = err == nil
		return err
	}

	res, err := cap67CatchUpOnce(ctx, *chAddr, uint32(*from), uint32(*to), uint32(*window), dryRun, uint32(*floorLedger), prog.record) //nolint:gosec // ledger sequences fit uint32
	if err != nil {
		return err
	}
	if *from == 0 && *to == 0 && !dryRun {
		fill := &cap67SupplyFill{chAddr: *chAddr, window: uint32(*window), floorLedger: uint32(*floorLedger), progress: prog.record} //nolint:gosec // window/floor fit uint32
		skipped, err := fill.drain(ctx)
		res.skipped += skipped
		if err != nil {
			return fmt.Errorf("supply fill: %w", err)
		}
	}
	err = enforceDecodeBudget("ch-cap67-movements", res.skipped, *maxDecodeErrs)
	ok = err == nil
	return err
}

// cap67Progress accumulates the derived-row total (cumulative across ticks, so small steady ticks still read as
// progress) and delegates to opsutil.JobHeartbeat.
type cap67Progress struct {
	hb    *opsutil.JobHeartbeat
	mu    sync.Mutex
	total int64
}

// newCap67Progress starts the heartbeat; heartbeatPath is the -heartbeat flag (opsutil.NewJobHeartbeat, inert off r1).
func newCap67Progress(heartbeatPath string) *cap67Progress {
	hb := opsutil.NewJobHeartbeat("ch-cap67-movements", heartbeatPath, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: heartbeat -> %s\n", hb.Path())
	}
	hb.Start()
	return &cap67Progress{hb: hb}
}

// record is the per-window progress callback for runCap67CatchUp: rows is the window's derived-row count, cursor the
// highest ledger reached.
func (p *cap67Progress) record(rows int64, cursor uint32) {
	p.mu.Lock()
	p.total += rows
	total := p.total
	p.mu.Unlock()
	p.hb.Progress(uint64(total), uint64(cursor)) //nolint:gosec // total/cursor are non-negative by construction
}

// stop writes the heartbeat's terminal state. Deferred so it runs on every
// exit path (including a signal-cancelled -follow).
func (p *cap67Progress) stop(ok bool) {
	p.hb.Stop(ok)
}

// cap67NetworkPassphrase maps -network to the passphrase SAC ids derive
// against; an unknown name is refused rather than defaulted.
func cap67NetworkPassphrase(network string) (string, error) {
	p := config.StellarConfig{Network: network}.Passphrase()
	if p == "" {
		return "", fmt.Errorf("-network %q: want pubnet, testnet or futurenet", network)
	}
	return p, nil
}

// cap67CatchUpOnce is a var so the exit-status contract is testable without ClickHouse; -follow keeps the per-window
// log only, as a daemon cannot exit on a deterministic skip without crash-looping.
var cap67CatchUpOnce = runCap67CatchUp

// cap67CatchUp is what one catch-up did. Idle is a property of the range, not the row count: an event-free window
// derives 0 rows yet advances the watermark.
type cap67CatchUp struct {
	start, last uint32
	rows        int64
	skipped     uint64 // transfer events dropped as undecodable
}

// idle reports an empty resolved range: the watermark is at or above the contiguous tip, or `start` is not in the lake.
func (c cap67CatchUp) idle() bool { return c.last < c.start }

// runCap67CatchUp streams the range into account_movements, advancing the watermark per window; it never steps past a lake hole (Cap67Range
// clamps, each advance re-proves its window hole-free). progress, when non-nil, runs per window so a long -follow tick shows a stalled
// watermark.
func runCap67CatchUp(ctx context.Context, chAddr string, from, to, window uint32, dryRun bool, floorLedger uint32, progress func(rows int64, cursor uint32)) (cap67CatchUp, error) {
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
			// The advance re-proves [lo,hi] hole-free and refuses otherwise (ErrCap67MovementsHole,
			// ErrCap67MovementsEventShortfall), as the range was resolved once up front. A refusal
			// leaves the watermark in place; -follow retries once ch-live-catchup heals the hole.
			if err := clickhouse.SetCap67MovementsWatermark(ctx, chAddr, lo, hi); err != nil {
				return res, fmt.Errorf("advance watermark to %d: %w", hi, err)
			}
			// A failure here leaves the window outside the supply range;
			// cap67SupplyFill re-derives it from the range's top.
			if err := extendCap67SupplyCoverage(ctx, chAddr, lo, hi); err != nil {
				return res, fmt.Errorf("extend supply coverage over [%d,%d]: %w", lo, hi, err)
			}
		}
		if progress != nil {
			progress(n, hi)
		}
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d] done — %d movement rows (total %d, elapsed %s)\n",
			lo, hi, n, res.rows, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return res, nil
		}
		lo = hi + 1
	}
}

// Idle-tick policy for -follow. An idle tick resolved an empty range. A healthy feed idles a few ticks per ledger, never many
// in a row, so a long run means stalled ingest, an unhealed hole or a resume point the lake never reaches (start=1 against a
// lake beginning at 2). Respond by logging at a bounded rate and backing off the ContiguousWatermark scan.
const (
	// cap67IdleLogEvery is the consecutive-idle-tick period of the
	// "idle: start=… contiguous tip=… min_present=…" journal line.
	cap67IdleLogEvery = 30
	// cap67IdleBackoffAfter is the consecutive idle ticks run at the base interval before stretching; 30 at the default 1 s
	// is six ledger cadences, which a healthy feed never idles, so real-time latency is untouched.
	cap67IdleBackoffAfter = 30
	// cap67IdleMaxInterval caps the stretched tick. A stall that heals
	// (the catch-up timer fills the hole) is noticed within this bound.
	cap67IdleMaxInterval = 30 * time.Second
)

// followTick is the sleep after `idle` empty ticks: the base through cap67IdleBackoffAfter, then doubling up to
// cap67IdleMaxInterval; never below base, and back to it once a tick does work.
func followTick(base time.Duration, idle int) time.Duration {
	ceiling := max(cap67IdleMaxInterval, base)
	d := base
	for i := cap67IdleBackoffAfter; i < idle && d < ceiling; i++ {
		d *= 2
	}
	return min(d, ceiling)
}

// runCap67Follow is the daemon loop keeping the feed real-time until ctx is cancelled; a transient error is retried
// next tick without skipping a ledger, as the watermark holds.
func runCap67Follow(ctx context.Context, chAddr string, window uint32, dryRun bool, interval time.Duration, floorLedger uint32, progress func(rows int64, cursor uint32)) error {
	fmt.Fprintf(os.Stderr, "ch-cap67-movements: FOLLOW mode — catch-up every %s, gated on the contiguous watermark, on %s\n", interval, chAddr)
	fill := &cap67SupplyFill{chAddr: chAddr, window: window, floorLedger: floorLedger, progress: progress}
	return followLoop(ctx, interval, func(ctx context.Context, idle int) (bool, error) {
		res, err := runCap67CatchUp(ctx, chAddr, 0, 0, window, dryRun, floorLedger, progress)
		if err != nil {
			return false, err
		}
		if filled := fill.tick(ctx); !res.idle() || filled {
			if res.rows > 0 {
				fmt.Fprintf(os.Stderr, "ch-cap67-movements: follow tick derived %d movement rows\n", res.rows)
			}
			return true, nil
		}
		// Every cap67IdleLogEvery consecutive idle ticks, name the stall: the resume point, the tip
		// it waits on and the lowest ledger the lake holds, which separates a lake beginning above
		// the resume point (never resolves) from a hole or lagging ingest (will).
		if n := idle + 1; n%cap67IdleLogEvery == 0 {
			cap67IdleLogTick(ctx, chAddr, res, n, interval, clickhouse.LakeMinLedger)
		}
		return false, nil
	})
}

// cap67IdleLogTick emits the idle diagnostic. Its min-present read failure is logged, not returned: an error would reach
// followLoop, which retries on the same idle count and would hammer LakeMinLedger every tick.
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

// followLoop runs catchUp now and after each sleep (followTick of the idle count) until ctx is cancelled. An error is
// retried with the idle count unchanged and skips no ledger, as the watermark advances only after a clean window.
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

// resolveStart is the first ledger a run derives: wm+1 when resumed, else the floor clamped up to the lake's lowest ledger, since
// ContiguousWatermark reads a `from` the lake lacks as a hole and would idle forever. An empty lake leaves the floor alone.
func resolveStart(wm, floor, lakeMin uint32) uint32 {
	if wm > 0 {
		return wm + 1
	}
	return max(floor, lakeMin)
}

// Cap67Range resolves the derive range: from=0 resumes from the watermark (see resolveStart), and the upper bound is the contiguous lake tip, with a non-zero `to` clamped to it.
func Cap67Range(ctx context.Context, chAddr string, from, to, floorLedger uint32) (uint32, uint32, error) {
	return resolveDeriveRange(ctx, chAddr, "ch-cap67-movements", from, to, floorLedger, clickhouse.Cap67MovementsWatermark)
}

// resolveDeriveRange is Cap67Range for any watermark-resumed lake derive:
// readWM is the derive's own watermark and verb names it in the log.
func resolveDeriveRange(ctx context.Context, chAddr, verb string, from, to, floorLedger uint32, readWM func(context.Context, string) (uint32, error)) (uint32, uint32, error) {
	if floorLedger == 0 {
		// Defensive: a first run starts AT the floor, and genesis is ledger
		// 1 — a floor of 0 is not a ledger. The CLI already rejects
		// -floor-ledger 0; this guards other callers.
		floorLedger = 1
	}
	start := from
	if start == 0 {
		wm, err := readWM(ctx, chAddr)
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
	// Contiguity gate: the LiveSink drops whole ledgers under buffer pressure, so the lake can have holes near the tip, and
	// deriving past one would advance the watermark over it and lose that ledger's movements permanently. Clamp to the
	// contiguous watermark (as projector.resolveTip does), keyed off stellar.ledgers, the commit marker flushed last; it
	// returns start-1 when start is a hole, which the caller's `last < start` guard treats as nothing to do. The clamp is
	// unconditional: an operator-supplied -to is min()'d against the tip, or `-to N` would walk past a hole below N and stamp
	// the watermark over it.
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
		fmt.Fprintf(os.Stderr, "%s: -to %d is above the contiguous lake tip %d — deriving through %d only; re-run once the lake is contiguous through %d (an unhealed hole below it, or ingest has yet to reach it)\n",
			verb, to, tip, last, to)
	}
	return start, last, nil
}

// cap67InsertBatch bounds one InsertAccountMovements flush.
const cap67InsertBatch = 50_000

// deriveCap67MovementsWindow streams one window's transfer events and writes the fanned-out rows, returning rows
// written and events skipped as undecodable.
func deriveCap67MovementsWindow(ctx context.Context, addr string, lo, hi uint32, dryRun bool) (int64, uint64, error) {
	return deriveCap67Window(ctx, addr, lo, hi, cap67MovementTopics, dryRun)
}

// deriveCap67Window is deriveCap67MovementsWindow over the given topic[0]
// symbols only.
func deriveCap67Window(ctx context.Context, addr string, lo, hi uint32, topics []string, dryRun bool) (int64, uint64, error) {
	// Decoders with an empty watched set: Decode() classifies by topic alone; Matches() (the watched-set gate) is deliberately
	// NOT consulted — this job's whole point is covering the unwatched contracts (native XLM above all).
	dec := sep41.NewUngatedDecoder()
	sup := sep41supply.NewUngatedDecoder()

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
		nil, topics, nil,
		false, // no FINAL — RMT dups collapse in the idempotent target
		false, // no OpArgs
		false, // no state-write keys
		func(ev events.Event) error {
			m, ok := cap67EventMovement(dec, sup, &ev)
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
		// Not fatal per window: a deterministically undecodable transfer event re-fails on every retry; the raw event stays in
		// contract_events for audit. The one-shot run's exit status accounts for it via enforceDecodeBudget.
		fmt.Fprintf(os.Stderr, "ch-cap67-movements: window [%d,%d]: %d events skipped (decode)\n", lo, hi, decErrs)
	}
	return written, decErrs, nil
}

// streamCap67TransferEvents is clickhouse.StreamContractEventsFiltered, a
// var so a window's derive is testable without a live lake.
var streamCap67TransferEvents = clickhouse.StreamContractEventsFiltered

// cap67FannedOutRows is the row count InsertAccountMovements writes (one or two per movement), so a dry run reports
// the same unit as a write run.
func cap67FannedOutRows(batch []clickhouse.AccountMovement) int64 {
	var n int64
	for _, m := range batch {
		n += int64(len(clickhouse.FanOutAccountMovement(m)))
	}
	return n
}

// cap67MovementTopics are the topic[0] symbols the derive streams: every
// CAP-67 event that moves a balance between an account and something else.
var cap67MovementTopics = append([]string{sep41.SymbolTransfer}, cap67SupplyTopics...)

// cap67SupplyTopics are the CAP-67 supply events: what cap67SupplyFill
// re-derives over ledgers whose transfers are already in the archive.
var cap67SupplyTopics = []string{sep41supply.SymbolMint, sep41supply.SymbolBurn, sep41supply.SymbolClawback}

// cap67EventMovement routes one streamed event to the decoder for its
// topic[0]; ok=false skips it as cap67MovementFromEvent does.
func cap67EventMovement(dec *sep41.Decoder, sup *sep41supply.Decoder, ev *events.Event) (clickhouse.AccountMovement, bool) {
	if len(ev.Topic) > 0 {
		switch ev.Topic[0] {
		case sep41supply.TopicSymbolMint, sep41supply.TopicSymbolBurn, sep41supply.TopicSymbolClawback:
			return cap67SupplyMovementFromEvent(sup, ev)
		}
	}
	return cap67MovementFromEvent(dec, ev)
}

// cap67MovementFromEvent decodes one transfer event; ok=false skips (non-transfer, decode failure, unusable close time).
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

// cap67SupplyMovementFromEvent decodes one mint, burn or clawback event. The other side is the issuer only when the
// emitter is a verified classic-asset SAC, else unknown: a custom token's admin is not the counterparty of record.
func cap67SupplyMovementFromEvent(dec *sep41supply.Decoder, ev *events.Event) (clickhouse.AccountMovement, bool) {
	outs, err := dec.Decode(*ev)
	if err != nil || len(outs) == 0 {
		return clickhouse.AccountMovement{}, false
	}
	se, ok := outs[0].(sep41supply.Event)
	if !ok {
		return clickhouse.AccountMovement{}, false
	}
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		return clickhouse.AccountMovement{}, false
	}
	asset, sac := cap67SACAsset(ev)
	m := clickhouse.AccountMovement{
		MovementKind:    se.Kind,
		Provenance:      clickhouse.ProvenanceCAP67Derived,
		Ledger:          ev.Ledger,
		LedgerCloseTime: closedAt.UTC(),
		TxHash:          ev.TxHash,
		OpIndex:         uint32(ev.OperationIndex), //nolint:gosec // non-negative by spec
		LegIndex:        uint32(ev.EventIndex),     //nolint:gosec // non-negative by spec
		Asset:           ev.ContractID,
		Amount:          se.Amount,
	}
	var issuer string
	if sac {
		m.Asset = asset.String()
		issuer = asset.Issuer
	}
	if se.Kind == sep41supply.SymbolMint {
		m.FromAddress, m.ToAddress = issuer, se.Counterparty
	} else {
		m.FromAddress, m.ToAddress = se.Counterparty, issuer
	}
	return m, true
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

// cap67AssetName resolves the movement's `asset`: a 4-topic transfer's sep0011 name maps to the canonical form only when
// the emitter is that asset's SAC (the stream is ungated; any contract can impersonate XLM or USDC), else the contract id.
func cap67AssetName(ev *events.Event) string {
	if len(ev.Topic) != 4 {
		return ev.ContractID
	}
	asset, ok := cap67SACAsset(ev)
	if !ok {
		return ev.ContractID
	}
	return asset.String()
}

// cap67SACAsset resolves the trailing sep0011 topic to its asset, ok only when the emitter is that asset's SAC here.
// The name is the last topic (topic[3] on transfer or legacy mint/clawback, topic[2] on CAP-67).
func cap67SACAsset(ev *events.Event) (canonical.Asset, bool) {
	if len(ev.Topic) < 3 {
		return canonical.Asset{}, false
	}
	sv, err := scval.Parse(ev.Topic[len(ev.Topic)-1])
	if err != nil {
		return canonical.Asset{}, false
	}
	s, ok := scvalText(sv)
	if !ok {
		return canonical.Asset{}, false
	}
	return canonical.SEP11SACAsset(s, ev.ContractID)
}
