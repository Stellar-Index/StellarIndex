package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// chGate runs the ADR-0034 Phase-2 §6 gates over a backfilled ledger range:
//
//   - Gate 2 (completeness): for every ledger it recomputes the
//     decoder-independent census (dispatcher.CensusLedger) AND the
//     structural extract (clickhouse.ExtractLedger) straight from galexie,
//     asserts the extractor matches the census, then reads the range back
//     out of ClickHouse and asserts the STORED per-ledger counts and the
//     ACTUAL child-table row counts both equal the census. Any divergence
//     is a dropped/miscounted row — the gate fails (non-zero exit).
//   - Gate 1 (footprint): reports compressed bytes/ledger from system.parts
//     and projects full-history size; reports the census-walk throughput.
//
// It writes nothing — it only reads galexie + ClickHouse — so it is safe to
// re-run, and it is the same comparison Phase 5 completeness will reuse.
func chGate(args []string) error { //nolint:gocognit,gocyclo,funlen // linear walk + compare + report; splitting reduces clarity.
	fs := flag.NewFlagSet("ch-gate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Uint("from", 0, "first ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "last ledger sequence (inclusive, required)")
	bucket := fs.String("bucket", "", "override storage bucket (default cfg.Storage.S3BucketLive)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address")
	examples := fs.Int("examples", 20, "max per-ledger mismatch examples to print")
	totalHistory := fs.Uint("project-to", 0, "tip ledger to project full-history footprint against (0 = skip projection)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" || *from == 0 || *to == 0 || *to < *from {
		return fmt.Errorf("-config, -from, -to are required; -to must be >= -from")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	streamBucket := cfg.Storage.S3BucketLive
	if *bucket != "" {
		streamBucket = *bucket
	}
	lsCfg := opsutil.NewBoundedLedgerStreamConfig(cfg, streamBucket, 1)
	passphrase := cfg.Stellar.Passphrase()

	fmt.Fprintf(os.Stderr, "ch-gate: census-walking ledgers %d..%d from %q, comparing to ClickHouse %s\n",
		*from, *to, streamBucket, *chAddr)

	var (
		walked                                         int
		extractMismatches                              int
		censusTx, censusOp, censusEvents, censusTrades uint64
		start                                          = time.Now()
		lastLog                                        = time.Now()
	)

	walkErr := ledgerstream.Stream(ctx, lsCfg, uint32(*from), uint32(*to),
		func(lcm sdkxdr.LedgerCloseMeta) error {
			walked++
			seq := lcm.LedgerSequence()

			census, cerr := dispatcher.CensusLedger(lcm, passphrase)
			if cerr != nil {
				fmt.Fprintf(os.Stderr, "ch-gate: ledger %d census: %v\n", seq, cerr)
				return nil
			}
			ext, eerr := clickhouse.ExtractLedger(lcm, passphrase)
			if eerr != nil {
				fmt.Fprintf(os.Stderr, "ch-gate: ledger %d extract: %v\n", seq, eerr)
				return nil
			}

			// Cross-check: the structural extractor must agree with the
			// independent census oracle on the protocol-meaningful counts.
			if uint32(census.SorobanEventCount) != ext.Ledger.SorobanEventCount ||
				uint32(census.ClassicTradeEffectCount) != ext.Ledger.ClassicTradeEffectCount {
				extractMismatches++
				if extractMismatches <= *examples {
					fmt.Fprintf(os.Stderr, "ch-gate: EXTRACT≠CENSUS ledger %d: events census=%d extract=%d | trades census=%d extract=%d\n",
						seq, census.SorobanEventCount, ext.Ledger.SorobanEventCount,
						census.ClassicTradeEffectCount, ext.Ledger.ClassicTradeEffectCount)
				}
			}

			censusEvents += uint64(census.SorobanEventCount)
			censusTrades += uint64(census.ClassicTradeEffectCount)
			censusTx += uint64(ext.Ledger.TxCount) // tx/op are pure LCM counts; census doesn't track them
			censusOp += uint64(ext.Ledger.OpCount)

			if time.Since(lastLog) >= 15*time.Second {
				rate := float64(walked) / time.Since(start).Seconds()
				fmt.Fprintf(os.Stderr, "ch-gate: walked %d (at %d, %.1f ledgers/s)\n", walked, seq, rate)
				lastLog = time.Now()
			}
			return nil
		},
	)
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return fmt.Errorf("ch-gate: stream (walked %d): %w", walked, walkErr)
	}
	walkRate := float64(walked) / time.Since(start).Seconds()

	// ─── read the same range back out of ClickHouse ──────────────
	ch, err := clickhouse.ReadGateCounts(ctx, *chAddr, uint32(*from), uint32(*to))
	if err != nil {
		return err
	}
	fp, err := clickhouse.ReadFootprint(ctx, *chAddr)
	if err != nil {
		return err
	}

	// ─── report ──────────────────────────────────────────────────
	fmt.Printf("\n=== ch-gate: ADR-0034 Phase-2 §6 ===\n")
	fmt.Printf("range:           %d..%d (%d ledgers requested, %d walked)\n", *from, *to, *to-*from+1, walked)
	fmt.Printf("CH ledger rows:  %d (min=%d max=%d)\n", ch.LedgerRows, ch.MinLedger, ch.MaxLedger)

	type check struct {
		name           string
		census, stored uint64
		row            uint64 // 0 + hasRow=false when no row table mirrors this count
		hasRow         bool
	}
	checks := []check{
		{"transactions", censusTx, ch.StoredTx, ch.RowTx, true},
		{"operations", censusOp, ch.StoredOp, ch.RowOp, true},
		{"contract_events", censusEvents, ch.StoredEvents, ch.RowEvents, true},
		{"classic_trades", censusTrades, ch.StoredTrades, 0, false}, // no row table; stored count only
	}

	gateFail := false
	fmt.Printf("\n%-16s %14s %14s %14s  %s\n", "count", "census", "CH-stored", "CH-rows", "verdict")
	for _, c := range checks {
		ok := c.census == c.stored && (!c.hasRow || c.stored == c.row)
		verdict := "OK"
		if !ok {
			verdict = "MISMATCH"
			gateFail = true
		}
		rowStr := "-"
		if c.hasRow {
			rowStr = fmt.Sprintf("%d", c.row)
		}
		fmt.Printf("%-16s %14d %14d %14s  %s\n", c.name, c.census, c.stored, rowStr, verdict)
	}

	// Coverage, in two halves. The gate's claim is "every REQUESTED
	// ledger was examined AND is present in CH exactly once", and the
	// halves fail for different reasons, so they report separately.
	// Measuring CH rows against `walked` alone let a short walk certify
	// itself: a wrong bucket or a hole tolerated by
	// TolerateTrailingMissing shrinks both sides together, so the gate
	// compared a subset to itself and printed OK (RLT-282).
	requested := uint64(*to) - uint64(*from) + 1
	if uint64(walked) != requested {
		fmt.Printf("walk coverage:   MISMATCH (requested %d, walked %d)\n", requested, walked)
	}
	if ch.LedgerRows != requested {
		fmt.Printf("ledger coverage: MISMATCH (requested %d, walked %d, CH ledgers %d)\n",
			requested, walked, ch.LedgerRows)
		gateFail = true
	}
	if extractMismatches > 0 {
		fmt.Printf("extractor vs census: MISMATCH on %d ledger(s)\n", extractMismatches)
		gateFail = true
	} else {
		fmt.Printf("extractor vs census: OK (%d of %d requested ledgers walked)\n", walked, requested)
	}

	// ─── footprint (gate 1) ──────────────────────────────────────
	var totalBytes, totalRows uint64
	fmt.Printf("\n%-22s %16s %16s\n", "table", "bytes", "rows")
	for _, f := range fp {
		fmt.Printf("%-22s %16d %16d\n", f.Table, f.Bytes, f.Rows)
		totalBytes += f.Bytes
		totalRows += f.Rows
	}
	fmt.Printf("%-22s %16d %16d\n", "TOTAL", totalBytes, totalRows)
	// system.parts is whole-DB, so divide by ALL ledgers in CH, not the gate
	// range. If the gate range == the whole DB (e.g. the 100k sample is all
	// that's loaded), this is the sample's true bytes/ledger.
	if ch.TotalLedgers > 0 {
		bpl := float64(totalBytes) / float64(ch.TotalLedgers)
		fmt.Printf("\nfootprint: %.0f bytes/ledger (%.2f GiB total over %d CH ledgers)\n",
			bpl, float64(totalBytes)/(1<<30), ch.TotalLedgers)
		if *totalHistory > 0 {
			projected := bpl * float64(*totalHistory)
			fmt.Printf("projected full history (to ledger %d, linear from this sample): %.1f TiB\n",
				*totalHistory, projected/(1<<40))
		}
	}
	fmt.Printf("census-walk throughput: %.1f ledgers/s\n", walkRate)

	// A walk that delivered fewer ledgers than it asked for is a BROKEN
	// GATE, not a clean range — every check above compares census vs
	// stored vs rows over the ledgers the walk produced, so on the part
	// it never reached there is nothing to disagree with. This is
	// checked before gateFail because every mismatch above is downstream
	// of it, and it names the bucket, which is the usual cause.
	if cerr := walkCoverageErr("ch-gate", uint32(*from), uint32(*to), walked, streamBucket); cerr != nil {
		return cerr
	}
	if gateFail {
		return fmt.Errorf("ch-gate: COMPLETENESS GATE FAILED")
	}
	fmt.Printf("\n✅ ch-gate: completeness gate PASSED\n")
	return nil
}

// walkCoverageErr is the one rule every galexie-walking subcommand in
// this package applies before it reports on what it saw: assert
// DELIVERED == REQUESTED, or say so and fail. Returns nil when the walk
// covered the whole range.
//
// It exists because a short walk is otherwise indistinguishable from a
// clean one (RLT-282). Two mechanisms make short walks routine rather
// than exotic:
//
//   - -bucket defaults to the TRIMMED live bucket, so verifying a
//     historical range without -bucket galexie-archive walks a prefix
//     of it, or none of it.
//   - opsutil.NewBoundedLedgerStreamConfig always sets
//     TolerateTrailingMissing, which converts the SDK's missing-object
//     error into a clean walk-complete for any hole within 65,536
//     ledgers of -to.
//
// Either way ledgerstream.Stream returns nil and the caller's tallies
// are simply smaller. A tally compared only against itself — CH rows
// against walked, drops against claims — then agrees perfectly over the
// slice that was read and says nothing at all about the rest. The
// zero-ledger case is called out separately because it is the loudest
// shape of the same defect and the one an operator misreads as "clean".
func walkCoverageErr(cmd string, from, to uint32, walked int, bucket string) error {
	requested := uint64(to) - uint64(from) + 1
	if uint64(walked) == requested {
		return nil
	}
	if walked == 0 {
		return fmt.Errorf(
			"%s walked 0 ledgers in [%d, %d] from bucket %q — nothing was examined; "+
				"historical ranges need -bucket galexie-archive. Refusing to pass vacuously",
			cmd, from, to, bucket)
	}
	return fmt.Errorf(
		"%s walked %d of %d requested ledgers in [%d, %d] from bucket %q — the range was only "+
			"partially examined, so every count above describes a subset; historical ranges need "+
			"-bucket galexie-archive. Refusing to report on a short walk as if it were complete",
		cmd, walked, requested, from, to, bucket)
}
