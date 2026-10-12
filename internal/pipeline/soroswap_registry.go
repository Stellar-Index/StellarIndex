package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// upsertHookTimeout caps how long the live new_pair upsert may block the dispatcher's ledger goroutine (sub-millisecond normally; 2s covers failover).
// A timeout beats a silent drop because the in-memory registry already has the pair and only future backfills or restarts miss it.
const upsertHookTimeout = 2 * time.Second

// SoroswapPersistenceOptions returns the soroswap.DecoderOption slice for [BuildDispatcher] (pair tokens seeded from soroswap_pairs plus a live new_pair upsert hook), since the in-memory registry is invisible to parallel backfill workers and restarts; hookCtx parents every upsert.
func SoroswapPersistenceOptions(
	ctx context.Context,
	store *timescale.Store,
	logger *slog.Logger,
	hookCtx context.Context,
) ([]soroswap.DecoderOption, error) {
	rows, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("load soroswap pair registry: %w", err)
	}

	seed := make(map[string]soroswap.PairTokens, len(rows))
	for _, r := range rows {
		t0, err := canonical.NewSorobanAsset(r.Token0Strkey)
		if err != nil {
			logger.Warn("skipping soroswap_pairs row with invalid token0",
				"pair", r.PairStrkey, "token0", r.Token0Strkey, "err", err)
			continue
		}
		t1, err := canonical.NewSorobanAsset(r.Token1Strkey)
		if err != nil {
			logger.Warn("skipping soroswap_pairs row with invalid token1",
				"pair", r.PairStrkey, "token1", r.Token1Strkey, "err", err)
			continue
		}
		seed[r.PairStrkey] = soroswap.PairTokens{Token0: t0, Token1: t1}
	}
	logger.Info("soroswap pair registry loaded", "pairs", len(seed))

	hook := func(pair, t0, t1 string) {
		hookTimeout, cancel := context.WithTimeout(hookCtx, upsertHookTimeout)
		defer cancel()
		if err := store.UpsertSoroswapPair(hookTimeout, pair, t0, t1); err != nil {
			logger.Warn("soroswap pair upsert (live new_pair)",
				"pair", pair, "token0", t0, "token1", t1, "err", err)
		}
	}

	return []soroswap.DecoderOption{
		soroswap.WithSeededPairTokensDecoder(seed),
		soroswap.WithPairUpsertHook(hook),
	}, nil
}
