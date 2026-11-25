package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// planUnderRedo plans the run. Under a pending redo only a run that redoes it plans (the timer's pass skips, any other
// refuses), and a re-derive records its progress with each mark.
func (r lendingRun) planUnderRedo(ctx context.Context, log io.Writer, wm uint32) (interestPlan, bool, error) {
	redo, pending, err := lendingInterestRedo(ctx, r.chAddr)
	if err != nil {
		return interestPlan{}, false, err
	}
	if pending && !r.redoes(redo) {
		msg := fmt.Sprintf("lending interest: a lending_state replay rewrote the state from ledger %d, so the rows from there "+
			"on are stale; re-derive them first: -interest-only -from %d -write, then from each run's end until one reaches the tip",
			redo.From, redo.Next())
		if r.timerInterest() {
			_, _ = fmt.Fprintln(log, msg)
			return interestPlan{}, true, nil
		}
		return interestPlan{}, false, errors.New(msg)
	}
	plan, err := r.interestPlan(ctx, wm)
	if err != nil || !pending || !plan.rederive {
		return plan, false, err
	}
	mark := plan.mark
	plan.mark = func(ctx context.Context, from, thru uint32) error {
		if err := mark(ctx, from, thru); err != nil {
			return err
		}
		return advanceLendingRedo(ctx, r.chAddr, redo.Gen, thru)
	}
	return plan, false, nil
}

// seedRun reads the run's seeds, which storage proves against the lake, and holds each pool's seeded instance to the
// code the WASM timeline has in force at start: an upgrade before the run's first window rewrites the instance.
func (p *interestPass) seedRun(ctx context.Context, st *interestCarry, keys []clickhouse.LendingInterestKey, start uint32) error {
	set, err := lendingInterestSeeds(ctx, p.addr, p.pools, keys, st.seed.CommittedBefore, start)
	if err != nil {
		return err
	}
	st.known = map[string]bool{}
	for _, r := range set.Rows {
		st.known[interestKeyID(r.Key.Pool, r.Key.Kind, r.Key.Asset)] = true
		if r.Key.Kind != lendinginterest.KindConfig {
			continue
		}
		seed := lendinginterest.Version{
			Pool: r.Key.Pool, Kind: lendinginterest.KindConfig, Ledger: start, WasmHash: r.Value.WasmHash,
			TxHash: "(the seed from ledger " + strconv.FormatUint(uint64(r.Ledger), 10) + ")",
		}
		if err := checkInstanceCode(p.code, []lendinginterest.Version{seed}); err != nil {
			return err
		}
	}
	st.seed, st.seedRows, st.seedCheck, st.seeded = set.Seeds, set.Rows, true, true
	return nil
}

func interestKeyID(pool string, kind lendinginterest.Kind, asset string) string {
	return pool + "/" + strconv.Itoa(int(kind)) + "/" + asset
}

// checkNewKeys refuses a key the run neither seeded nor read before whose first version is not its creation:
// lending_state_hourly holds no row of a key that existed before the run, so its history there is missing.
func (st *interestCarry) checkNewKeys(vs []lendinginterest.Version) error {
	for _, v := range vs {
		id := interestKeyID(v.Pool, v.Kind, v.Asset)
		if st.known[id] {
			continue
		}
		if !v.Created {
			return fmt.Errorf("lending interest: pool %s key %s first read at ledger %d tx %s as a change, not its creation, "+
				"yet lending_state_hourly holds no row of it before the run: lending_state is missing its history; replay it over the key's past first",
				v.Pool, id, v.Ledger, v.TxHash)
		}
		st.known[id] = true
	}
	return nil
}

// withSeedsBefore puts the proved seeds in place of lending_state's rows before the first checked window, so the
// check starts from the values the fold did.
func withSeedsBefore(state, seeds []clickhouse.LendingInterestStateRow) []clickhouse.LendingInterestStateRow {
	out := make([]clickhouse.LendingInterestStateRow, 0, len(state)+len(seeds))
	for _, s := range state {
		if !s.Before {
			out = append(out, s)
		}
	}
	for _, s := range seeds {
		if s.Key.Kind != lendinginterest.KindConfig {
			out = append(out, s)
		}
	}
	return out
}

// checkDerived refuses a window while a pool deployed by hi has no instance row in lending_state_hourly: every pool
// writes its instance at deploy, so the state pass never derived it, and its empty key list would mark it covered.
// A pool with an instance row and no reserve is empty and passes.
func (p *interestPass) checkDerived(ctx context.Context, st *interestCarry, hi uint32) error {
	var check []clickhouse.LendingContract
	for _, c := range p.pools {
		if c.Deployed <= hi && !st.derived[c.ID] {
			check = append(check, c)
		}
	}
	if len(check) == 0 {
		return nil
	}
	missing, err := lendingInterestUnderived(ctx, p.addr, check, hi)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		why := make([]string, 0, len(missing))
		for _, m := range missing {
			if m.Lake {
				why = append(why, fmt.Sprintf("lending_state has not derived pool %s (the lake holds its instance at deploy ledger %d, "+
					"lending_state_hourly no instance row); replay lending_state from its deploy ledger first: -lending-only -from %d -write",
					m.Pool, m.Deployed, m.Deployed))
				continue
			}
			why = append(why, fmt.Sprintf("pool %s: neither lending_state nor the lake holds its instance at deploy ledger %d; "+
				"the lake is missing that ledger's changes", m.Pool, m.Deployed))
		}
		return fmt.Errorf("lending interest: %s", strings.Join(why, "; "))
	}
	if st.derived == nil {
		st.derived = map[string]bool{}
	}
	for _, c := range check {
		st.derived[c.ID] = true
	}
	return nil
}
