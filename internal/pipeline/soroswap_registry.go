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

// upsertHookTimeout caps how long one live admission write may block the dispatcher's ledger goroutine (sub-millisecond normally; 2s covers
// failover). A write that misses it is retried off that goroutine ([gateAdmissions]).
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

	admissions := newGateAdmissions(hookCtx, logger)
	hook := func(pair, t0, t1 string) { //nolint:contextcheck // the hook carries no context; the retrier holds the hook context it was built with
		admissions.persist(soroswap.SourceName, pair, func(wctx context.Context) error {
			return store.UpsertSoroswapPair(wctx, pair, t0, t1)
		})
	}

	return []soroswap.DecoderOption{
		soroswap.WithSeededPairTokensDecoder(seed),
		soroswap.WithPairUpsertHook(hook),
	}, nil
}
