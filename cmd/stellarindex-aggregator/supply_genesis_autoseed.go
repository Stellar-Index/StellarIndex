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
)

// newGenesisAutoSeeder is the in-process equivalent of
// `stellarindex-ops supply seed-sep41-genesis -write` for one contract. It
// dials the lake per attempt (seeding is rare) so construction never touches
// the network.
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
		reader, err := clickhouse.NewSupplyReader(ctx, addr)
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
