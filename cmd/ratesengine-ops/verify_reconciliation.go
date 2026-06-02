package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/completeness"
	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/sources/aquarius"
	"github.com/RatesEngine/rates-engine/internal/sources/comet"
	"github.com/RatesEngine/rates-engine/internal/sources/phoenix"
	"github.com/RatesEngine/rates-engine/internal/sources/soroswap"
	"github.com/RatesEngine/rates-engine/internal/stellarrpc"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// verifyReconciliation implements ADR-0033 Claim 2b (projection
// reconciliation): per ledger, the rows a source SHOULD have produced
// must equal the rows actually in its table.
//
// Two oracles for "should have produced", by source class:
//
//   - Soroban trade sources (soroswap, aquarius, phoenix, comet) —
//     re-derive by running the real decoder over soroban_events
//     (deterministic recomputation). Correlation sources reconcile
//     correctly because each logical record's events share one
//     (ledger, tx, op).
//   - SDEX — predates Soroban, so there is no soroban_events to
//     re-derive from. Use the LCM-derived classic_trade_effect_count
//     census in ledger_ingest_log (one ClaimAtom = one trade). This is
//     gated on the substrate record covering the range; if it has gaps,
//     run `census-backfill` first. The external Hubble anchor
//     (`hubble-check`) is the defense-in-depth cross-check.
//
// Exits non-zero if any mismatch is found. Cron/CI-gateable.
func verifyReconciliation(args []string) error { //nolint:gocognit,gocyclo,funlen // linear per-source loop; splitting reduces clarity (same as backfillRouter).
	fs := flag.NewFlagSet("verify-reconciliation", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	only := fs.String("source", "", "Limit to one source (soroswap|aquarius|phoenix|comet|sdex); default: all")
	maxList := fs.Int("max-list", 50, "Max gap ledgers to print per source")
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

	// Build a reconciliation job per source. Each yields (expected,
	// actual) per-ledger count maps; the diff + report is common.
	type job struct {
		name     string
		expected func() (map[uint32]int, error)
	}
	soroswapDec := soroswap.NewDecoder()
	sorobanJob := func(name string, dec completeness.Decoder) job {
		return job{name: name, expected: func() (map[uint32]int, error) {
			return completeness.ReDeriveOutputCounts(ctx, store, dec, nil, nil, lo, hi)
		}}
	}
	jobs := []job{
		sorobanJob("soroswap", soroswapDec),
		sorobanJob("aquarius", aquarius.NewDecoder()),
		sorobanJob("phoenix", phoenix.NewDecoder()),
		sorobanJob("comet", comet.NewDecoder()),
		{name: "sdex", expected: func() (map[uint32]int, error) {
			// SDEX expected comes from the substrate census, which is
			// only trustworthy where ledger_ingest_log is continuous.
			gaps, gerr := store.FindLedgerIngestGaps(ctx, lo, hi)
			if gerr != nil {
				return nil, gerr
			}
			if len(gaps) > 0 {
				return nil, fmt.Errorf("ledger_ingest_log has %d gap(s) in [%d,%d] — run `census-backfill` first (first gap %d-%d)",
					len(gaps), lo, hi, gaps[0].Start, gaps[0].End)
			}
			return store.ClassicTradeEffectCountsByLedger(ctx, lo, hi)
		}},
	}

	// Seed soroswap pairs so its re-derive resolves token identities.
	if *only == "" || *only == "soroswap" {
		if err := seedSoroswapForRecon(ctx, cfg, soroswapDec); err != nil {
			fmt.Fprintf(os.Stderr, "verify-reconciliation: soroswap seed failed (%v) — soroswap counts may undercount pre-%d pairs\n", err, lo)
		}
	}

	anyGaps := false
	for _, j := range jobs {
		if *only != "" && j.name != *only {
			continue
		}
		expected, err := j.expected()
		if err != nil {
			return fmt.Errorf("%s: expected counts: %w", j.name, err)
		}
		actual, err := store.CountRowsByLedger(ctx, "trades", "ledger", "source='"+j.name+"'", lo, hi)
		if err != nil {
			return fmt.Errorf("%s: actual counts: %w", j.name, err)
		}
		gaps := completeness.ReconcileCounts(expected, actual)
		expTotal, actTotal := sumCounts(expected), sumCounts(actual)
		if len(gaps) == 0 {
			fmt.Fprintf(os.Stderr, "verify-reconciliation: %-9s OK — expected=%d actual=%d across [%d,%d]\n",
				j.name, expTotal, actTotal, lo, hi)
			continue
		}
		anyGaps = true
		fmt.Fprintf(os.Stderr, "verify-reconciliation: %-9s %d MISMATCHED ledger(s) (expected=%d actual=%d):\n",
			j.name, len(gaps), expTotal, actTotal)
		for i, g := range gaps {
			if i >= *maxList {
				_, _ = fmt.Fprintf(os.Stdout, "  … %d more (raise -max-list to see)\n", len(gaps)-*maxList)
				break
			}
			_, _ = fmt.Fprintf(os.Stdout, "  source=%s ledger=%d expected=%d actual=%d (delta %+d)\n",
				j.name, g.Ledger, g.Expected, g.Actual, g.Actual-g.Expected)
		}
	}

	if anyGaps {
		return fmt.Errorf("projection reconciliation found mismatches — see above (ADR-0033 Claim 2b)")
	}
	return nil
}

func sumCounts(m map[uint32]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// seedSoroswapForRecon seeds the soroswap pair registry from the
// factory via RPC — mirrors verify-decoders so the re-derive resolves
// token identities for pairs created before the audited range.
func seedSoroswapForRecon(ctx context.Context, cfg config.Config, dec *soroswap.Decoder) error {
	if cfg.Oracle.Soroswap.FactoryContract == "" {
		return fmt.Errorf("oracle.soroswap.factory_contract empty")
	}
	endpoint := cfg.Oracle.Soroswap.SeedRPCEndpoint
	if endpoint == "" && len(cfg.Stellar.RPCEndpoints) > 0 {
		endpoint = cfg.Stellar.RPCEndpoints[0]
	}
	if endpoint == "" {
		return fmt.Errorf("no RPC endpoint (set oracle.soroswap.seed_rpc_endpoint or stellar.rpc_endpoints)")
	}
	seedCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	rpc := stellarrpc.New(endpoint, stellarrpc.WithTimeout(60*time.Second))
	n, err := dec.SeedFromFactoryRPC(seedCtx, rpc, cfg.Oracle.Soroswap.FactoryContract)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "verify-reconciliation: seeded %d soroswap pairs\n", n)
	return nil
}
