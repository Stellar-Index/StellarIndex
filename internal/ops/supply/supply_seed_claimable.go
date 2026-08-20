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
// run-heavy-job.sh on r1 (CLAUDE.md heavy-job doctrine). Expect several hours
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
//	-dry-run         Read + print per-asset claimable count + summed
//	                 balance without writing.
func supplySeedClaimableBalances(args []string) error {
	fs := flag.NewFlagSet("supply seed-claimable-balances", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	assetsRaw := fs.String("assets", "", "Comma-separated classic assets (CODE-ISSUER or CODE:ISSUER) to scope the seed to. EMPTY (the default) seeds EVERY classic credit asset — narrowing leaves every asset omitted still under-counted")
	timeout := fs.Duration("timeout", 12*time.Hour, "Whole-run deadline; the lake scan is hours long and every insert lands at the end, so an expiring deadline loses the entire pass")
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

	w := &claimableSeedWriter{dryRun: dryRun, tallies: map[string]*claimableSeedTally{}}
	if !dryRun {
		store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		w.ctx, w.store = ctx, store
	}

	if err := clickhouse.StreamClaimableBalanceSeeds(ctx, *chAddr, assets, w.add); err != nil {
		return err
	}
	if err := w.flush(); err != nil {
		return err
	}
	printClaimableSeedSummary(w, assets, dryRun)
	return nil
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

	store   *timescale.Store
	dryRun  bool
	pending []timescale.ClaimableObservation
	tallies map[string]*claimableSeedTally
	total   int
}

func (w *claimableSeedWriter) add(seed clickhouse.ClaimableBalanceSeed) error {
	t := w.tallies[seed.AssetKey]
	if t == nil {
		t = &claimableSeedTally{sum: big.NewInt(0)}
		w.tallies[seed.AssetKey] = t
	}
	t.count++
	t.sum.Add(t.sum, seed.Balance)
	t.observe(seed.LedgerSeq)
	w.total++

	if w.dryRun {
		return nil
	}
	w.pending = append(w.pending, timescale.ClaimableObservation{
		ClaimableID: seed.ClaimableID,
		AssetKey:    seed.AssetKey,
		Ledger:      seed.LedgerSeq,
		ObservedAt:  seed.CloseTime,
		Balance:     seed.Balance,
		// A live claimable balance, by construction: the reducer drops every
		// key whose latest change is a removal, so a seeded row is never a
		// tombstone. The claim that removes it arrives from the LIVE observer
		// at a higher ledger and wins the served reader's at-or-before pick.
		IsRemoval: false,
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
		return err
	}
	w.pending = w.pending[:0]
	return nil
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
		fmt.Printf("SEED  %-70s  balances=%-8d  sum_stroops=%-24s  ledgers=[%d, %d]\n",
			k, t.count, t.sum.String(), t.minLedger, t.maxLedger)
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d live claimable balances across %d classic asset(s)\n", label, w.total, len(keys))
	if dryRun {
		fmt.Println("─── DRY RUN ─── nothing written to claimable_observations.")
	}
	if len(assets) > 0 {
		fmt.Printf("─── PARTIAL ─── scoped to %d asset(s) via -assets; every OTHER classic asset keeps its pre-seed claimable under-count. Re-run without -assets for a whole-network seed.\n", len(assets))
	}
}
