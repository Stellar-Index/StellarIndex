package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/sources/blend"
	"github.com/RatesEngine/rates-engine/internal/sources/sorobanevents"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// blendBackfill is the SQL-driven historical-fill subcommand for
// Blend's money-market / emissions / admin events (the 18 topics
// added in PR #25). Each topic is a Symbol on topic[0], so the SQL
// filter is exact.
//
// The decoder dispatches each kind to one of three output types:
//   - PositionEvent → blend_positions (supply / withdraw /
//     supply_collateral / withdraw_collateral / borrow / repay /
//     flash_loan)
//   - EmissionEvent → blend_emissions (gulp / claim /
//     reserve_emission_update / gulp_emissions / bad_debt /
//     defaulted_debt)
//   - AdminEvent → blend_admin (set_admin / update_pool /
//     queue_set_reserve / cancel_set_reserve / set_reserve /
//     set_status / deploy)
//
// Auction events (new_auction / fill_auction / delete_auction) are
// NOT backfilled here — they're the legacy directional-price
// signal handled by blend_auctions via live ingest; the
// backfill scope is the 18 money-market / emissions / admin events
// PR #25 added.
//
//nolint:funlen,gocognit,gocyclo // linear pipeline; the type-switch fan-out reads better inline than as a per-kind helper.
func blendBackfill(args []string) error {
	fs := flag.NewFlagSet("blend-backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	dryRun := fs.Bool("dry-run", false, "Decode without inserting; print summary only")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return errors.New("-config, -from, and -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 18 topic[0] Symbol kinds the PR #25 decoder claims. Each one
	// pushes the filter into Postgres so we don't stream auction
	// events / unrelated rows.
	kinds := []string{
		blend.EventSupply, blend.EventWithdraw,
		blend.EventSupplyCollateral, blend.EventWithdrawCollateral,
		blend.EventBorrow, blend.EventRepay, blend.EventFlashLoan,
		blend.EventGulp, blend.EventClaim,
		blend.EventReserveEmissions, blend.EventGulpEmissions,
		blend.EventBadDebt, blend.EventDefaultedDebt,
		blend.EventSetAdmin, blend.EventUpdatePool,
		blend.EventQueueSetReserve, blend.EventCancelSetReserve,
		blend.EventSetReserve, blend.EventSetStatus, blend.EventDeploy,
	}
	fmt.Fprintf(os.Stderr,
		"blend-backfill: ledgers=[%d,%d] kinds=%d dry_run=%v\n",
		*from, *to, len(kinds), *dryRun)

	dec := blend.NewDecoder()
	startedAt := time.Now()
	var (
		rowsScanned  int64
		positionRows int64
		emissionRows int64
		adminRows    int64
		decodeErrors int64
		insertErrors int64
	)

	err = store.StreamSorobanEvents(ctx, uint32(*from), uint32(*to),
		nil, // no contract filter — Blend pool emitters + factory share these symbols
		kinds,
		func(row sorobanevents.Row) error {
			rowsScanned++
			ev, rerr := sorobanevents.Reconstruct(row)
			if rerr != nil {
				decodeErrors++
				fmt.Fprintf(os.Stderr, "  reconstruct ledger=%d contract=%s: %v\n",
					row.Ledger, row.ContractID, rerr)
				return nil
			}
			outs, derr := dec.Decode(ev)
			if derr != nil {
				decodeErrors++
				fmt.Fprintf(os.Stderr, "  decode ledger=%d contract=%s tx=%s: %v\n",
					row.Ledger, row.ContractID, ev.TxHash, derr)
				return nil
			}
			for _, out := range outs {
				switch e := out.(type) {
				case blend.PositionEvent:
					positionRows++
					if *dryRun {
						continue
					}
					if ierr := store.InsertBlendPositionEvent(ctx, e); ierr != nil {
						insertErrors++
						fmt.Fprintf(os.Stderr, "  insert position ledger=%d tx=%s: %v\n",
							e.Ledger, e.TxHash, ierr)
					}
				case blend.EmissionEvent:
					emissionRows++
					if *dryRun {
						continue
					}
					if ierr := store.InsertBlendEmissionEvent(ctx, e); ierr != nil {
						insertErrors++
						fmt.Fprintf(os.Stderr, "  insert emission ledger=%d tx=%s: %v\n",
							e.Ledger, e.TxHash, ierr)
					}
				case blend.AdminEvent:
					adminRows++
					if *dryRun {
						continue
					}
					if ierr := store.InsertBlendAdminEvent(ctx, e); ierr != nil {
						insertErrors++
						fmt.Fprintf(os.Stderr, "  insert admin ledger=%d tx=%s: %v\n",
							e.Ledger, e.TxHash, ierr)
					}
				default:
					// Auction events would fall here but the SQL filter
					// excluded them — surface defensively.
					return fmt.Errorf("blend.Decoder emitted unexpected %T at ledger %d tx %s", out, row.Ledger, ev.TxHash)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("StreamSorobanEvents: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"blend-backfill: done in %s — rows_scanned=%d position_rows=%d emission_rows=%d admin_rows=%d decode_errors=%d insert_errors=%d dry_run=%v\n",
		time.Since(startedAt).Round(time.Second),
		rowsScanned, positionRows, emissionRows, adminRows, decodeErrors, insertErrors, *dryRun)
	if decodeErrors > 0 || insertErrors > 0 {
		return fmt.Errorf("blend-backfill: %d decode errors + %d insert errors (see stderr)", decodeErrors, insertErrors)
	}
	return nil
}
