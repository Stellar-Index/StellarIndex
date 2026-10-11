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

// seedSoroswapPairs reads every pair contract the Soroswap factory has deployed via
// stellar-rpc simulateTransaction and writes the (pair, token0, token1) tuples to
// the soroswap_pairs registry.
//
// One-shot bootstrap for first deployment or a factory reset, so the indexer and
// backfill chunks boot with the registry primed; later new_pair events are upserted
// live (internal/pipeline/soroswap_registry.go). The sweep is ~3N+1 simulate calls
// at a 300ms throttle, under public stellar-rpc's ~3-5 req/s limit.
//
// Insert-only: a pair already in the registry is never rewritten, since on-chain
// new_pair rows outrank an unauthenticated RPC simulation. A pair whose RPC tokens
// disagree with its row is printed as a diff, left untouched, and fails the run.
//
// Fail-closed (opsutil.WriteGate): the default run is a DRY RUN reporting the pairs
// it WOULD insert; -write applies.
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

	seeder, err := newSoroswapPairSeeder(ctx, os.Stderr, store, write)
	if err != nil {
		return err
	}
	// Decoder.SeedPair fires the WithPairUpsertHook callback for every
	// pair the factory walk discovers, so SeedFromFactoryRPC + the hook
	// is the entire pipeline. The decoder's in-memory map is throwaway
	// here — postgres is the durable side.
	dec := soroswap.NewDecoder(soroswap.WithPairUpsertHook(seeder.seed))

	rpc := stellarrpc.New(endpoint, stellarrpc.WithTimeout(60*time.Second))
	for _, factory := range soroswapFactoriesToSeed(cfg.Oracle.Soroswap.FactoryContract) {
		count, err := dec.SeedFromFactoryRPC(ctx, rpc, factory)
		if err != nil {
			return fmt.Errorf("rpc seed factory %s: %w (in-memory: %d, inserted: %d, failed: %d)",
				factory, err, count, seeder.inserted.Load(), seeder.failed.Load())
		}
	}
	return seeder.finish()
}

// soroswapFactoriesToSeed returns the configured factory first, then — when
// it is a mainnet factory — the rest of soroswap.MainnetFactories, so the
// early launch-era factories' pairs are registered too. A non-mainnet
// (testnet) factory is seeded alone.
func soroswapFactoriesToSeed(configured string) []string {
	out := []string{configured}
	if !soroswap.IsMainnetFactory(configured) {
		return out
	}
	for _, f := range soroswap.MainnetFactories {
		if f != configured {
			out = append(out, f)
		}
	}
	return out
}

// soroswapPairSeedStore is the slice of *timescale.Store the seed path uses.
type soroswapPairSeedStore interface {
	LoadSoroswapPairRegistry(ctx context.Context) ([]timescale.SoroswapPair, error)
	InsertSoroswapPairIfAbsent(ctx context.Context, pair, token0, token1 string) (bool, error)
}

// soroswapPairSeeder classifies each RPC-discovered pair against the
// registry snapshot taken before the sweep and inserts only new pairs.
type soroswapPairSeeder struct {
	ctx      context.Context
	out      io.Writer
	store    soroswapPairSeedStore
	existing map[string]timescale.SoroswapPair
	write    bool

	inserted, unchanged, conflicts, failed atomic.Int64
}

func newSoroswapPairSeeder(ctx context.Context, out io.Writer, store soroswapPairSeedStore, write bool) (*soroswapPairSeeder, error) {
	rows, err := store.LoadSoroswapPairRegistry(ctx)
	if err != nil {
		return nil, fmt.Errorf("load soroswap_pairs registry: %w", err)
	}
	existing := make(map[string]timescale.SoroswapPair, len(rows))
	for _, r := range rows {
		existing[r.PairStrkey] = r
	}
	return &soroswapPairSeeder{ctx: ctx, out: out, store: store, existing: existing, write: write}, nil
}

func (s *soroswapPairSeeder) seed(pair, t0, t1 string) {
	if t0 == "" || t1 == "" {
		_, _ = fmt.Fprintf(s.out, "  skip pair %s — empty token strkey\n", pair)
		s.failed.Add(1)
		return
	}
	if cur, ok := s.existing[pair]; ok {
		if cur.Token0Strkey == t0 && cur.Token1Strkey == t1 {
			s.unchanged.Add(1)
			return
		}
		_, _ = fmt.Fprintf(s.out, "  CONFLICT pair %s: registry token0=%s token1=%s, rpc token0=%s token1=%s — registry kept\n",
			pair, cur.Token0Strkey, cur.Token1Strkey, t0, t1)
		s.conflicts.Add(1)
		return
	}
	if !s.write {
		s.inserted.Add(1)
		return
	}
	ok, err := s.store.InsertSoroswapPairIfAbsent(s.ctx, pair, t0, t1)
	if err != nil {
		_, _ = fmt.Fprintf(s.out, "  insert pair %s: %v\n", pair, err)
		s.failed.Add(1)
		return
	}
	if !ok {
		_, _ = fmt.Fprintf(s.out, "  pair %s was registered by the indexer during the sweep — left as is\n", pair)
		s.unchanged.Add(1)
		return
	}
	s.inserted.Add(1)
}

func (s *soroswapPairSeeder) finish() error {
	_, _ = fmt.Fprintf(s.out, "seed-soroswap-pairs: %d new pairs %s; %d already registered; %d conflicting (kept); %d failed\n",
		s.inserted.Load(),
		writeModeVerb(s.write, "inserted into soroswap_pairs", "WOULD be inserted into soroswap_pairs (pass -write to apply)"),
		s.unchanged.Load(), s.conflicts.Load(), s.failed.Load())
	if n := s.conflicts.Load(); n > 0 {
		return fmt.Errorf("%d pairs disagree with the registry and were NOT overwritten — the RPC answer contradicts on-chain new_pair data; check the endpoint", n)
	}
	if n := s.failed.Load(); n > 0 {
		return fmt.Errorf("%d pairs failed — check logs above", n)
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
