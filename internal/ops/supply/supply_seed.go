package supply

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// supplySeedObservations seeds account_observations from the ClickHouse
// lake for every `[supply] sdf_reserve_accounts` entry (ADR-0021).
//
// Why this exists: the live AccountEntry observer only writes a row
// when an account CHANGES after the observer started. A dormant
// reserve account therefore never gets an observation, and the
// chained reserve-balance reader (live-LCM first, operator-static
// fallback) stays on the hand-maintained static map forever. One
// seeding pass reads each account's latest AccountEntry from
// stellar.ledger_entries_current (point lookup via the account_id
// skip-index — cheap) and inserts it at the account's true
// last-modified ledger; the live observer supersedes it on the next
// real change, and the insert is idempotent (`ON CONFLICT DO
// NOTHING` on (account_id, ledger)).
//
// Accounts with no lake row (dormant since before the lake's
// entry-change capture window) are reported, not fabricated — run
// `stellarindex-ops state-snapshot` with the account-state scope
// first to fill the dormant tail from a history-archive checkpoint.
//
// Flags:
//
//	-config PATH   Required. Operator TOML config (provides
//	               sdf_reserve_accounts + the Postgres DSN).
//	-ch-addr ADDR  ClickHouse native address (default 127.0.0.1:9300).
//	-dry-run       Read + print without writing.
func supplySeedObservations(args []string) error {
	fs := flag.NewFlagSet("supply seed-observations", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	dryRun := fs.Bool("dry-run", false, "Read + print without writing to account_observations")
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
	watched := cfg.Supply.SDFReserveAccounts
	if len(watched) == 0 {
		return errors.New("supply seed-observations: no [supply] sdf_reserve_accounts configured — nothing to seed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reader, err := clickhouse.NewExplorerReader(ctx, *chAddr)
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

	var seeded, missing, removed int
	for _, acc := range watched {
		seed, err := reader.LatestAccountEntrySeed(ctx, acc)
		if err != nil {
			return err
		}
		switch {
		case !seed.Found:
			missing++
			fmt.Printf("MISSING  %s — no AccountEntry in the lake's capture window; run `stellarindex-ops state-snapshot` (account-state scope) first\n", acc)
			continue
		case seed.Removed:
			removed++
			fmt.Printf("REMOVED  %s — latest change merged the account away (ledger %d); not seeding\n", acc, seed.LedgerSeq)
			continue
		}
		fmt.Printf("SEED     %s ledger=%d balance=%d stroops home_domain=%q\n",
			seed.AccountID, seed.LedgerSeq, seed.Balance, seed.HomeDomain)
		if *dryRun {
			seeded++
			continue
		}
		obs := accounts.Observation{
			AccountID:  seed.AccountID,
			Ledger:     seed.LedgerSeq,
			ObservedAt: seed.CloseTime,
			Balance:    big.NewInt(seed.Balance),
			HomeDomain: seed.HomeDomain,
			Flags:      seed.Flags,
			SeqNum:     seed.SeqNum,
			// Authoritative reconstructed FINAL state for the ledger — sits at
			// the top of the intra-ledger order so a live per-ledger change
			// can't overwrite it and a re-seed stays corrective (C2-6).
			IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
		}
		if err := store.InsertAccountObservation(ctx, domain.AccountObservation(obs)); err != nil {
			return fmt.Errorf("insert observation for %s: %w", seed.AccountID, err)
		}
		seeded++
	}

	label := "seeded"
	if *dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d/%d reserve accounts (%d missing from lake, %d removed)\n",
		label, seeded, len(watched), missing, removed)
	if missing > 0 {
		fmt.Println("NOTE: missing accounts keep using the operator-static [supply] reserve_balances_stroops fallback until seeded.")
	}
	return nil
}
