package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// supplySeedSEP41Genesis seeds each watched SEP-41 contract's pre-Soroban per-kind OPENING BALANCE into
// sep41_supply_rollup: the observer fills sep41_supply_events only from Soroban genesis, so classic-issued SAC supply would
// fail the negative-total guard. It sums the lake's pre-Soroban supply_flows (ADR-0034, replay-derived, so re-run
// if re-derived). Idempotent (baseline SET, fold rebuilt in one transaction); without -write it is a dry run.
func supplySeedSEP41Genesis(args []string) error {
	fs := flag.NewFlagSet("supply seed-sep41-genesis", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	genesisLedger := fs.Uint("genesis-ledger", 0,
		"Exclusive upper ledger bound of the pre-Soroban baseline sum (0 = stellar.soroban_genesis_ledger from -config; pubnet = protocol-20 activation, test nets = 1)")
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
	// Resolve the boundary: -genesis-ledger wins, else the config's network value. A test-net seed must NOT
	// fall back to the pubnet boundary or it would double-count its whole history as pre-Soroban.
	genesis := uint(*genesisLedger)
	if genesis == 0 {
		genesis = uint(cfg.Stellar.SorobanGenesisLedger)
	}
	if err := validateGenesisLedgerBoundary(genesis); err != nil {
		return err
	}
	if err := cfg.Supply.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	watched := cfg.Supply.WatchedSEP41Contracts
	if len(watched) == 0 {
		return errors.New("supply seed-sep41-genesis: no [supply] watched_sep41_contracts configured — nothing to seed")
	}
	gate.Banner()
	dryRun := gate.DryRun()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	reader, err := clickhouse.NewSupplyReader(ctx, *chAddr)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	var store *timescale.Store
	if !dryRun {
		store, err = timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
	}

	boundary := uint32(genesis)
	var seeded, nonzero int
	for _, contractID := range watched {
		// Per contract, not per run: each write now carries a cold fold, so
		// a run-wide budget would shrink with the size of the watch-list.
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Minute)
		isNonZero, err := seedOneSEP41Genesis(cctx, reader, store, contractID, boundary, dryRun)
		ccancel()
		if err != nil {
			return err
		}
		if isNonZero {
			nonzero++
		}
		seeded++
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d/%d watched SEP-41 contracts (%d with a non-zero pre-Soroban baseline, boundary ledger %d)\n",
		label, seeded, len(watched), nonzero, boundary)
	fmt.Println("NOTE: pre-Soroban supply_flows are REPLAY-DERIVED (post-P23 CAP-67 synthesis) — legitimate but core-version-dependent (ADR-0033). Re-run after a lake re-derive below the boundary.")
	return nil
}

// validateGenesisLedgerBoundary vets -genesis-ledger. The CH sum (ledger < boundary) and the PG Soroban-era
// total must be a DISJOINT partition, which holds only when boundary <= clickhouse.SorobanGenesisLedger;
// above it the served supply double-counts, so fail closed. boundary == genesis passes.
func validateGenesisLedgerBoundary(genesisLedger uint) error {
	if genesisLedger == 0 {
		return errors.New("-genesis-ledger must be > 0")
	}
	if genesisLedger > uint(clickhouse.SorobanGenesisLedger) {
		return fmt.Errorf(
			"-genesis-ledger %d exceeds the Soroban genesis ledger %d: a boundary above it "+
				"double-counts Soroban-era flows (the CH pre-genesis sum over ledger<boundary would "+
				"overlap the PG Soroban-era total over ledger>=%d) and inflates served supply — pass a "+
				"value <= %d (the default)",
			genesisLedger, clickhouse.SorobanGenesisLedger,
			clickhouse.SorobanGenesisLedger, clickhouse.SorobanGenesisLedger)
	}
	return nil
}

// seedOneSEP41Genesis sums one contract's pre-Soroban (ledger < boundary)
// per-kind flows from the CH lake and upserts them as the genesis baseline
// (unless dryRun). Returns whether the baseline is non-zero (for the summary
// count). store is nil on dry-run.
func seedOneSEP41Genesis(ctx context.Context, reader *clickhouse.SupplyReader, store *timescale.Store, contractID string, boundary uint32, dryRun bool) (bool, error) {
	below, err := reader.TokenSupplyBelowLedger(ctx, contractID, boundary)
	if err != nil {
		return false, fmt.Errorf("seed-sep41-genesis: CH pre-genesis sum for %s: %w", contractID, err)
	}
	genesis := timescale.SEP41KindTotals{Mint: below.Mint, Burn: below.Burn, Clawback: below.Clawback}
	net := new(big.Int).Sub(genesis.Mint, new(big.Int).Add(genesis.Burn, genesis.Clawback))
	nonzero := genesis.Mint.Sign() != 0 || genesis.Burn.Sign() != 0 || genesis.Clawback.Sign() != 0
	fmt.Printf("SEED  %s  ledger<%d  mint=%s burn=%s clawback=%s  net=%s  (flows=%d)\n",
		contractID, boundary, genesis.Mint, genesis.Burn, genesis.Clawback, net, below.FlowCount)
	if dryRun {
		return nonzero, nil
	}
	if err := store.UpsertSEP41GenesisBaseline(ctx, contractID, genesis, boundary); err != nil {
		return false, fmt.Errorf("seed-sep41-genesis: upsert baseline for %s: %w", contractID, err)
	}
	return nonzero, nil
}
