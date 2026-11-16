package chops

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
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

// lendingPartition is the lake's partition width; catch-up windows end on its boundaries.
const lendingPartition = 1_000_000

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
		return proveLendingState(ctx, r.chAddr, os.Stdout)
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
	for lo := start; ; {
		hi := lendingWindowEnd(lo, last)
		txLedgers, err := clickhouse.DeriveLendingWindow(ctx, r.chAddr, lo, hi, ps)
		if err != nil {
			return fmt.Errorf("window [%d,%d]: %w — resume with no -from (the watermark holds)", lo, hi, err)
		}
		if err := clickhouse.SetLendingStateWatermark(ctx, r.chAddr, lo, hi, txLedgers); err != nil {
			return fmt.Errorf("advance lending_state watermark to %d: %w", hi, err)
		}
		windows++
		hb.Progress(windows, uint64(hi))
		fmt.Fprintf(os.Stderr, "%s: window [%d,%d] done (elapsed %s)\n", lendingJob, lo, hi, time.Since(runStart).Round(time.Second))
		if hi >= last {
			return nil
		}
		lo = hi + 1
	}
}

// lendingWindowEnd is the last ledger of the window starting at lo: the end of lo's lake partition, or last.
func lendingWindowEnd(lo, last uint32) uint32 {
	end := uint64(lo)/lendingPartition*lendingPartition + lendingPartition - 1
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

// proveLendingState checks every prefixed key's latest history row against the lake's state at the watermark.
func proveLendingState(ctx context.Context, chAddr string, out io.Writer) error {
	wm, err := clickhouse.LendingStateWatermark(ctx, chAddr)
	if err != nil {
		return err
	}
	if wm == 0 {
		return fmt.Errorf("%s: no lending_state watermark; nothing derived to prove", lendingJob)
	}
	reg, err := lendingRegistry(ctx, chAddr, wm)
	if err != nil {
		return err
	}
	ps, err := clickhouse.LendingPrefixes(reg)
	if err != nil {
		return err
	}
	p, err := clickhouse.ProveLendingState(ctx, chAddr, ps, wm)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "lending_state @%d: %d keys, %d mismatched\n", wm, p.Keys, p.Mismatched); err != nil {
		return err
	}
	if p.Mismatched > 0 {
		return fmt.Errorf("%s: %d of %d keys disagree with the lake at %d", lendingJob, p.Mismatched, p.Keys, wm)
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

// lendingRegistry lists every lending contract as of ledger thru: the pools the three factories' `deploy` events
// announced, decoded with each protocol's own decoder, then the backstops.
func lendingRegistry(ctx context.Context, addr string, thru uint32) ([]clickhouse.LendingContract, error) {
	factories := make([]string, 0, len(lendingFactories))
	for f := range lendingFactories {
		factories = append(factories, f)
	}
	sort.Strings(factories)
	pools := map[string]string{}
	if thru >= blend.FactoryGenesisLedger {
		err := clickhouse.StreamContractEventsFiltered(ctx, addr, blend.FactoryGenesisLedger, thru, factories, []string{blend.EventDeploy}, nil, false, false, false,
			func(ev events.Event) error {
				c, err := lendingPoolFromDeploy(ev)
				if err != nil {
					return err
				}
				pools[c.ID] = c.Protocol
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("lending registry through %d: %w", thru, err)
		}
	}
	out := make([]clickhouse.LendingContract, 0, len(pools)+len(lendingBackstops))
	for id, p := range pools {
		out = append(out, clickhouse.LendingContract{ID: id, Protocol: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
	return clickhouse.LendingContract{ID: pool, Protocol: protocol}, nil
}
