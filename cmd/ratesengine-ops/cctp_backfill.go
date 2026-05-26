package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/consumer"
	"github.com/RatesEngine/rates-engine/internal/sources/cctp"
	"github.com/RatesEngine/rates-engine/internal/sources/sorobanevents"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// cctpBackfill is the SQL-driven historical-fill subcommand for the
// Circle CCTP v2 source (ADR-0029 §"SQL-backfill from soroban_events").
//
// Reads soroban_events rows in [from, to] filtered by the three
// known CCTP mainnet contracts × four claimed topic_0 symbols,
// reconstructs the [events.Event] via [sorobanevents.Reconstruct],
// runs each through the live [cctp.Decoder] (same code path as live
// ingest), and persists the resulting rows to `cctp_events` via
// `Store.InsertCCTPEvent`. `INSERT … ON CONFLICT DO NOTHING` makes
// re-runs idempotent.
//
// CCTP is the simplest per-source backfill case: stateless decoder
// (no correlation buffer), one consumer.Event per source row, one
// hypertable. Phoenix / Blend etc. need more work (per-action
// correlation, multiple target tables) and are tracked as
// follow-ups under task #4.
//
//nolint:funlen,gocognit // linear pipeline — splitting reduces readability of the flag/config/loop dependency order.
func cctpBackfill(args []string) error {
	fs := flag.NewFlagSet("cctp-backfill", flag.ContinueOnError)
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

	contracts := []string{
		cctp.MainnetTokenMessengerMinter,
		cctp.MainnetMessageTransmitter,
		cctp.MainnetCctpForwarder,
	}
	topics := []string{
		cctp.EventDepositForBurn,
		cctp.EventMintAndWithdraw,
		cctp.EventMessageSent,
		cctp.EventMessageReceived,
	}

	fmt.Fprintf(os.Stderr,
		"cctp-backfill: ledgers=[%d,%d] contracts=%d topics=%v dry_run=%v\n",
		*from, *to, len(contracts), topics, *dryRun)

	dec := cctp.NewDecoder()
	startedAt := time.Now()
	var (
		rowsScanned   int64
		decodeErrors  int64
		eventsEmitted int64
		insertErrors  int64
	)

	err = store.StreamSorobanEvents(ctx, uint32(*from), uint32(*to), contracts, topics,
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
				cevt, ok := out.(cctp.Event)
				if !ok {
					return fmt.Errorf("cctp.Decoder emitted non-cctp.Event %T", out)
				}
				eventsEmitted++
				if *dryRun {
					continue
				}
				if ierr := store.InsertCCTPEvent(ctx, timescale.CCTPEvent{
					ContractID:         cevt.ContractID,
					Ledger:             cevt.Ledger,
					TxHash:             cevt.TxHash,
					OpIndex:            uint32(cevt.OpIndex),
					ObservedAt:         cevt.ObservedAt,
					EventType:          timescale.CCTPEventType(cevt.EventType),
					Amount:             cevt.Amount,
					Fee:                cevt.Fee,
					Token:              cevt.Token,
					CounterpartyDomain: cevt.CounterpartyDomain,
					Attributes:         cevt.Attributes,
				}); ierr != nil {
					insertErrors++
					fmt.Fprintf(os.Stderr, "  insert ledger=%d tx=%s: %v\n",
						cevt.Ledger, cevt.TxHash, ierr)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("StreamSorobanEvents: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"cctp-backfill: done in %s — rows_scanned=%d events_emitted=%d decode_errors=%d insert_errors=%d dry_run=%v\n",
		time.Since(startedAt).Round(time.Second),
		rowsScanned, eventsEmitted, decodeErrors, insertErrors, *dryRun)
	if decodeErrors > 0 || insertErrors > 0 {
		return fmt.Errorf("cctp-backfill: %d decode errors + %d insert errors (see stderr)", decodeErrors, insertErrors)
	}
	return nil
}

// statically assert cctp.Event satisfies consumer.Event so the
// switch above is compile-time exhaustive.
var _ consumer.Event = cctp.Event{}
