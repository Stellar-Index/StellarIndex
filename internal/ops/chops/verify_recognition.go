package chops

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// verifyRecognition implements ADR-0033 Claim 2a: every (contract_id, topic_0_sym) shape in contract_events must be claimed by a decoder,
// else a gap. It runs computeRecognitionGapsCH, the census behind recognition_ok, so they cannot disagree. Non-zero exit on any gap or an
// empty range.
func verifyRecognition(args []string) error {
	fs := flag.NewFlagSet("verify-recognition", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	from := fs.Uint("from", 0, "First ledger sequence (inclusive, required)")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive, required)")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (the event shapes come from the lake's contract_events)")
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Warm the factory-anchored gated registries (ADR-0035) read-only so a
	// real protocol child's event shape is recognized and a FOREIGN
	// emitter of the same topic is correctly flagged. Requires
	// protocol_contracts seeded (`seed-protocol-contracts`).
	gatedOpts, err := pipeline.GatedRegistryOptions(ctx, store, slog.Default(), ctx, false)
	if err != nil {
		return fmt.Errorf("gated registry warm: %w", err)
	}

	seedOpt, err := soroswapRecognitionSeed(ctx, store)
	if err != nil {
		return err
	}

	gaps, err := computeRecognitionGapsCH(ctx, cfg, *chAddr, gatedOpts, uint32(*from), uint32(*to), seedOpt)
	if err != nil {
		return fmt.Errorf("recognition scan: %w", err)
	}
	if len(gaps) == 0 {
		fmt.Fprintf(os.Stderr, "verify-recognition: OK — every on-chain event shape in ledgers [%d, %d] is recognized by a decoder\n", *from, *to)
		return nil
	}

	fmt.Fprintf(os.Stderr, "verify-recognition: %d UNRECOGNIZED event shape(s):\n", len(gaps))
	for _, g := range gaps {
		sym := g.Topic0Sym
		if sym == "" {
			sym = "(non-symbol topic[0])"
		}
		_, _ = fmt.Fprintf(os.Stdout, "  contract=%s topic0=%q count=%d ledgers=[%d,%d] — %s\n",
			g.ContractID, sym, g.Count, g.MinLedger, g.MaxLedger, g.Reason)
	}
	return fmt.Errorf("%d unrecognized event shape(s) — a decoder is missing a topic (ADR-0033 EVERY-event policy)", len(gaps))
}

// soroswapRecognitionSeed loads the soroswap pair registry as the dispatcher option seeding the decoder's pair-tokens cache; without it
// Matches() rejects every real SoroswapPair event as a false gap. Shared with ch-recognition; fails closed on a load error.
func soroswapRecognitionSeed(ctx context.Context, store *timescale.Store) (soroswap.DecoderOption, error) {
	pairs, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("load soroswap pair registry: %w", err)
	}
	seed := make(map[string]soroswap.PairTokens, len(pairs))
	for _, p := range pairs {
		seed[p.PairStrkey] = soroswap.PairTokens{}
	}
	return soroswap.WithSeededPairTokensDecoder(seed), nil
}
