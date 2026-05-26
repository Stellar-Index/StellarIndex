package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/sources/rozo"
	"github.com/RatesEngine/rates-engine/internal/sources/sorobanevents"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// rozoBackfill is the SQL-driven historical-fill subcommand for the
// Rozo intent-bridge source. Mirrors `cctp-backfill` exactly — Rozo
// has the same shape (stateless decoder, one consumer.Event per
// source row, one hypertable).
//
//nolint:funlen,gocognit // linear pipeline — splitting reduces readability of the flag/config/loop dependency order.
func rozoBackfill(args []string) error {
	fs := flag.NewFlagSet("rozo-backfill", flag.ContinueOnError)
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

	topics := []string{
		rozo.EventPayment,
		rozo.EventFlush,
	}

	fmt.Fprintf(os.Stderr,
		"rozo-backfill: ledgers=[%d,%d] contracts=%d topics=%v dry_run=%v\n",
		*from, *to, len(rozo.MainnetPaymentContracts), topics, *dryRun)

	dec := rozo.NewDecoder()
	startedAt := time.Now()
	var (
		rowsScanned   int64
		decodeErrors  int64
		eventsEmitted int64
		insertErrors  int64
	)

	err = store.StreamSorobanEvents(ctx, uint32(*from), uint32(*to),
		rozo.MainnetPaymentContracts, topics,
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
				revt, ok := out.(rozo.Event)
				if !ok {
					return fmt.Errorf("rozo.Decoder emitted non-rozo.Event %T", out)
				}
				eventsEmitted++
				if *dryRun {
					continue
				}
				if ierr := store.InsertRozoEvent(ctx, timescale.RozoEvent{
					ContractID:  revt.ContractID,
					Ledger:      revt.Ledger,
					TxHash:      revt.TxHash,
					OpIndex:     uint32(revt.OpIndex),
					ObservedAt:  revt.ObservedAt,
					EventType:   timescale.RozoEventType(revt.EventType),
					Amount:      revt.Amount,
					Destination: revt.Destination,
					From:        revt.From,
					Memo:        revt.Memo,
					Token:       revt.Token,
				}); ierr != nil {
					insertErrors++
					fmt.Fprintf(os.Stderr, "  insert ledger=%d tx=%s: %v\n",
						revt.Ledger, revt.TxHash, ierr)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("StreamSorobanEvents: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"rozo-backfill: done in %s — rows_scanned=%d events_emitted=%d decode_errors=%d insert_errors=%d dry_run=%v\n",
		time.Since(startedAt).Round(time.Second),
		rowsScanned, eventsEmitted, decodeErrors, insertErrors, *dryRun)
	if decodeErrors > 0 || insertErrors > 0 {
		return fmt.Errorf("rozo-backfill: %d decode errors + %d insert errors (see stderr)", decodeErrors, insertErrors)
	}
	return nil
}
