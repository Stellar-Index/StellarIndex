package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GatedMeta describes a factory-anchored decoder (ADR-0035): its trust-root factory SET, creation-event topic[0] name, genesis ledger bounding the deploy walk, and a constructor applying contractid child-gate options.
type GatedMeta struct {
	Factories []string // canonical factory C-strkeys (gate trust roots); empty for curated-only sources
	// CreationSym is the creation event's topic[0] name, empty for curated-only sources. Matched against both on-wire encodings
	// (topic_0_sym and the ScvString in topics_xdr[1]; phoenix's String topics leave topic_0_sym empty): see event_reader.go topic0Predicate.
	CreationSym string
	Genesis     uint32 // earliest factory deploy ledger; lower bound for the walk
	// CuratedSet is the in-code trust root for sources with no factory namespace (ADR-0040 mechanism 3) or untrusted creation events (defindex);
	// nothing discovers these at runtime. It is seeded into every warmed registry and reconciled into protocol_contracts as CuratedFactoryID.
	// A curated-only entry with an empty set is rejected (TestGatedSources_curatedOnlyDeclaresTrustRoot).
	CuratedSet []string
	// NewDecoder builds the source's decoder with the given contractid
	// options (WithSeed / WithHook). Returned as a dispatcher.Decoder so
	// the genesis-seed CLI can drive it generically.
	NewDecoder func(opts ...contractid.Option) dispatcher.Decoder
}

// gatedSources is the registry of contract-gated sources. Adding one: an entry here, the decoder's Matches() gate,
// the NewDecoder call in BuildDispatcher + BuildRegistry (forwarding gated[source]), and the reconcile-catalogue fields. Soroswap keeps its own soroswap_pairs registry.
var gatedSources = map[string]GatedMeta{
	comet.SourceName: {
		// Curated-set gate (ADR-0040 mechanism 3): comet has no factory namespace (every Balancer-v1 deployment shares the ("POOL",...) topics),
		// so the decoder's in-code MainnetGatedSet is the trust root; this entry adds the protocol_contracts warm.
		Genesis:    51_499_546,
		CuratedSet: comet.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder { return comet.NewDecoder(opts...) },
	},
	blend_emitter.SourceName: {
		// Curated-set gate (ADR-0040 mechanism 3) like comet: one canonical mainnet Emitter, no creation event; MainnetGatedSet is the trust root.
		// Genesis is the earliest Emitter event on the lake.
		Genesis:    51_499_914,
		CuratedSet: blend_emitter.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder { return blend_emitter.NewDecoder(opts...) },
	},
	phoenix.SourceName: {
		// Factory-anchored gate (ADR-0040 mechanism 1): pools announced by the factory's ("create","liquidity_pool") events.
		// CreationSym is the ScvString "create" (topic_0_sym is empty for these rows, hence the topics_xdr prefilter).
		// CuratedSet carries only the stake contracts, which the factory never announces; pools already have the factory as root.
		Factories:   []string{phoenix.MainnetFactory},
		CreationSym: phoenix.EventActionCreate,
		Genesis:     51_572_016,
		CuratedSet:  phoenix.MainnetStakeContracts,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return phoenix.NewDecoder(opts...) },
	},
	blend.SourceName: {
		Factories:   blend.MainnetPoolFactories,
		CreationSym: blend.EventDeploy,
		Genesis:     blend.FactoryGenesisLedger,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return blend.NewDecoder(opts...) },
	},
	aquarius.SourceName: {
		// Router-anchored gate (ADR-0040): the router is the registry, its add_pool events announcing the pool set the public API serves.
		// MainnetGatedSet covers history; live add_pool events self-register new pools.
		Factories:   []string{aquarius.MainnetRouter},
		CreationSym: aquarius.EventAddPool,
		Genesis:     52_728_375,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return aquarius.NewDecoder(opts...) },
	},
	sushiswap_v3.SourceName: {
		// Factory-anchored gate (ADR-0035 mechanism 1): pool_created announces each pool and its token identities.
		// MainnetPools is the cold-start trust root; this entry adds the warm and live-upsert hook so later pools survive a restart.
		Factories:   sushiswap_v3.MainnetFactories,
		CreationSym: sushiswap_v3.EventPoolCreated,
		Genesis:     sushiswap_v3.FactoryGenesisLedger,
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return sushiswap_v3.NewDecoder(opts...)
		},
	},
	spectra.SourceName: {
		// Factory-anchored (ADR-0035): pt_deployed admits a PT, which then
		// admits its YT. CuratedSet is the decoder's own hand-kept seed: the
		// registry, IBTs, router, order engines and the PTs/YTs listed so far.
		Factories:   []string{spectra.MainnetFactory},
		CreationSym: spectra.EventPTDeployed,
		Genesis:     spectra.GenesisLedger,
		CuratedSet:  spectraCuratedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return spectra.NewDecoder(opts...)
		},
	},
	upshift.SourceName: {
		// Curated-set gate (ADR-0040 mechanism 3), the comet shape: the vaults have no creation event in the lake, so MainnetGatedSet is the trust root.
		// Upkeep is the bespoke-symbol sweep (deployed_assets_changed and siblings) documented in the package doc.
		Genesis:    upshift.GenesisLedger,
		CuratedSet: upshift.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return upshift.NewDecoder(opts...)
		},
	},
	defindex.SourceName: {
		// Curated-set gate (ADR-0035/0040). Neither vaults nor strategies self-register from factory create events:
		// the body's strategy addresses are attacker-controlled, so auto-seeding is a registry-poisoning vector. Hence CuratedSet is the ONLY writer of defindex's protocol_contracts rows;
		// a new strategy fail-closes into an ADR-0033 recognition gap until seeded. Factories stay declared to gate the factory's own create/n_fee events.
		Factories:   defindex.MainnetFactories,
		CreationSym: defindex.PrefixFactory,
		Genesis:     defindex.GenesisLedger,
		CuratedSet:  defindex.MainnetGatedSet(),
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return defindex.NewDecoder(opts...) },
	},
}

// GatedMetaFor returns the metadata for a factory-anchored source and whether it is gated.
func GatedMetaFor(source string) (GatedMeta, bool) {
	m, ok := gatedSources[source]
	return m, ok
}

// GatedSourceNames returns the factory-anchored source names; order is not guaranteed, so callers needing determinism must sort.
func GatedSourceNames() []string {
	out := make([]string, 0, len(gatedSources))
	for name := range gatedSources {
		out = append(out, name)
	}
	return out
}

// GatedFactories returns the canonical factory C-strkey SET for a gated source, or nil if it is not factory-anchored.
func GatedFactories(source string) []string { return gatedSources[source].Factories }

// CuratedFactoryID is the provenance on protocol_contracts rows from a source's in-code curated set, written only via SeedCuratedContracts.
const CuratedFactoryID = "curated"

// curatedReconcileTimeout caps the warm-time curated reconcile (a few single-row upserts); it must never hold up indexer boot.
const curatedReconcileTimeout = 10 * time.Second

// protocolContractStore is the protocol_contracts seam the gated-registry warm needs (read to warm, write to record a child), faked in unit tests.
type protocolContractStore interface {
	LoadProtocolContracts(ctx context.Context, source string) ([]string, error)
	ProtocolContractUpserter
}

// ProtocolContractUpserter is the protocol_contracts write seam [SeedCuratedContracts] needs; *timescale.Store implements it.
type ProtocolContractUpserter interface {
	UpsertProtocolContract(ctx context.Context, source, contractID, factoryID string, firstLedger uint32) error
}

// SeedCuratedContracts upserts a curated source's CuratedSet into protocol_contracts (factory_id = CuratedFactoryID, first_ledger = meta.Genesis) and returns rows written; it is the ONE writer of curated rows, and an empty CuratedSet on a curated-only source is an error.
func SeedCuratedContracts(
	ctx context.Context,
	store ProtocolContractUpserter,
	source string,
	meta GatedMeta,
	have []string,
) (int, error) {
	return seedCuratedContracts(ctx, store, source, meta, have)
}

func seedCuratedContracts(
	ctx context.Context,
	store ProtocolContractUpserter,
	source string,
	meta GatedMeta,
	have []string,
) (int, error) {
	if len(meta.CuratedSet) == 0 {
		return 0, fmt.Errorf("seed curated contracts %s: GatedMeta.CuratedSet is empty — "+
			"a curated-set source (ADR-0040 §1 mechanism 3) must declare its in-code trust root", source)
	}
	known := make(map[string]struct{}, len(have))
	for _, id := range have {
		known[id] = struct{}{}
	}
	seeded := 0
	for _, id := range meta.CuratedSet {
		if _, ok := known[id]; ok {
			continue
		}
		if err := store.UpsertProtocolContract(ctx, source, id, CuratedFactoryID, meta.Genesis); err != nil {
			return seeded, fmt.Errorf("seed curated contracts %s: upsert %s: %w", source, id, err)
		}
		seeded++
	}
	return seeded, nil
}

// GatedRegistryOptions warms the contractid.Registry for every gated source from protocol_contracts and meta.CuratedSet (ADR-0035, ADR-0040); withHook installs the live-upsert callback and arms curated reconcile, so read-only audits pass false and write nothing.
func GatedRegistryOptions(
	ctx context.Context,
	store *timescale.Store,
	logger *slog.Logger,
	hookCtx context.Context,
	withHook bool,
) (map[string][]contractid.Option, error) {
	return gatedRegistryOptions(ctx, store, logger, hookCtx, withHook)
}

func gatedRegistryOptions(
	ctx context.Context,
	store protocolContractStore,
	logger *slog.Logger,
	hookCtx context.Context,
	withHook bool,
) (map[string][]contractid.Option, error) {
	out := make(map[string][]contractid.Option, len(gatedSources))
	sushiOpts, err := sushiswapPoolOptions(ctx, store, logger, hookCtx, withHook)
	if err != nil {
		return nil, err
	}
	spectraOpts, err := spectraMarketOptions(ctx, store, logger)
	if err != nil {
		return nil, err
	}
	extra := map[string][]contractid.Option{
		sushiswap_v3.SourceName: sushiOpts,
		spectra.SourceName:      spectraOpts,
	}
	for source, meta := range gatedSources {
		ids, err := store.LoadProtocolContracts(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("gated registry warm %s: %w", source, err)
		}

		// The in-code curated set is part of the gate, not an optional
		// warm: a curated-only source has no other trust root at all.
		seed := make([]string, 0, len(ids)+len(meta.CuratedSet))
		seed = append(seed, ids...)
		seed = append(seed, meta.CuratedSet...)

		opts := []contractid.Option{contractid.WithSeed(seed)}
		if withHook {
			src := source // capture per iteration
			opts = append(opts, contractid.WithHook(func(childID, factoryID string, firstLedger uint32) {
				hookTimeout, cancel := context.WithTimeout(hookCtx, upsertHookTimeout)
				defer cancel()
				if err := store.UpsertProtocolContract(hookTimeout, src, childID, factoryID, firstLedger); err != nil {
					logger.Warn("protocol_contracts upsert (live factory creation)",
						"source", src, "child", childID, "factory", factoryID, "ledger", firstLedger, "err", err)
				}
			}))
		}

		// Reconcile the curated set into the table (indexer path) so roster reads agree with the gate.
		// Best-effort: the gate is already complete, so a failed upsert degrades the roster, never ingestion.
		reconciled := 0
		if withHook && len(meta.CuratedSet) > 0 {
			warmCtx, cancel := context.WithTimeout(ctx, curatedReconcileTimeout)
			n, cerr := seedCuratedContracts(warmCtx, store, source, meta, ids)
			cancel()
			reconciled = n
			if cerr != nil {
				logger.Warn("protocol_contracts reconcile (curated set)",
					"source", source, "seeded", n, "err", cerr)
			}
		}

		logger.Info("gated registry warmed",
			"source", source, "factories", meta.Factories,
			"children", len(ids), "curated", len(meta.CuratedSet),
			"gated", len(seed), "reconciled", reconciled)

		// Only a factory-anchored source without a curated set reaches this before its genesis walk: its gate is whatever the decoder seeds from code,
		// so a missing child is dropped silently. The remedy works because such decoders admit children from creation events.
		if len(seed) == 0 {
			logger.Warn("gated registry warmed with NO protocol_contracts children — "+
				"events from any child the decoder's in-code seed lacks are dropped until it is seeded",
				"source", source, "factories", meta.Factories,
				"remedy", "stellarindex-ops seed-protocol-contracts -source "+source)
		}

		opts = append(opts, extra[source]...)

		out[source] = opts
	}
	return out, nil
}

func spectraCuratedSet() []string {
	return append(spectra.MainnetGatedSet(), append([]string{spectra.MainnetRouter}, spectra.MainnetOrderEngines...)...)
}

// spectraMarketStore is the spectra_markets seam; *timescale.Store
// implements it. Optional, as sushiswapPoolStore is.
type spectraMarketStore interface {
	SpectraMarkets(ctx context.Context) ([]timescale.SpectraMarket, error)
}

// spectraMarketOptions restores each recorded market's PT and YT roles after a restart; a bare protocol_contracts row carries no role and
// fails closed.
func spectraMarketOptions(ctx context.Context, store protocolContractStore, logger *slog.Logger) ([]contractid.Option, error) {
	ms, ok := store.(spectraMarketStore)
	if !ok {
		return nil, nil
	}
	rows, err := ms.SpectraMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("gated registry warm %s markets: %w", spectra.SourceName, err)
	}
	seed := make(map[string]contractid.Attrs, 2*len(rows))
	for _, m := range rows {
		seed[m.PT] = contractid.Attrs{spectra.AttrRole: string(spectra.RolePT)}
		if m.YT != "" {
			seed[m.YT] = contractid.Attrs{spectra.AttrRole: string(spectra.RoleYT), spectra.AttrMarketPT: m.PT}
		}
	}
	logger.Info("spectra market table loaded", "markets", len(rows))
	return []contractid.Option{contractid.WithAttrSeed(seed)}, nil
}

// sushiswapPoolStore is the sushiswap_v3_pools seam; *timescale.Store
// implements it. Optional: a store without it (unit doubles) keeps the
// protocol_contracts-only warm.
type sushiswapPoolStore interface {
	LoadSushiswapV3Pools(ctx context.Context) ([]timescale.SushiswapV3Pool, error)
	UpsertSushiswapV3Pool(ctx context.Context, p timescale.SushiswapV3Pool) error
}

// sushiswapPoolOptions seeds the decoder's pool→token map from
// sushiswap_v3_pools and, on the indexer path, persists each observed
// pool_created so a pool admitted later keeps its token identities.
func sushiswapPoolOptions(
	ctx context.Context,
	store protocolContractStore,
	logger *slog.Logger,
	hookCtx context.Context,
	withHook bool,
) ([]contractid.Option, error) {
	ps, ok := store.(sushiswapPoolStore)
	if !ok {
		return nil, nil
	}
	rows, err := ps.LoadSushiswapV3Pools(ctx)
	if err != nil {
		return nil, fmt.Errorf("gated registry warm %s pools: %w", sushiswap_v3.SourceName, err)
	}
	seed := make(map[string]contractid.Attrs, len(rows))
	for _, r := range rows {
		seed[r.PoolID] = contractid.Attrs{
			sushiswap_v3.AttrToken0: r.Token0,
			sushiswap_v3.AttrToken1: r.Token1,
		}
	}
	logger.Info("sushiswap_v3 pool table loaded", "pools", len(seed))
	opts := []contractid.Option{contractid.WithAttrSeed(seed)}
	if withHook {
		opts = append(opts, contractid.WithAttrHook(func(pool, factoryID string, ledger uint32, a contractid.Attrs) {
			fee, ferr := strconv.ParseInt(a[sushiswap_v3.AttrFeePips], 10, 32)
			tick, terr := strconv.ParseInt(a[sushiswap_v3.AttrTickSpacing], 10, 32)
			if ferr != nil || terr != nil {
				logger.Warn("sushiswap_v3_pools upsert skipped: unparseable fee/tick",
					"pool", pool, "ledger", ledger, "fee_err", ferr, "tick_err", terr)
				return
			}
			hookTimeout, cancel := context.WithTimeout(hookCtx, upsertHookTimeout)
			defer cancel()
			if err := ps.UpsertSushiswapV3Pool(hookTimeout, timescale.SushiswapV3Pool{
				PoolID: pool, FactoryID: factoryID,
				Token0: a[sushiswap_v3.AttrToken0], Token1: a[sushiswap_v3.AttrToken1],
				FeePips: int32(fee), TickSpacing: int32(tick), CreationLedger: ledger,
			}); err != nil {
				logger.Warn("sushiswap_v3_pools upsert (live pool_created)",
					"pool", pool, "factory", factoryID, "ledger", ledger, "err", err)
			}
		}))
	}
	return opts, nil
}
