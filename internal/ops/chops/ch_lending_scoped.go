package chops

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// checkScoped refuses -lending-contract outside a -from re-derive: the scoped walk fills keys under the watermark and
// owns no range of its own.
func (r lendingRun) checkScoped() error {
	switch {
	case len(r.contracts) == 0:
		return nil
	case r.printPrefixes || r.prove:
		return fmt.Errorf("-lending-contract applies to a -from re-derive, not -print-prefixes or -prove")
	case r.interest != (interestFlags{}):
		return fmt.Errorf("-lending-contract re-derives lending_state only; drop the -interest-* flags")
	case r.from == 0:
		return fmt.Errorf("-lending-contract needs -from: the scoped walk re-derives a range under the watermark")
	}
	return nil
}

// scopedRange is [from, min(to, wm)]; to = 0 means the watermark. A range past the watermark is refused: the scoped walk
// never moves it, so it must not write hours the watermark does not yet claim.
func scopedRange(from, to, wm uint32) (uint32, uint32, error) {
	if wm == 0 {
		return 0, 0, fmt.Errorf("-lending-contract: lending_state has no watermark yet; run the full derive first")
	}
	if to == 0 {
		to = wm
	}
	if to > wm || from > to {
		return 0, 0, fmt.Errorf("-lending-contract: [-from %d, -to %d] must lie within [1, the watermark %d]", from, to, wm)
	}
	return from, to, nil
}

// scopeLendingRegistry is reg's entries for ids, refusing an id the registry does not hold.
func scopeLendingRegistry(reg []clickhouse.LendingContract, ids []string) ([]clickhouse.LendingContract, error) {
	var out []clickhouse.LendingContract
	for _, id := range ids {
		i := slices.IndexFunc(reg, func(c clickhouse.LendingContract) bool { return c.ID == id })
		if i < 0 {
			return nil, fmt.Errorf("-lending-contract %s is not a registered lending-state contract", id)
		}
		out = append(out, reg[i])
	}
	return out, nil
}

// scopedPass re-derives only r.contracts' keys over [-from, min(-to, watermark)] into the same table, holding -lock-file
// exclusive. It never moves the watermark or writes the heartbeat, and arms the interest redo only when an interest
// pool is in scope: a Comet pool or backstop feeds no interest row.
func (r lendingRun) scopedPass(ctx context.Context) error {
	if !r.dryRun {
		unlock, err := acquireRollupLock(lendingJob, r.lockFile)
		if err != nil {
			return err
		}
		defer unlock()
	}
	wm, err := lendingInterestStateWM(ctx, r.chAddr)
	if err != nil {
		return err
	}
	lo, hi, err := scopedRange(r.from, r.to, wm)
	if err != nil {
		return err
	}
	reg, err := lendingRegistry(ctx, r.chAddr, hi)
	if err != nil {
		return err
	}
	scope, err := scopeLendingRegistry(reg, r.contracts)
	if err != nil {
		return err
	}
	ps, err := clickhouse.LendingPrefixes(scope)
	if err != nil {
		return err
	}
	redo := len(lendingPools(scope)) > 0
	if r.dryRun {
		fmt.Fprintf(os.Stderr, "%s: would re-derive [%d,%d] over %d key prefixes of %d contracts (interest redo: %t)\n", lendingJob, lo, hi, len(ps), len(scope), redo)
		return nil
	}
	if redo {
		if err := clickhouse.ArmLendingInterestRedo(ctx, r.chAddr, lo); err != nil {
			return err
		}
	}
	runStart := time.Now()
	err = deriveLendingWindows(lo, hi, os.Stderr,
		func(lo, hi uint32) (uint64, error) { return clickhouse.DeriveLendingWindow(ctx, r.chAddr, lo, hi, ps) },
		func(uint32, uint32, uint64) error { return nil },
		func(lo, hi uint32) {
			fmt.Fprintf(os.Stderr, "%s: scoped window [%d,%d] done (elapsed %s)\n", lendingJob, lo, hi, time.Since(runStart).Round(time.Second))
		})
	if err != nil {
		return fmt.Errorf("%w; the scoped walk keeps no cursor: re-run it from the failed window's start", err)
	}
	return nil
}
