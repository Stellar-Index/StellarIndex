package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rollupTruthReader is the storage seam supplyVerifyRollup depends on —
// split out so the drift-computation core (verifyRollupDrifts) is
// unit-testable with a fake, without a live Postgres.
type rollupTruthReader interface {
	ListSEP41RollupCheckpoints(ctx context.Context, contractIDs []string) ([]timescale.SEP41RollupCheckpoint, error)
	SEP41SupplyEventKindResum(ctx context.Context, contractID string, asOfLedger uint32, statementTimeout time.Duration) (timescale.SEP41KindTotals, error)
}

// supplyVerifyRollup wires internal/completeness.ReconcileRunningTotals
// — the fourth ADR-0033 integrity check, the DERIVED-CHECKPOINT
// reconcile — into a runnable operator command. Before this it was pure
// but uncalled: a comparator with no caller.
//
// It diffs every watched contract's sep41_supply_rollup fold (the served
// incremental checkpoint) against the AUTHORITATIVE same-source PG re-sum
// of the exact sep41_supply_events rows that checkpoint folds
// (ledger ≤ last_ledger), and reports any (contract, kind) that disagree
// by more than -tolerance. It would have caught the KALE 2× double-fold
// (incident 2026-07-06): a re-derive re-folded history below the
// checkpoint, so checkpoint = 2×truth — invisible to the row-count
// reconciles because the raw rows were correct.
//
// TRUTH SOURCE (see internal/completeness/rollup.go): the re-sum is the
// SAME-SOURCE sep41_supply_events aggregate, NOT the ClickHouse lake. The
// PG observer is watched-set-gated + bare-i128-only while the lake is
// network-wide + map-variant-aware, so their per-contract totals
// legitimately differ (migrations 0085/0088). The projection reconcile
// (internal/completeness/reconcile.go) separately proves
// sep41_supply_events faithful to the lake row-for-row, so
// checkpoint == PG re-sum ⇒ checkpoint == lake truth transitively —
// without importing the lake's methodology into this check.
//
// CADENCE — this is a SLOW / post-re-derive operator check, NOT a
// per-tick job, and it must NEVER run in the aggregator hot path (it is a
// one-shot CLI). Each per-contract re-sum is the full at-or-before
// aggregate the served fast path avoids; on the hundreds-of-millions-row
// hypertable it can scan every chunk (the incident's 30s probe timed out
// at just 6 contracts). Consequences baked in here:
//   - each re-sum runs under a generous -statement-timeout (default 15m),
//   - -contracts scopes the run to a subset so an operator can check
//     incrementally / resume,
//   - -timeout bounds the whole run (default 2h).
//
// On r1, run it under the heavy-job wrapper so a runaway scan can't
// starve galexie's captive core (CLAUDE.md "Heavy one-shot jobs"):
//
//	run-heavy-job.sh verify-rollup stellarindex-ops supply verify-rollup -config /etc/stellarindex/stellarindex.toml
//
// Flags:
//
//	-config PATH             Required. Operator TOML (Postgres DSN).
//	-contracts C1,C2,...     Restrict the check to these contract
//	                         C-strkeys (default: every sep41_supply_rollup
//	                         row). Makes the check scoped + resumable.
//	-tolerance N             Absolute stroop tolerance per (contract,
//	                         kind) before a diff is reported (default 0 —
//	                         exact, the expected posture since both sides
//	                         sum the same integer amounts). A small
//	                         nonzero value absorbs an in-flight worker
//	                         advance racing the re-sum snapshot.
//	-statement-timeout DUR   PG statement_timeout for EACH per-contract
//	                         re-sum (default 15m).
//	-timeout DUR             Overall wall-clock budget (default 2h).
func supplyVerifyRollup(args []string) error {
	fs := flag.NewFlagSet("supply verify-rollup", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	contractsRaw := fs.String("contracts", "", "Comma-separated contract C-strkeys to check (default: all sep41_supply_rollup rows)")
	toleranceRaw := fs.String("tolerance", "0", "Absolute stroop tolerance per (contract,kind) before a diff is reported")
	stmtTimeout := fs.Duration("statement-timeout", 15*time.Minute, "PG statement_timeout for each per-contract re-sum")
	timeout := fs.Duration("timeout", 2*time.Hour, "overall wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	tolerance, err := parseTolerance(*toleranceRaw)
	if err != nil {
		return err
	}
	if *stmtTimeout <= 0 {
		return fmt.Errorf("-statement-timeout must be > 0 (got %s)", *stmtTimeout)
	}
	contracts := parseContractsCSV(*contractsRaw)

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	drifts, checked, err := verifyRollupDrifts(ctx, store, contracts, tolerance, *stmtTimeout)
	if err != nil {
		return err
	}
	reportRollupDrifts(os.Stdout, drifts, checked, tolerance)
	if len(drifts) > 0 {
		return fmt.Errorf("sep41 rollup drift: %d (contract,kind) checkpoint(s) diverge from the authoritative re-sum — reset the fold and re-fold (`ch-rebuild -sep41 -write`, or Store.ResetSEP41SupplyRollupFold for a scoped set)", len(drifts))
	}
	return nil
}

// verifyRollupDrifts is the DB-free core: for each checkpoint it computes
// the same-source bounded re-sum (truth) at the checkpoint's own
// last_ledger, then hands both maps to
// completeness.ReconcileRunningTotals. Returns the drifts (empty = clean)
// and the number of contracts checked. Bounding the re-sum at each
// contract's last_ledger is load-bearing: the fold covers exactly
// ledger ≤ last_ledger, so summing to the tip would count the (correctly)
// un-folded live delta and false-positive.
func verifyRollupDrifts(ctx context.Context, r rollupTruthReader, contractIDs []string, tolerance *big.Int, stmtTimeout time.Duration) ([]completeness.TotalsDrift, int, error) {
	checkpoints, err := r.ListSEP41RollupCheckpoints(ctx, contractIDs)
	if err != nil {
		return nil, 0, err
	}
	cpMap := make(map[string]completeness.RunningTotals, len(checkpoints))
	truthMap := make(map[string]completeness.RunningTotals, len(checkpoints))
	for _, cp := range checkpoints {
		cpMap[cp.ContractID] = kindTotalsToRunning(cp.Fold)
		resum, rerr := r.SEP41SupplyEventKindResum(ctx, cp.ContractID, cp.LastLedger, stmtTimeout)
		if rerr != nil {
			return nil, 0, fmt.Errorf("re-sum %s@%d: %w", cp.ContractID, cp.LastLedger, rerr)
		}
		truthMap[cp.ContractID] = kindTotalsToRunning(resum)
	}
	return completeness.ReconcileRunningTotals(cpMap, truthMap, tolerance), len(checkpoints), nil
}

// kindTotalsToRunning adapts the storage per-kind totals to the
// completeness comparator's shape (both are i128-safe *big.Int triples).
func kindTotalsToRunning(t timescale.SEP41KindTotals) completeness.RunningTotals {
	return completeness.RunningTotals{Mint: t.Mint, Burn: t.Burn, Clawback: t.Clawback}
}

// reportRollupDrifts writes a stable, diffable report: one row per
// (contract, kind) drift with checkpoint / truth / delta, or an OK line
// when clean. Delta > 0 is an over-count (double-fold signature),
// Delta < 0 an under-count (a below-checkpoint edit the worker never
// re-summed).
func reportRollupDrifts(out io.Writer, drifts []completeness.TotalsDrift, checked int, tolerance *big.Int) {
	if len(drifts) == 0 {
		_, _ = fmt.Fprintf(out, "OK: %d checkpoint(s) reconcile with the authoritative re-sum (0 drift, tolerance %s)\n", checked, tolerance)
		return
	}
	_, _ = fmt.Fprintf(out, "DRIFT: %d of %d checkpoint(s) diverge (tolerance %s)\n", len(drifts), checked, tolerance)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CONTRACT\tKIND\tCHECKPOINT\tTRUTH\tDELTA")
	for _, d := range drifts {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			d.ContractID, d.Kind, d.Checkpoint.String(), d.Truth.String(), d.Delta.String())
	}
	_ = w.Flush()
}

// parseContractsCSV splits a comma-separated contract list, trimming
// whitespace and dropping empties. Empty input → nil (check all).
func parseContractsCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil // all-blank (e.g. " , ,") reads as "check all"
	}
	return out
}

// parseTolerance parses the -tolerance flag into a non-negative *big.Int
// (i128-safe; the KALE delta alone can exceed int64). Empty → zero.
func parseTolerance(raw string) (*big.Int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return big.NewInt(0), nil
	}
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("-tolerance %q is not a base-10 integer", raw)
	}
	if v.Sign() < 0 {
		return nil, fmt.Errorf("-tolerance must be ≥ 0 (got %s)", v)
	}
	return v, nil
}
