package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GatedMeta is the per-source description a factory-anchored decoder
// (ADR-0035) needs to seed its contractid.Registry: the trust-root factory
// SET (a protocol can have several factories — e.g. Blend was redeployed),
// the topic_0_sym of the factories' creation event (which announces a new
// child), the genesis ledger (lower bound for the deploy walk across all
// factories), and a constructor that builds the source's decoder with
// contractid (child-gate) options applied.
type GatedMeta struct {
	Factories   []string // canonical factory C-strkeys (gate trust roots); empty for curated-only sources
	CreationSym string   // topic_0_sym of the creation event (e.g. "deploy"); empty for curated-only sources
	Genesis     uint32   // earliest factory deploy ledger; lower bound for the walk
	// CuratedSet is the in-code curated child set for sources with NO
	// factory namespace (ADR-0040 §1 mechanism 3 — comet). When
	// Factories is empty, seed-protocol-contracts upserts this set
	// directly (provenance factory_id = "curated") instead of walking
	// creation events that don't exist.
	CuratedSet []string
	// NewDecoder builds the source's decoder with the given contractid
	// options (WithSeed / WithHook). Returned as a dispatcher.Decoder so
	// the genesis-seed CLI can drive it generically.
	NewDecoder func(opts ...contractid.Option) dispatcher.Decoder
}

// gatedSources is the registry of contract-gated sources. Adding one is
// four edits: an entry here, the decoder's Matches() gate, the NewDecoder
// call in BuildDispatcher + BuildRegistry (forwarding gated[source]), and
// the reconcile-catalogue factory/creationSym fields.
//
// Soroswap is NOT here — it keeps its richer soroswap_pairs registry (it
// carries token identities, not just a contract set); see
// SoroswapPersistenceOptions.
var gatedSources = map[string]GatedMeta{
	comet.SourceName: {
		// Curated-set gate (ADR-0040 §1 mechanism 3, CS-026 closed
		// 2026-07-08): comet has NO factory namespace — every
		// Balancer-v1 deployment shares the ("POOL",…) topic family —
		// so there is no creation event to anchor on. The decoder's
		// in-code seed (MainnetGatedSet: exactly one pool, Blend's
		// backstop) is the trust root; this entry adds the
		// protocol_contracts warm (the operator seam for admitting a
		// future pool without a redeploy). The WASM-hash sweep is the
		// registered upkeep loop for discovering byte-identical pools.
		Genesis:    51_499_546,
		CuratedSet: comet.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder { return comet.NewDecoder(opts...) },
	},
	blend_emitter.SourceName: {
		// Curated-set gate (ADR-0040 §1 mechanism 3), same shape as
		// comet: the Emitter has NO factory namespace — a single
		// canonical mainnet instance spanning Blend V1→V2, no
		// creation event to anchor on. The decoder's in-code seed
		// (MainnetGatedSet: exactly the one known mainnet Emitter)
		// is the trust root; this entry adds the protocol_contracts
		// warm (the operator seam for admitting a future instance
		// without a redeploy). Genesis is the earliest observed
		// Emitter event on the lake (the ledger-51,499,914 `drop`
		// airdrop).
		Genesis:    51_499_914,
		CuratedSet: blend_emitter.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder { return blend_emitter.NewDecoder(opts...) },
	},
	phoenix.SourceName: {
		// Curated-set gate (ADR-0040 §1 mechanism 2): the factory's
		// creation events predate the lake, so the decoder's in-code
		// seed (MainnetGatedSet) is the trust root; this entry adds
		// the protocol_contracts warm + live-upsert hook on top
		// (no-ops until a decodable creation event ever appears).
		Factories:   []string{phoenix.MainnetFactory},
		CreationSym: "create",
		Genesis:     51_572_016,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return phoenix.NewDecoder(opts...) },
	},
	blend.SourceName: {
		Factories:   blend.MainnetPoolFactories,
		CreationSym: blend.EventDeploy,
		Genesis:     blend.FactoryGenesisLedger,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return blend.NewDecoder(opts...) },
	},
	aquarius.SourceName: {
		// Router-anchored gate (ADR-0040, CS-026): the router IS the
		// protocol's registry — its add_pool events announce exactly
		// the pool set the protocol's public API serves (verified
		// byte-identical 2026-07-05, docs/protocols/aquarius.md).
		// The decoder's in-code seed (MainnetGatedSet) covers history
		// (the PG soroban_events landing zone is capture-scoped and
		// holds only recent add_pool rows); live add_pool events
		// self-register new pools blend-style, and this entry adds
		// the protocol_contracts warm + live-upsert hook.
		Factories:   []string{aquarius.MainnetRouter},
		CreationSym: aquarius.EventAddPool,
		Genesis:     52_728_375,
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return aquarius.NewDecoder(opts...) },
	},
	defindex.SourceName: {
		// Mixed gate (ADR-0035/0040, ROADMAP #7 residual 2026-07-10):
		// the factory `create` event does NOT carry the new VAULT's
		// address, so the deploy-graph cannot self-register vaults —
		// the decoder's in-code evidence-verified seed (MainnetVaults)
		// remains the trust root for that half of the set. STRATEGIES
		// are different: the same `create` body DOES carry each
		// asset's assigned BlendStrategy address(es)
		// (defindex.decodeFactoryCreateStrategies), so this entry's
		// live-upsert hook is an ACTIVE fan-out for strategies (no
		// longer the "no-op unless a future WASM announces children"
		// case other curated-set sources are in) — a new strategy
		// self-registers the same tx it's created in, converging with
		// MainnetStrategies at the next seed-protocol-contracts /
		// restart. The protocol_contracts warm still exists for BOTH
		// halves (it's the operator seam for admitting a newly
		// verified VAULT without a redeploy).
		Factories:   defindex.MainnetFactories,
		CreationSym: "create",
		Genesis:     55_484_403, // earliest factory create event (CAVP2QLP…)
		NewDecoder:  func(opts ...contractid.Option) dispatcher.Decoder { return defindex.NewDecoder(opts...) },
	},
}

// GatedMetaFor returns the metadata for a factory-anchored source and
// whether it is gated. Used by the genesis-seed CLI.
func GatedMetaFor(source string) (GatedMeta, bool) {
	m, ok := gatedSources[source]
	return m, ok
}

// GatedSourceNames returns the factory-anchored source names (those that
// warm a contractid.Registry from protocol_contracts). Stable order is not
// guaranteed; callers that need determinism should sort.
func GatedSourceNames() []string {
	out := make([]string, 0, len(gatedSources))
	for name := range gatedSources {
		out = append(out, name)
	}
	return out
}

// GatedFactories returns the canonical factory C-strkey SET for a gated
// source, or nil if the source is not factory-anchored.
func GatedFactories(source string) []string { return gatedSources[source].Factories }

// GatedRegistryOptions warms the contractid.Registry for every
// factory-anchored source from the protocol_contracts table and returns a
// map keyed by source name. BuildDispatcher / BuildRegistry forward
// out[source] to each gated decoder's NewDecoder so the in-memory gate
// resumes with a COMPLETE registry across restarts (the projector cursor
// advances past the creation events, so live-only seeding would miss every
// pool deployed before boot — ADR-0035 coverage note).
//
// withHook installs the live-upsert persistence callback (the indexer
// path): when a decoder observes a NEW factory creation event it upserts
// the child into protocol_contracts so the next restart inherits it.
// Read-only consumers (the recognition / completeness audits) pass
// withHook=false — they must not mutate the registry while auditing it.
//
// hookCtx scopes the live upserts' lifetime (typically the process root
// context) and is unused when withHook is false.
func GatedRegistryOptions(
	ctx context.Context,
	store *timescale.Store,
	logger *slog.Logger,
	hookCtx context.Context,
	withHook bool,
) (map[string][]contractid.Option, error) {
	out := make(map[string][]contractid.Option, len(gatedSources))
	for source, meta := range gatedSources {
		ids, err := store.LoadProtocolContracts(ctx, source)
		if err != nil {
			return nil, fmt.Errorf("gated registry warm %s: %w", source, err)
		}
		opts := []contractid.Option{contractid.WithSeed(ids)}
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
		logger.Info("gated registry warmed", "source", source, "factories", meta.Factories, "children", len(ids))
		out[source] = opts
	}
	return out, nil
}
