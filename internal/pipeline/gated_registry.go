package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	blend_emitter "github.com/Stellar-Index/StellarIndex/internal/sources/blend_emitter"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
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
	// factory namespace (ADR-0040 §1 mechanism 3 — comet,
	// blend_emitter, upshift). It is the source's whole trust root:
	// there are no creation events to walk, so nothing discovers these
	// contracts at runtime. GatedRegistryOptions seeds it into every
	// warmed registry and (on the indexer path) reconciles it into
	// protocol_contracts with provenance factory_id = CuratedFactoryID;
	// seed-protocol-contracts upserts the same set through the same
	// writer. A curated-only entry (Factories empty) with an empty
	// CuratedSet declares a gate with no trust root and is rejected —
	// see TestGatedSources_curatedOnlyDeclaresTrustRoot.
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
	sushiswap_v3.SourceName: {
		// Factory-anchored gate (ADR-0035 mechanism 1): one pool factory
		// whose `pool_created` event announces every pool AND carries the
		// pool's token identities. The decoder's in-code curated table
		// (MainnetPools — all 58 pools the factory has created, decoded from
		// the lake) is the cold-start trust root; this entry adds the
		// protocol_contracts warm plus the live-upsert hook so a pool created
		// after the table was frozen survives a restart.
		Factories:   sushiswap_v3.MainnetFactories,
		CreationSym: sushiswap_v3.EventPoolCreated,
		Genesis:     sushiswap_v3.FactoryGenesisLedger,
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return sushiswap_v3.NewDecoder(opts...)
		},
	},
	upshift.SourceName: {
		// Curated-set gate (ADR-0040 §1 mechanism 3), the comet shape:
		// the Upshift vaults have NO factory namespace — neither vault
		// has a creation event anywhere in the lake, each one's first
		// event being its own `admin_set` — so there is nothing to
		// anchor a fan-out on. The decoder's in-code seed
		// (MainnetGatedSet: earnUSDC + earnXLM, both verified against
		// the lake) is the trust root; this entry adds the
		// protocol_contracts warm, the operator seam for admitting a
		// third vault without a redeploy. The upkeep loop is the
		// bespoke-symbol sweep documented in the package doc: the two
		// vaults are the only contracts emitting
		// `deployed_assets_changed` and its four siblings.
		Genesis:    upshift.GenesisLedger,
		CuratedSet: upshift.MainnetGatedSet(),
		NewDecoder: func(opts ...contractid.Option) dispatcher.Decoder {
			return upshift.NewDecoder(opts...)
		},
	},
	defindex.SourceName: {
		// Curated-set gate (ADR-0035/0040; strategy create-body
		// self-registration REMOVED 2026-08-25, W8 6c). Neither vaults
		// NOR strategies self-register from factory `create` events any
		// more: the create body's strategy addresses are attacker-
		// controlled bytes (anyone can call the public factory naming
		// arbitrary addresses), so auto-seeding them was a permissionless
		// registry-poisoning vector — a named contract would then decode
		// as a recognised DeFindex flow, contaminating TVL/flow
		// attribution. The decoder's in-code evidence-verified seed
		// (MainnetStrategies + MainnetVaults, lake-proven complete —
		// the curated strategy set is byte-identical to the full
		// create-body extraction, 16/16) is now the sole trust root, and
		// the protocol_contracts warm is the operator seam for admitting
		// a newly-verified vault OR strategy without a redeploy. A new
		// strategy first appearing after the curated freeze fail-closes
		// into an ADR-0033 recognition gap until an operator seeds it.
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

// CuratedFactoryID is the provenance stamped on a protocol_contracts row
// that comes from a source's IN-CODE curated set (ADR-0040 §1 mechanism 3)
// rather than from a factory creation event. Both writers of those rows —
// the warm-time reconcile in GatedRegistryOptions and the
// `seed-protocol-contracts` CLI — go through [SeedCuratedContracts], so
// the two paths cannot drift into different provenance.
const CuratedFactoryID = "curated"

// curatedReconcileTimeout caps the warm-time curated reconcile. It writes
// at most len(CuratedSet) single-row upserts (today: 1-2 per source) and
// must never be able to hold up indexer boot.
const curatedReconcileTimeout = 10 * time.Second

// protocolContractStore is the `protocol_contracts` seam the gated-registry
// warm needs: the read that warms a decoder's gate, and the write that
// records a child. *timescale.Store implements it; the unit tests
// substitute an in-memory double so the warm's own invariants — chiefly
// "a curated source's trust root is installed whatever the table holds" —
// are provable without a database.
type protocolContractStore interface {
	LoadProtocolContracts(ctx context.Context, source string) ([]string, error)
	UpsertProtocolContract(ctx context.Context, source, contractID, factoryID string, firstLedger uint32) error
}

// SeedCuratedContracts upserts a curated-set source's in-code trust root
// (GatedMeta.CuratedSet) into protocol_contracts with provenance
// factory_id = [CuratedFactoryID] and first_ledger = meta.Genesis, and
// returns how many rows it wrote.
//
// It is the ONE writer of curated rows. `seed-protocol-contracts -source
// <name>` calls it with have=nil (its documented contract is an idempotent
// full re-seed); the warm-time reconcile calls it with the ids the table
// already holds, so a restart does not re-stamp observed_at on rows that
// are already correct.
//
// A curated-only source with an EMPTY CuratedSet is an error, not a
// zero-row success: that entry declares a gate with no trust root, and
// reporting "upserted 0 child contract(s)" for it is precisely the silence
// this seam exists to end.
func SeedCuratedContracts(
	ctx context.Context,
	store *timescale.Store,
	source string,
	meta GatedMeta,
	have []string,
) (int, error) {
	return seedCuratedContracts(ctx, store, source, meta, have)
}

func seedCuratedContracts(
	ctx context.Context,
	store protocolContractStore,
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

// GatedRegistryOptions warms the contractid.Registry for every
// contract-gated source and returns a map keyed by source name.
// BuildDispatcher / BuildRegistry forward out[source] to each gated
// decoder's NewDecoder so the in-memory gate resumes with a COMPLETE
// registry across restarts (the projector cursor advances past the
// creation events, so live-only seeding would miss every pool deployed
// before boot — ADR-0035 coverage note).
//
// The warm has TWO inputs, not one:
//
//   - the protocol_contracts table, which is where a FACTORY-anchored
//     source's children live (they are discovered from creation events;
//     there is nothing in code to seed them with), and
//   - meta.CuratedSet, the IN-CODE trust root of a curated-set source
//     (ADR-0040 §1 mechanism 3 — comet, blend_emitter, upshift). Those
//     sources have no factory and no creation events, so nothing ever
//     writes their contracts to the table on its own.
//
// Seeding the curated set here is what makes the returned options
// self-sufficient. Before this, the only thing that put a curated
// source's contracts anywhere was an operator remembering to run
// `stellarindex-ops seed-protocol-contracts -source <name>`, and until
// they did, two things were true. The options this map handed out
// carried NOTHING for that source: the gate a caller ended up with held
// only what the decoder package happened to re-install in its own
// constructor (every curated decoder does today — which is a redundancy
// this layer must not silently depend on, since GatedMeta.CuratedSet is
// where the trust root is declared and blend's constructor deliberately
// installs no children at all). And the table itself stayed empty, so
// GET /v1/protocols/{name} served an empty roster and the explorer's
// contract-attribution overlay tagged none of the contracts — silently,
// because "children=0" reads exactly like a protocol that has not
// deployed a pool yet. Measured on r1 2026-09-09: aquarius 352, blend
// 29, defindex 16, sushiswap_v3 58, upshift 0.
//
// withHook installs the live-upsert persistence callback (the indexer
// path): when a decoder observes a NEW factory creation event it upserts
// the child into protocol_contracts so the next restart inherits it. It
// ALSO arms the curated reconcile — the indexer writes any curated
// contract the table is missing, so the operator step disappears.
// Read-only consumers (the recognition / completeness audits) pass
// withHook=false: they still GATE on the curated set (contractid.WithSeed
// is a pure constructor option over a compile-time constant and fires no
// hook — see TestRegistry_WithSeed_doesNotFireHook — so it is not the
// mutation that clause forbids), but they write nothing, because an audit
// that registered contracts while auditing them would be manufacturing
// its own evidence.
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

		// Reconcile the curated set into the table on the indexer path so
		// the roster reads (GET /v1/protocols/{name}, the explorer's
		// contract attribution) agree with the gate. Best-effort: the
		// gate above is already complete, so a failed upsert degrades the
		// roster, never ingestion — same idiom as the live-upsert hook.
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

		// A gated source with an EMPTY gate drops every event it sees,
		// and every upstream signal (cursor advancing, lake complete,
		// decoder linked in) still looks healthy. Curated sources can no
		// longer reach this state; a factory-anchored one can, before its
		// genesis walk has run, so say so at WARN with the remedy in the
		// line rather than leaving `children=0` to be read as normal.
		if len(seed) == 0 {
			logger.Warn("gated registry warmed with an EMPTY contract gate — "+
				"every event for this source will be dropped until it is seeded",
				"source", source, "factories", meta.Factories,
				"remedy", "stellarindex-ops seed-protocol-contracts -source "+source)
		}

		out[source] = opts
	}
	return out, nil
}
