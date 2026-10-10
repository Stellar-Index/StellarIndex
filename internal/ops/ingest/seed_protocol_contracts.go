package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// seedProtocolContracts is the genesis bootstrap for a factory-anchored gated
// decoder's pool/vault registry (ADR-0035): it walks the source's factory creation
// events (e.g. Blend pool-factory `deploy`) from the factory genesis ledger in the
// lake's contract_events and upserts every announced child into protocol_contracts.
//
// Run once per FACTORY-anchored source as a DEPLOY PRECONDITION: until then the
// registry holds no discovered children and (ADR-0035) drops their events.
// CURATED-set sources (ADR-0040 §1 mechanism 3) are seeded by the indexer on every
// boot; this remains a repair step for them.
//
// Idempotent (UpsertProtocolContract is ON CONFLICT DO UPDATE). Without -write the
// run is a fail-closed DRY RUN (opsutil.WriteGate) reporting what it WOULD upsert.
func seedProtocolContracts(args []string) error {
	fs, gate := opsutil.NewMutatingFlagSet("seed-protocol-contracts")
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	source := fs.String("source", "", "gated source to seed (blend, … or 'all') (required)")
	to := fs.Uint("to", 0, "last ledger (inclusive); 0 = the ClickHouse lake's max ledger")
	chAddr := fs.String("ch-addr", "127.0.0.1:9300", "ClickHouse native address (the creation events come from the lake's contract_events)")
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
		maxL, merr := clickhouse.MaxLedger(ctx, *chAddr)
		if merr != nil {
			return fmt.Errorf("resolve lake max ledger: %w", merr)
		}
		if maxL == 0 {
			return errors.New("the ClickHouse lake holds no ledgers — nothing to walk")
		}
		hi = maxL
	}
	seedStore := lakeSeedStore{Store: store, ReconcileEventStreamer: clickhouse.ReconcileEventStreamer{Addr: *chAddr}}

	var sources []string
	if strings.EqualFold(*source, "all") {
		sources = sortedGatedNames()
	} else {
		sources = []string{strings.ToLower(*source)}
	}

	verb := writeModeVerb(write, "upserted", "WOULD upsert")
	var errs []error
	for _, src := range sources {
		n, serr := seedOneGatedSource(ctx, seedStore, write, src, hi)
		fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s — %s %d child contract(s) into protocol_contracts (%s)\n",
			src, verb, n, seedScope(src, hi))
		if serr != nil {
			errs = append(errs, serr)
		}
	}
	// Every source is attempted; any failure makes the precondition fail.
	return errors.Join(errs...)
}

// seedScope names what a source's seed covered: the factory-walk range
// (from the factory genesis, not 0) and/or the in-code curated set.
func seedScope(source string, hi uint32) string {
	meta, ok := pipeline.GatedMetaFor(source)
	switch {
	case !ok:
		return "not a gated source"
	case len(meta.Factories) == 0:
		return "curated set"
	case len(meta.CuratedSet) > 0:
		return fmt.Sprintf("curated set + walked [%d, %d]", meta.Genesis, hi)
	default:
		return fmt.Sprintf("walked [%d, %d]", meta.Genesis, hi)
	}
}

// gatedSeedStore writes protocol_contracts and reads the creation events.
type gatedSeedStore interface {
	pipeline.ProtocolContractUpserter
	completeness.EventStreamer
}

// lakeSeedStore upserts into Postgres and walks the ClickHouse lake.
type lakeSeedStore struct {
	*timescale.Store
	clickhouse.ReconcileEventStreamer
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
	// Each per-row failure is counted, not just printed: a child that
	// misses protocol_contracts has its events dropped by the gate forever.
	seeded, failed := 0, 0
	hook := func(childID, factoryID string, firstLedger uint32) {
		if write {
			if err := store.UpsertProtocolContract(ctx, source, childID, factoryID, firstLedger); err != nil {
				fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s upsert %s failed: %v\n", source, childID, err)
				failed++
				return
			}
		}
		seeded++
	}
	dec := meta.NewDecoder(contractid.WithHook(hook))

	// Walk every factory's creation events in one lake scan (filter on the
	// factory SET — Blend has more than one factory).
	var last events.Event
	haveLast := false
	err := store.StreamContractEvents(ctx, meta.Genesis, hi,
		meta.Factories, []string{meta.CreationSym},
		func(ev events.Event) error {
			// The stream reads without FINAL, so an unmerged duplicate part
			// arrives adjacent to its twin; count each creation once.
			if haveLast && sameEventIdentity(ev, last) {
				return nil
			}
			last, haveLast = ev, true
			if dec.Matches(ev) {
				if _, derr := dec.Decode(ev); derr != nil {
					fmt.Fprintf(os.Stderr, "seed-protocol-contracts: %s decode at ledger %d: %v\n", source, ev.Ledger, derr)
					failed++
				}
			}
			return nil
		})
	if err != nil {
		return seeded, fmt.Errorf("%s: walk factory creation events: %w", source, err)
	}
	if failed > 0 {
		return seeded, fmt.Errorf("%s: %d creation event(s) or child upsert(s) failed (see above); "+
			"their children are missing from protocol_contracts — re-run after fixing the cause", source, failed)
	}
	return seeded, nil
}

func sameEventIdentity(a, b events.Event) bool {
	return a.Ledger == b.Ledger && a.TxHash == b.TxHash && a.OperationIndex == b.OperationIndex && a.EventIndex == b.EventIndex
}

func sortedGatedNames() []string {
	names := pipeline.GatedSourceNames()
	sort.Strings(names)
	return names
}
