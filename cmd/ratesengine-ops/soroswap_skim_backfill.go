package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/sources/sorobanevents"
	"github.com/RatesEngine/rates-engine/internal/sources/soroswap"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// soroswapSkimBackfill is the SQL-driven historical-fill subcommand
// for Soroswap `skim` events (the missing 5th pair-contract event
// added in PR #28).
//
// Soroswap events have a two-tuple topic: topic[0]="SoroswapPair"
// (a String prefix; the dispatcher's topic_0_sym persists it as
// such), topic[1] is the event symbol (`swap`/`sync`/`deposit`/
// `withdraw`/`skim`). soroban_events only indexes topic_0_sym, so
// we pull all SoroswapPair-prefixed rows and compare topic_1_xdr
// against the pre-encoded skim symbol blob in the callback —
// cheap byte equality, no XDR decode.
//
// The soroswap.Decoder is stateful for swap/sync (correlation
// buffer) but skim is decoded standalone. Feeding skim events
// through Decode is safe — the buffer is untouched.
//
//nolint:funlen,gocognit // linear pipeline.
func soroswapSkimBackfill(args []string) error {
	fs := flag.NewFlagSet("soroswap-skim-backfill", flag.ContinueOnError)
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

	// Pre-decode the skim topic[1] blob once so the row-callback
	// is byte-equality only.
	skimTopic1XDR, err := base64.StdEncoding.DecodeString(soroswap.TopicSymbolSkim)
	if err != nil {
		return fmt.Errorf("decode TopicSymbolSkim: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"soroswap-skim-backfill: ledgers=[%d,%d] topic_0_sym=%q dry_run=%v\n",
		*from, *to, soroswap.PrefixPair, *dryRun)

	dec := soroswap.NewDecoder()
	startedAt := time.Now()
	var (
		rowsScanned   int64
		skimRows      int64
		decodeErrors  int64
		eventsEmitted int64
		insertErrors  int64
	)

	err = store.StreamSorobanEvents(ctx, uint32(*from), uint32(*to),
		nil, // no contract filter — skim could come from any future pair WASM
		[]string{soroswap.PrefixPair},
		func(row sorobanevents.Row) error {
			rowsScanned++
			if !bytes.Equal(row.Topic1XDR, skimTopic1XDR) {
				return nil // not a skim event
			}
			skimRows++
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
				skim, ok := out.(soroswap.SkimEvent)
				if !ok {
					// Decoder emitted something else for what we thought
					// was a skim — surface as an error rather than
					// silently discarding.
					return fmt.Errorf("soroswap.Decoder emitted %T for a skim-topic row at ledger %d tx %s", out, row.Ledger, ev.TxHash)
				}
				eventsEmitted++
				if *dryRun {
					continue
				}
				txHash, terr := timescale.DecodeSoroswapTxHash(skim.TxHash)
				if terr != nil {
					insertErrors++
					fmt.Fprintf(os.Stderr, "  decode tx_hash ledger=%d tx=%s: %v\n",
						skim.Ledger, skim.TxHash, terr)
					continue
				}
				if ierr := store.InsertSoroswapSkimEvent(ctx, timescale.SoroswapSkimEvent{
					ContractID:      skim.ContractID,
					Ledger:          skim.Ledger,
					LedgerCloseTime: skim.ObservedAt,
					TxHash:          txHash,
					OpIndex:         int16(skim.OpIndex),
					EventIndex:      int16(skim.EventIndex),
					To:              skim.To,
					Amount0:         skim.Amount0.String(),
					Amount1:         skim.Amount1.String(),
				}); ierr != nil {
					insertErrors++
					fmt.Fprintf(os.Stderr, "  insert ledger=%d tx=%s: %v\n",
						skim.Ledger, skim.TxHash, ierr)
				}
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("StreamSorobanEvents: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"soroswap-skim-backfill: done in %s — rows_scanned=%d skim_rows=%d events_emitted=%d decode_errors=%d insert_errors=%d dry_run=%v\n",
		time.Since(startedAt).Round(time.Second),
		rowsScanned, skimRows, eventsEmitted, decodeErrors, insertErrors, *dryRun)
	if decodeErrors > 0 || insertErrors > 0 {
		return fmt.Errorf("soroswap-skim-backfill: %d decode errors + %d insert errors (see stderr)", decodeErrors, insertErrors)
	}
	return nil
}
