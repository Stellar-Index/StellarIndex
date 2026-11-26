package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingInterestJob names the interest pass's heartbeat (its own cursor series) and log lines.
const lendingInterestJob = "ch-entry-history-lending-interest"

// lendingInterestRev is every row's derive_rev, shared with the API's reader.
const lendingInterestRev = lendinginterest.DeriveRev

// interestFlags are the -interest-* flags.
type interestFlags struct {
	init, only   bool
	prove        bool // -interest: with -prove, prove lending_interest_hourly instead of lending_state
	maxWindows   int
	maxUnordered int
	maxVersions  int // 0: lendingInterestMaxVersions
	pool         string
	recent       uint // -prove-recent: with -prove -interest, prove the last N ledgers to the watermark
}

// register defines the -interest* flags on fs.
func (f *interestFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.init, "interest-init", false, "with -lending-only: start stellar.lending_interest_hourly at the hour of max(-floor-ledger, the Blend factory genesis); without it a run whose interest pass has no watermark only logs that it is not initialised")
	fs.BoolVar(&f.only, "interest-only", false, "with -lending-only: skip the lending_state pass; -from (snapped down to its hour's first ledger) re-derives the interest rows and deletes the hours' superseded ones, -to is snapped up to its hour's last ledger")
	fs.IntVar(&f.maxWindows, "interest-max-windows", 0, "with -lending-only: stop the interest pass after N windows (0 = to lending_state's watermark)")
	fs.IntVar(&f.maxUnordered, "interest-max-unordered", 0, "with -lending-only: accept up to N versions per interest row whose apply order is unknown (intra_ledger_seq 0); more refuses the window")
	fs.Func("interest-max-versions", fmt.Sprintf("with -lending-only (and -prove -interest): hold up to N versions and cause events per window in memory (default %d, ~4 KB each); a window over it halves, an open hour over it refuses; raise it only within lending-state.service's MemoryMax", lendingInterestMaxVersions),
		func(s string) error {
			n, err := strconv.Atoi(s)
			if err != nil || n <= 0 {
				return fmt.Errorf("must be an integer > 0, got %q", s)
			}
			f.maxVersions = n
			return nil
		})
	fs.BoolVar(&f.prove, "interest", false, "with -lending-only -prove: prove stellar.lending_interest_hourly instead (reads only): per window of [-from, -to] (default the floor to the lending_interest watermark; -prove-recent the last N ledgers; -interest-pool one pool) the stored rows against a fresh fold of the lake's versions, version counts, telescoped gross and the in-line identities; exits non-zero on a mismatch or INCOMPLETE hours, 0 with nothing to prove on an uninitialised host")
	fs.UintVar(&f.recent, "prove-recent", 0, "with -lending-only -prove -interest: prove from the hour of (watermark − N ledgers), never below the floor, to the watermark instead of the whole range (the daily unit's rolling week; a spot check by hand), its verdict on the daily proof's heartbeat; without it the proof's heartbeat is the full range's, ops_job …-prove-full")
	fs.StringVar(&f.pool, "interest-pool", "", "with -lending-only: backfill the interest rows of one pool the lending_interest watermark does not cover, up to that watermark (skips the lending_state pass); with -prove -interest, prove that pool only")
}

// Seams so the interest pass is testable without a live lake.
var (
	lendingInterestWatermark     = clickhouse.LendingInterestWatermark
	lendingInterestFloor         = clickhouse.LendingInterestFloor
	lendingInterestStateWM       = clickhouse.LendingStateWatermark
	lendingInterestHour          = clickhouse.LendingInterestHour
	lendingInterestKeys          = clickhouse.LendingInterestKeys
	lendingInterestSeeds         = clickhouse.LendingInterestSeeds
	lendingInterestVersions      = clickhouse.LendingInterestVersions
	lendingInterestEvents        = clickhouse.LendingInterestEvents
	lendingInterestEventsCovered = clickhouse.LendingInterestEventsCovered
	lendingInterestState         = clickhouse.LendingInterestStateRows
	lendingInterestUncovered     = clickhouse.LendingInterestUncoveredPools
	insertLendingInterest        = clickhouse.InsertLendingInterestRows
	deleteSupersededInterest     = clickhouse.DeleteSupersededLendingInterestRows
	setLendingInterestWM         = clickhouse.SetLendingInterestWatermark
	setLendingInterestPoolWM     = clickhouse.SetLendingInterestPoolWatermark
	lendingInterestServerNow     = clickhouse.ServerNowMilli
	lendingInterestUnderived     = clickhouse.LendingInterestUnderivedPools
	lendingInterestRedo          = clickhouse.LendingInterestPendingRedo
	advanceLendingRedo           = clickhouse.AdvanceLendingInterestRedo
	lendingInterestRegistry      = lendingRegistry
	openLendingCodeHistory       = func(ctx context.Context, addr string) (lendingCodeHistory, func(), error) {
		r, err := clickhouse.NewExplorerReader(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		return r, func() { _ = r.Close() }, nil
	}
)

// lendingCodeHistory is the genesis-complete WASM timeline the replay gate reads.
type lendingCodeHistory interface {
	ReplayCodeHistory(ctx context.Context, contractID string) ([]clickhouse.ContractCodeVersion, error)
}

// interestPass is one run of the interest pass.
type interestPass struct {
	addr       string
	pools      []clickhouse.LendingContract
	opts       lendinginterest.Options
	events     interestEventDecoder
	code       map[string][]clickhouse.ContractCodeVersion
	dryRun     bool
	rederive   bool // the run starts at or below the watermark: rows it no longer writes are deleted
	maxWindows int
	// maxVersions is -interest-max-versions; 0 is lendingInterestMaxVersions.
	maxVersions int
	mark        func(ctx context.Context, from, thru uint32) error
	hb          *opsutil.JobHeartbeat
	cursorAt    *uint32 // the heartbeat cursor when it is not the run's progress
	log         io.Writer
	proof       *interestProof // set on -prove -interest: compare, never write
	// undecodable counts the cause events the run's folded reads could not decode.
	undecodable int64
}

// interestPlan is what a run derives: [start, last] over pools, marked by mark.
type interestPlan struct {
	start, last uint32
	pools       []clickhouse.LendingContract
	rederive    bool
	pool        bool // one pool's bounded backfill
	mark        func(ctx context.Context, from, thru uint32) error
}

// lendingInterestTimerBacklog is the most the timer's pass catches up: further behind is a backfill, run by hand in
// chunks under the heavy-job wrapper, never inside the 10-minute timer's unit.
const lendingInterestTimerBacklog = lendingWindow

// interestLock serialises interest runs, beside -lock-file.
const interestLock = lendingInterestJob + ".lock"

// timerInterest is the pass as lending-state.timer runs it: no -interest-only, -interest-init or -interest-pool.
func (r lendingRun) timerInterest() bool {
	return !r.interest.only && !r.interest.init && r.interest.pool == ""
}

// lockedInterestPass runs the interest pass holding -lock-file shared, so no lending_state run (which holds it
// exclusive) changes the rows it reads, and its own lock exclusive. The timer's pass skips while either is held
// against it; an explicit interest run refuses.
func (r lendingRun) lockedInterestPass(ctx context.Context, log io.Writer) error {
	unlockState, err := acquireSharedLock(lendingJob, r.lockFile)
	if err != nil {
		if r.timerInterest() && errors.Is(err, syscall.EWOULDBLOCK) {
			_, _ = fmt.Fprintf(log, "%s: skipped, a lending_state run holds its lock: %v\n", lendingInterestJob, err)
			return nil
		}
		return err
	}
	defer unlockState()
	unlock, err := acquireRollupLock(lendingInterestJob, filepath.Join(filepath.Dir(r.lockFile), interestLock))
	if err != nil {
		if r.timerInterest() && errors.Is(err, syscall.EWOULDBLOCK) {
			_, _ = fmt.Fprintf(log, "%s: skipped, another interest run holds the lock: %v\n", lendingInterestJob, err)
			return nil
		}
		return err
	}
	defer unlock()
	return r.interestPass(ctx, log)
}

// interestPass runs the interest pass after the state pass (or alone with -interest-only). The timer's run never
// starts one: with no lending_interest watermark it logs and returns, so only -interest-init begins the backfill.
func (r lendingRun) interestPass(ctx context.Context, log io.Writer) error {
	wm, err := lendingInterestWatermark(ctx, r.chAddr)
	if err != nil {
		return err
	}
	if wm == 0 && !r.interest.init {
		if !r.timerInterest() {
			return fmt.Errorf("lending interest: not initialised; start it with -interest-init")
		}
		_, _ = fmt.Fprintln(log, "lending interest: not initialised (run -interest-init)")
		return nil
	}
	if wm > 0 && r.interest.init {
		return fmt.Errorf("lending interest: already initialised through %d; drop -interest-init to resume", wm)
	}
	var hb *opsutil.JobHeartbeat
	if !r.dryRun {
		// Before planning, so a refusal there is a failed run on the pass's own series.
		hb = opsutil.NewJobHeartbeat(lendingInterestJob, r.interestHeartbeat(), nil)
		hb.Start()
		hb.Progress(0, uint64(wm))
	}
	err = r.deriveInterest(ctx, log, wm, hb)
	hb.Stop(err == nil)
	return err
}

// deriveInterest plans and runs the pass from watermark wm. The timer's pass only catches up: behind by more than
// lendingInterestTimerBacklog it logs and returns, leaving the cursor where it is for the lagging alert.
func (r lendingRun) deriveInterest(ctx context.Context, log io.Writer, wm uint32, hb *opsutil.JobHeartbeat) error {
	if r.timerInterest() {
		state, err := lendingInterestStateWM(ctx, r.chAddr)
		if err != nil {
			return err
		}
		if state > wm && state-wm > lendingInterestTimerBacklog {
			_, _ = fmt.Fprintf(log, "lending interest: backfill incomplete (%d ledgers behind; run the backfill chunks)\n", state-wm)
			return nil
		}
	}
	plan, skip, err := r.planUnderRedo(ctx, log, wm, hb)
	if err != nil || skip {
		return err
	}
	if plan.last < plan.start {
		_, _ = fmt.Fprintf(log, "%s: nothing to derive (start %d, through %d)\n", lendingInterestJob, plan.start, plan.last)
		return nil
	}
	p := &interestPass{
		addr: r.chAddr, pools: plan.pools, dryRun: r.dryRun, rederive: plan.rederive,
		maxWindows: r.interest.maxWindows, maxVersions: r.interest.maxVersions, mark: plan.mark, log: log, hb: hb,
		opts: lendinginterest.Options{Protocols: map[string]lendinginterest.Protocol{}, MaxUnordered: r.interest.maxUnordered},
	}
	if plan.pool {
		// A pool's backfill stays below the shared watermark, which the cursor reports.
		p.cursorAt = &wm
	}
	for _, c := range plan.pools {
		p.opts.Protocols[c.ID] = lendinginterest.Protocol(c.Protocol)
	}
	if p.code, err = readLendingCode(ctx, r.chAddr, plan.pools); err != nil {
		return err
	}
	return p.run(ctx, plan.start, plan.last)
}

// redoes reports whether the run may proceed under a pending redo: a re-derive that keeps the redo contiguous, or a
// pool backfill, whose rows come from the replayed state.
func (r lendingRun) redoes(redo clickhouse.LendingInterestRedo) bool {
	return r.interest.pool != "" || (r.interest.only && r.from != 0 && r.from <= redo.Next())
}

// interestHeartbeat sits beside an explicit -heartbeat path, else in the default textfile directory.
func (r lendingRun) interestHeartbeat() string { return r.siblingHeartbeat(lendingInterestJob) }

// siblingHeartbeat is job's textfile beside an explicit -heartbeat path, else "" (the default textfile directory).
func (r lendingRun) siblingHeartbeat(job string) string {
	if r.heartbeat == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(r.heartbeat), "ops_job_"+job+".prom")
}

// interestPlan resolves the run's range and pools: a per-pool backfill, a first run, a re-derive from -from, or a
// resume from the watermark; never past lending_state's watermark.
func (r lendingRun) interestPlan(ctx context.Context, wm uint32) (interestPlan, error) {
	state, err := lendingInterestStateWM(ctx, r.chAddr)
	if err != nil {
		return interestPlan{}, err
	}
	if r.interest.pool != "" {
		return r.interestPoolPlan(ctx, wm)
	}
	plan := interestPlan{start: wm + 1, last: state}
	var floor uint32
	switch {
	case r.interest.init:
		plan.start = max(r.floor, blend.FactoryGenesisLedger)
	case r.interest.only && r.from != 0:
		if floor, err = r.rederiveFloor(ctx, state); err != nil {
			return interestPlan{}, err
		}
		plan.start, plan.rederive = r.from, true
	}
	if r.interest.only && r.to != 0 && r.to < plan.start {
		return interestPlan{}, fmt.Errorf("lending interest: -to %d is below the run's first ledger %d", r.to, plan.start)
	}
	if plan.last < plan.start {
		return plan, nil
	}
	if plan.start, err = hourStart(ctx, r.chAddr, plan.start); err != nil {
		return interestPlan{}, err
	}
	// The floor may sit inside its hour; the rows start there.
	plan.start = max(plan.start, floor)
	if plan.rederive && plan.start > wm+1 {
		return interestPlan{}, fmt.Errorf("lending interest: -from %d skips [%d,%d], not yet derived (watermark %d)", r.from, wm+1, plan.start-1, wm)
	}
	if plan.last, err = r.clipTo(ctx, plan.last); err != nil {
		return interestPlan{}, err
	}
	reg, err := lendingInterestRegistry(ctx, r.chAddr, plan.last)
	if err != nil {
		return interestPlan{}, err
	}
	plan.pools = lendingPools(reg)
	gaps, err := lendingInterestUncovered(ctx, r.chAddr, plan.pools)
	if err != nil {
		return interestPlan{}, err
	}
	if len(gaps) > 0 {
		return interestPlan{}, fmt.Errorf("lending interest: pools below the watermark %d: %+v; bring each up with -interest-only -interest-pool <pool> -write", wm, gaps)
	}
	pools := plan.pools
	plan.mark = func(ctx context.Context, from, thru uint32) error {
		return setLendingInterestWM(ctx, r.chAddr, from, thru, pools)
	}
	return plan, nil
}

// clipTo lowers last to the end of -to's hour on an -interest-only run.
func (r lendingRun) clipTo(ctx context.Context, last uint32) (uint32, error) {
	if !r.interest.only || r.to == 0 || r.to >= last {
		return last, nil
	}
	h, err := lendingInterestHour(ctx, r.chAddr, r.to)
	if err != nil {
		return 0, err
	}
	return min(h.Last, last), nil
}

// rederiveFloor refuses a -from past lending_state's watermark or below the recorded floor, and returns the floor.
func (r lendingRun) rederiveFloor(ctx context.Context, state uint32) (uint32, error) {
	if r.from > state {
		return 0, fmt.Errorf("lending interest: -from %d is past lending_state's watermark %d", r.from, state)
	}
	floor, err := lendingInterestFloor(ctx, r.chAddr)
	if err != nil {
		return 0, err
	}
	if r.from < floor {
		return 0, fmt.Errorf("lending interest: -from %d is below the recorded floor %d, where the rows start", r.from, floor)
	}
	return floor, nil
}

// interestPoolPlan is the bounded backfill of one pool the watermark does not cover, up to the watermark.
func (r lendingRun) interestPoolPlan(ctx context.Context, wm uint32) (interestPlan, error) {
	reg, err := lendingInterestRegistry(ctx, r.chAddr, wm)
	if err != nil {
		return interestPlan{}, err
	}
	var pool *clickhouse.LendingContract
	pools := lendingPools(reg)
	for i := range pools {
		if pools[i].ID == r.interest.pool {
			pool = &pools[i]
		}
	}
	if pool == nil {
		return interestPlan{}, fmt.Errorf("lending interest: %s is not a pool in the registry through %d", r.interest.pool, wm)
	}
	gaps, err := lendingInterestUncovered(ctx, r.chAddr, []clickhouse.LendingContract{*pool})
	if err != nil {
		return interestPlan{}, err
	}
	if len(gaps) == 0 {
		return interestPlan{start: 1, last: 0}, nil
	}
	plan := interestPlan{start: gaps[0].Thru + 1, last: wm, pools: []clickhouse.LendingContract{*pool}, pool: true}
	if gaps[0].Thru == 0 {
		floor, err := lendingInterestFloor(ctx, r.chAddr)
		if err != nil {
			return interestPlan{}, err
		}
		plan.start = floor
		if pool.Deployed > floor {
			if plan.start, err = hourStart(ctx, r.chAddr, pool.Deployed); err != nil {
				return interestPlan{}, err
			}
		}
	}
	c := *pool
	plan.mark = func(ctx context.Context, from, thru uint32) error {
		return setLendingInterestPoolWM(ctx, r.chAddr, c, from, thru)
	}
	return plan, nil
}

// lendingPools drops the backstops: interest accrues on pools.
func lendingPools(reg []clickhouse.LendingContract) []clickhouse.LendingContract {
	var out []clickhouse.LendingContract
	for _, c := range reg {
		if !c.Backstop {
			out = append(out, c)
		}
	}
	return out
}

// hourStart is the first stored ledger of seq's hour.
func hourStart(ctx context.Context, addr string, seq uint32) (uint32, error) {
	h, err := lendingInterestHour(ctx, addr, seq)
	if err != nil {
		return 0, err
	}
	return h.First, nil
}

// readLendingCode reads each pool's WASM timeline once per run.
func readLendingCode(ctx context.Context, addr string, pools []clickhouse.LendingContract) (map[string][]clickhouse.ContractCodeVersion, error) {
	h, closeFn, err := openLendingCodeHistory(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	out := make(map[string][]clickhouse.ContractCodeVersion, len(pools))
	for _, c := range pools {
		vs, err := h.ReplayCodeHistory(ctx, c.ID)
		switch {
		case errors.Is(err, clickhouse.ErrContractWasmUnresolved):
			// No instance in the lake: gateLendingWASM refuses any window the pool is deployed in.
		case err != nil:
			return nil, fmt.Errorf("lending interest: pool %s WASM history: %w", c.ID, err)
		}
		out[c.ID] = vs
	}
	return out, nil
}

// lendingInterestMaxVersions is -interest-max-versions' default bound on one window's versions and cause events.
// Estimated, not measured: a decoded version with its big.Ints, strings and BLNT fee map is ~1-2 KB, and the fold
// holds a sorted copy plus a dedupe key per version, so ~4 KB each at peak; 150,000 keeps a window near 600 MB,
// inside lending-state.service's MemoryMax=1G.
const lendingInterestMaxVersions = 150_000

// versionBound is the run's bound on a window's versions and cause events.
func (p *interestPass) versionBound() int {
	if p.maxVersions > 0 {
		return p.maxVersions
	}
	return lendingInterestMaxVersions
}

// carried is what the carry's open hour adds to every window's read: its versions and its cause events.
func (st *interestCarry) carried() int { return len(st.seed.Pending) + len(st.seed.PendingEvents) }

var errInterestWindowTooLarge = errors.New("lending interest: window over the version bound")

// interestCarry is the state between windows of one run: the fold's carry and the first ledger not yet marked.
type interestCarry struct {
	seed   lendinginterest.Seeds
	seeded bool
	from   uint32
	// seedRows are the proved seeds: while seedCheck, the first checked window's lending_state values before it.
	seedRows  []clickhouse.LendingInterestStateRow
	seedCheck bool
	known     map[string]bool // keys seeded or read: any other key's first version must be its creation
	derived   map[string]bool // pools whose instance lending_state is proved to hold
	// eventReads counts the cause events read per ledger of the carried open hour, for its census when it commits.
	eventReads map[uint32]uint64
}

// takeReads keeps the window's event read counts for the ledgers after thru, the open hour the carry continues.
func (st *interestCarry) takeReads(reads map[uint32]uint64, thru uint32) {
	maps.Copy(st.eventReads, reads)
	maps.DeleteFunc(st.eventReads, func(l uint32, _ uint64) bool { return l <= thru })
}

// run derives [start, last] window by window; start is the first ledger of an hour.
func (p *interestPass) run(ctx context.Context, start, last uint32) error {
	h, err := lendingInterestHour(ctx, p.addr, start)
	if err != nil {
		return err
	}
	if h.First != start {
		return fmt.Errorf("lending interest: start %d is not the first ledger of its hour (%d)", start, h.First)
	}
	st := interestCarry{seed: lendinginterest.Seeds{CommittedBefore: h.Hour}, from: start, eventReads: map[uint32]uint64{}}
	p.events = newInterestEventDecoder(p.opts.Protocols)
	p.countUndecodable(nil)
	runStart := time.Now()
	for lo, n := start, 0; lo <= last; n++ {
		if p.maxWindows > 0 && n == p.maxWindows {
			_, _ = fmt.Fprintf(p.log, "%s: stopping after %d windows at %d; run again to continue\n", lendingInterestJob, n, st.from-1)
			return nil
		}
		hi, err := gateLendingWASM(p.code, p.pools, lo, lendingWindowEnd(lo, last))
		if err != nil {
			return fmt.Errorf("%w — the watermark holds at %d", err, st.from-1)
		}
		err = p.window(ctx, &st, lo, hi)
		for errors.Is(err, errInterestWindowTooLarge) && hi > lo {
			// The fold holds the whole window in memory: narrow it rather than meet the unit's MemoryMax.
			_, _ = fmt.Fprintf(p.log, "%s: window [%d,%d] holds over %d versions; halving it\n", lendingInterestJob, lo, hi, p.versionBound())
			hi = lo + (hi-lo)/2
			err = p.window(ctx, &st, lo, hi)
		}
		if errors.Is(err, errInterestWindowTooLarge) {
			err = fmt.Errorf("%w: ledger %d with the carried open hour holds over %d versions and cause events; the fold holds an "+
				"hour whole: raise -interest-max-versions (within lending-state.service's MemoryMax)", err, lo, p.versionBound())
		}
		if err != nil {
			return fmt.Errorf("%s: window [%d,%d]: %w — the watermark holds at %d", lendingInterestJob, lo, hi, err, st.from-1)
		}
		cursor := st.from - 1
		if p.cursorAt != nil {
			cursor = *p.cursorAt
		}
		p.hb.Progress(uint64(n+1), uint64(cursor))
		_, _ = fmt.Fprintf(p.log, "%s: window [%d,%d] done, complete through %d (elapsed %s)\n", lendingInterestJob, lo, hi,
			st.from-1, time.Since(runStart).Round(time.Second))
		lo = hi + 1
	}
	return nil
}

// window reads, folds and checks [lo, hi]; once the checks pass it writes the complete hours, deletes on a
// re-derive the rows of those hours it no longer writes, and marks them. Nothing is written before the check. A
// proof run compares the stored rows instead and writes nothing.
func (p *interestPass) window(ctx context.Context, st *interestCarry, lo, hi uint32) error {
	w, ok, err := p.fold(ctx, st, lo, hi)
	if err != nil || !ok {
		return err
	}
	if p.proof != nil {
		if err := p.proof.window(ctx, p, w); err != nil {
			return err
		}
	} else {
		checked, err := checkLendingInterest(p.opts.Protocols, w.rows, w.state, w.versions, w.from, w.to)
		if err != nil {
			return err
		}
		reportInterestWindow(p.log, w.from, w.to, w.rows, checked, p.dryRun)
		if !p.dryRun {
			if err := p.write(ctx, w.rows, w.from, w.to, st.from, w.thru); err != nil {
				return err
			}
		}
	}
	st.seedRows, st.seedCheck = nil, false
	st.seed, st.from = w.carry, w.thru+1
	st.takeReads(w.reads, w.thru)
	return nil
}

// foldedWindow is one window read and folded: its complete hours [from, to), ending at ledger thru, and what the
// checks compare the rows with.
type foldedWindow struct {
	from, to time.Time
	thru     uint32
	rows     []lendinginterest.HourRow
	carry    lendinginterest.Seeds
	versions []lendinginterest.Version // the fold's input: the carry's open hour, then the window's read
	state    []clickhouse.LendingInterestStateRow
	reads    map[uint32]uint64 // cause events read per ledger of the window
}

// fold reads and folds [lo, hi]; ok is false, with the carry already taken, when no hour completes in it.
func (p *interestPass) fold(ctx context.Context, st *interestCarry, lo, hi uint32) (foldedWindow, bool, error) {
	keys, err := lendingInterestKeys(ctx, p.addr, p.pools, hi)
	if err != nil {
		return foldedWindow{}, false, err
	}
	if err := p.checkDerived(ctx, st, hi); err != nil {
		return foldedWindow{}, false, err
	}
	if !st.seeded {
		// Seeds once per run; later windows continue from the carry and read from hi+1, never both.
		if err := p.seedRun(ctx, st, keys, lo); err != nil {
			return foldedWindow{}, false, err
		}
	}
	if n := st.carried(); n >= p.versionBound() {
		return foldedWindow{}, false, fmt.Errorf("lending interest: the open hour from %s carries %d versions and cause events, over the bound %d: "+
			"the fold holds an hour whole: raise -interest-max-versions (within lending-state.service's MemoryMax)",
			st.seed.CommittedBefore.Format(time.RFC3339), n, p.versionBound())
	}
	vs, err := p.readVersions(ctx, keys, lo, hi, st.carried())
	if err != nil {
		return foldedWindow{}, false, err
	}
	if err := checkInstanceCode(p.code, vs); err != nil {
		return foldedWindow{}, false, err
	}
	if err := st.checkNewKeys(vs); err != nil {
		return foldedWindow{}, false, err
	}
	opts := p.opts
	var reads map[uint32]uint64
	budget := p.versionBound() - st.carried() - len(vs)
	if opts.Events, reads, err = p.readInterestEvents(ctx, lo, hi, budget); err != nil {
		return foldedWindow{}, false, err
	}
	w := foldedWindow{from: st.seed.CommittedBefore, reads: reads}
	if w.to, w.thru, err = p.commitPoint(ctx, hi); err != nil {
		return foldedWindow{}, false, err
	}
	if w.rows, w.carry, err = lendinginterest.Fold(st.seed, vs, w.to, opts); err != nil {
		return foldedWindow{}, false, err
	}
	p.countUndecodable(opts.Events) // after the fold: a window halved before it is read again
	if !w.to.After(w.from) {
		st.seed = w.carry // no hour completes in the window
		st.takeReads(w.reads, 0)
		return foldedWindow{}, false, nil
	}
	if err := p.eventsCovered(ctx, w.rows, st.eventReads, w.reads); err != nil {
		return foldedWindow{}, false, err
	}
	if w.state, err = p.stateRows(ctx, keys, w.rows, w.from, w.to); err != nil {
		return foldedWindow{}, false, err
	}
	if st.seedCheck {
		w.state = withSeedsBefore(w.state, st.seedRows)
	}
	w.versions = append(append([]lendinginterest.Version(nil), st.seed.Pending...), vs...)
	return w, true, nil
}

// stateRows reads lending_state's rows for the checks; with no ResData or ProtocolFees key (a pool not yet or no
// longer holding a reserve) the fold has no rows to check and there is nothing to read.
func (p *interestPass) stateRows(ctx context.Context, keys []clickhouse.LendingInterestKey, rows []lendinginterest.HourRow,
	from, to time.Time,
) ([]clickhouse.LendingInterestStateRow, error) {
	if hasStateKeys(keys) {
		return lendingInterestState(ctx, p.addr, p.pools, keys, from, to)
	}
	if len(rows) > 0 {
		return nil, fmt.Errorf("lending interest: %d rows with no ResData key read", len(rows))
	}
	return nil, nil
}

// readVersions reads [lo, hi]'s versions, halving a read over the lake's memory limit; with the carried open hour's
// versions over lendingInterestMaxVersions it stops with errInterestWindowTooLarge.
func (p *interestPass) readVersions(ctx context.Context, keys []clickhouse.LendingInterestKey, lo, hi uint32, carried int) ([]lendinginterest.Version, error) {
	var vs []lendinginterest.Version
	err := splitOnMemoryLimit(lo, hi, "interest window", p.log, func(lo, hi uint32) error {
		var got []lendinginterest.Version
		if err := lendingInterestVersions(ctx, p.addr, lo, hi, keys, func(v lendinginterest.Version) error {
			if carried+len(vs)+len(got) >= p.versionBound() {
				return errInterestWindowTooLarge
			}
			got = append(got, v)
			return nil
		}); err != nil {
			return err
		}
		vs = append(vs, got...)
		return nil
	})
	return vs, err
}

// commitPoint is the fold's cut for a window ending at hi: the start of hi's hour, or the next hour when hi is the
// last ledger of an hour the lake has closed; thru is the last ledger before the cut.
func (p *interestPass) commitPoint(ctx context.Context, hi uint32) (time.Time, uint32, error) {
	h, err := lendingInterestHour(ctx, p.addr, hi)
	if err != nil {
		return time.Time{}, 0, err
	}
	if h.Closed && h.Last == hi {
		return h.Hour.Add(time.Hour), hi, nil
	}
	return h.Hour, h.First - 1, nil
}

// write inserts the checked rows of hours [from, to), deletes on a re-derive the older rows of those hours, and marks
// ledgers [fromLedger, thru].
func (p *interestPass) write(ctx context.Context, rows []lendinginterest.HourRow, from, to time.Time, fromLedger, thru uint32) error {
	var before time.Time
	if p.rederive {
		var err error
		// Read before the insert: every row it writes is stamped at or after it.
		if before, err = lendingInterestServerNow(ctx, p.addr); err != nil {
			return err
		}
	}
	if err := insertLendingInterest(ctx, p.addr, rows, lendingInterestRev); err != nil {
		return err
	}
	if p.rederive {
		if err := deleteSupersededInterest(ctx, p.addr, p.pools, from, to.Add(-time.Hour), before); err != nil {
			return err
		}
	}
	if err := p.mark(ctx, fromLedger, thru); err != nil {
		return fmt.Errorf("advance lending_interest to %d: %w", thru, err)
	}
	return nil
}

// hasStateKeys reports whether keys hold a ResData or ProtocolFees key, the kinds lending_state rows are read for.
func hasStateKeys(keys []clickhouse.LendingInterestKey) bool {
	for _, k := range keys {
		if k.Kind == lendinginterest.KindReserve || k.Kind == lendinginterest.KindFees {
			return true
		}
	}
	return false
}

// reportInterestWindow prints the window's counts, the residuals a drift would show in, and what the checks compared;
// amounts are per asset, so none is summed across rows.
func reportInterestWindow(log io.Writer, from, to time.Time, rows []lendinginterest.HourRow, c interestChecked, dryRun bool) {
	var versions, accrued, errs, unordered, unreconciled, bRate, feeMis, feeOnly, explained, attributed, unexplained uint64
	residualRows := 0
	for _, r := range rows {
		versions += uint64(r.Versions)
		accrued += uint64(r.AccruedVersions)
		errs += uint64(r.AccrualErrors)
		unordered += uint64(r.UnorderedVersions)
		unreconciled += uint64(r.UnreconciledVersions)
		bRate += uint64(r.BRateMismatches)
		feeMis += uint64(r.FeeMismatches)
		feeOnly += uint64(r.FeeOnlyVersions)
		explained += uint64(r.ExplainedVersions)
		attributed += uint64(r.AttributedVersions)
		unexplained += uint64(r.UnexplainedVersions)
		if r.BackstopResidual.Sign() != 0 {
			residualRows++
		}
	}
	verb := "wrote"
	if dryRun {
		verb = "would write"
	}
	span := fmt.Sprintf("hours [%s, %s)", from.Format(time.RFC3339), to.Format(time.RFC3339))
	_, _ = fmt.Fprintf(log, "%s: %s: %s %d rows; versions %d, accrued %d, accrual errors %d, unordered %d; "+
		"unreconciled %d (b_rate mismatches %d, fee mismatches %d, rows with a backstop residual %d; "+
		"versions explained %d, attributed %d, unexplained %d), fee-only moves %d; "+
		"checked: chain endpoints %d rows, gross telescoped %d (%d of them around %d excluded pairs), fee carry %d (%d not)\n",
		lendingInterestJob, span, verb, len(rows), versions, accrued, errs, unordered,
		unreconciled, bRate, feeMis, residualRows, explained, attributed, unexplained, feeOnly, c.telescoped, c.telescoped,
		c.telescopeSplit, c.excludedPairs, c.feeChecked, c.feeSkipped)
	reportUnexplainedRows(log, span, rows)
}

// lendingInterestSampleMax caps the unexplained rows a window line names.
const lendingInterestSampleMax = 5

// reportUnexplainedRows names the window's first rows holding an unexplained version, with the ledger span to read.
func reportUnexplainedRows(log io.Writer, span string, rows []lendinginterest.HourRow) {
	n := 0
	for _, r := range rows {
		if r.UnexplainedVersions == 0 {
			continue
		}
		if n++; n <= lendingInterestSampleMax {
			_, _ = fmt.Fprintf(log, "%s: %s: unexplained %s %s %s: backstop residual %s (unexplained %s); versions explained %d, "+
				"attributed %d, unexplained %d; ledgers %d-%d\n", lendingInterestJob, span, r.Pool, r.Asset, r.Hour.Format(time.RFC3339),
				r.BackstopResidual, r.ResidualUnexplained, r.ExplainedVersions, r.AttributedVersions, r.UnexplainedVersions,
				r.FirstLedger, r.LastLedger)
		}
	}
	if n > lendingInterestSampleMax {
		_, _ = fmt.Fprintf(log, "%s: %s: %d more rows with an unexplained version\n", lendingInterestJob, span, n-lendingInterestSampleMax)
	}
}
