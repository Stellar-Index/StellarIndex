package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
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
// reconcile — into a runnable operator command.
//
// It diffs every watched contract's sep41_supply_rollup checkpoint against
// a re-sum of the sep41_supply_events rows it folds (ledger ≤ last_ledger)
// and reports any (contract, kind) that differ by more than -tolerance. It
// catches a checkpoint double-fold, which row-count reconciles cannot see
// because the raw rows are correct.
//
// The re-sum is the same-source PG aggregate, not the lake: the PG observer
// is watched-set-gated and bare-i128-only, so per-contract totals
// legitimately differ from the lake (migrations 0085/0088). The projection
// reconcile proves sep41_supply_events faithful to the lake, so agreement
// here implies agreement with the lake.
//
// Each re-sum can scan every chunk of a hundreds-of-millions-row hypertable,
// so this is a one-shot post-re-derive check, never a per-tick job. Run it
// on r1 under the heavy-job wrapper:
//
//	run-heavy-job.sh verify-rollup stellarindex-ops supply verify-rollup -config /etc/stellarindex/stellarindex.toml
//
// Flags:
//
//	-config PATH             Required. Operator TOML (Postgres DSN).
//	-contracts C1,C2,...     Restrict the check to these contract
//	                         C-strkeys (default: the operator's
//	                         [supply].watched_sep41_contracts; falls back
//	                         to every sep41_supply_rollup row when that
//	                         list is empty). A requested contract with no
//	                         checkpoint row is reported MISSING and fails
//	                         the run.
//	-tolerance N             Absolute stroop tolerance per (contract,
//	                         kind) before a diff is reported (default 0;
//	                         a small value absorbs a worker advance racing
//	                         the re-sum).
//	-statement-timeout DUR   PG statement_timeout for EACH per-contract
//	                         re-sum (default 15m).
//	-timeout DUR             Overall wall-clock budget (default 2h).
//	-textfile-output PATH    Path to write a Prometheus textfile
//	                         (node_exporter textfile_collector format) so
//	                         a "clean" claim is a scrape, not a pasted
//	                         transcript. Empty = no metrics emit.
func supplyVerifyRollup(args []string) error {
	fs := flag.NewFlagSet("supply verify-rollup", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	contractsRaw := fs.String("contracts", "", "Comma-separated contract C-strkeys to check (default: the configured watched_sep41_contracts, else all sep41_supply_rollup rows)")
	toleranceRaw := fs.String("tolerance", "0", "Absolute stroop tolerance per (contract,kind) before a diff is reported")
	stmtTimeout := fs.Duration("statement-timeout", 15*time.Minute, "PG statement_timeout for each per-contract re-sum")
	timeout := fs.Duration("timeout", 2*time.Hour, "overall wall-clock budget")
	textfileOut := fs.String("textfile-output", "", "Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	startedAt := time.Now()
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
		return verifyRollupFail(*textfileOut, startedAt, err)
	}
	contracts = resolveVerifyRollupContracts(contracts, cfg.Supply.WatchedSEP41Contracts)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return verifyRollupFail(*textfileOut, startedAt, fmt.Errorf("storage: %w", err))
	}
	defer func() { _ = store.Close() }()

	drifts, checked, missing, err := verifyRollupDrifts(ctx, store, contracts, tolerance, *stmtTimeout)
	if err != nil {
		return verifyRollupFail(*textfileOut, startedAt, err)
	}
	reportRollupDrifts(os.Stdout, drifts, checked, tolerance)
	reportRollupMissing(os.Stdout, missing)
	decision := rollupExitDecision(drifts, checked, missing)
	if *textfileOut != "" {
		if werr := supply.WriteVerifyRollupTextfile(*textfileOut, checked, len(drifts), len(missing), time.Since(startedAt).Seconds(), decision == nil); werr != nil && decision == nil {
			return fmt.Errorf("write verify-rollup textfile: %w", werr)
		}
	}
	return decision
}

// verifyRollupFail is the early-failure path: a config/storage/query error
// before checked/drift/missing counts exist. Mirrors
// supplySnapshotMaybeEmitFailure — emits the minimal failure textfile
// (carrying the previous last_success_timestamp forward) without masking
// the original cause.
func verifyRollupFail(textfileOut string, startedAt time.Time, cause error) error {
	if textfileOut != "" {
		_ = supply.WriteVerifyRollupFailureTextfile(textfileOut, time.Since(startedAt).Seconds())
	}
	return cause
}

// rollupExitDecision is the DB-free core of the command's exit status: a
// missing watched contract or a drift is always fatal, and — so "nothing
// checked" is never conflated with "all clean" — checking zero checkpoints
// is fatal too. An empty sep41_supply_rollup table or a -contracts filter
// that matched nothing must not print "OK: 0 checkpoint(s) reconcile" and
// exit 0; that reads as a clean bill of health when in fact nothing was
// verified.
func rollupExitDecision(drifts []completeness.TotalsDrift, checked int, missing []string) error {
	if len(missing) > 0 {
		return fmt.Errorf("verify-rollup: %d watched contract(s) have no sep41_supply_rollup checkpoint — unexamined, not clean: %s", len(missing), strings.Join(missing, ", "))
	}
	if len(drifts) > 0 {
		return fmt.Errorf("sep41 rollup drift: %d (contract,kind) checkpoint(s) diverge from the authoritative re-sum — reset the fold and re-fold (`ch-rebuild -sep41 -write`, or Store.ResetSEP41SupplyRollupFold for a scoped set)", len(drifts))
	}
	if checked == 0 {
		return fmt.Errorf("verify-rollup checked 0 checkpoints — sep41_supply_rollup is empty or -contracts matched nothing; refusing to certify clean")
	}
	return nil
}

// verifyRollupDrifts is the DB-free core: for each checkpoint it computes
// the same-source bounded re-sum (truth) at the checkpoint's own
// last_ledger, then hands both maps to
// completeness.ReconcileRunningTotals. Returns the drifts (empty = clean),
// the number of contracts checked, and — when contractIDs was given
// explicitly (an operator -contracts subset, or the watched-set default) —
// which of those requested contracts came back with no checkpoint row at
// all (missing, sorted). Bounding the re-sum at each contract's
// last_ledger is load-bearing: the fold covers exactly ledger ≤
// last_ledger, so summing to the tip would count the (correctly)
// un-folded live delta and false-positive.
func verifyRollupDrifts(ctx context.Context, r rollupTruthReader, contractIDs []string, tolerance *big.Int, stmtTimeout time.Duration) (drifts []completeness.TotalsDrift, checked int, missing []string, err error) {
	checkpoints, err := r.ListSEP41RollupCheckpoints(ctx, contractIDs)
	if err != nil {
		return nil, 0, nil, err
	}
	cpMap := make(map[string]completeness.RunningTotals, len(checkpoints))
	truthMap := make(map[string]completeness.RunningTotals, len(checkpoints))
	got := make(map[string]struct{}, len(checkpoints))
	for _, cp := range checkpoints {
		got[cp.ContractID] = struct{}{}
		cpMap[cp.ContractID] = kindTotalsToRunning(cp.Fold)
		resum, rerr := r.SEP41SupplyEventKindResum(ctx, cp.ContractID, cp.LastLedger, stmtTimeout)
		if rerr != nil {
			return nil, 0, nil, fmt.Errorf("re-sum %s@%d: %w", cp.ContractID, cp.LastLedger, rerr)
		}
		truthMap[cp.ContractID] = kindTotalsToRunning(resum)
	}
	for _, c := range contractIDs {
		if _, ok := got[c]; !ok {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return completeness.ReconcileRunningTotals(cpMap, truthMap, tolerance), len(checkpoints), missing, nil
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

// reportRollupMissing prints the watched contracts that came back with no
// sep41_supply_rollup checkpoint row at all — the case reportRollupDrifts
// can't show, since a missing contract contributes no drift and no
// checked count.
func reportRollupMissing(out io.Writer, missing []string) {
	if len(missing) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "MISSING: %d watched contract(s) have no sep41_supply_rollup checkpoint — unexamined, not clean:\n", len(missing))
	for _, c := range missing {
		_, _ = fmt.Fprintf(out, "  %s\n", c)
	}
}

// resolveVerifyRollupContracts is the DB-free default-selection core: an
// explicit -contracts flag always wins (operator-scoped / resumed run);
// otherwise the run cross-checks against the configured watched set so a
// watched contract whose checkpoint row was deleted is reported MISSING
// rather than silently absent from "every row the table happens to hold".
// Falls back to nil ("check all") only when nothing is configured (the
// SEP-41 pipeline is off) — there is no watched set to cross-check against.
func resolveVerifyRollupContracts(explicit, watched []string) []string {
	if len(explicit) > 0 {
		return explicit
	}
	return watched
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
