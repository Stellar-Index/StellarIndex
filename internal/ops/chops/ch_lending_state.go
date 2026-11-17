package chops

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingJob names the -lending-only run's lock, heartbeat and log lines.
const lendingJob = "ch-entry-history-lending"

// lendingLockPath is -lending-only's own lock: its catch-up runs for hours beside the sponsorship windows.
const lendingLockPath = "/var/lib/stellarindex/ch-entry-history-lending.lock"

// lendingWindow divides the lake's 1M-ledger partition, so a window never spans two; a whole partition in one
// derive exceeds the lake's per-query memory limit.
const lendingWindow = 100_000

// lendingRun is one `ch-entry-history -lending-only` invocation.
type lendingRun struct {
	chAddr              string
	from, to, floor     uint32
	dryRun              bool
	lockFile, heartbeat string
	printPrefixes       bool
	prove               bool
}

// start validates the mode flags, then runs.
func (r lendingRun) start(lendingOnly bool, gate *opsutil.WriteGate) error {
	if !lendingOnly {
		return fmt.Errorf("-print-prefixes and -prove apply to -lending-only")
	}
	if r.printPrefixes && r.prove {
		return fmt.Errorf("-print-prefixes and -prove are separate reads; run one at a time")
	}
	if !r.printPrefixes && !r.prove {
		gate.Banner()
	}
	return r.run()
}

func (r lendingRun) run() error {
	ctx, cancel := opsutil.SignalContext()
	defer cancel()
	switch {
	case r.printPrefixes:
		return printLendingPrefixes(ctx, r.chAddr, os.Stdout)
	case r.prove:
		return r.proveLendingState(ctx, os.Stdout)
	case r.dryRun:
		return r.exec(ctx, nil)
	}
	unlock, err := acquireRollupLock(lendingJob, r.lockFile)
	if err != nil {
		return err
	}
	defer unlock()
	hb := opsutil.NewJobHeartbeat(lendingJob, r.heartbeat, nil)
	if hb.Enabled() {
		fmt.Fprintf(os.Stderr, "%s: heartbeat -> %s\n", lendingJob, hb.Path())
	}
	hb.Start()
	err = r.exec(ctx, hb)
	hb.Stop(err == nil)
	return err
}

// exec derives from the watermark (the Blend factory genesis on first run) to the contiguous tip in windows that end
// on partition boundaries; hb is nil in a dry run.
func (r lendingRun) exec(ctx context.Context, hb *opsutil.JobHeartbeat) error {
	start, last, err := resolveDeriveRange(ctx, r.chAddr, lendingJob, r.from, r.to, max(r.floor, blend.FactoryGenesisLedger), clickhouse.LendingStateWatermark)
	if err != nil {
		return err
	}
	// The lagging alert reads the cursor, so a run with nothing to do still reports the watermark.
	hb.Progress(0, uint64(start-1))
	if last < start {
		fmt.Fprintf(os.Stderr, "%s: nothing to derive (start %d, contiguous tip %d)\n", lendingJob, start, last)
		return nil
	}
	reg, err := lendingRegistry(ctx, r.chAddr, last)
	if err != nil {
		return err
	}
	ps, err := clickhouse.LendingPrefixes(reg)
	if err != nil {
		return err
	}
	if hb == nil {
		fmt.Fprintf(os.Stderr, "%s: would derive [%d,%d] over %d key prefixes of %d contracts\n", lendingJob, start, last, len(ps), len(reg))
		return nil
	}
	runStart := time.Now()
	var windows uint64
	return deriveLendingWindows(start, last, os.Stderr,
		func(lo, hi uint32) (uint64, error) { return clickhouse.DeriveLendingWindow(ctx, r.chAddr, lo, hi, ps) },
		func(lo, hi uint32, txLedgers uint64) error {
			return clickhouse.SetLendingStateWatermark(ctx, r.chAddr, lo, hi, txLedgers)
		},
		func(lo, hi uint32) {
			windows++
			hb.Progress(windows, uint64(hi))
			fmt.Fprintf(os.Stderr, "%s: window [%d,%d] done (elapsed %s)\n", lendingJob, lo, hi, time.Since(runStart).Round(time.Second))
		})
}

// deriveLendingWindows derives [start, last] window by window, advancing the watermark (mark) after each range is
// written, in ledger order; a range over the lake's memory limit is derived as halves.
func deriveLendingWindows(start, last uint32, log io.Writer, derive func(lo, hi uint32) (uint64, error),
	mark func(lo, hi uint32, txLedgers uint64) error, done func(lo, hi uint32),
) error {
	for lo := start; ; {
		hi := lendingWindowEnd(lo, last)
		err := splitOnMemoryLimit(lo, hi, "window", log, func(lo, hi uint32) error {
			txLedgers, err := derive(lo, hi)
			if err != nil {
				return fmt.Errorf("window [%d,%d]: %w — resume with no -from (the watermark holds)", lo, hi, err)
			}
			if err := mark(lo, hi, txLedgers); err != nil {
				return fmt.Errorf("advance lending_state watermark to %d: %w", hi, err)
			}
			done(lo, hi)
			return nil
		})
		if err != nil {
			return err
		}
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// lendingSplitFloor is the narrowest range a memory-limit split retries: a 100k-ledger window halved five times.
const lendingSplitFloor = lendingWindow / 32

// splitOnMemoryLimit runs [lo, hi]; on ClickHouse's memory-limit error it runs the two halves in order instead,
// recursively down to lendingSplitFloor ledgers. Halves stay inside the window, so inside its partition.
func splitOnMemoryLimit(lo, hi uint32, what string, log io.Writer, run func(lo, hi uint32) error) error {
	err := run(lo, hi)
	if err == nil || !clickhouse.IsMemoryLimitExceeded(err) {
		return err
	}
	if hi-lo+1 <= lendingSplitFloor {
		return fmt.Errorf("%s [%d,%d] exceeds the ClickHouse memory limit at the %d-ledger split floor: %w", what, lo, hi, lendingSplitFloor, err)
	}
	mid := lo + (hi-lo)/2
	// A lost progress line must not stop the fill.
	_, _ = fmt.Fprintf(log, "%s: %s [%d,%d] exceeded the ClickHouse memory limit; splitting into [%d,%d] and [%d,%d]\n",
		lendingJob, what, lo, hi, lo, mid, mid+1, hi)
	if err := splitOnMemoryLimit(lo, mid, what, log, run); err != nil {
		return err
	}
	return splitOnMemoryLimit(mid+1, hi, what, log, run)
}

// lendingWindowEnd is the last ledger of the window starting at lo: the next lendingWindow boundary, or last.
func lendingWindowEnd(lo, last uint32) uint32 {
	end := uint64(lo)/lendingWindow*lendingWindow + lendingWindow - 1
	return uint32(min(end, uint64(last))) //nolint:gosec // bounded by last
}

// printLendingPrefixes prints the registry as of the lake tip and every key prefix the derive matches; it writes nothing.
func printLendingPrefixes(ctx context.Context, chAddr string, out io.Writer) error {
	reg, err := lendingRegistry(ctx, chAddr, math.MaxUint32)
	if err != nil {
		return err
	}
	ps, err := clickhouse.LendingPrefixes(reg)
	if err != nil {
		return err
	}
	for _, c := range reg {
		role := "pool"
		if c.Backstop {
			role = "backstop"
		}
		if _, err := fmt.Fprintf(out, "contract %s %s %s\n", c.ID, c.Protocol, role); err != nil {
			return err
		}
	}
	for _, p := range ps {
		if _, err := fmt.Fprintf(out, "prefix %s %s %s %s\n", p.Contract, p.Protocol, p.Key, p.Prefix); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "%d contracts, %d prefixes\n", len(reg), len(ps))
	return err
}

// proveLendingState checks every prefixed key's latest history row against the lake at the watermark, that every
// registered contract has its keys in the table, and that each window of [-from, -to] (bounded by the watermark) holds
// one row per changed (key, hour).
func (r lendingRun) proveLendingState(ctx context.Context, out io.Writer) error {
	wm, err := clickhouse.LendingStateWatermark(ctx, r.chAddr)
	if err != nil {
		return err
	}
	if wm == 0 {
		return fmt.Errorf("%s: no lending_state watermark; nothing derived to prove", lendingJob)
	}
	reg, err := lendingRegistry(ctx, r.chAddr, wm)
	if err != nil {
		return err
	}
	ps, err := clickhouse.LendingPrefixes(reg)
	if err != nil {
		return err
	}
	p, err := r.proveLendingAt(ctx, reg, ps, wm)
	if err != nil {
		return err
	}
	if err := p.report(out, wm); err != nil {
		return err
	}
	if p.mismatched > 0 {
		return fmt.Errorf("%s: %d of %d keys disagree with the lake at %d", lendingJob, p.mismatched, p.keys, wm)
	}
	if err := proveLendingKeys(ctx, r.chAddr, reg, ps, out); err != nil {
		return err
	}
	if err := r.proveLendingCounts(ctx, ps, wm, out); err != nil {
		return err
	}
	if n := len(p.deep) + len(p.unplaced); n > 0 {
		return fmt.Errorf("%s: INCOMPLETE: %d keys not compared at %d (listed above); the rest passed", lendingJob, n, wm)
	}
	return nil
}

// lendingProofDepth bounds how far below the watermark the proof reads the lake for a key changed since it: ten
// fill-sized FINAL windows. A key whose comparison needs a deeper read is counted not compared.
const lendingProofDepth = 10 * lendingWindow

// lendingProofListMax caps the not-compared keys -prove prints per reason.
const lendingProofListMax = 50

// lendingProof is the state check at the watermark. keys were compared; ahead keys were not, because a history row
// past the watermark has replaced (under FINAL) the one to compare. deep keys' latest history row lies more than
// lendingProofDepth below the watermark; unplaced keys have no history row at or below it and the lake was not read
// back to their contract's deploy, so new-since-the-watermark and missed cannot be told apart.
type lendingProof struct {
	keys, mismatched, ahead uint64
	deep, unplaced          []string
}

// lendingPending is a key changed since the watermark: its state there is the lake's last change in [lo, wm].
// exact: lo is the earliest ledger the key can exist at, so finding no change proves it new since the watermark.
type lendingPending struct {
	k     clickhouse.LendingProofKey
	lo    uint32
	exact bool
}

// classifyLendingKeys settles every key the current table shows at or below wm and lists the rest to read from the
// lake. floor gives a key's earliest possible ledger (0 if unknown).
func classifyLendingKeys(ks []clickhouse.LendingProofKey, wm uint32, floor func(key string) uint32) (lendingProof, []lendingPending) {
	var p lendingProof
	var pend []lendingPending
	depthLo := wm - min(wm, lendingProofDepth)
	for _, k := range ks {
		switch {
		case !k.Changed:
			p.keys++
			if k.AheadHit {
				p.ahead++
			} else if !k.Present || !k.Same {
				p.mismatched++
			}
		case k.Present && k.Ledger < depthLo:
			p.deep = append(p.deep, k.Key)
		case k.Present:
			// A change in (Ledger, wm] the history lacks, or none at Ledger, both show in the fold over [Ledger, wm].
			pend = append(pend, lendingPending{k: k, lo: k.Ledger})
		default:
			f := floor(k.Key)
			pend = append(pend, lendingPending{k: k, lo: max(f, depthLo), exact: f >= depthLo})
		}
	}
	return p, pend
}

// settle compares each pending key with its last change in [lo, wm] (last, by key).
func (p *lendingProof) settle(pend []lendingPending, last map[string]clickhouse.LendingChange) {
	for _, q := range pend {
		c, ok := last[q.k.Key]
		switch {
		case !ok && q.k.Present:
			p.keys++
			p.mismatched++ // the history names a change at Ledger the lake does not hold
		case !ok && q.exact:
			// created after the watermark: nothing to compare
		case !ok:
			p.unplaced = append(p.unplaced, q.k.Key)
		default:
			p.keys++
			if slices.ContainsFunc(q.k.Ahead, c.Hour.Equal) {
				p.ahead++
			} else if !q.k.Present || c.Ledger != q.k.Ledger || c.Entry != q.k.Entry {
				p.mismatched++
			}
		}
	}
}

// report prints the counters and every not-compared key (up to lendingProofListMax per reason).
func (p lendingProof) report(out io.Writer, wm uint32) error {
	if _, err := fmt.Fprintf(out, "lending_state @%d: %d keys compared, %d mismatched; not compared: %d (history row past the watermark), "+
		"%d (history row more than %d ledgers below the watermark), %d (no history row, lake read only %d ledgers back)\n",
		wm, p.keys, p.mismatched, p.ahead, len(p.deep), lendingProofDepth, len(p.unplaced), lendingProofDepth); err != nil {
		return err
	}
	for _, l := range []struct {
		why  string
		keys []string
	}{{"history row too deep", p.deep}, {"no history row, lake not read to the contract's deploy", p.unplaced}} {
		for i, k := range l.keys {
			if i == lendingProofListMax {
				if _, err := fmt.Fprintf(out, "lending_state: not compared (%s): %d more\n", l.why, len(l.keys)-i); err != nil {
					return err
				}
				break
			}
			if _, err := fmt.Fprintf(out, "lending_state: not compared (%s): %s\n", l.why, k); err != nil {
				return err
			}
		}
	}
	if len(p.deep)+len(p.unplaced) > 0 {
		_, err := fmt.Fprintf(out, "lending_state @%d: INCOMPLETE\n", wm)
		return err
	}
	return nil
}

// proveLendingAt reads the current table once, then the lake over [min lo, wm] for keys changed since wm, window by
// window (each inside one partition, halved on the memory limit), keeping each key's last change at or above its lo.
func (r lendingRun) proveLendingAt(ctx context.Context, reg []clickhouse.LendingContract, ps []clickhouse.LendingPrefix, wm uint32) (lendingProof, error) {
	ks, err := clickhouse.LendingProofKeys(ctx, r.chAddr, ps, wm)
	if err != nil {
		return lendingProof{}, err
	}
	deployed := map[string]uint32{}
	for _, c := range reg {
		deployed[c.ID] = c.Deployed
	}
	p, pend := classifyLendingKeys(ks, wm, func(key string) uint32 {
		for _, pr := range ps {
			if strings.HasPrefix(key, pr.Prefix) {
				return deployed[pr.Contract]
			}
		}
		return 0
	})
	if len(pend) == 0 {
		return p, nil
	}
	last, err := r.foldLendingChanges(ctx, pend, wm)
	if err != nil {
		return lendingProof{}, err
	}
	p.settle(pend, last)
	return p, nil
}

// foldLendingChanges returns each pending key's last change in [lo, wm], reading partition-aligned windows in ledger
// order so a later range's change replaces an earlier one's.
func (r lendingRun) foldLendingChanges(ctx context.Context, pend []lendingPending, wm uint32) (map[string]clickhouse.LendingChange, error) {
	from := map[string]uint32{}
	lo := wm
	for _, q := range pend {
		from[q.k.Key] = q.lo
		lo = min(lo, q.lo)
	}
	last := map[string]clickhouse.LendingChange{}
	read := func(lo, hi uint32) error {
		var keys []string
		for _, q := range pend {
			if q.lo <= hi {
				keys = append(keys, q.k.Key)
			}
		}
		if len(keys) == 0 {
			return nil
		}
		cs, err := clickhouse.LendingLastChanges(ctx, r.chAddr, keys, lo, hi)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Ledger >= from[c.Key] {
				last[c.Key] = c
			}
		}
		return nil
	}
	for ; ; lo = lendingWindowEnd(lo, wm) + 1 {
		hi := lendingWindowEnd(lo, wm)
		if err := splitOnMemoryLimit(lo, hi, "proof window", os.Stderr, read); err != nil {
			return nil, err
		}
		if hi >= wm {
			return last, nil
		}
	}
}

// proveLendingKeys fails a registered pool missing a ResData, ResConfig or instance key in the table, or a backstop
// with no balance key, unless the current table holds no reserve (balance) for it either: that contract is empty.
func proveLendingKeys(ctx context.Context, chAddr string, reg []clickhouse.LendingContract, ps []clickhouse.LendingPrefix, out io.Writer) error {
	counts, err := clickhouse.LendingKeyCounts(ctx, chAddr, ps)
	if err != nil {
		return err
	}
	type kc struct{ table, current uint64 }
	by := map[string]kc{}
	for _, k := range counts {
		by[k.Contract+"/"+k.Key] = kc{k.Table, k.Current}
	}
	var failed []string
	for _, c := range reg {
		need, live := []string{"ResData", "ResConfig", "Instance"}, by[c.ID+"/ResData"].current
		if c.Backstop {
			need, live = []string{"PoolBalance"}, by[c.ID+"/PoolBalance"].current+by[c.ID+"/TierPoolBalance"].current
		}
		if live == 0 {
			if _, err := fmt.Fprintf(out, "lending_state: %s %s is empty (no live reserve or balance key)\n", c.Protocol, c.ID); err != nil {
				return err
			}
			continue
		}
		for _, k := range need {
			if by[c.ID+"/"+k].table == 0 && (k != "PoolBalance" || by[c.ID+"/TierPoolBalance"].table == 0) {
				failed = append(failed, fmt.Sprintf("%s %s has no %s key in lending_state_hourly", c.Protocol, c.ID, k))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%s: %q", lendingJob, failed)
	}
	return nil
}

// proveLendingCounts runs the per-contract row-count proof over partition-aligned windows of [-from, -to].
func (r lendingRun) proveLendingCounts(ctx context.Context, ps []clickhouse.LendingPrefix, wm uint32, out io.Writer) error {
	lo, last := r.from, wm
	if lo == 0 {
		lo = max(r.floor, blend.FactoryGenesisLedger)
	}
	if r.to != 0 {
		last = min(r.to, wm)
	}
	var bad []string
	for ; lo <= last; lo = lendingWindowEnd(lo, last) + 1 {
		hi := lendingWindowEnd(lo, last)
		// Each (key, hour) counts in the one range holding its last change, so halves sum to the window.
		err := splitOnMemoryLimit(lo, hi, "count window", os.Stderr, func(lo, hi uint32) error {
			ms, err := clickhouse.ProveLendingCounts(ctx, r.chAddr, ps, lo, hi, wm)
			if err != nil {
				return err
			}
			for _, m := range ms {
				bad = append(bad, fmt.Sprintf("[%d,%d] %s: %d (key, hour) pairs in the lake, %d rows", lo, hi, m.Contract, m.Lake, m.Table))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if hi >= last {
			break
		}
	}
	if _, err := fmt.Fprintf(out, "lending_state rows per (key, hour) through %d: %d window mismatches\n", last, len(bad)); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s: row counts disagree with the lake: %q", lendingJob, bad)
	}
	return nil
}

// lendingBackstops are the backstops whose keys are captured. The Blend V1 backstop is absent: its storage key names
// are not verified against a contract source, and a wrong prefix would match nothing and look complete.
var lendingBackstops = []clickhouse.LendingContract{
	{ID: blend.MainnetBackstop, Protocol: clickhouse.LendingBlendV2, Backstop: true},
	{ID: blnt.MainnetBackstop, Protocol: clickhouse.LendingBLNT, Backstop: true},
}

// lendingFactories maps each pool factory to the protocol of the pools it deploys.
var lendingFactories = map[string]string{
	blend.MainnetPoolFactoryV1: clickhouse.LendingBlendV1,
	blend.MainnetPoolFactory:   clickhouse.LendingBlendV2,
	blnt.MainnetPoolFactory:    clickhouse.LendingBLNT,
}

// lendingKnownPools are the pools the code already names, with their deploy ledgers; a registry through a later
// ledger that lacks one is missing a factory `deploy` row and would silently drop the pool.
var lendingKnownPools = []struct {
	id, protocol string
	deployed     uint32
}{
	{blnt.MainnetFixedPool, clickhouse.LendingBLNT, 65_134_362},
	{blnt.MainnetYieldBlentPool, clickhouse.LendingBLNT, 65_327_498},
}

// checkLendingRegistry refuses a registry through thru that lacks a known pool deployed by then.
func checkLendingRegistry(reg []clickhouse.LendingContract, thru uint32) error {
	have := map[string]string{}
	for _, c := range reg {
		if !c.Backstop {
			have[c.ID] = c.Protocol
		}
	}
	var missing []string
	for _, k := range lendingKnownPools {
		if thru >= k.deployed && have[k.id] != k.protocol {
			missing = append(missing, k.protocol+" "+k.id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("lending registry through %d lacks known pools %v: the lake is missing their factory deploy events", thru, missing)
	}
	return nil
}

// lendingRegistry lists every lending contract as of ledger thru: the pools the three factories' `deploy` events
// announced, decoded with each protocol's own decoder, then the backstops.
func lendingRegistry(ctx context.Context, addr string, thru uint32) ([]clickhouse.LendingContract, error) {
	factories := make([]string, 0, len(lendingFactories))
	for f := range lendingFactories {
		factories = append(factories, f)
	}
	sort.Strings(factories)
	pools := map[string]clickhouse.LendingContract{}
	if thru >= blend.FactoryGenesisLedger {
		err := clickhouse.StreamContractEventsFiltered(ctx, addr, blend.FactoryGenesisLedger, thru, factories, []string{blend.EventDeploy}, nil, false, false, false, false,
			func(ev events.Event) error {
				c, err := lendingPoolFromDeploy(ev)
				if err != nil {
					return err
				}
				if prev, ok := pools[c.ID]; !ok || c.Deployed < prev.Deployed {
					pools[c.ID] = c
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("lending registry through %d: %w", thru, err)
		}
	}
	out := make([]clickhouse.LendingContract, 0, len(pools)+len(lendingBackstops))
	for _, c := range pools {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if err := checkLendingRegistry(out, thru); err != nil {
		return nil, err
	}
	return append(out, lendingBackstops...), nil
}

// lendingPoolFromDeploy decodes one factory `deploy` event into the pool it announced.
func lendingPoolFromDeploy(ev events.Event) (clickhouse.LendingContract, error) {
	protocol, ok := lendingFactories[ev.ContractID]
	if !ok {
		return clickhouse.LendingContract{}, fmt.Errorf("%s is not a lending pool factory", ev.ContractID)
	}
	var pool string
	if protocol == clickhouse.LendingBLNT {
		outs, err := blnt.NewDecoder().Decode(ev)
		if err != nil {
			return clickhouse.LendingContract{}, fmt.Errorf("blnt deploy at %d: %w", ev.Ledger, err)
		}
		if e, ok := outs[0].(blnt.Event); ok {
			pool = e.Pool
		}
	} else {
		outs, err := blend.NewDecoder().Decode(ev)
		if err != nil {
			return clickhouse.LendingContract{}, fmt.Errorf("blend deploy at %d: %w", ev.Ledger, err)
		}
		if e, ok := outs[0].(blend.AdminEvent); ok && e.Kind == blend.EventDeploy {
			pool = e.Target
		}
	}
	if pool == "" {
		return clickhouse.LendingContract{}, fmt.Errorf("deploy at %d tx %s names no pool", ev.Ledger, ev.TxHash)
	}
	return clickhouse.LendingContract{ID: pool, Protocol: protocol, Deployed: ev.Ledger}, nil
}
