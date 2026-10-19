package chops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// verifyReconciliation implements ADR-0033 Claim 2b: per ledger, the rows a source should have produced must equal its table's. Soroban
// sources re-run the decoder over contract_events ([expectedProjection]); event-less ContractCall sources re-derive from ops; SDEX uses
// the decoder and served write filter, not the census (it counts one-side-zero fills trades cannot hold).
func verifyReconciliation(args []string) error { //nolint:gocognit,gocyclo,funlen // linear per-source loop; splitting reduces clarity (same as backfillRouter).
	fs := flag.NewFlagSet("verify-reconciliation", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	only := fs.String("source", "", "Limit to one source (soroswap|aquarius|phoenix|comet|sushiswap_v3|upshift|spectra|sdex); default: all")
	maxList := fs.Int("max-list", 50, "Max gap ledgers to print per source")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (every re-derive reads the lake: contract_events, and operations for sdex and the ContractCall sources)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	lo, hi := uint32(*from), uint32(*to)

	catalogue, soroswapDec, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		return fmt.Errorf("verify-reconciliation: reconciliation catalogue: %w", err)
	}
	// Fail CLOSED on an unknown -source before the per-source loop, which
	// would otherwise skip every source and report "no gaps" having
	// reconciled nothing (F7 fail-open).
	if verr := validateSourceFilter(*only, catalogue); verr != nil {
		return fmt.Errorf("verify-reconciliation: %w", verr)
	}
	// Re-derive on the live indexer's gate (curated set plus protocol_contracts), not the bare
	// seed, or an admitted contract's rows would show as a false mismatch. Read-only, and it
	// must precede the per-source preseed below, which seeds the decoders this rebuilds.
	if catalogue, err = warmCatalogueGates(ctx, store, slog.Default(), catalogue); err != nil {
		return fmt.Errorf("verify-reconciliation: %w", err)
	}
	if *only == "" || *only == "soroswap" {
		// Fail CLOSED: a failed or partial seed leaves the re-derive
		// decoder rejecting real pair events, which reads as expected=0
		// against real served rows — a mismatch that is not in the data.
		if err := seedSoroswapForRecon(ctx, cfg, *chAddr, soroswapDec); err != nil {
			return fmt.Errorf("verify-reconciliation: soroswap pair seed: %w", err)
		}
	}

	anyGaps := false
	for _, src := range catalogue {
		if *only != "" && src.name != *only {
			continue
		}

		expectedFor, blind, eerr := verifyReconExpected(ctx, *chAddr, src, lo, hi)
		if eerr != nil {
			return fmt.Errorf("%s: %w", src.name, eerr)
		}
		// Undecodable rows drop from both sides, so the per-ledger diff below is blind to them
		// and would report OK; report a mismatch so the command exits non-zero.
		if blind.Any() {
			anyGaps = true
			fmt.Fprintf(os.Stderr, "verify-reconciliation: %-28s %s\n", src.name, blind.Detail())
		}

		for _, tgt := range src.targets {
			expected := expectedFor(tgt)
			actual, aerr := store.CountRowsByLedger(ctx, tgt.table, "ledger", tgt.countFilter(), lo, hi)
			if aerr != nil {
				return fmt.Errorf("%s/%s: actual counts: %w", src.name, tgt.table, aerr)
			}
			gaps := completeness.ReconcileCounts(expected, actual)
			label := src.name + "/" + tgt.table
			expTotal, actTotal := sumCounts(expected), sumCounts(actual)
			if len(gaps) == 0 {
				// Refuse expected=0 actual=0 rather than certify it: a target dark for weeks must
				// not print OK. See reconciliationIsVacuous.
				if reconciliationIsVacuous(expTotal, actTotal) {
					anyGaps = true
					fmt.Fprintf(os.Stderr, "verify-reconciliation: %-28s VACUOUS — expected=0 actual=0 over ledgers [%d, %d]; refusing to certify reconciliation vacuously\n", label, lo, hi)
					continue
				}
				fmt.Fprintf(os.Stderr, "verify-reconciliation: %-28s OK — expected=%d actual=%d\n", label, expTotal, actTotal)
				continue
			}
			anyGaps = true
			fmt.Fprintf(os.Stderr, "verify-reconciliation: %-28s %d MISMATCHED ledger(s) (expected=%d actual=%d):\n",
				label, len(gaps), expTotal, actTotal)
			for i, g := range gaps {
				if i >= *maxList {
					_, _ = fmt.Fprintf(os.Stdout, "  … %d more (raise -max-list to see)\n", len(gaps)-*maxList)
					break
				}
				_, _ = fmt.Fprintf(os.Stdout, "  %s ledger=%d expected=%d actual=%d (delta %+d)\n",
					label, g.Ledger, g.Expected, g.Actual, g.Actual-g.Expected)
			}
		}
	}

	if anyGaps {
		return fmt.Errorf("projection reconciliation found mismatches — see above (ADR-0033 Claim 2b)")
	}
	return nil
}

// verifyReconExpected returns a source's per-target expected rows. SDEX goes through sdexProjectionExpected, which also refuses a range
// whose substrate is not contiguous; others take the expected side compute-completeness -ch publishes.
func verifyReconExpected(ctx context.Context, chAddr string, src reconSource, lo, hi uint32) (func(reconTarget) map[uint32]int, completeness.BlindSpots, error) {
	if src.census {
		expected, blind, err := sdexProjectionExpected(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil
	}
	streamer := clickhouse.ReconcileEventStreamer{Addr: chAddr, NeedOpArgs: src.needsOpArgs, NeedStateWriteKeys: src.needsStateWriteKeys, SymbolTopic0Only: src.symbolTopic0}
	expectedFor, blind, err := expectedProjection(ctx, streamer, chAddr, src, lo, hi)
	if err != nil {
		return nil, completeness.BlindSpots{}, fmt.Errorf("re-derive: %w", err)
	}
	return expectedFor, blind, nil
}

func sumCounts(m map[uint32]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// reconciliationIsVacuous reports a no-gap target with zero events on both sides: a source redeployed behind a new contract id writes
// nothing, so two empty maps would look like a full reconciliation.
func reconciliationIsVacuous(expTotal, actTotal int) bool {
	return expTotal == 0 && actTotal == 0
}

// seedSoroswapForRecon seeds the soroswap pair registry from the verified factories' new_pair events in the lake, the
// events and gate the live decoder learns from; empty oracle.soroswap.factory_contract disables it. Fail closed: an
// unseeded pair expects 0 rows and would publish projection_ok=false over healthy data.
func seedSoroswapForRecon(ctx context.Context, cfg config.Config, chAddr string, dec *soroswap.Decoder) error {
	if cfg.Oracle.Soroswap.FactoryContract == "" {
		fmt.Fprintln(os.Stderr, "soroswap pair seed: disabled (oracle.soroswap.factory_contract empty)")
		return nil
	}
	tip, err := clickhouse.MaxLedger(ctx, chAddr)
	if err != nil {
		return fmt.Errorf("lake tip: %w", err)
	}
	n, err := seedSoroswapPairs(dec, func(fn func(events.Event) error) error {
		return clickhouse.StreamContractEventsFiltered(ctx, chAddr, 1, tip, soroswap.MainnetFactories, nil, nil, false, false, false, fn)
	})
	if err != nil {
		return fmt.Errorf("after %d new_pair event(s): %w", n, err)
	}
	fmt.Fprintf(os.Stderr, "soroswap pair seed: %d new_pair events from the lake\n", n)
	return nil
}

// seedSoroswapPairs passes each streamed factory event through dec's own gate and decoder. Zero new_pair events is an
// error: a configured factory with none in the lake means the read missed them.
func seedSoroswapPairs(dec *soroswap.Decoder, stream func(func(events.Event) error) error) (int, error) {
	n := 0
	err := stream(func(ev events.Event) error {
		var derr error
		if perr := completeness.Guard(func() {
			if dec.Matches(ev) {
				_, derr = dec.Decode(ev)
				if derr == nil {
					n++
				}
			}
		}); perr != nil {
			derr = perr
		}
		if derr != nil {
			return fmt.Errorf("ledger %d tx %s: %w", ev.Ledger, ev.TxHash, derr)
		}
		return nil
	})
	if err == nil && n == 0 {
		err = errors.New("no factory new_pair events in the lake")
	}
	return n, err
}
