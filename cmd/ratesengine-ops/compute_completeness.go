package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/RatesEngine/rates-engine/internal/completeness"
	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/pipeline"
	"github.com/RatesEngine/rates-engine/internal/sources/aquarius"
	"github.com/RatesEngine/rates-engine/internal/sources/comet"
	"github.com/RatesEngine/rates-engine/internal/sources/phoenix"
	"github.com/RatesEngine/rates-engine/internal/sources/soroswap"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// sorobanEraGenesis is the first pubnet ledger with Soroban — the lower
// bound for the global recognition scan.
const sorobanEraGenesis = 50_457_424

// computeCompleteness is the ADR-0033 Phase 6 computor: it derives the
// per-source completeness WATERMARK (substrate ∧ projection) and a
// system-level recognition verdict, and writes them to
// completeness_snapshots for the API + status page to read. Operator /
// cron-driven, the same compute-once / read-cheap shape as the gap
// detector's source_coverage_snapshots.
//
// Per-source watermark = substrate continuity + hash chain (Claim 1)
// AND projection reconciliation (Claim 2b). Recognition (Claim 2a) is a
// GLOBAL property — an unhandled topic is not cleanly attributable to
// one source — so it gets its own `recognition` snapshot rather than
// (mis)capping an unrelated source's watermark; every source also
// carries the global recognition flag informationally.
//
// Projection reconciliation is bounded to the substrate-verified region
// [genesis, substrateWatermark]: there is no point re-deriving where the
// substrate itself is already incomplete.
func computeCompleteness(args []string) error { //nolint:funlen,gocognit,gocyclo // linear computor; one block per claim.
	fs := flag.NewFlagSet("compute-completeness", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	toFlag := fs.Uint("to", 0, "Tip ledger (inclusive); 0 = resolve from the live ledgerstream cursor")
	only := fs.String("source", "", "Limit to one source (soroswap|aquarius|phoenix|comet|sdex)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Minute)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	tip := uint32(*toFlag)
	if tip == 0 {
		cur, gerr := store.GetCursor(ctx, "ledgerstream", "")
		if gerr != nil {
			return fmt.Errorf("resolve tip from ledgerstream cursor: %w (pass -to to override)", gerr)
		}
		tip = cur.LastLedger
	}
	if tip == 0 {
		return fmt.Errorf("tip resolved to 0 — pass -to")
	}
	fmt.Fprintf(os.Stderr, "compute-completeness: tip=%d\n", tip)

	// ── Global recognition (Claim 2a) over the Soroban era ──────────
	recProblem, recOK, recErr := computeRecognition(ctx, store, cfg, tip)
	if recErr != nil {
		fmt.Fprintf(os.Stderr, "compute-completeness: recognition scan failed: %v\n", recErr)
	} else {
		recW := completeness.ComputeWatermark(sorobanEraGenesis, tip, nilOrOne(recProblem))
		detail := "no unrecognized event shapes"
		if !recOK {
			detail = fmt.Sprintf("unrecognized event shape at ledger %d — run verify-recognition", recProblem)
		}
		if err := store.UpsertCompletenessSnapshot(ctx, timescale.CompletenessSnapshot{
			Source: "recognition", Genesis: sorobanEraGenesis, Tip: tip,
			Watermark: recW.Ledger, CoveragePct: recW.CoveragePct, Complete: recW.Complete,
			FirstProblem: recW.FirstProblem, SubstrateOK: true, RecognitionOK: recOK, ProjectionOK: true,
			Detail: detail,
		}); err != nil {
			return fmt.Errorf("upsert recognition snapshot: %w", err)
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: recognition ok=%v coverage=%.4f\n", recOK, recW.CoveragePct)
	}

	// ── Per-source watermark (substrate ∧ projection) ───────────────
	soroswapDec := soroswap.NewDecoder()
	if *only == "" || *only == "soroswap" {
		if serr := seedSoroswapForRecon(ctx, cfg, soroswapDec); serr != nil {
			fmt.Fprintf(os.Stderr, "compute-completeness: soroswap seed failed (%v) — soroswap projection may undercount\n", serr)
		}
	}
	decoders := map[string]completeness.Decoder{
		"soroswap": soroswapDec,
		"aquarius": aquarius.NewDecoder(),
		"phoenix":  phoenix.NewDecoder(),
		"comet":    comet.NewDecoder(),
	}

	for _, name := range []string{"soroswap", "aquarius", "phoenix", "comet", "sdex"} {
		if *only != "" && name != *only {
			continue
		}
		genesis, ok := tradesGenesisOf(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "compute-completeness: no gap target for %q — skipping\n", name)
			continue
		}

		var problems []uint32
		var detail []string

		// Claim 1: substrate continuity + hash chain over [genesis, tip].
		subGaps, err := store.FindLedgerIngestGaps(ctx, genesis, tip)
		if err != nil {
			return fmt.Errorf("%s: substrate gaps: %w", name, err)
		}
		breaks, err := store.VerifyLedgerHashChain(ctx, genesis, tip)
		if err != nil {
			return fmt.Errorf("%s: hash chain: %w", name, err)
		}
		substrateOK := len(subGaps) == 0 && len(breaks) == 0
		for _, g := range subGaps {
			problems = append(problems, uint32(g.Start))
		}
		for _, b := range breaks {
			problems = append(problems, b.LedgerSeq)
		}
		if !substrateOK {
			detail = append(detail, fmt.Sprintf("substrate: %d gap(s), %d chain break(s)", len(subGaps), len(breaks)))
		}

		// Bound projection to the substrate-verified region.
		srW := completeness.ComputeWatermark(genesis, tip, problems)
		projOK := true
		if srW.Ledger >= genesis {
			projHi := srW.Ledger
			expected, actual, perr := projectionCounts(ctx, store, name, decoders[name], genesis, projHi)
			if perr != nil {
				return fmt.Errorf("%s: projection: %w", name, perr)
			}
			pgaps := completeness.ReconcileCounts(expected, actual)
			projOK = len(pgaps) == 0
			for _, g := range pgaps {
				problems = append(problems, g.Ledger)
			}
			if !projOK {
				detail = append(detail, fmt.Sprintf("projection: %d mismatched ledger(s) in [%d,%d]", len(pgaps), genesis, projHi))
			}
		} else {
			projOK = false
			detail = append(detail, "projection: not evaluated (substrate incomplete at genesis)")
		}

		w := completeness.ComputeWatermark(genesis, tip, problems)
		if len(detail) == 0 {
			detail = append(detail, "complete: substrate + projection verified to tip")
		}
		snap := timescale.CompletenessSnapshot{
			Source: name, Genesis: genesis, Tip: tip,
			Watermark: w.Ledger, CoveragePct: w.CoveragePct, Complete: w.Complete,
			FirstProblem: w.FirstProblem,
			SubstrateOK:  substrateOK, RecognitionOK: recOK, ProjectionOK: projOK,
			Detail: strings.Join(detail, "; "),
		}
		if err := store.UpsertCompletenessSnapshot(ctx, snap); err != nil {
			return fmt.Errorf("%s: upsert snapshot: %w", name, err)
		}
		fmt.Fprintf(os.Stderr, "compute-completeness: %-9s watermark=%d coverage=%.4f complete=%v (%s)\n",
			name, w.Ledger, w.CoveragePct, w.Complete, strings.Join(detail, "; "))
	}

	return nil
}

// computeRecognition runs the global recognition audit over the Soroban
// era and returns the earliest unrecognized-shape ledger (0 if none).
func computeRecognition(ctx context.Context, store *timescale.Store, cfg config.Config, tip uint32) (uint32, bool, error) {
	disp, err := pipeline.BuildDispatcher(cfg.Ingestion.EnabledSources, cfg.Oracle)
	if err != nil {
		return 0, false, fmt.Errorf("build dispatcher: %w", err)
	}
	samples, err := store.DistinctSorobanTopicSamples(ctx, sorobanEraGenesis, tip)
	if err != nil {
		return 0, false, err
	}
	gaps := completeness.AuditRecognition(samples, disp)
	if len(gaps) == 0 {
		return 0, true, nil
	}
	earliest := gaps[0].MinLedger
	for _, g := range gaps {
		if g.MinLedger < earliest {
			earliest = g.MinLedger
		}
	}
	return earliest, false, nil
}

// projectionCounts returns (expected, actual) per-ledger row counts for
// a source over [genesis, hi]. SDEX uses the LCM census; the Soroban
// trade sources re-derive via their decoder.
func projectionCounts(ctx context.Context, store *timescale.Store, name string, dec completeness.Decoder, genesis, hi uint32) (expected, actual map[uint32]int, err error) {
	if name == "sdex" {
		expected, err = store.ClassicTradeEffectCountsByLedger(ctx, genesis, hi)
	} else {
		expected, err = completeness.ReDeriveOutputCounts(ctx, store, dec, nil, nil, genesis, hi)
	}
	if err != nil {
		return nil, nil, err
	}
	actual, err = store.CountRowsByLedger(ctx, "trades", "ledger", "source='"+name+"'", genesis, hi)
	if err != nil {
		return nil, nil, err
	}
	return expected, actual, nil
}

// tradesGenesisOf looks up a trades source's genesis ledger from the
// gap-detector target list (the WASM-audit-sourced authority), so it
// never drifts from the coverage machinery.
func tradesGenesisOf(name string) (uint32, bool) {
	for _, t := range timescale.DefaultGapDetectorTargets {
		if t.Source == name && t.Table == "trades" {
			return uint32(t.Genesis), true
		}
	}
	return 0, false
}

func nilOrOne(v uint32) []uint32 {
	if v == 0 {
		return nil
	}
	return []uint32{v}
}
