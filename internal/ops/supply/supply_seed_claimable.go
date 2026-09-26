package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// claimableSeedBatchSize is how many seeded observations go into one
// multi-row INSERT. Big enough that the seed is not round-trip-bound (a
// network-wide pass can emit millions of rows), small enough that the
// statement stays well inside Postgres' 65,535-bind-parameter ceiling —
// 7 columns x 2,000 rows = 14,000 parameters.
const claimableSeedBatchSize = 2000

// supplySeedClaimableBalances seeds claimable_observations from the
// ClickHouse lake for every CURRENTLY-LIVE claimable balance paying a classic
// credit asset (ADR-0022 / migration 0012). It is the claimable analogue of
// `supply seed-sac-balances -full-history`.
//
// # Why this exists (verified on r1, 2026-07-27)
//
// claimable_observations was never seeded from history: 997 rows, minimum
// ledger 63,301,831 — i.e. only what the live LedgerEntryChange observer
// (internal/sources/claimable_balances) has seen since it started. Every
// claimable balance created before that floor and still unclaimed is missing
// from the Algorithm-2 classic-supply sum
// (Trustline + Claimable + LPReserve + SACWrapped), leaving the claimable
// component roughly 4% populated. Measured against Horizon for AQUA: we serve
// 86,711,792,598 against Horizon's component sum 99,923,674,166 (−13.2%),
// while against Horizon's total MINUS its claimable component
// (86,186,028,534) we are only +0.61% — the claimable component IS the gap.
//
// The seed reads AUTHORITATIVE on-chain state (the ClaimableBalanceEntry
// itself, reduced latest-write-wins out of stellar.ledger_entry_changes), so
// it is always correct to run, and it is idempotent: rows land at each
// balance's TRUE last-modified ledger with intra_ledger_seq =
// [timescale.SeedIntraLedgerSeq], so a re-seed rewrites the same row and a
// later live observation (notably the is_removal row a claim produces) always
// wins the served reader's `DISTINCT ON (claimable_id) … ORDER BY ledger DESC`.
// A balance the served tier still holds as live but the lake shows claimed
// (claimed while the live observer was not recording) is written as an
// is_removal tombstone at the claim's ledger, so a re-seed retracts it without a
// DELETE. A served-live balance the lake has no record of through the walk's
// upper ledger fails the pass by name after the writes.
//
// # Scope
//
// EVERY classic credit asset by default. Unlike the SAC seed there is no
// operator-curated watched set to scope to — claimable balances span the whole
// network — and a seed that quietly covered only some assets would leave the
// rest under-reported in exactly the way this command exists to fix. `-assets`
// narrows a run (useful for a targeted re-seed, or to bound memory on a host
// under pressure) and is EMPTY by default; a narrowed run prints a loud
// PARTIAL banner because the assets left out keep the pre-seed under-count.
//
// Native (XLM) claimable balances are never seeded, matching the live
// observer: they belong to Algorithm 1, whose reader does not consume
// claimable_observations.
//
// # Cost
//
// This walks the whole chain over a ~150-billion-row table and MUST run under
// run-heavy-job.sh on r1 (AGENTS.md heavy-job doctrine). Expect several hours
// and NO output until the end — the latest-write-wins reduction can only emit
// once the last ledger window has been folded, so every insert lands after the
// scan rather than interleaved with it. Silence is not a hang.
//
// Flags:
//
//	-config PATH     Required. Operator TOML config (Postgres DSN).
//	-ch-addr ADDR    ClickHouse native address (default 127.0.0.1:9300).
//	-assets LIST     Comma-separated classic assets (CODE-ISSUER or
//	                 CODE:ISSUER) to scope the seed to. EMPTY (default) =
//	                 every classic credit asset.
//	-timeout DUR     Whole-run deadline (default 12h). The scan is hours
//	                 long and all writes happen at the end, so a deadline
//	                 that expires mid-scan loses the whole pass.
//	-heartbeat PATH  node_exporter textfile for the ops-job heartbeat
//	                 (default: the textfile-collector dir when present). The
//	                 walk reports ledgers reduced, so the long silent scan
//	                 still shows progress.
//	-write           Apply. Without it the pass is a dry run: read + print
//	                 per-asset claimable count + summed balance, nothing
//	                 written (-dry-run is a no-op alias).
//
// A batch insert that fails is retried row by row, so one bad row does not
// abort the pass; rows that still fail are named and exit non-zero. Only a
// clean pass (every row written, every served-live balance resolved) upserts
// claimable_seed_provenance (migration 0183), one row per asset, carrying the
// ledger the lake was verified through. There is no resume cursor: the output
// exists only after the whole walk, and every write is an idempotent upsert, so
// a re-run is the resume.
func supplySeedClaimableBalances(args []string) error {
	fs := flag.NewFlagSet("supply seed-claimable-balances", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	assetsRaw := fs.String("assets", "", "Comma-separated classic assets (CODE-ISSUER or CODE:ISSUER) to scope the seed to. EMPTY (the default) seeds EVERY classic credit asset — narrowing leaves every asset omitted still under-counted")
	timeout := fs.Duration("timeout", 12*time.Hour, "Whole-run deadline; the lake scan is hours long and every insert lands at the end, so an expiring deadline loses the entire pass")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_supply_seed_claimable_balances.prom when that directory exists (r1), otherwise no heartbeat")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	assets, err := parseClaimableSeedAssets(*assetsRaw)
	if err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// The store is opened in dry-run too: the served live set decides which
	// balances the pass would retract, and a dry run should show that.
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	served, err := servedLiveClaimables(ctx, store, assets)
	if err != nil {
		return err
	}

	w := &claimableSeedWriter{dryRun: dryRun, tallies: map[string]*claimableSeedTally{}, unresolved: served}
	if !dryRun {
		w.ctx, w.store = ctx, store
	}

	hb := startSeedHeartbeat("supply-seed-claimable-balances", *heartbeat)
	w.progress = &seedProgress{hb: hb}
	err = w.seed(ctx, *chAddr, assets, served)
	hb.Stop(err == nil)
	printClaimableSeedSummary(w, assets, dryRun)
	if err != nil || dryRun {
		return err
	}
	return w.stampProvenance(ctx, store)
}

// seed runs the lake walk into the writer and settles every row. A pass whose
// rows all landed and whose served-live set all resolved is recorded as clean,
// the precondition for stamping provenance.
func (w *claimableSeedWriter) seed(ctx context.Context, chAddr string, assets map[string]struct{}, served map[string]timescale.LiveClaimable) error {
	walk := clickhouse.SeedWalk{VerifyLake: true, Progress: w.progress.window}
	ev, err := clickhouse.StreamClaimableBalanceSeeds(ctx, chAddr, assets, servedAssetKeys(served), walk, w.add)
	if err != nil {
		return err
	}
	if err := w.flush(); err != nil {
		return err
	}
	w.lakeVerifiedThrough = ev.LakeVerifiedThrough
	return errors.Join(w.failedErr(), unresolvedClaimablesErr(w.unresolved, ev.ToLedger))
}

// stampProvenance upserts one claimable_seed_provenance row per asset the
// clean pass touched (migration 0183).
func (w *claimableSeedWriter) stampProvenance(ctx context.Context, store *timescale.Store) error {
	var errs []error
	for ak, t := range w.tallies {
		p := timescale.ClaimableSeedProvenance{
			AssetKey:            ak,
			ClaimablesSeeded:    t.count,
			ClaimablesRetracted: t.retracted,
			LakeVerifiedThrough: w.lakeVerifiedThrough,
		}
		if t.haveLedgerBounds {
			minL, maxL := t.minLedger, t.maxLedger
			p.MinLedgerSeen, p.MaxLedgerSeen = &minL, &maxL
		}
		if err := store.UpsertClaimableSeedProvenance(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("write claimable seed provenance: %w", err)
	}
	return nil
}

// servedLiveClaimables is the served tier's live claimable set within the
// -assets scope: the balances this pass retracts when the lake shows them
// claimed.
func servedLiveClaimables(ctx context.Context, store *timescale.Store, assets map[string]struct{}) (map[string]timescale.LiveClaimable, error) {
	served, err := store.LiveClaimableObservations(ctx)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return served, nil
	}
	for id, row := range served {
		if _, in := assets[row.AssetKey]; !in {
			delete(served, id)
		}
	}
	return served, nil
}

func servedAssetKeys(served map[string]timescale.LiveClaimable) map[string]string {
	out := make(map[string]string, len(served))
	for id, row := range served {
		out[id] = row.AssetKey
	}
	return out
}

// unresolvedClaimablesErr fails the pass for served-live balances at or below
// the walk's upper ledger that the lake neither re-seeded nor retracted: the
// walk has no record of them, so their served rows keep counting toward supply
// and need investigating by hand. A row above walkTo is newer than the walk and
// is not judged.
func unresolvedClaimablesErr(unresolved map[string]timescale.LiveClaimable, walkTo uint32) error {
	var ids []string
	for id, row := range unresolved {
		if row.Ledger <= walkTo {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	return fmt.Errorf("%d served-live claimable balance(s) have no change in the lake through ledger %d and were neither re-seeded nor retracted; they still serve their old balance (first: %s %s)",
		len(ids), walkTo, ids[0], unresolved[ids[0]].AssetKey)
}

// parseClaimableSeedAssets turns the -assets flag into the CODE:ISSUER set the
// lake reader filters on. Empty input means EVERY classic credit asset (nil
// set) — the default, and the only value that leaves the supply numbers whole.
//
// Entries go through supply.CanonicalizeWatchedClassic, so an operator may
// paste either the canonical wire form (`AQUA-GBNZ…`) or the storage form
// (`AQUA:GBNZ…`) and a typo is a loud error rather than a silently-empty
// filter — the 2026-07-02 production bug that zeroed three supply components
// was exactly a dash/colon mismatch that failed open.
func parseClaimableSeedAssets(raw string) (map[string]struct{}, error) {
	fields := strings.Split(raw, ",")
	entries := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			entries = append(entries, f)
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	keys, err := supply.CanonicalizeWatchedClassic(entries)
	if err != nil {
		return nil, fmt.Errorf("-assets: %w", err)
	}
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out, nil
}

// claimableSeedTally is a per-asset running tally for the summary + the
// -dry-run report.
type claimableSeedTally struct {
	count            int
	retracted        int
	sum              *big.Int
	minLedger        uint32
	maxLedger        uint32
	haveLedgerBounds bool
}

// observe folds one seeded balance's ledger into the tally's [min, max] bound.
// The MIN is the evidence that matters: it is how an operator sees the pass
// actually reached below the live observer's 63,301,831 floor rather than
// re-confirming what was already there.
func (t *claimableSeedTally) observe(ledger uint32) {
	if !t.haveLedgerBounds {
		t.minLedger, t.maxLedger, t.haveLedgerBounds = ledger, ledger, true
		return
	}
	if ledger < t.minLedger {
		t.minLedger = ledger
	}
	if ledger > t.maxLedger {
		t.maxLedger = ledger
	}
}

// claimableSeedWriter tallies every emitted seed and (unless -dry-run)
// accumulates it into batched upserts. Batching matters here in a way it does
// not for the SAC seed: this pass can emit one row per claimable balance live
// on the entire network, and a row-at-a-time writer would be round-trip-bound
// for hours after an already-hours-long scan.
type claimableSeedWriter struct {
	// ctx is held rather than passed because the batch flush happens inside
	// the lake reader's per-seed callback, which takes no context.
	ctx context.Context

	store    claimableObservationStore
	dryRun   bool
	progress *seedProgress
	pending  []timescale.ClaimableObservation
	tallies  map[string]*claimableSeedTally
	total    int
	// retracted counts tombstones written for served balances the lake shows
	// claimed.
	retracted int
	// unresolved starts as the served live set; each id the lake re-seeds or
	// retracts is removed from it.
	unresolved map[string]timescale.LiveClaimable
	// failed counts rows that still failed after a failed batch was retried
	// row by row; failures keeps the first few errors.
	failed   int
	failures []error

	lakeVerifiedThrough uint32
}

// claimableObservationStore is the writer's slice of *timescale.Store.
type claimableObservationStore interface {
	InsertClaimableObservationBatch(ctx context.Context, rows []timescale.ClaimableObservation) error
	InsertClaimableObservation(ctx context.Context, o timescale.ClaimableObservation) error
}

// claimableSeedFailuresKept bounds the errors a failed pass reports by name.
const claimableSeedFailuresKept = 5

func (w *claimableSeedWriter) add(seed clickhouse.ClaimableBalanceSeed) error {
	t := w.tallies[seed.AssetKey]
	if t == nil {
		t = &claimableSeedTally{sum: big.NewInt(0)}
		w.tallies[seed.AssetKey] = t
	}
	delete(w.unresolved, seed.ClaimableID)
	// A tombstone is a retraction, not a balance: it stays out of the count,
	// the sum and the ledger bounds.
	if seed.IsRemoval {
		t.retracted++
		w.retracted++
	} else {
		t.count++
		t.sum.Add(t.sum, seed.Balance)
		t.observe(seed.LedgerSeq)
		w.total++
	}
	w.progress.row()

	if w.dryRun {
		return nil
	}
	w.pending = append(w.pending, timescale.ClaimableObservation{
		ClaimableID: seed.ClaimableID,
		AssetKey:    seed.AssetKey,
		Ledger:      seed.LedgerSeq,
		ObservedAt:  seed.CloseTime,
		Balance:     seed.Balance,
		// A tombstone at the claim's ledger retracts a balance the served
		// tier still holds as live; every other seed is a live balance.
		IsRemoval: seed.IsRemoval,
		// The seed is the authoritative reconstructed FINAL state for its
		// ledger, so it sits at the top of the intra-ledger order — a live
		// per-ledger change can never overwrite it, and a re-seed stays
		// corrective under the `<=` guard (audit-2026-07-16 C2-6).
		IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
	})
	if len(w.pending) < claimableSeedBatchSize {
		return nil
	}
	return w.flush()
}

func (w *claimableSeedWriter) flush() error {
	if w.dryRun || len(w.pending) == 0 {
		return nil
	}
	if err := w.store.InsertClaimableObservationBatch(w.ctx, w.pending); err != nil {
		if w.ctx.Err() != nil {
			return err
		}
		// One bad row must not cost the hours-long pass its other rows: retry
		// the batch row by row and carry on, reporting what still fails.
		for _, o := range w.pending {
			if rerr := w.store.InsertClaimableObservation(w.ctx, o); rerr != nil {
				w.failed++
				if len(w.failures) < claimableSeedFailuresKept {
					w.failures = append(w.failures, fmt.Errorf("%s at ledger %d: %w", o.ClaimableID, o.Ledger, rerr))
				}
			}
		}
	}
	w.pending = w.pending[:0]
	return nil
}

// failedErr fails a pass that left rows unwritten. Re-running is the recovery:
// every write is an idempotent upsert at the balance's own ledger.
func (w *claimableSeedWriter) failedErr() error {
	if w.failed == 0 {
		return nil
	}
	return fmt.Errorf("%d claimable observation row(s) failed to write after a per-row retry; provenance not stamped (first %d: %w)",
		w.failed, len(w.failures), errors.Join(w.failures...))
}

// printClaimableSeedSummary prints one stable line per asset (sorted by
// asset_key, so two runs diff cleanly) plus a totals footer. The per-asset
// ledger range is the operator's evidence that the pass reached below the live
// observer's floor.
func printClaimableSeedSummary(w *claimableSeedWriter, assets map[string]struct{}, dryRun bool) {
	keys := make([]string, 0, len(w.tallies))
	for k := range w.tallies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := w.tallies[k]
		fmt.Printf("SEED  %-70s  balances=%-8d  retracted=%-8d  sum_stroops=%-24s  ledgers=[%d, %d]\n",
			k, t.count, t.retracted, t.sum.String(), t.minLedger, t.maxLedger)
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d live claimable balances and %d retraction(s) across %d classic asset(s)\n", label, w.total, w.retracted, len(keys))
	if dryRun {
		fmt.Println("─── DRY RUN ─── nothing written to claimable_observations.")
	}
	if len(assets) > 0 {
		fmt.Printf("─── PARTIAL ─── scoped to %d asset(s) via -assets; every OTHER classic asset keeps its pre-seed claimable under-count. Re-run without -assets for a whole-network seed.\n", len(assets))
	}
}
