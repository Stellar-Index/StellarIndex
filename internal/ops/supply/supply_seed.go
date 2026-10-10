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
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seedFlags holds the parsed and validated inputs for one
// `supply seed-observations` pass.
type seedFlags struct {
	chAddr  string
	dryRun  bool
	cfg     config.Config
	watched []string
}

// parseSeedFlags parses flags, loads and validates config, and resolves the
// watchlist. Factored out of supplySeedObservations to keep flag/config
// validation out of the pass-level function (gocognit).
func parseSeedFlags(args []string) (seedFlags, error) {
	fs := flag.NewFlagSet("supply seed-observations", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return seedFlags{}, err
	}
	if *cfgPath == "" {
		return seedFlags{}, errors.New("-config is required")
	}
	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return seedFlags{}, err
	}
	if err := cfg.Supply.Validate(); err != nil {
		return seedFlags{}, fmt.Errorf("config: %w", err)
	}
	watched := cfg.Supply.SDFReserveAccounts
	if len(watched) == 0 {
		return seedFlags{}, errors.New("supply seed-observations: no [supply] sdf_reserve_accounts configured — nothing to seed")
	}
	return seedFlags{
		chAddr:  *chAddr,
		dryRun:  !gate.Banner(), // Banner prints the mode + returns Enabled()
		cfg:     cfg,
		watched: watched,
	}, nil
}

// openSeedStore opens the Postgres store for a write pass, or returns nil
// unopened for a dry run.
func openSeedStore(ctx context.Context, dryRun bool, dsn string) (*timescale.Store, error) {
	if dryRun {
		return nil, nil
	}
	return timescale.Open(ctx, dsn)
}

// supplySeedObservations seeds account_observations from the ClickHouse lake for
// every `[supply] sdf_reserve_accounts` entry (ADR-0021).
//
// The live AccountEntry observer only writes when an account CHANGES, so a
// dormant reserve account never gets an observation and the chained
// reserve-balance reader stays on the static map. One pass reads each account's
// latest AccountEntry from stellar.ledger_entries_current and inserts it at the
// account's true last-modified ledger; the insert is idempotent (`ON CONFLICT DO
// NOTHING` on (account_id, ledger)) and the live observer supersedes it on the
// next change.
//
// Accounts with no lake row (dormant since before the entry-change capture
// window) are reported, not fabricated: run `stellarindex-ops state-snapshot`
// with the account-state scope first to fill them from a history-archive
// checkpoint.
//
// A pass that reaches the end of its watchlist upserts
// account_observation_seed_provenance (migration 0189): watched-and-missing
// accounts (so a `missing` count traces to a specific G-strkey even after the
// watchlist changes), seeded/missing/removed counts, and the seeded ledger
// range. -dry-run never writes it and an error mid-pass returns before it.
// Without -write it is a dry run.
func supplySeedObservations(args []string) error {
	flags, err := parseSeedFlags(args)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	reader, err := clickhouse.NewExplorerReader(ctx, flags.chAddr)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	store, err := openSeedStore(ctx, flags.dryRun, flags.cfg.Storage.PostgresDSN)
	if err != nil {
		return err
	}
	if store != nil {
		defer func() { _ = store.Close() }()
	}
	watched := flags.watched
	dryRun := flags.dryRun

	var seeded, missing, removed int
	var missingAccounts []string
	var minLedger, maxLedger uint32
	haveLedgerBounds := false
	for _, acc := range watched {
		outcome, ledger, err := seedOneAccount(ctx, reader, store, dryRun, acc)
		if err != nil {
			return err
		}
		switch outcome {
		case seedOutcomeMissing:
			missing++
			missingAccounts = append(missingAccounts, acc)
			continue
		case seedOutcomeRemoved:
			removed++
			continue
		case seedOutcomeSeeded:
			// ledger bounds tracked below
		}
		if !haveLedgerBounds || ledger < minLedger {
			minLedger = ledger
		}
		if !haveLedgerBounds || ledger > maxLedger {
			maxLedger = ledger
		}
		haveLedgerBounds = true
		seeded++
	}

	label := "seeded"
	if dryRun {
		label = "would seed (dry-run)"
	}
	fmt.Printf("\n%s %d/%d reserve accounts (%d missing from lake, %d removed)\n",
		label, seeded, len(watched), missing, removed)
	if missing > 0 {
		fmt.Println("NOTE: missing accounts keep using the operator-static [supply] reserve_balances_stroops fallback until seeded.")
	}
	if dryRun {
		return nil
	}
	return store.UpsertAccountObservationSeedProvenance(ctx, accountObservationSeedProvenance(watched, seeded, missing, removed, minLedger, maxLedger, haveLedgerBounds, missingAccounts))
}

// seedAccountOutcome classifies one watched account's seed-observations result.
type seedAccountOutcome int

const (
	seedOutcomeMissing seedAccountOutcome = iota
	seedOutcomeRemoved
	seedOutcomeSeeded
)

// seedOneAccount fetches one watched account's latest AccountEntry seed from
// the lake and either reports it MISSING/REMOVED or inserts the observation
// (skipped on dry-run). Factored out of supplySeedObservations to keep the
// per-account branching out of the pass-level loop (gocognit).
func seedOneAccount(ctx context.Context, reader *clickhouse.ExplorerReader, store *timescale.Store, dryRun bool, acc string) (seedAccountOutcome, uint32, error) {
	seed, err := reader.LatestAccountEntrySeed(ctx, acc)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case !seed.Found:
		fmt.Printf("MISSING  %s — no AccountEntry in the lake's capture window; run `stellarindex-ops state-snapshot` (account-state scope) first\n", acc)
		return seedOutcomeMissing, 0, nil
	case seed.Removed:
		fmt.Printf("REMOVED  %s — latest change merged the account away (ledger %d); not seeding\n", acc, seed.LedgerSeq)
		return seedOutcomeRemoved, 0, nil
	}
	fmt.Printf("SEED     %s ledger=%d balance=%d stroops home_domain=%q\n",
		seed.AccountID, seed.LedgerSeq, seed.Balance, seed.HomeDomain)
	if dryRun {
		return seedOutcomeSeeded, seed.LedgerSeq, nil
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
		// the top of the intra-ledger order, unbeatable within its walk_version
		// (a stamped re-derive under a higher version replaces it), so a live
		// per-ledger change can't overwrite it and a re-seed stays corrective.
		IntraLedgerSeq: timescale.SeedIntraLedgerSeq,
	}
	if err := store.InsertAccountObservation(ctx, domain.AccountObservation(obs)); err != nil {
		return 0, 0, fmt.Errorf("insert observation for %s: %w", seed.AccountID, err)
	}
	return seedOutcomeSeeded, seed.LedgerSeq, nil
}

// accountObservationSeedProvenance builds the audit record for one COMPLETE
// `supply seed-observations` pass (migration 0189). Factored out of
// supplySeedObservations so the record shape — nil ledger bounds when
// nothing was seeded — is testable without a live store. watched and
// missingAccounts are the actual G-strkeys (the store sorts them
// deterministically before writing); accounts_watched/accounts_missing are
// their lengths.
func accountObservationSeedProvenance(watched []string, seeded, missing, removed int, minLedger, maxLedger uint32, haveLedgerBounds bool, missingAccounts []string) timescale.AccountObservationSeedProvenance {
	p := timescale.AccountObservationSeedProvenance{
		AccountsWatched: len(watched),
		WatchedAccounts: watched,
		AccountsSeeded:  seeded,
		AccountsMissing: missing,
		MissingAccounts: missingAccounts,
		AccountsRemoved: removed,
	}
	if haveLedgerBounds {
		minL, maxL := minLedger, maxLedger
		p.MinLedgerSeen, p.MaxLedgerSeen = &minL, &maxL
	}
	return p
}
