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

// upsertHookTimeout caps how long the live new_pair upsert is allowed
// to block the dispatcher's ledger goroutine. A locally-attached
// postgres handles a single bound INSERT in sub-millisecond — the 2s
// ceiling exists for the pathological case (postgres in failover,
// network blip). A timeout is preferable to dropping the upsert
// silently because the in-memory registry already absorbed the new
// pair; any upsert miss only matters for FUTURE backfill chunks /
// indexer restarts and we'd want the operator to see the warning.
const upsertHookTimeout = 2 * time.Second

// SoroswapPersistenceOptions returns the soroswap.DecoderOption slice
// the indexer + every parallel backfill chunk should pass to
// [BuildDispatcher] when soroswap is in the enabled-sources list.
//
//  1. WithSeededPairTokensDecoder — preloaded from the soroswap_pairs table
//     (empty is fine; run `stellarindex-ops seed-soroswap-pairs` once to bootstrap).
//  2. WithPairUpsertHook — bound to store.UpsertSoroswapPair so every live
//     factory new_pair event persists to the table the next load reads.
//
// Both, because the live in-memory pair registry is invisible to parallel
// backfill workers and restarts (migrations/0016_create_soroswap_pairs.up.sql).
//
// hookCtx is the parent context of every UpsertSoroswapPair call; pass the
// binary's root context.
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
