package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingInterestJob names the interest pass's heartbeat (its own cursor series) and log lines.
const lendingInterestJob = "ch-entry-history-lending-interest"

// lendingInterestRev is every row's derive_rev. Bump it, and re-derive, on any change to blend/accrual.go,
// blnt/accrual.go or internal/lendinginterest: rows are computed when written, not decoded at read time.
const lendingInterestRev uint16 = 1

// interestFlags are the -interest-* flags.
type interestFlags struct {
	init, only   bool
	maxWindows   int
	maxUnordered int
	pool         string
}

// Seams so the interest pass is testable without a live lake.
var (
	lendingInterestWatermark = clickhouse.LendingInterestWatermark
	lendingInterestFloor     = clickhouse.LendingInterestFloor
	lendingInterestStateWM   = clickhouse.LendingStateWatermark
	lendingInterestHour      = clickhouse.LendingInterestHour
	lendingInterestKeys      = clickhouse.LendingInterestKeys
	lendingInterestSeeds     = clickhouse.LendingInterestSeeds
	lendingInterestVersions  = clickhouse.LendingInterestVersions
	lendingInterestState     = clickhouse.LendingInterestStateRows
	lendingInterestUncovered = clickhouse.LendingInterestUncoveredPools
	insertLendingInterest    = clickhouse.InsertLendingInterestRows
	deleteSupersededInterest = clickhouse.DeleteSupersededLendingInterestRows
	setLendingInterestWM     = clickhouse.SetLendingInterestWatermark
	setLendingInterestPoolWM = clickhouse.SetLendingInterestPoolWatermark
	lendingInterestServerNow = clickhouse.ServerNow
	lendingInterestRegistry  = lendingRegistry
	openLendingCodeHistory   = func(ctx context.Context, addr string) (lendingCodeHistory, func(), error) {
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
	code       map[string][]clickhouse.ContractCodeVersion
	dryRun     bool
	rederive   bool // the run starts at or below the watermark: rows it no longer writes are deleted
	maxWindows int
	mark       func(ctx context.Context, from, thru uint32) error
	hb         *opsutil.JobHeartbeat
	log        io.Writer
}

// interestPlan is what a run derives: [start, last] over pools, marked by mark.
type interestPlan struct {
	start, last uint32
	pools       []clickhouse.LendingContract
	rederive    bool
	mark        func(ctx context.Context, from, thru uint32) error
}

// interestPass runs the interest pass after the state pass (or alone with -interest-only). The hourly timer's run
// never starts one: with no lending_interest watermark it logs and returns, so only -interest-init begins the
// backfill.
func (r lendingRun) interestPass(ctx context.Context, log io.Writer) error {
	wm, err := lendingInterestWatermark(ctx, r.chAddr)
	if err != nil {
		return err
	}
	if wm == 0 && !r.interest.init {
		if r.interest.only || r.interest.pool != "" {
			return fmt.Errorf("lending interest: not initialised; start it with -interest-init")
		}
		_, _ = fmt.Fprintln(log, "lending interest: not initialised (run -interest-init)")
		return nil
	}
	if wm > 0 && r.interest.init {
		return fmt.Errorf("lending interest: already initialised through %d; drop -interest-init to resume", wm)
	}
	plan, err := r.interestPlan(ctx, wm)
	if err != nil {
		return err
	}
	if plan.last < plan.start {
		_, _ = fmt.Fprintf(log, "%s: nothing to derive (start %d, through %d)\n", lendingInterestJob, plan.start, plan.last)
		return nil
	}
	p := &interestPass{
		addr: r.chAddr, pools: plan.pools, dryRun: r.dryRun, rederive: plan.rederive,
		maxWindows: r.interest.maxWindows, mark: plan.mark, log: log,
		opts: lendinginterest.Options{Protocols: map[string]lendinginterest.Protocol{}, MaxUnordered: r.interest.maxUnordered},
	}
	for _, c := range plan.pools {
		p.opts.Protocols[c.ID] = lendinginterest.Protocol(c.Protocol)
	}
	if p.code, err = readLendingCode(ctx, r.chAddr, plan.pools); err != nil {
		return err
	}
	if !r.dryRun {
		p.hb = opsutil.NewJobHeartbeat(lendingInterestJob, r.interestHeartbeat(), nil)
		p.hb.Start()
		p.hb.Progress(0, uint64(plan.start-1))
	}
	err = p.run(ctx, plan.start, plan.last)
	p.hb.Stop(err == nil)
	return err
}

// interestHeartbeat sits beside an explicit -heartbeat path, else in the default textfile directory.
func (r lendingRun) interestHeartbeat() string {
	if r.heartbeat == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(r.heartbeat), "ops_job_"+lendingInterestJob+".prom")
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
	switch {
	case r.interest.init:
		plan.start = max(r.floor, blend.FactoryGenesisLedger)
	case r.interest.only && r.from != 0:
		plan.start, plan.rederive = r.from, true
	}
	if plan.last < plan.start {
		return plan, nil
	}
	if plan.start, err = hourStart(ctx, r.chAddr, plan.start); err != nil {
		return interestPlan{}, err
	}
	if plan.rederive && plan.start > wm+1 {
		return interestPlan{}, fmt.Errorf("lending interest: -from %d skips [%d,%d], not yet derived (watermark %d)", r.from, wm+1, plan.start-1, wm)
	}
	if r.interest.only && r.to != 0 && r.to < plan.last {
		h, err := lendingInterestHour(ctx, r.chAddr, r.to)
		if err != nil {
			return interestPlan{}, err
		}
		plan.last = min(h.Last, plan.last)
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
	plan := interestPlan{start: gaps[0].Thru + 1, last: wm, pools: []clickhouse.LendingContract{*pool}}
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

// interestCarry is the state between windows of one run: the fold's carry and the first ledger not yet marked.
type interestCarry struct {
	seed   lendinginterest.Seeds
	seeded bool
	from   uint32
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
	st := interestCarry{seed: lendinginterest.Seeds{CommittedBefore: h.Hour}, from: start}
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
		if err := p.window(ctx, &st, lo, hi); err != nil {
			return fmt.Errorf("%s: window [%d,%d]: %w — the watermark holds at %d", lendingInterestJob, lo, hi, err, st.from-1)
		}
		p.hb.Progress(uint64(n+1), uint64(st.from-1))
		_, _ = fmt.Fprintf(p.log, "%s: window [%d,%d] done, complete through %d (elapsed %s)\n", lendingInterestJob, lo, hi,
			st.from-1, time.Since(runStart).Round(time.Second))
		lo = hi + 1
	}
	return nil
}

// window reads, folds and checks [lo, hi]; once the identities hold it writes the complete hours, deletes on a
// re-derive the rows of those hours it no longer writes, and marks them. Nothing is written before the check.
func (p *interestPass) window(ctx context.Context, st *interestCarry, lo, hi uint32) error {
	keys, err := lendingInterestKeys(ctx, p.addr, p.pools, hi)
	if err != nil {
		return err
	}
	if !st.seeded {
		// Seeds once per run; later windows continue from the carry and read from hi+1, never both.
		if st.seed, err = lendingInterestSeeds(ctx, p.addr, p.pools, keys, st.seed.CommittedBefore); err != nil {
			return err
		}
		st.seeded = true
	}
	var vs []lendinginterest.Version
	err = splitOnMemoryLimit(lo, hi, "interest window", p.log, func(lo, hi uint32) error {
		var got []lendinginterest.Version
		if err := lendingInterestVersions(ctx, p.addr, lo, hi, keys, func(v lendinginterest.Version) error {
			got = append(got, v)
			return nil
		}); err != nil {
			return err
		}
		vs = append(vs, got...)
		return nil
	})
	if err != nil {
		return err
	}
	commitBefore, thru, err := p.commitPoint(ctx, hi)
	if err != nil {
		return err
	}
	from := st.seed.CommittedBefore
	rows, carry, err := lendinginterest.Fold(st.seed, vs, commitBefore, p.opts)
	if err != nil {
		return err
	}
	if !commitBefore.After(from) {
		st.seed = carry // no hour completes in the window
		return nil
	}
	state, err := lendingInterestState(ctx, p.addr, p.pools, keys, from, commitBefore)
	if err != nil {
		return err
	}
	if err := checkLendingInterest(p.opts.Protocols, rows, state, append(st.seed.Pending, vs...), from, commitBefore); err != nil {
		return err
	}
	reportInterestWindow(p.log, from, commitBefore, rows, p.dryRun)
	if !p.dryRun {
		if err := p.write(ctx, rows, from, commitBefore, st.from, thru); err != nil {
			return err
		}
	}
	st.seed, st.from = carry, thru+1
	return nil
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

// reportInterestWindow prints the window's counts; amounts are per asset, so none is summed across rows.
func reportInterestWindow(log io.Writer, from, to time.Time, rows []lendinginterest.HourRow, dryRun bool) {
	var versions, accrued, errs, unordered, unreconciled uint64
	for _, r := range rows {
		versions += uint64(r.Versions)
		accrued += uint64(r.AccruedVersions)
		errs += uint64(r.AccrualErrors)
		unordered += uint64(r.UnorderedVersions)
		unreconciled += uint64(r.UnreconciledVersions)
	}
	verb := "wrote"
	if dryRun {
		verb = "would write"
	}
	_, _ = fmt.Fprintf(log, "%s: hours [%s, %s): %s %d rows; versions %d, accrued %d, unreconciled %d, accrual errors %d, unordered %d; identities hold\n",
		lendingInterestJob, from.Format(time.RFC3339), to.Format(time.RFC3339), verb, len(rows), versions, accrued, unreconciled, errs, unordered)
}
