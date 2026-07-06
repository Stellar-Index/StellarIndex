package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"time"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/storage/clickhouse"
	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

// supplySeedSEP41Genesis seeds each watched SEP-41 contract's pre-Soroban
// per-kind OPENING BALANCE into sep41_supply_rollup (migration 0088, incident
// 2026-07-06).
//
// Why this exists. The SEP-41 Algorithm-3 supply refresher derives total as
// Σmint−Σburn−Σclawback from `sep41_supply_events` (Postgres), which the
// supply observer fills ONLY over the Soroban era [50457424, tip]. A classic
// asset's SAC wrapper (VELO, AQUA, yXLM, LIBRE, ACT, MBC, XAU, BTC, GQX, …)
// was largely issued BEFORE Soroban existed, so over the Soroban-era-only
// window it reads Σburn > Σmint → negative total → the negative-total guard
// rejects it and the refresh + cross-check alerts fire. The certified
// ClickHouse lake (stellar.supply_flows, ADR-0034) carries those pre-Soroban
// mint/burn/clawback flows (the post-P23 CAP-67 replay synthesized the unified
// asset events for classic history). This one-time seed sums them below the
// Soroban genesis ledger and writes the per-kind baseline that the reader adds
// to the Soroban-era totals so lifetime supply comes out correct + positive.
//
// PROVENANCE (ADR-0033). The pre-Soroban supply_flows rows are REPLAY-DERIVED
// and thus core-version-dependent; genesis_baseline_ledger + genesis_seeded_at
// record the boundary + capture time so a re-seed is auditable.
//
// Idempotent: the baseline is SET (not added) — re-running overwrites with the
// (deterministic) CH sum, never double-counts. A Soroban-only contract (no
// pre-genesis flows) is seeded with a zero baseline, leaving its served total
// unchanged. NOTE: if the CH supply_flows history below the boundary is
// re-derived, re-run this seed to refresh the baseline.
//
// Flags:
//
//	-config PATH        Required. Operator TOML (watched_sep41_contracts + PG DSN).
//	-ch-addr ADDR       ClickHouse native address (default 127.0.0.1:9300).
//	-genesis-ledger N   Exclusive upper ledger bound of the baseline sum
//	                    (default clickhouse.SorobanGenesisLedger = 50457424).
//	-dry-run            Read + print the per-contract baselines without writing.
func supplySeedSEP41Genesis(args []string) error {
	fs := flag.NewFlagSet("supply seed-sep41-genesis", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	genesisLedger := fs.Uint("genesis-ledger", uint(clickhouse.SorobanGenesisLedger),
		"Exclusive upper ledger bound of the pre-Soroban baseline sum (default = protocol-20 activation)")
	dryRun := fs.Bool("dry-run", false, "Read + print without writing to sep41_supply_rollup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if err := validateGenesisLedgerBoundary(*genesisLedger); err != nil {
		return err
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Supply.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	watched := cfg.Supply.WatchedSEP41Contracts
	if len(watched) == 0 {
		return errors.New("supply seed-sep41-genesis: no [supply] watched_sep41_contracts configured — nothing to seed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	reader, err := clickhouse.NewSupplyReader(ctx, *chAddr)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	var store *timescale.Store
	if !*dryRun {
		store, err = timescale.Open(ctx, cfg.Storage.PostgresDSN)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
	}

	boundary := uint32(*genesisLedger)
	var seeded, nonzero int
	for _, contractID := range watched {
		isNonZero, err := seedOneSEP41Genesis(ctx, reader, store, contractID, boundary, *dryRun)
		if err != nil {
			return err
		}
		if isNonZero {
			nonzero++
		}
		seeded++
	}

	label := "seeded"
	if *dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d/%d watched SEP-41 contracts (%d with a non-zero pre-Soroban baseline, boundary ledger %d)\n",
		label, seeded, len(watched), nonzero, boundary)
	fmt.Println("NOTE: pre-Soroban supply_flows are REPLAY-DERIVED (post-P23 CAP-67 synthesis) — legitimate but core-version-dependent (ADR-0033). Re-run after a lake re-derive below the boundary.")
	return nil
}

// validateGenesisLedgerBoundary vets the operator-supplied -genesis-ledger.
//
// The whole reason the seed is CORRECT is that the two supply slices it stitches
// together are a DISJOINT ledger partition: the ClickHouse genesis sum covers
// `ledger < boundary`, and the Postgres Soroban-era total covers the era the
// SEP-41 observer populates, `ledger >= clickhouse.SorobanGenesisLedger`. That
// disjointness — and therefore "no double-count" — only holds when the boundary
// is AT-OR-BELOW the true Soroban genesis ledger. A boundary set ABOVE it would
// make the CH `ledger < boundary` slice reach up into the Soroban era and sum
// flows that the PG total ALSO counts, so the seeded baseline would inflate the
// served lifetime supply. Fail closed: reject rather than silently double-count.
//
// The default (clickhouse.SorobanGenesisLedger) is accepted — the check is
// strictly-greater-than, so boundary == genesis passes.
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
