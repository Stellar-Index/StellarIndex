package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
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
// seeding at an OLD ledger can never clobber a newer live observation.
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
// Flags:
//
//	-config PATH   Required. Operator TOML config (provides
//	               [supply.sac_wrappers] + the Postgres DSN).
//	-ch-addr ADDR  ClickHouse native address (default 127.0.0.1:9300).
//	-dry-run       Read + print per-contract holder count + summed
//	               balance without writing.
func supplySeedSACBalances(args []string) error {
	fs := flag.NewFlagSet("supply seed-sac-balances", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	dryRun := fs.Bool("dry-run", false, "Read + print per-contract holder count + summed balance without writing to sac_balance_observations")
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
	watched := cfg.Supply.SACWrappers
	if len(watched) == 0 {
		return errors.New("supply seed-sac-balances: no [supply.sac_wrappers] configured — nothing to seed")
	}

	// The scan is a full-history FINAL read over every contract_data
	// entry — generous budget; the heavy-job wrapper bounds memory.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	var store *timescale.Store
	if !*dryRun {
		store, err = timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
	}

	tallies := make(map[string]*sacSeedTally, len(watched))
	var total int
	err = clickhouse.StreamSACBalanceSeeds(ctx, *chAddr, watched, func(seed clickhouse.SACBalanceSeed) error {
		t := tallies[seed.ContractID]
		if t == nil {
			t = &sacSeedTally{sum: big.NewInt(0)}
			tallies[seed.ContractID] = t
		}
		t.holders++
		t.sum.Add(t.sum, seed.Balance)
		total++
		if *dryRun {
			return nil
		}
		return store.InsertSACBalanceObservation(ctx, timescale.SACBalanceObservation{
			ContractID: seed.ContractID,
			AssetKey:   seed.AssetKey,
			Holder:     seed.Holder,
			Ledger:     seed.LedgerSeq,
			ObservedAt: seed.CloseTime,
			Balance:    seed.Balance,
		})
	})
	if err != nil {
		return err
	}

	printSACSeedSummary(watched, tallies, total, *dryRun)
	return nil
}

// sacSeedTally is a per-contract running tally for the seed summary.
type sacSeedTally struct {
	holders int
	sum     *big.Int
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

	var withBalances int
	for _, r := range rows {
		t := tallies[r.contractID]
		holders, sum := 0, big.NewInt(0)
		if t != nil {
			holders, sum = t.holders, t.sum
			withBalances++
		}
		fmt.Printf("SEED  %-56s  %-32s  holders=%-6d  sum=%s\n", r.contractID, r.assetKey, holders, sum.String())
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d Balance rows across %d/%d SAC wrappers (%d wrappers had ≥1 current Balance entry)\n",
		label, total, withBalances, len(watched), withBalances)
	if dryRun {
		fmt.Println("─── DRY RUN ─── nothing written to sac_balance_observations.")
	}
}
