package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

const (
	genesisAutoSeedRetry   = 10 * time.Minute
	genesisAutoSeedTimeout = 15 * time.Minute
	// sep41CoverageSettle keeps the compared range below ledgers either tier may still be writing (about ten minutes).
	sep41CoverageSettle = 120
)

// newSEP41CoverageCheck compares a contract's Soroban-era per-kind sums in the served tier with the lake's, which is not watch-list filtered.
func newSEP41CoverageCheck(cfg config.Config, store *timescale.Store) supply.HistoryCoverageCheck {
	boundary := cfg.Stellar.SorobanGenesisLedger
	if boundary == 0 {
		boundary = clickhouse.SorobanGenesisLedger
	}
	addr := cfg.Storage.ClickHouseAddr
	return func(ctx context.Context, contractID string, ledger uint32) error {
		ctx, cancel := context.WithTimeout(ctx, genesisAutoSeedTimeout)
		defer cancel()
		reader, err := dialSupplyLake(ctx, addr)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()
		tip, err := reader.LakeTip(ctx)
		if err != nil {
			return err
		}
		upTo := min(tip, ledger)
		if upTo < uint32(boundary)+sep41CoverageSettle {
			return fmt.Errorf("no settled Soroban-era ledger to compare (lake tip %d, snapshot %d)", tip, ledger)
		}
		upTo -= sep41CoverageSettle
		lake, err := reader.TokenSupplyInLedgerRange(ctx, contractID, uint32(boundary), upTo)
		if err != nil {
			return err
		}
		served, err := store.SEP41EventTotalsInRange(ctx, contractID, uint32(boundary), upTo)
		if err != nil {
			return err
		}
		return sep41TotalsMatch(lake, served, uint32(boundary), upTo)
	}
}

func sep41TotalsMatch(lake clickhouse.TokenSupply, served timescale.SEP41KindTotals, from, to uint32) error {
	if lake.Mint.Cmp(served.Mint) == 0 && lake.Burn.Cmp(served.Burn) == 0 && lake.Clawback.Cmp(served.Clawback) == 0 {
		return nil
	}
	return fmt.Errorf("ledgers [%d, %d]: lake mint=%s burn=%s clawback=%s, served mint=%s burn=%s clawback=%s",
		from, to, lake.Mint, lake.Burn, lake.Clawback, served.Mint, served.Burn, served.Clawback)
}

// newGenesisAutoSeeder is the in-process equivalent of `stellarindex-ops supply seed-sep41-genesis -write` for one contract;
// it dials the lake per attempt (seeding is rare) so construction never touches the network.
func newGenesisAutoSeeder(cfg config.Config, store *timescale.Store) supply.GenesisSeeder {
	boundary := cfg.Stellar.SorobanGenesisLedger
	if boundary == 0 {
		boundary = clickhouse.SorobanGenesisLedger
	}
	addr := cfg.Storage.ClickHouseAddr
	return func(ctx context.Context, contractID string) error {
		// A boundary above the Soroban genesis would double-count Soroban-era flows.
		if boundary > clickhouse.SorobanGenesisLedger {
			return fmt.Errorf("genesis boundary %d exceeds the Soroban genesis ledger %d", boundary, clickhouse.SorobanGenesisLedger)
		}
		ctx, cancel := context.WithTimeout(ctx, genesisAutoSeedTimeout)
		defer cancel()
		reader, err := dialSupplyLake(ctx, addr)
		if err != nil {
			return err
		}
		defer func() { _ = reader.Close() }()
		below, err := reader.TokenSupplyBelowLedger(ctx, contractID, uint32(boundary))
		if err != nil {
			return fmt.Errorf("pre-genesis sum for %s: %w", contractID, err)
		}
		return store.UpsertSEP41GenesisBaseline(ctx, contractID,
			timescale.SEP41KindTotals{Mint: below.Mint, Burn: below.Burn, Clawback: below.Clawback}, uint32(boundary))
	}
}

// dialSupplyLake dials per attempt: both callers run rarely and retry on their own timer, so a cold lake costs one attempt.
func dialSupplyLake(ctx context.Context, addr string) (*clickhouse.SupplyReader, error) {
	return clickhouse.NewSupplyReader(ctx, addr)
}
