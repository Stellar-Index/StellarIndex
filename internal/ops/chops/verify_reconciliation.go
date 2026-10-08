package chops

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// verifyReconciliation implements ADR-0033 Claim 2b: per ledger, the rows a
// source SHOULD have produced must equal the rows in its table. The expected
// side, by source class:
//
//   - Soroban sources: run the real decoder over the lake's contract_events,
//     as compute-completeness -ch does ([expectedProjection]). Event-less
//     ContractCall sources (band, soroswap-router) re-derive from the lake's
//     InvokeContract ops.
//   - SDEX: run the lake's operations through the SDEX decoder and the served
//     write filter (Validate + primary-key de-dup), gated on lake coverage.
//     Not the classic_trade_effect_count census, which counts one-side-zero
//     fills the trades table cannot hold. `hubble-check` is the external
//     cross-check.
//
// Exits non-zero on any mismatch.
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
	// Re-derive on the gate the live indexer runs with — curated set ∪
	// protocol_contracts — not on the bare in-code seed: a
	// contract an operator admitted through protocol_contracts is decoded
	// live, so its rows are in the table, while an unwarmed re-derive
	// expects none of them and reports the source as a mismatch that is
	// not in the data. Read-only (no upsert hook). Must precede the
	// per-source preseed below, which seeds into the decoders this
	// rebuilds.
	if catalogue, err = warmCatalogueGates(ctx, store, slog.Default(), catalogue); err != nil {
		return fmt.Errorf("verify-reconciliation: %w", err)
	}
	if *only == "" || *only == "soroswap" {
		// Fail CLOSED: a failed or partial seed leaves the re-derive
		// decoder rejecting real pair events, which reads as expected=0
		// against real served rows — a mismatch that is not in the data.
		if err := seedSoroswapForRecon(ctx, cfg, soroswapDec); err != nil {
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
		// Rows the re-derive could not decode are dropped from the
		// EXPECTED side, and the projector dropped them from the ACTUAL side
		// for the same reason — so the per-ledger diff below is structurally
		// blind there and would report OK. Report it as a mismatch so the
		// command exits non-zero.
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
				// A source redeployed behind a new contract id (config not
				// updated) writes nothing, the re-derive expects nothing over
				// two empty maps, and ReconcileCounts sees no mismatch — the
				// same vacuous-pass shape verify-recognition guards.
				// expected=0 actual=0 is refused rather than
				// certified: a target dark for weeks must not print OK.
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

// verifyReconExpected returns a source's per-target expected rows from the
// lake. SDEX goes through sdexProjectionExpected, which also refuses a range
// whose lake substrate is not contiguous; every other source takes the same
// expected side compute-completeness -ch publishes.
func verifyReconExpected(ctx context.Context, chAddr string, src reconSource, lo, hi uint32) (func(reconTarget) map[uint32]int, completeness.BlindSpots, error) {
	if src.census {
		expected, blind, err := sdexProjectionExpected(ctx, chAddr, lo, hi)
		if err != nil {
			return nil, completeness.BlindSpots{}, err
		}
		return func(reconTarget) map[uint32]int { return expected }, blind, nil
	}
	streamer := clickhouse.ReconcileEventStreamer{Addr: chAddr, NeedOpArgs: src.needsOpArgs, NeedStateWriteKeys: src.needsStateWriteKeys}
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

// reconciliationIsVacuous reports whether a no-gap target saw zero events on
// both sides — the vacuous-pass shape: a source redeployed behind a
// new contract id with the config not updated writes nothing, the re-derive
// expects nothing over two empty maps, and ReconcileCounts sees no mismatch.
// That is indistinguishable from "fully reconciled" unless it is refused.
func reconciliationIsVacuous(expTotal, actTotal int) bool {
	return expTotal == 0 && actTotal == 0
}

// seedSoroswapForRecon seeds the soroswap pair registry from the factory via
// RPC so the re-derive resolves pairs created before the audited range, with
// the same contract as verify-decoders' seed:
//
//   - An empty oracle.soroswap.factory_contract (the default, and both test
//     nets) disables the seed: nil, nothing seeded; pairs whose new_pair
//     events fall inside the range are still learned.
//   - A factory with no RPC endpoint, or a failed sweep, is an error the
//     caller must not swallow. SeedFromFactoryRPC can stop mid-loop, and a
//     partly seeded registry makes the re-derive expect 0 rows for unseeded
//     pairs, so compute-completeness would publish projection_ok=false over
//     healthy data and trigger a from-genesis re-derive the next night.
//
// SeedFromFactoryRPC retries transient failures within a bounded budget, so
// an error here is deterministic or an endpoint down for the whole budget;
// the 15-minute context below bounds the sweep. Notices carry no command
// prefix because both verify-reconciliation and compute-completeness call
// this.
func seedSoroswapForRecon(ctx context.Context, cfg config.Config, dec *soroswap.Decoder) error {
	factory := cfg.Oracle.Soroswap.FactoryContract
	if factory == "" {
		fmt.Fprintln(os.Stderr, "soroswap pair seed: disabled (oracle.soroswap.factory_contract empty)")
		return nil
	}
	endpoint := cfg.Oracle.Soroswap.SeedRPCEndpoint
	if endpoint == "" && len(cfg.Stellar.RPCEndpoints) > 0 {
		endpoint = cfg.Stellar.RPCEndpoints[0]
	}
	if endpoint == "" {
		return fmt.Errorf("oracle.soroswap.factory_contract is set but no RPC endpoint (set oracle.soroswap.seed_rpc_endpoint or stellar.rpc_endpoints)")
	}
	seedCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	rpc := stellarrpc.New(endpoint, stellarrpc.WithTimeout(60*time.Second))
	n, err := dec.SeedFromFactoryRPC(seedCtx, rpc, factory)
	if err != nil {
		return fmt.Errorf("after %d pair(s) seeded: %w", n, err)
	}
	fmt.Fprintf(os.Stderr, "soroswap pair seed: seeded %d pairs\n", n)
	return nil
}
