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
)

// supplySeedSACBalances seeds sac_balance_observations from the
// ClickHouse lake for every current `Balance(Address)` contract_data
// entry of each `[supply.sac_wrappers]` contract (ADR-0022 / migration
// 0014). It is the SAC analogue of `supply seed-observations`.
//
// Why this exists. The live SAC balance observer only writes a row when
// a `Balance(Address)` entry CHANGES after the observer started. A
// Balance entry created before that window and idle since never emits a
// LedgerEntryChange, so dormant contract-held (C-address) SAC balances
// are invisible to Algorithm-2 classic supply — dragging a token's
// Algorithm-2 total under its true supply (incident 2026-07-06: ~98% of
// PHO sits dormant in a handful of Phoenix contracts → PHO reads 156.9%
// under; BLND 12.4% under). That under-count also flows to
// `/v1/assets/{id}` circulating_supply + market_cap.
//
// One seeding pass scans stellar.ledger_entries_current for every live
// Balance entry of a watched wrapper and upserts it at the entry's true
// last-modified ledger; the live observer supersedes it on the next real
// change, and the insert is idempotent (ON CONFLICT DO UPDATE on
// (contract_id, holder, ledger, observed_at)). Because the served-tier
// readers pick the most-recent row per (contract_id, holder) by ledger
// DESC (SumSACBalancesAtOrBefore / SACBalanceForContractAtOrBefore),
// seeding at an OLD ledger can never clobber a newer live observation. A
// removed or TTL-archived entry is written as an is_removal tombstone at its
// removal / archival ledger, retracting a balance served before it left live
// state; tombstones are tallied as retractions, never as holders.
//
// Unlike `supply seed-sep41-genesis` (which sums replay-derived
// pre-Soroban flows), this seed reads AUTHORITATIVE current on-chain
// state — the live ContractData Balance entry itself — so it is always
// correct to run.
//
// The scan touches EVERY contract_data entry network-wide (the contract
// id lives inside key_xdr, so the watched-set filter runs in Go, not
// SQL) — it is READ-HEAVY and MUST run under run-heavy-job.sh on r1.
//
// -full-history (incident 2026-07-06 PHO/BLND VERDICT follow-up, ROADMAP
// #14). The default source, stellar.ledger_entries_current, is fed by a
// ClickHouse materialized view that only processes rows inserted AFTER
// the MV was created (~ledger 62,000,000) — a Balance entry dormant
// since before that floor is invisible to it even though it has always
// existed in the certified lake. PHO/BLND/EURC/KALE's largest holders
// are Phoenix/Blend pool contracts that acquired the SAC token via an
// ordinary transfer years before the floor and have been dormant since —
// exactly this shape. Passing -full-history switches the read to
// clickhouse.StreamSACBalanceSeedsFullHistory (stellar.ledger_entry_changes,
// the append-log, complete to genesis per ADR-0034) to recover them. It
// is substantially heavier than the default scan (every historical
// write, not just current state) — reserve it for the small watched set
// that's known to have the floor problem, always under run-heavy-job.sh,
// never as a routine re-run.
//
// Expect the full-history pass to run for roughly an hour on r1 and to print
// nothing until it finishes: the reader walks the append-log in ledger windows
// (a ClickHouse memory bound — incident 2026-07-27) and can only emit once the
// last window has been reduced, so all inserts land at the end of the scan
// rather than interleaved with it. Silence is not a hang.
//
// Before walking, the full-history pass proves stellar.ledgers contiguous and
// hash-linked over the range it reduces and refuses otherwise: a hole hides the
// change that superseded an entry. Its provenance row records the ledger the
// lake was verified through; the -full-history flag alone stamps nothing.
//
// Flags:
//
//	-config PATH     Required. Operator TOML config (provides
//	                 [supply.sac_wrappers] + the Postgres DSN).
//	-ch-addr ADDR    ClickHouse native address (default 127.0.0.1:9300).
//	-full-history    Read from stellar.ledger_entry_changes (complete to
//	                 genesis) instead of the floor-limited
//	                 stellar.ledger_entries_current. Heavier; closes the
//	                 ~62M current-state coverage floor.
//	-timeout DUR     Whole-run deadline (default 12h). All writes happen
//	                 after the scan, so a deadline that expires mid-scan
//	                 loses the whole pass.
//	-contracts LIST  Comma-separated [supply.sac_wrappers] contract ids to
//	                 scope the pass to (default: every configured wrapper).
//	                 Only the scoped wrappers' provenance rows are touched.
//	-heartbeat PATH  node_exporter textfile for the ops-job heartbeat
//	                 (default: the textfile-collector dir when present).
//	-write           Apply. Without it the pass is a dry run: read + print
//	                 per-contract holder count + summed balance, nothing
//	                 written (-dry-run is a no-op alias).
func supplySeedSACBalances(args []string) error {
	fs := flag.NewFlagSet("supply seed-sac-balances", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	fullHistory := fs.Bool("full-history", false, "Read stellar.ledger_entry_changes (complete to genesis) instead of the floor-limited stellar.ledger_entries_current — closes the ~62M current-state coverage floor (heavier; run-heavy-job.sh only)")
	timeout := fs.Duration("timeout", 12*time.Hour, "Whole-run deadline; every insert lands after the lake scan, so an expiring deadline loses the entire pass")
	contracts := fs.String("contracts", "", "Comma-separated [supply.sac_wrappers] contract ids to scope the pass to. EMPTY (the default) seeds every configured wrapper; only the scoped wrappers' provenance is touched")
	heartbeat := fs.String("heartbeat", "", "node_exporter textfile path for the liveness/progress gauges. Empty = "+opsutil.DefaultTextfileDir+"/ops_job_supply_seed_sac_balances.prom when that directory exists (r1), otherwise no heartbeat")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Supply.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	watched, err := scopeSACWrappers(cfg.Supply.SACWrappers, *contracts)
	if err != nil {
		return err
	}
	gate.Banner()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	run := sacSeedRun{chAddr: *chAddr, watched: watched, fullHistory: *fullHistory, dryRun: gate.DryRun()}
	if !run.dryRun {
		store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		run.store = store
	}
	hb := startSeedHeartbeat("supply-seed-sac-balances", *heartbeat)
	err = run.seed(ctx, &seedProgress{hb: hb})
	hb.Stop(err == nil)
	if len(watched) < len(cfg.Supply.SACWrappers) {
		fmt.Printf("─── PARTIAL ─── scoped to %d of %d SAC wrapper(s) via -contracts; the rest were not re-seeded and keep their provenance.\n", len(watched), len(cfg.Supply.SACWrappers))
	}
	return err
}

// scopeSACWrappers narrows the configured wrapper set to -contracts. Every
// listed id must be configured: a typo is an error, never an empty pass.
func scopeSACWrappers(configured map[string]string, contractsRaw string) (map[string]string, error) {
	if len(configured) == 0 {
		return nil, errors.New("supply seed-sac-balances: no [supply.sac_wrappers] configured — nothing to seed")
	}
	var ids []string
	for _, f := range strings.Split(contractsRaw, ",") {
		if f = strings.TrimSpace(f); f != "" {
			ids = append(ids, f)
		}
	}
	if len(ids) == 0 {
		return configured, nil
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		ak, ok := configured[id]
		if !ok {
			return nil, fmt.Errorf("-contracts: %s is not a [supply.sac_wrappers] contract id", id)
		}
		out[id] = ak
	}
	return out, nil
}

// sacSeedRun is one seed-sac-balances pass over an already-scoped wrapper set.
type sacSeedRun struct {
	chAddr      string
	watched     map[string]string
	fullHistory bool
	dryRun      bool
	store       *timescale.Store // nil in dry-run
}

func (r sacSeedRun) seed(ctx context.Context, progress *seedProgress) error {
	source := timescale.SACBalanceSeedSourceCurrentState
	if r.fullHistory {
		source = timescale.SACBalanceSeedSourceFullHistory
	}
	tallies := make(map[string]*sacSeedTally, len(r.watched))
	var total int
	walk := clickhouse.SeedWalk{VerifyLake: true, Progress: progress.window}
	ev, err := streamSACSeeds(ctx, r.chAddr, r.watched, r.fullHistory, walk, func(seed clickhouse.SACBalanceSeed) error {
		t := tallies[seed.ContractID]
		if t == nil {
			t = &sacSeedTally{sum: big.NewInt(0)}
			tallies[seed.ContractID] = t
		}
		t.add(seed)
		total++
		progress.row()
		if r.dryRun {
			return nil
		}
		return r.store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID: seed.ContractID,
			AssetKey:   seed.AssetKey,
			Holder:     seed.Holder,
			Ledger:     seed.LedgerSeq,
			ObservedAt: seed.CloseTime,
			Balance:    seed.Balance,
			IsRemoval:  seed.IsRemoval,
			// The seed is the authoritative reconstructed FINAL state for its
			// ledger (latest lake entry), so it sits at the top of the
			// intra-ledger order — a live per-ledger change can never
			// overwrite it, a re-seed stays corrective (audit-2026-07-16 C2-6).
			IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
		})
	})
	if err != nil {
		return err
	}

	printSACSeedSummary(r.watched, tallies, total, r.dryRun)

	var provErr error
	if !r.dryRun {
		if err := writeSACSeedProvenance(ctx, r.store, r.watched, tallies, source, ev); err != nil {
			provErr = fmt.Errorf("write seed provenance: %w", err)
		}
	}
	return errors.Join(unmatchedSACWrappersErr(r.watched, tallies), provErr)
}

// streamSACSeeds runs the pass's lake reader. Only the full-history walk yields
// lake evidence: the current-state read is not a coverage claim.
func streamSACSeeds(ctx context.Context, addr string, watched map[string]string, fullHistory bool, walk clickhouse.SeedWalk, fn func(clickhouse.SACBalanceSeed) error) (clickhouse.SeedEvidence, error) {
	if !fullHistory {
		return clickhouse.SeedEvidence{}, clickhouse.StreamSACBalanceSeeds(ctx, addr, watched, fn)
	}
	return clickhouse.StreamSACBalanceSeedsFullHistory(ctx, addr, watched, walk, fn)
}

// sacSeedTally is a per-contract running tally for the seed summary +
// provenance record.
type sacSeedTally struct {
	holders          int
	retracted        int
	sum              *big.Int
	minLedger        uint32
	maxLedger        uint32
	haveLedgerBounds bool
}

// add folds one streamed seed into the tally. A tombstone (IsRemoval) is a
// retraction, not a holder: it is counted apart and kept out of the sum and
// the ledger bounds, so provenance's holders_seeded and min/max_ledger_seen
// describe only balances actually seeded.
func (t *sacSeedTally) add(seed clickhouse.SACBalanceSeed) {
	if seed.IsRemoval {
		t.retracted++
		return
	}
	t.holders++
	t.sum.Add(t.sum, seed.Balance)
	t.observe(seed.LedgerSeq)
}

// observe folds one seeded entry's ledger into the tally's [min, max]
// bound, used to populate sac_balance_seed_provenance.min_ledger_seen /
// max_ledger_seen — the evidence that a -full-history pass actually
// reached below the current-state floor, not just a source-label claim.
func (t *sacSeedTally) observe(ledger uint32) {
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

// matched is how many of the wrapper's Balance keys this pass resolved, live
// or retracted.
func (t *sacSeedTally) matched() int {
	if t == nil {
		return 0
	}
	return t.holders + t.retracted
}

// unmatchedSACWrappersErr fails the pass for every watched wrapper that
// matched no Balance entry at all. Such a wrapper gets no provenance row: a
// row with zero holders and NULL bounds is indistinguishable from a mis-keyed
// [supply.sac_wrappers] entry, and would read as a completed seed.
func unmatchedSACWrappersErr(watched map[string]string, tallies map[string]*sacSeedTally) error {
	var unmatched []string
	for cid, ak := range watched {
		if tallies[cid].matched() == 0 {
			unmatched = append(unmatched, cid+" ("+ak+")")
		}
	}
	if len(unmatched) == 0 {
		return nil
	}
	sort.Strings(unmatched)
	return fmt.Errorf("%d watched SAC wrapper(s) matched no Balance entry and were not stamped in provenance — check the [supply.sac_wrappers] contract id: %s",
		len(unmatched), strings.Join(unmatched, ", "))
}

// checkSACSeedShrink refuses a pass that accounts for fewer keys than the
// previous same-source pass seeded. Each pass re-resolves every key its reader
// has ever held, as a live holder or a tombstone, so holders + retracted cannot
// fall; a shortfall is holders neither re-seeded nor retracted, whose old rows
// the served tier still sums. A different source is not comparable:
// current_state cannot see holders below its floor that full_history seeded.
func checkSACSeedShrink(prev timescale.SACBalanceSeedProvenance, source timescale.SACBalanceSeedSource, t *sacSeedTally) error {
	if prev.Source != source || t.matched() >= prev.HoldersSeeded {
		return nil
	}
	return fmt.Errorf("%s: %s pass accounted for %d key(s) (%d holders, %d retracted) but the previous %s pass seeded %d holders; %d were neither re-seeded nor retracted and still serve their old balance — provenance left unchanged",
		prev.ContractID, source, t.matched(), t.holders, t.retracted, prev.Source, prev.HoldersSeeded, prev.HoldersSeeded-t.matched())
}

// writeSACSeedProvenance upserts one sac_balance_seed_provenance row per
// watched contract that matched at least one Balance key (migration 0102).
// A contract that fails [checkSACSeedShrink] keeps its previous row. Best-effort
// per contract: a failure is reported but does not unwind the observations
// already committed (the audit trail is secondary to the supply data itself).
func writeSACSeedProvenance(ctx context.Context, store *timescale.Store, watched map[string]string, tallies map[string]*sacSeedTally, source timescale.SACBalanceSeedSource, ev clickhouse.SeedEvidence) error {
	var errs []error
	for cid, ak := range watched {
		t := tallies[cid]
		if t.matched() == 0 {
			continue // reported by unmatchedSACWrappersErr
		}
		prev, ok, err := store.SACBalanceSeedProvenanceFor(ctx, cid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			if err := checkSACSeedShrink(prev, source, t); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		p, err := sacSeedProvenance(cid, ak, source, ev, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := store.UpsertSACBalanceSeedProvenance(ctx, p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sacSeedProvenance builds one contract's provenance row from what the pass
// established. A full_history row carries the ledger the lake was proven intact
// through; without that evidence the -full-history flag alone stamps nothing.
func sacSeedProvenance(cid, ak string, source timescale.SACBalanceSeedSource, ev clickhouse.SeedEvidence, t *sacSeedTally) (timescale.SACBalanceSeedProvenance, error) {
	retracted := t.retracted
	p := timescale.SACBalanceSeedProvenance{
		ContractID:       cid,
		AssetKey:         ak,
		Source:           source,
		HoldersSeeded:    t.holders,
		HoldersRetracted: &retracted,
	}
	if t.haveLedgerBounds {
		minL, maxL := t.minLedger, t.maxLedger
		p.MinLedgerSeen, p.MaxLedgerSeen = &minL, &maxL
	}
	if source != timescale.SACBalanceSeedSourceFullHistory {
		return p, nil
	}
	if ev.LakeVerifiedThrough == 0 {
		return timescale.SACBalanceSeedProvenance{}, fmt.Errorf("%s: full_history pass carries no lake verification; provenance left unchanged", cid)
	}
	through := ev.LakeVerifiedThrough
	p.LakeVerifiedThrough = &through
	return p, nil
}

// printSACSeedSummary prints one stable line per watched wrapper
// (sorted by asset_key then contract id) plus a totals footer. Wrappers
// with no current Balance entries in the lake are printed with
// holders=0 so the operator sees which had nothing to seed.
func printSACSeedSummary(watched map[string]string, tallies map[string]*sacSeedTally, total int, dryRun bool) {
	type row struct {
		contractID, assetKey string
	}
	rows := make([]row, 0, len(watched))
	for cid, ak := range watched {
		rows = append(rows, row{contractID: cid, assetKey: ak})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].assetKey != rows[j].assetKey {
			return rows[i].assetKey < rows[j].assetKey
		}
		return rows[i].contractID < rows[j].contractID
	})

	var withBalances, retracted int
	for _, r := range rows {
		t := tallies[r.contractID]
		holders, removed, sum := 0, 0, big.NewInt(0)
		if t != nil {
			holders, removed, sum = t.holders, t.retracted, t.sum
			retracted += removed
		}
		if holders > 0 {
			withBalances++
		}
		fmt.Printf("SEED  %-56s  %-32s  holders=%-6d  retracted=%-6d  sum=%s\n", r.contractID, r.assetKey, holders, removed, sum.String())
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d Balance rows (%d holders, %d retractions) across %d/%d SAC wrappers (%d wrappers had ≥1 current Balance entry)\n",
		label, total, total-retracted, retracted, withBalances, len(watched), withBalances)
	if dryRun {
		fmt.Println("─── DRY RUN ─── nothing written to sac_balance_observations.")
	}
}
