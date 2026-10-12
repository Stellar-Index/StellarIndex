package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// ledgerCloseTimeReader resolves a ledger's real close time from ClickHouse stellar.ledgers (found=false means no lake row); an interface so resolveSnapshotLedger is testable without a lake.
type ledgerCloseTimeReader interface {
	CloseTimeForLedger(ctx context.Context, ledger uint32) (time.Time, bool, error)
	// LatestLedgerAtOrBefore returns the newest stellar.ledgers row in [maxSeq-maxAutoSnapshotClampLedgers, maxSeq]; found=false means none in that window.
	LatestLedgerAtOrBefore(ctx context.Context, maxSeq uint32) (uint32, time.Time, bool, error)
}

// maxAutoSnapshotClampLedgers bounds how far the auto snapshot-ledger clamp may walk back from the live cursor to the lake's landed tip.
// The clamp absorbs the dual-sink landing race (single-digit ledgers); a larger gap is a stalled lake and must fail closed, not be hidden.
// It is the lookup's own scan window ([clickhouse.LatestLedgerLookbackLedgers]) so refusal and read cannot drift; same value as cmd/stellarindex-aggregator maxSupplyLakeClampLedgers.
const maxAutoSnapshotClampLedgers = clickhouse.LatestLedgerLookbackLedgers

// cursorReader is the ingestion-cursor input of resolveSnapshotLedger, an interface so auto-resolve is testable without Postgres.
type cursorReader interface {
	ListCursors(ctx context.Context) ([]timescale.Cursor, error)
}

// chainCursorSource is the ingestion_cursors row for the live indexer's walk (the indexer's `cursorSource`): the only cursor that means "chain position the supply components derive from"; the rest are ops-job progress.
const chainCursorSource = "ledgerstream"

// Run is the entry point of the supply ops package; see discovery.Run for the shared calling convention.
func Run(args []string) error {
	switch args[0] {
	case "supply":
		return supplyCmd(args[1:])
	default:
		return fmt.Errorf("internal/ops/supply: unknown subcommand %q", args[0])
	}
}

// supplyCmd dispatches the supply subcommands: audit (read), snapshot (write), seed-observations, seed-sac-balances, seed-claimable-balances, seed-sep41-genesis (lake-derived bootstraps) and verify-rollup (sep41_supply_rollup fold-drift reconcile).
func supplyCmd(args []string) error {
	const usage = "usage: supply <audit|snapshot|seed-observations|seed-sac-balances|seed-claimable-balances|seed-sep41-genesis|verify-rollup> [flags]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	if opsutil.IsHelpArg(args[0]) {
		return opsutil.Usage(usage)
	}
	switch args[0] {
	case "audit":
		return supplyAudit(args[1:])
	case "snapshot":
		return supplySnapshot(args[1:])
	case "seed-observations":
		return supplySeedObservations(args[1:])
	case "seed-sac-balances":
		return supplySeedSACBalances(args[1:])
	case "seed-claimable-balances":
		return supplySeedClaimableBalances(args[1:])
	case "seed-sep41-genesis":
		return supplySeedSEP41Genesis(args[1:])
	case "verify-rollup":
		return supplyVerifyRollup(args[1:])
	default:
		return fmt.Errorf("unknown supply subcommand %q (expected: audit | snapshot | seed-observations | seed-sac-balances | seed-claimable-balances | seed-sep41-genesis | verify-rollup)", args[0])
	}
}

// supplySnapshot computes a supply snapshot and writes it to asset_supply_history. The CLI is native-XLM-only: classic and SEP-41 assets use the aggregator goroutine path (docs/operations/supply-snapshot.md), and the two paths are mutually exclusive.
// ObservedAt is the chosen ledger's REAL close time from stellar.ledgers (-ch-addr), never wall-clock, so a re-derived historical snapshot stays point-in-time correct.
func supplySnapshot(args []string) error {
	fs := flag.NewFlagSet("supply snapshot", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	assetRaw := fs.String("asset", "native", "Asset to snapshot (native only at v1)")
	ledgerArg := fs.Int("ledger", 0, "Ledger sequence to attribute to (default: latest from ingestion_cursors)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (source of the ledger close time stamped as ObservedAt)")
	gate := opsutil.RegisterWriteGate(fs)
	textfileOut := fs.String("textfile-output", "", "Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *assetRaw != "native" {
		return fmt.Errorf("supply snapshot: asset %q is not served by this CLI subcommand — classic (Algorithm 2) + SEP-41 (Algorithm 3) computers ship in `internal/supply/{classic,sep41}.go` and run via the aggregator-resident goroutine path (`[supply] aggregator_refresh_enabled`). See docs/operations/supply-snapshot.md §\"Asset-class scope\"", *assetRaw)
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Supply.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	startedAt := time.Now()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, err)
	}
	defer func() { _ = store.Close() }()
	// Re-derive path (migration 0109): stamp a positive
	// derive_generation so a corrected supply snapshot UPDATEs the stored
	// value in place and wins over the live gen-0 snapshot.
	store.SetDeriveGeneration(startedAt.Unix())

	// The snapshot's ObservedAt must be the chosen ledger's REAL close time
	// — a re-derived HISTORICAL snapshot stamped with the wall-clock
	// write-time corrupts point-in-time supply queries. The authoritative
	// every-ledger source is ClickHouse stellar.ledgers.
	closeTimes, err := clickhouse.NewExplorerReader(ctx, *chAddr)
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, fmt.Errorf("clickhouse close-time reader: %w", err))
	}
	defer func() { _ = closeTimes.Close() }()

	ledger, observedAt, err := resolveSnapshotLedger(ctx, store, closeTimes, uint32(*ledgerArg))
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, err)
	}

	staticReader, err := cfg.Supply.NewStaticReserveReader()
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, fmt.Errorf("reserve reader: %w", err))
	}
	// Live LCM reader tries first; ErrNoObservation falls back to
	// the dated static config per ADR-0021.
	reader := supply.NewChainedReserveBalanceReader(
		supply.NewLCMReserveBalanceReader(supplyStoreLookup{s: store}),
		staticReader,
	)
	computer, err := supply.NewXLMComputerForNetwork(cfg.Stellar.Passphrase(), cfg.Supply.SDFReserveAccounts, reader)
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, fmt.Errorf("xlm computer: %w", err))
	}

	snap, err := computer.Compute(ctx, ledger, observedAt)
	if err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, fmt.Errorf("compute: %w", err))
	}

	printSupplySnapshot("SNAPSHOT", "native", snap.AssetKey, snap)
	if dryRun {
		fmt.Println("─── DRY RUN ─── snapshot NOT written to asset_supply_history.")
		return nil
	}
	if err := persistSnapshot(ctx, store, snap, time.Now()); err != nil {
		return supplySnapshotMaybeEmitFailure(*textfileOut, *assetRaw, startedAt, err)
	}

	if *textfileOut != "" {
		if err := supply.WriteSnapshotTextfile(*textfileOut, snap, time.Since(startedAt).Seconds(), true); err != nil {
			return fmt.Errorf("write textfile: %w", err)
		}
	}
	return nil
}

// persistSnapshot writes snap and, when its day bucket is older than supply_1d's refresh policy reaches, refreshes it explicitly. A
// failed refresh fails the run: the row is written but not yet served.
func persistSnapshot(ctx context.Context, store *timescale.Store, snap supply.Supply, now time.Time) error {
	if err := store.InsertSupply(ctx, snap); err != nil {
		return fmt.Errorf("InsertSupply: %w", err)
	}
	fmt.Printf("Wrote snapshot for asset_key=%s ledger=%d basis=%s\n",
		snap.AssetKey, snap.LedgerSequence, snap.Basis)

	from, to, ok := supplyCAGGRefreshWindow(snap.ObservedAt, now)
	if !ok {
		return nil
	}
	if err := store.RefreshContinuousAggregate(ctx, timescale.SupplyCAGG.Name, from, to); err != nil {
		return fmt.Errorf("refresh %s [%s, %s): %w", timescale.SupplyCAGG.Name,
			from.Format(time.DateOnly), to.Format(time.DateOnly), err)
	}
	fmt.Printf("Refreshed %s over [%s, %s)\n", timescale.SupplyCAGG.Name,
		from.Format(time.DateOnly), to.Format(time.DateOnly))
	return nil
}

// supplyCAGGPolicyCover is how far back supply_1d's own refresh policy is trusted to pick a snapshot up: a day short of its 7-day
// start_offset, so a straddling bucket is refreshed here.
const supplyCAGGPolicyCover = 6 * 24 * time.Hour

// supplyCAGGRefreshWindow returns the supply_1d range to refresh for a snapshot observed at observedAt (its UTC day padded a day each
// side, meeting [timescale.SupplyCAGG]'s MinWindow), or ok=false when the policy covers it.
func supplyCAGGRefreshWindow(observedAt, now time.Time) (from, to time.Time, ok bool) {
	day := observedAt.UTC().Truncate(24 * time.Hour)
	if !day.Before(now.Add(-supplyCAGGPolicyCover)) {
		return time.Time{}, time.Time{}, false
	}
	return day.Add(-24 * time.Hour), day.Add(48 * time.Hour), true
}

// supplyStoreLookup adapts *timescale.Store to supply.AccountObservationLookup, projecting to the smaller AccountObservationRow.
type supplyStoreLookup struct{ s *timescale.Store }

func (a supplyStoreLookup) LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (supply.AccountObservationRow, error) {
	row, err := a.s.LatestAccountObservationAtOrBefore(ctx, accountID, asOfLedger)
	if err != nil {
		return supply.AccountObservationRow{}, err
	}
	return supply.AccountObservationRow{
		Balance:   row.Balance,
		IsRemoval: row.IsRemoval,
		Ledger:    row.Ledger,
	}, nil
}

// MaxAccountObservationLedger forwards the account-observer watermark used by the XLM freshness gate.
func (a supplyStoreLookup) MaxAccountObservationLedger(ctx context.Context, asOfLedger uint32) (uint32, error) {
	return a.s.MaxAccountObservationLedger(ctx, asOfLedger)
}

// supplySnapshotMaybeEmitFailure writes a fail-marker textfile (so the staleness alert keys on time-since-last-pass) and returns cause;
// an empty textfileOut skips the emit.
func supplySnapshotMaybeEmitFailure(textfileOut, assetRaw string, startedAt time.Time, cause error) error {
	if textfileOut != "" {
		// Best-effort fail emit — don't mask the original cause.
		_ = supply.WriteSnapshotFailureTextfile(textfileOut, assetRaw, time.Since(startedAt).Seconds())
	}
	return cause
}

// resolveSnapshotLedger picks the snapshot ledger (operator -ledger wins, else the live ledgerstream cursor) and stamps ObservedAt with its close_time from stellar.ledgers, NEVER time.Now(): a wall-clock stamp corrupts point-in-time supply queries.
// Fail closed: a missing lake row is a gap to surface. KNOWN LIMIT: a stalled supply observer can leave a component behind the snapshot ledger (see supplyAggregatorLedgers in cmd/stellarindex-aggregator/main.go).
func resolveSnapshotLedger(ctx context.Context, store cursorReader, closeTimes ledgerCloseTimeReader, opLedger uint32) (uint32, time.Time, error) {
	ledger := opLedger
	if ledger == 0 {
		cursors, err := store.ListCursors(ctx)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("ListCursors: %w", err)
		}
		var chosen string
		ledger, chosen = autoSnapshotLedger(cursors)
		if ledger == 0 {
			return 0, time.Time{}, errors.New("no ingestion cursors yet — pass -ledger explicitly until the indexer has produced a cursor")
		}
		fmt.Fprintf(os.Stderr, "supply: auto-resolved snapshot ledger %d from the %q cursor\n", ledger, chosen)

		// The ledgerstream cursor leads the lake by seconds, so its own row may not be landed yet when a timer fires. Clamp to the newest LANDED ledger at or before the cursor, bounded so a stalled lake still fails closed and the lake read scans only that window.
		lakeLedger, lakeClose, found, err := closeTimes.LatestLedgerAtOrBefore(ctx, ledger)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("resolve lake tip at or before cursor ledger %d: %w", ledger, err)
		}
		if !found {
			return 0, time.Time{}, fmt.Errorf("stellar.ledgers has no row within %d ledgers below cursor ledger %d — lake empty, gapped below the chain position, or a sink stalled further back than the clamp would accept; refusing to stamp the snapshot with wall-clock time", maxAutoSnapshotClampLedgers, ledger)
		}
		// The production reader cannot return a row beyond the bound (same constant as its scan window); this stays as the resolver's own invariant over any ledgerCloseTimeReader, since stamping an unchecked row is the failure being prevented.
		if gap := ledger - lakeLedger; gap > maxAutoSnapshotClampLedgers {
			return 0, time.Time{}, fmt.Errorf("lake tip %d trails the %q cursor %d by %d ledgers (> %d) — that is a stalled lake, not a landing race; refusing to stamp a snapshot that far behind the chain", lakeLedger, chosen, ledger, gap, maxAutoSnapshotClampLedgers)
		}
		if lakeLedger != ledger {
			fmt.Fprintf(os.Stderr, "supply: clamped snapshot ledger %d -> %d (lake landed tip; cursor leads by %d)\n", ledger, lakeLedger, ledger-lakeLedger)
		}
		return lakeLedger, lakeClose, nil
	}

	// Operator-named -ledger: exact fail-closed lookup, unchanged. An
	// explicitly requested position that is absent from the lake is a
	// genuine gap worth surfacing, never something to silently shift.
	closeTime, found, err := closeTimes.CloseTimeForLedger(ctx, ledger)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("resolve close time for ledger %d: %w", ledger, err)
	}
	if !found {
		return 0, time.Time{}, fmt.Errorf("ledger %d has no stellar.ledgers row — cannot resolve its close time (lake gap?); refusing to stamp the snapshot with wall-clock time", ledger)
	}
	return ledger, closeTime, nil
}

// autoSnapshotLedger picks the stamp ledger for a `-ledger`-less snapshot and names the source cursor. Pure.
// It must not be MAX(last_ledger) over ingestion_cursors, which holds JOB positions: only `ledgerstream` means how far supply data has been ingested, and MAX could attribute a wrong ledger to the most-consumed number we serve.
// The MAX fallback only covers a host with no ledgerstream row yet; the KNOWN LIMIT on resolveSnapshotLedger still applies.
func autoSnapshotLedger(cursors []timescale.Cursor) (ledger uint32, source string) {
	for _, c := range cursors {
		if c.Source == chainCursorSource {
			return c.LastLedger, c.Source
		}
	}
	var best timescale.Cursor
	for _, c := range cursors {
		if c.LastLedger > best.LastLedger {
			best = c
		}
	}
	if best.LastLedger == 0 {
		return 0, ""
	}
	name := best.Source
	if best.Sub != "" {
		name += "/" + best.Sub
	}
	return best.LastLedger, name + " (no " + chainCursorSource + " cursor yet — FALLBACK)"
}

// supplyAudit prints the latest supply snapshot for an asset (one positional arg: native | CODE-ISSUER | C-strkey, anywhere among the flags), with optional history and SAC cross-check
// (docs/operations/runbooks/supply.md). Flags: -config PATH (required), -cross-check <asset> (runs supply.CrossCheck per ADR-0011), -history-hours N (default 0, latest only).
func supplyAudit(args []string) error {
	a, err := parseSupplyAuditArgs(args)
	if err != nil {
		return err
	}

	primaryRaw := a.asset
	primary, err := canonical.ParseAsset(primaryRaw)
	if err != nil {
		return fmt.Errorf("parse asset %q: %w", primaryRaw, err)
	}
	primaryKey, err := supply.AssetKey(primary)
	if err != nil {
		return fmt.Errorf("asset %s has no supply key: %w", primaryRaw, err)
	}

	cfg, err := config.LoadWithEnv(a.cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	primarySnap, err := fetchSupplyOrReport(ctx, store, primaryKey, primaryRaw)
	if err != nil {
		return err
	}
	printSupplySnapshot("PRIMARY", primaryRaw, primaryKey, primarySnap)

	if a.historyHours > 0 {
		if err := printSupplyHistory(ctx, store, primaryKey, a.historyHours); err != nil {
			return err
		}
	}

	if a.crossCheck != "" {
		if err := runSupplyCrossCheck(ctx, store, a.crossCheck, primaryKey, primarySnap,
			crossCheckWrapClass(cfg.Supply.FullyWrappedSACs, a.crossCheck)); err != nil {
			return err
		}
	}
	return nil
}

// supplyAuditArgs is supply audit's parsed command line.
type supplyAuditArgs struct {
	cfgPath, crossCheck, asset string
	historyHours               int
}

const supplyAuditUsage = "usage: supply audit <asset> -config PATH [-cross-check <asset>] [-history-hours N]"

// parseSupplyAuditArgs parses flags on both sides of the asset: Go's flag package stops at the first positional, so `supply audit native
// -config PATH` would otherwise fail.
func parseSupplyAuditArgs(args []string) (supplyAuditArgs, error) {
	var a supplyAuditArgs
	fs := flag.NewFlagSet("supply audit", flag.ContinueOnError)
	fs.StringVar(&a.cfgPath, "config", "", "Path to TOML config file (required)")
	fs.StringVar(&a.crossCheck, "cross-check", "",
		"Counterpart asset to compare against (canonical wire form). "+
			"Typically the SAC contract id of a classic asset, or vice versa.")
	fs.IntVar(&a.historyHours, "history-hours", 0,
		"Trailing window of historical snapshots to print (hours). 0 = latest only.")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return supplyAuditArgs{}, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return supplyAuditArgs{}, fmt.Errorf("%s (got %d positional argument(s): %q)", supplyAuditUsage, len(positional), positional)
	}
	a.asset = positional[0]
	if a.cfgPath == "" {
		return supplyAuditArgs{}, errors.New("-config is required")
	}
	if a.historyHours < 0 {
		return supplyAuditArgs{}, fmt.Errorf("-history-hours must be ≥ 0 (got %d)", a.historyHours)
	}
	return a, nil
}

// fetchSupplyOrReport fetches the latest supply for assetKey and returns a typed error when no snapshot exists; audit treats absence as a hard fail.
func fetchSupplyOrReport(ctx context.Context, store *timescale.Store, assetKey, displayName string) (supply.Supply, error) {
	snap, err := store.LatestSupply(ctx, assetKey)
	if errors.Is(err, timescale.ErrNotFound) {
		return supply.Supply{}, fmt.Errorf("no supply snapshot for %s (key %s) — populate asset_supply_history via your supply snapshot writer before using this audit", displayName, assetKey)
	}
	if err != nil {
		return supply.Supply{}, fmt.Errorf("LatestSupply %s: %w", assetKey, err)
	}
	return snap, nil
}

// printSupplySnapshot writes a labelled block for one snapshot.
// Stable layout so log-scrapers can pick fields out by line.
func printSupplySnapshot(label, displayName, key string, snap supply.Supply) {
	fmt.Printf("─── %s: %s ───\n", label, displayName)
	fmt.Printf("  asset_key:        %s\n", key)
	fmt.Printf("  total_supply:     %s\n", snap.TotalSupply.String())
	fmt.Printf("  circulating:      %s\n", snap.CirculatingSupply.String())
	if snap.MaxSupply == nil {
		fmt.Printf("  max_supply:       (null — uncapped)\n")
	} else {
		fmt.Printf("  max_supply:       %s\n", snap.MaxSupply.String())
	}
	fmt.Printf("  basis:            %s\n", snap.Basis)
	fmt.Printf("  ledger_sequence:  %d\n", snap.LedgerSequence)
	fmt.Printf("  observed_at:      %s\n", snap.ObservedAt.UTC().Format(time.RFC3339))
	fmt.Println()
}

// printSupplyHistory prints the trailing-N-hour snapshot window for
// the asset. Used by the runbook's "is this divergence fresh or
// chronic" question.
func printSupplyHistory(ctx context.Context, store *timescale.Store, assetKey string, hours int) error {
	now := time.Now().UTC()
	from := now.Add(-time.Duration(hours) * time.Hour)
	rows, err := store.SupplyHistory(ctx, assetKey, from, now, 0)
	if err != nil {
		return fmt.Errorf("SupplyHistory %s: %w", assetKey, err)
	}
	if len(rows) == 0 {
		fmt.Printf("─── HISTORY (last %dh): no snapshots ───\n\n", hours)
		return nil
	}
	fmt.Printf("─── HISTORY (last %dh, %d snapshots) ───\n", hours, len(rows))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "TIME\tLEDGER\tBASIS\tTOTAL\tCIRCULATING\tMAX"); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, r := range rows {
		maxStr := "(null)"
		if r.MaxSupply != nil {
			maxStr = r.MaxSupply.String()
		}
		if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n",
			r.ObservedAt.UTC().Format(time.RFC3339),
			r.LedgerSequence,
			r.Basis,
			r.TotalSupply.String(),
			r.CirculatingSupply.String(),
			maxStr,
		); err != nil {
			return fmt.Errorf("write row: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	fmt.Println()
	return nil
}

// crossCheckWrapClass picks the invariant, mirroring buildCrossCheckRefresher: an attested fully-wrapped SAC gets the strict ADR-0011 equality compare, everything else the subset bound.
// Equality on a partially-wrapped pair is a guaranteed false failure (classic supply that never entered the SAC), which would make the runbook's diagnostic report an error on healthy data.
func crossCheckWrapClass(fullyWrapped []string, sacID string) supply.WrapClass {
	for _, id := range fullyWrapped {
		if id == sacID {
			return supply.WrapClassFull
		}
	}
	return supply.WrapClassPartial
}

// runSupplyCrossCheck fetches the counterpart's snapshot and runs the wrap-class cross-check: equality within 1 stroop (ADR-0011) for fully-wrapped, SAC total not exceeding classic for partial. See [crossCheckWrapClass].
func runSupplyCrossCheck(
	ctx context.Context,
	store *timescale.Store,
	otherRaw, primaryKey string,
	primarySnap supply.Supply,
	wrapClass supply.WrapClass,
) error {
	otherAsset, err := canonical.ParseAsset(otherRaw)
	if err != nil {
		return fmt.Errorf("parse cross-check asset %q: %w", otherRaw, err)
	}
	otherKey, err := supply.AssetKey(otherAsset)
	if err != nil {
		return fmt.Errorf("cross-check asset %s has no supply key: %w", otherRaw, err)
	}
	otherSnap, err := fetchSupplyOrReport(ctx, store, otherKey, otherRaw)
	if err != nil {
		return err
	}
	printSupplySnapshot("CROSS-CHECK", otherRaw, otherKey, otherSnap)

	return crossCheckAndReport(os.Stdout, primarySnap, otherSnap, wrapClass, primaryKey)
}

// crossCheckAndReport prints the comparison verdict. Snapshots too many ledgers apart are reported MISALIGNED as an error, as the
// aggregator's refresher refuses the same pair.
func crossCheckAndReport(w io.Writer, primarySnap, otherSnap supply.Supply, wrapClass supply.WrapClass, primaryKey string) error {
	result, err := supply.CrossCheckForClass(primarySnap, otherSnap, wrapClass)
	if errors.Is(err, supply.ErrCrossCheckMisaligned) {
		_, _ = fmt.Fprintln(w, "─── CROSS-CHECK RESULT ───")
		_, _ = fmt.Fprintf(w, "  primary_ledger:       %d\n", primarySnap.LedgerSequence)
		_, _ = fmt.Fprintf(w, "  counterpart_ledger:   %d\n", otherSnap.LedgerSequence)
		_, _ = fmt.Fprintf(w, "  status:               MISALIGNED ? — snapshots more than %d ledgers apart; no verdict\n",
			supply.CrossCheckLedgerTolerance)
		_, _ = fmt.Fprintln(w, "  next action:          find which side's supply refresher is stalled (supply audit <asset> -history-hours 24)")
		_, _ = fmt.Fprintln(w)
		return err
	}
	if err != nil {
		return fmt.Errorf("CrossCheckForClass(%s): %w", wrapClass, err)
	}
	return reportCrossCheck(w, result, primaryKey)
}

// stroopsOrNA renders a cross-check leg; nil means the leg was not evaluated.
func stroopsOrNA(v *big.Int) string {
	if v == nil {
		return "n/a (not evaluated)"
	}
	return v.String()
}

// errCrossCheckUnchecked is returned when a partial-wrap result carries
// no escrow leg: DivergenceStroops is then 0 by construction, so a green
// status would certify a check that compared nothing (ADR-0011).
var errCrossCheckUnchecked = errors.New("cross-check inconclusive — escrow leg not evaluated (classic snapshot has no sac_wrapped_stroops)")

// reportCrossCheck prints result and returns non-nil unless the check
// both ran and passed.
func reportCrossCheck(w io.Writer, result supply.CrossCheckResult, primaryKey string) error {
	_, _ = fmt.Fprintln(w, "─── CROSS-CHECK RESULT ───")
	_, _ = fmt.Fprintf(w, "  wrap_class:           %s\n", result.WrapClass)
	_, _ = fmt.Fprintf(w, "  ledgers:              primary=%d counterpart=%d\n", result.ClassicLedger, result.SACLedger)
	_, _ = fmt.Fprintf(w, "  primary_total:        %s\n", result.ClassicTotal.String())
	_, _ = fmt.Fprintf(w, "  counterpart_total:    %s\n", result.SACTotal.String())
	_, _ = fmt.Fprintf(w, "  divergence_stroops:   %s\n", result.DivergenceStroops.String())
	_, _ = fmt.Fprintf(w, "  subset_bound_checked: %t\n", result.SubsetBoundChecked)
	_, _ = fmt.Fprintf(w, "  over_mint_stroops:    %s\n", stroopsOrNA(result.OverMintStroops))
	_, _ = fmt.Fprintf(w, "  escrow_excess_stroops: %s\n", stroopsOrNA(result.EscrowExcessStroops))
	unchecked := result.WrapClass == supply.WrapClassPartial && !result.SubsetBoundChecked
	switch {
	case !result.WithinTolerance:
		_, _ = fmt.Fprintf(w, "  status:               OVER TOLERANCE ✗ — investigate per the supply runbook (supply.md)\n")
		_, _ = fmt.Fprintf(w, "  alert label:          classic_key=\"%s\"\n", primaryKey)
		_, _ = fmt.Fprintln(w, "  next action:          stellarindex-ops supply audit <asset> -config /etc/stellarindex.toml -history-hours 24 to identify when divergence appeared")
	case unchecked:
		_, _ = fmt.Fprintln(w, "  status:               UNCHECKED ? — escrow leg not evaluated; the classic snapshot carries no sac_wrapped_stroops, so divergence 0 verifies nothing")
		_, _ = fmt.Fprintln(w, "  next action:          wait for a classic snapshot recorded after migration 0117, then re-run")
	default:
		_, _ = fmt.Fprintf(w, "  status:               WITHIN TOLERANCE ✓ (≤ %s stroop per ADR-0011)\n",
			supply.CrossCheckTolerance.String())
	}
	_, _ = fmt.Fprintln(w)

	if !result.WithinTolerance {
		return errors.New("cross-check failed — divergence exceeds 1 stroop tolerance")
	}
	if unchecked {
		return errCrossCheckUnchecked
	}
	return nil
}
