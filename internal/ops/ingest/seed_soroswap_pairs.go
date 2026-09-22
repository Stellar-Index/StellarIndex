package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	externalchainlink "github.com/Stellar-Index/StellarIndex/internal/sources/external/chainlink"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seedSoroswapPairs reads every pair contract the Soroswap factory
// has ever deployed via stellar-rpc simulateTransaction, then writes
// the (pair, token0, token1) tuples to the soroswap_pairs registry
// table.
//
// Run once on first deployment (or after a factory reset) so the
// indexer + parallel backfill chunks boot with the registry primed.
// Subsequent factory new_pair events are upserted live by the
// indexer (see internal/pipeline/soroswap_registry.go), so this
// subcommand is one-shot bootstrap, not a regular cron.
//
// Flags:
//
//	-config PATH    TOML config (required). Used for postgres DSN +
//	                soroswap factory contract + RPC fallback.
//	-rpc URL        Override RPC endpoint. Falls back to
//	                cfg.Oracle.Soroswap.SeedRPCEndpoint, then to the
//	                first cfg.Stellar.RPCEndpoints entry.
//	-timeout DUR    Total wall-clock budget. Default 15m — enough
//	                for ~200 mainnet pairs at 300ms throttle.
//
// The sweep is ~3N+1 simulateTransaction calls with a 300ms throttle
// between each, so wall-time scales linearly with pair count. Public
// stellar-rpc endpoints rate-limit at ~3-5 req/s; the throttle keeps
// us comfortably below.
//
// Idempotent on every level:
//   - The factory's view functions are pure reads.
//   - UpsertSoroswapPair is ON CONFLICT (pair_strkey) DO UPDATE.
//   - Re-running after the indexer has already learned new pairs is
//     safe (every row is rewritten with the same data).
//
// Fail-closed (opsutil.WriteGate): the default run is a DRY RUN that
// walks the factory and reports the pairs it WOULD upsert, writing none
// of them. -write applies.
func seedSoroswapPairs(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("seed-soroswap-pairs")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	rpcOverride := fs.String("rpc", "", "stellar-rpc endpoint URL (overrides config)")
	timeout := fs.Duration("timeout", 15*time.Minute, "wall-clock budget for the sweep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config required")
	}
	write := gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Oracle.Soroswap.FactoryContract == "" {
		return errors.New("oracle.soroswap.factory_contract is empty — refusing to sweep an unset factory")
	}

	endpoint := *rpcOverride
	if endpoint == "" {
		endpoint = cfg.Oracle.Soroswap.SeedRPCEndpoint
	}
	if endpoint == "" && len(cfg.Stellar.RPCEndpoints) > 0 {
		endpoint = cfg.Stellar.RPCEndpoints[0]
	}
	if endpoint == "" {
		return errors.New("no RPC endpoint — set -rpc, oracle.soroswap.seed_rpc_endpoint, or stellar.rpc_endpoints")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	logSeedStart(os.Stderr, cfg.Oracle.Soroswap.FactoryContract, endpoint)

	// Decoder.SeedPair fires the WithPairUpsertHook callback for every
	// pair the factory walk discovers, so SeedFromFactoryRPC + the hook
	// is the entire pipeline. The decoder's in-memory map is throwaway
	// here — postgres is the durable side.
	var (
		upserts atomic.Int64
		failed  atomic.Int64
	)
	dec := soroswap.NewDecoder(soroswap.WithPairUpsertHook(func(pair, t0, t1 string) {
		if t0 == "" || t1 == "" {
			fmt.Fprintf(os.Stderr, "  skip pair %s — empty token strkey\n", pair)
			failed.Add(1)
			return
		}
		if write {
			if err := store.UpsertSoroswapPair(ctx, pair, t0, t1); err != nil {
				fmt.Fprintf(os.Stderr, "  upsert pair %s: %v\n", pair, err)
				failed.Add(1)
				return
			}
		}
		upserts.Add(1)
	}))

	rpc := stellarrpc.New(endpoint, stellarrpc.WithTimeout(60*time.Second))
	count, err := dec.SeedFromFactoryRPC(ctx, rpc, cfg.Oracle.Soroswap.FactoryContract)
	if err != nil {
		return fmt.Errorf("rpc seed: %w (in-memory: %d, persisted: %d, failed: %d)",
			err, count, upserts.Load(), failed.Load())
	}

	fmt.Fprintf(os.Stderr, "seed-soroswap-pairs: %d pairs %s (%d failed)\n",
		upserts.Load(),
		writeModeVerb(write, "persisted to soroswap_pairs", "WOULD be persisted to soroswap_pairs (pass -write to apply)"),
		failed.Load())
	if failed.Load() > 0 {
		return fmt.Errorf("%d upserts failed — check logs above", failed.Load())
	}
	return nil
}

// logSeedStart announces the sweep target. The RPC endpoint is redacted:
// -rpc / seed_rpc_endpoint / rpc_endpoints[0] can point at a keyed
// third-party provider that carries its API key in the URL path, the
// same shape externalchainlink.RedactEndpoint already scrubs for the
// Chainlink RPC client — printing it raw would put the key in stderr,
// and from there in any log aggregator that captures ops output.
func logSeedStart(w io.Writer, factory, endpoint string) {
	_, _ = fmt.Fprintf(w, "seed-soroswap-pairs: factory=%s rpc=%s\n",
		factory, externalchainlink.RedactEndpoint(endpoint))
}
