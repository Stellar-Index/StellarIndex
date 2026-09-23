package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seedProtocolContracts is the genesis bootstrap for a factory-anchored
// gated decoder's pool/vault registry (ADR-0035). It walks the source's
// factory creation events (e.g. Blend pool-factory `deploy`) from the
// factory genesis ledger forward in the Postgres soroban_events lake and
// upserts every announced child contract into protocol_contracts.
//
// Run once per FACTORY-anchored source as a DEPLOY PRECONDITION before
// relying on the gate — like the migration 0057-0060 re-derive. Until it
// runs, that decoder's registry holds no discovered children and
// (correctly, per ADR-0035) drops their events; after it runs, the indexer
// keeps the table current live and every consumer warms a complete
// registry from it.
//
// CURATED-set sources (ADR-0040 §1 mechanism 3) no longer need it as a
// precondition: their trust root is in code, and the indexer's gated
// registry warm seeds it and reconciles it into protocol_contracts on
// every boot. Running it for them stays useful as a repair step (e.g. to
// re-stamp the table from a read-only host).
//
// Idempotent: the factory creation events are immutable history and
// UpsertProtocolContract is ON CONFLICT DO UPDATE, so re-running re-walks
// the same set harmlessly. Cheap: creation events are rare and the
// (contract_id, topic_0_sym) index on soroban_events serves the filter.
//
// Flags:
//
//	-config PATH   TOML config (required) — postgres DSN.
//	-source NAME   gated source to seed (required): blend, …
//	               (`-source all` seeds every gated source).
//	-to LEDGER     last ledger to walk (inclusive); 0 = the soroban_events
//	               max ledger.
//	-timeout DUR   wall-clock budget. Default 15m.
//	-write         apply. WITHOUT it the run is a fail-closed DRY RUN
//	               (opsutil.WriteGate) that walks the creation events and
//	               reports the children it WOULD upsert, writing none.
func seedProtocolContracts(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("seed-protocol-contracts")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	source := fs.String("source", "", "gated source to seed (blend, … or 'all') (required)")
	to := fs.Uint("to", 0, "last ledger (inclusive); 0 = soroban_events max ledger")
	timeout := fs.Duration("timeout", 15*time.Minute, "wall-clock budget")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config required")
	}
	if *source == "" {
		return fmt.Errorf("-source required (one of: %s, or 'all')", strings.Join(sortedGatedNames(), ", "))
	}
	write := gate.Banner()

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage open: %w", err)
	}
	defer func() { _ = store.Close() }()

	hi := uint32(*to)
	if hi == 0 {
		maxL, ok, merr := store.MaxSorobanEventLedger(ctx)
		if merr != nil {
			return fmt.Errorf("resolve soroban_events max ledger: %w", merr)
		}
		if !ok {
			return errors.New("soroban_events is empty — nothing to walk")
		}
		hi = maxL
	}

	var sources []string
	if strings.EqualFold(*source, "all") {
		sources = sortedGatedNames()
	} else {
		sources = []string{strings.ToLower(*source)}
	}

	verb := writeModeVerb(write, "upserted", "WOULD upsert")
	for _, src := range sources {
		n, serr := seedOneGatedSource(ctx, store, write, src, hi)
		if serr != nil {
			return serr
		}
		fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s — %s %d child contract(s) into protocol_contracts (walked [%d, %d])\n",
			src, verb, n, 0, hi)
	}
	return nil
}

// gatedSeedStore is the slice of *timescale.Store the seed needs.
type gatedSeedStore interface {
	pipeline.ProtocolContractUpserter
	StreamSorobanEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms, excludeTopic0Syms []string,
		fn func(sorobanevents.Row) error) error
}

// seedOneGatedSource upserts one source's protocol_contracts rows and
// returns how many it seeded: its in-code curated set, if it declares one,
// then every child its factories' creation events announce.
//
// Curated sets (ADR-0040 §1 mechanism 3 — comet, blend_emitter, upshift,
// and defindex, whose permissionless factory's creation events never
// admit a child) are upserted with provenance factory_id =
// pipeline.CuratedFactoryID. The indexer reconciles the same set at warm
// time, so running this for them is a repair/verification step rather
// than a deploy precondition.
func seedOneGatedSource(ctx context.Context, store gatedSeedStore, write bool, source string, hi uint32) (int, error) {
	meta, ok := pipeline.GatedMetaFor(source)
	if !ok {
		return 0, fmt.Errorf("%q is not a factory-anchored gated source (one of: %s)", source, strings.Join(sortedGatedNames(), ", "))
	}

	curated := 0
	if len(meta.CuratedSet) > 0 || len(meta.Factories) == 0 {
		n, err := seedCuratedSet(ctx, store, write, source, meta)
		if err != nil || len(meta.Factories) == 0 {
			return n, err
		}
		curated = n
	}
	walked, err := walkFactoryCreations(ctx, store, write, source, meta, hi)
	return curated + walked, err
}

// seedCuratedSet upserts meta.CuratedSet, or in preview counts it.
func seedCuratedSet(ctx context.Context, store gatedSeedStore, write bool, source string, meta pipeline.GatedMeta) (int, error) {
	if !write {
		// Preview of the curated path. It mirrors
		// SeedCuratedContracts' own refusal rather than reporting a
		// clean zero: a curated-only source with an empty CuratedSet
		// declares a gate with no trust root, and a preview that
		// prints "0 contracts" for it reads as "nothing to do".
		if len(meta.CuratedSet) == 0 {
			return 0, fmt.Errorf("seed curated contracts %s: GatedMeta.CuratedSet is empty — "+
				"a curated-set source (ADR-0040 §1 mechanism 3) must declare its in-code trust root", source)
		}
		return len(meta.CuratedSet), nil
	}
	// One writer for curated rows (pipeline.SeedCuratedContracts), so
	// this and the indexer's warm-time reconcile cannot drift into
	// different provenance or first_ledger. have=nil: the CLI's
	// documented contract is a full idempotent re-seed.
	return pipeline.SeedCuratedContracts(ctx, store, source, meta, nil)
}

// walkFactoryCreations replays meta's factory creation events through the
// source's decoder and upserts each child it admits.
func walkFactoryCreations(ctx context.Context, store gatedSeedStore, write bool, source string, meta pipeline.GatedMeta, hi uint32) (int, error) {
	// Build the source's decoder with a hook that upserts each newly
	// observed child into protocol_contracts — the SAME persistence path
	// the live indexer uses, so the genesis walk and live ingest converge
	// on identical rows. The factory that deployed each child is supplied by
	// the decoder (a protocol can have several factories).
	seeded := 0
	hook := func(childID, factoryID string, firstLedger uint32) {
		if write {
			if err := store.UpsertProtocolContract(ctx, source, childID, factoryID, firstLedger); err != nil {
				fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s upsert %s failed: %v\n", source, childID, err)
				return
			}
		}
		seeded++
	}
	dec := meta.NewDecoder(contractid.WithHook(hook))

	// Walk every factory's creation events in one lake scan (filter on the
	// factory SET — Blend has more than one factory).
	err := store.StreamSorobanEvents(ctx, meta.Genesis, hi,
		meta.Factories, []string{meta.CreationSym}, nil,
		func(row sorobanevents.Row) error {
			ev, rerr := sorobanevents.Reconstruct(row)
			if rerr != nil {
				return nil //nolint:nilerr // skip a broken creation row
			}
			if dec.Matches(ev) {
				if _, derr := dec.Decode(ev); derr != nil {
					fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s decode at ledger %d: %v\n", source, ev.Ledger, derr)
				}
			}
			return nil
		})
	if err != nil {
		return seeded, fmt.Errorf("%s: walk factory creation events: %w", source, err)
	}
	return seeded, nil
}

func sortedGatedNames() []string {
	names := pipeline.GatedSourceNames()
	sort.Strings(names)
	return names
}
