// Package pipeline holds the shared ingest-pipeline glue used by both
// the long-running indexer (`cmd/stellarindex-indexer`) and the
// bounded-replay backfill subcommand (`cmd/stellarindex-ops backfill`).
//
// What's here vs what isn't:
//
//   - BuildDispatcher: registers the right per-source decoders given
//     the operator's enabled-sources list + the oracle contract IDs.
//   - ProcessLedger: runs the dispatcher over one LCM and forwards
//     emitted events to a sink channel. Does NOT touch cursors or
//     emit cursor metrics (a long-running-indexer concern).
//   - PersistEvents: drains a sink channel and writes each event to
//     its hypertable. Type-switch covers every event kind any
//     registered source can emit.
//   - LedgerstreamConfig: builds a ledgerstream.Config from a global
//     config + bucket name, so both binaries share the datastore wiring.
//
// Cursors, signals, flags and metrics-server lifecycle stay in the binaries.
package pipeline

import (
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/accounts"
	"github.com/Stellar-Index/StellarIndex/internal/sources/claimable_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/liquidity_pools"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sac_balances"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/trustlines"
)

// BuildDispatcher constructs a dispatcher with decoders registered
// for every name in `names`. Returns an error on unknown names or
// when an oracle source is requested without its required contract
// ID populated in `oracle`.
//
// Each name is wired per its [SourceSpec]; watched (sep41) specs are
// never enabled by name, see [RegisterSupplyEventDecoders].
//
// soroswapOpts is forwarded to soroswap.NewDecoder when soroswap is
// in `names`. The indexer + backfill chunks pass:
//
//   - WithSeededPairTokensDecoder seeded from
//     timescale.LoadSoroswapPairRegistry, so a parallel chunk that
//     doesn't cover the original new_pair event emits no
//     "skipped_unknown_pair" noise.
//   - WithPairUpsertHook bound to timescale.UpsertSoroswapPair, so
//     newly-discovered pairs are persisted as live new_pair events.
func BuildDispatcher(names []string, oracle config.OracleConfig, gated map[string][]contractid.Option, soroswapOpts ...soroswap.DecoderOption) (*dispatcher.Dispatcher, error) {
	// Oracle-staleness policy is installed BEFORE any
	// decoder is built, so the first update a source persists already
	// publishes the right budget. Overrides go in unconditionally —
	// they are keyed by (source, asset) and a row naming a source this
	// replica does not run simply never matches a series, which is
	// cheaper than reasoning about which replica owns which asset.
	// Per-source DEFAULTS are declared by each spec's OnDispatch, so a
	// source that is not enabled here publishes neither a resolution
	// nor a budget.
	obs.SetOracleStalenessOverrides(oracleStalenessOverrides(oracle))

	a := BuildArgs{Oracle: oracle, Gated: gated, SoroswapOpts: soroswapOpts}
	disp := dispatcher.New()
	for _, name := range names {
		spec, ok := SpecByName(name)
		if !ok || spec.Watched {
			return nil, fmt.Errorf("unknown source %q in ingestion.enabled_sources — check internal/sources/", name)
		}
		if err := spec.addToDispatcher(disp, a); err != nil {
			return nil, err
		}
	}
	return disp, nil
}

// oracleStalenessOverrides translates the operator's
// `[[oracle.staleness_overrides]]` rows into the obs package's shape.
//
// The translation exists so internal/obs stays free of an
// internal/config import: obs is the leaf every binary links, and the
// budget policy it owns is described by (source, asset, seconds) —
// the `reason` field is for humans reading the config, not for the
// gauge. config.Validate has already rejected unknown sources,
// non-canonical asset spellings, non-positive budgets and duplicate
// pairs by the time this runs.
func oracleStalenessOverrides(oracle config.OracleConfig) []obs.OracleStalenessOverride {
	if len(oracle.StalenessOverrides) == 0 {
		return nil
	}
	out := make([]obs.OracleStalenessOverride, 0, len(oracle.StalenessOverrides))
	for _, ov := range oracle.StalenessOverrides {
		out = append(out, obs.OracleStalenessOverride{
			Source:        ov.Source,
			Asset:         ov.Asset,
			BudgetSeconds: float64(ov.BudgetSeconds),
		})
	}
	return out
}

// AccountObserverWatchSet is the account set the accounts observer
// watches: the SDF reserve accounts plus the metadata-watched issuers,
// deduplicated in first-seen order. Empty means the observer is not
// registered. The indexer gates the observer's watermark on the same
// set.
func AccountObserverWatchSet(sup config.SupplyConfig, meta config.MetadataConfig) []string {
	seen := make(map[string]struct{}, len(sup.SDFReserveAccounts)+len(meta.WatchedIssuerAccounts))
	var out []string
	for _, lists := range [][]string{sup.SDFReserveAccounts, meta.WatchedIssuerAccounts} {
		for _, acc := range lists {
			if _, dup := seen[acc]; dup {
				continue
			}
			seen[acc] = struct{}{}
			out = append(out, acc)
		}
	}
	return out
}

// RegisterSupplyEntryDecoders attaches the LCM-based supply observers
// to disp based on the supply config. Each observer is opt-in: an
// empty watched-set leaves it unregistered (no decoder, no per-ledger
// work). Returns the registered observer names for boot logs.
//
// Wired observers and their watch sets:
//
//   - accounts.Observer: [AccountObserverWatchSet], i.e.
//     [supply.SDFReserveAccounts] plus [metadata.WatchedIssuerAccounts].
//     Readers query by account id, so issuers do not leak into the
//     reserve sum.
//   - trustlines.Observer, claimable_balances.Observer and
//     liquidity_pools.Observer: [supply.WatchedClassicAssets].
//   - sac_balances.Observer: [supply.SACWrappers] (SAC C-strkey ->
//     asset_key map); covers SAC-wrapped classics and pure SEP-41.
//
// Persistence is the type-switch in internal/pipeline/sink.go. The
// event-stream sep41_supply observer registers in
// [RegisterSupplyEventDecoders]; call both.
func RegisterSupplyEntryDecoders(disp *dispatcher.Dispatcher, sup config.SupplyConfig, meta config.MetadataConfig) ([]string, error) {
	var registered []string
	if watched := AccountObserverWatchSet(sup, meta); len(watched) > 0 {
		obs, err := accounts.NewObserver(watched)
		if err != nil {
			return nil, fmt.Errorf("accounts observer: %w", err)
		}
		disp.AddEntryDecoder(obs)
		registered = append(registered, accounts.SourceName)
	}
	if len(sup.WatchedClassicAssets) > 0 {
		tl, err := trustlines.NewObserver(sup.WatchedClassicAssets)
		if err != nil {
			return nil, fmt.Errorf("trustlines observer: %w", err)
		}
		disp.AddEntryDecoder(tl)
		registered = append(registered, trustlines.SourceName)

		cb, err := claimable_balances.NewObserver(sup.WatchedClassicAssets)
		if err != nil {
			return nil, fmt.Errorf("claimable_balances observer: %w", err)
		}
		disp.AddEntryDecoder(cb)
		registered = append(registered, claimable_balances.SourceName)

		lp, err := liquidity_pools.NewObserver(sup.WatchedClassicAssets)
		if err != nil {
			return nil, fmt.Errorf("liquidity_pools observer: %w", err)
		}
		disp.AddEntryDecoder(lp)
		registered = append(registered, liquidity_pools.SourceName)
	}
	if len(sup.SACWrappers) > 0 {
		sac, err := sac_balances.NewObserver(sup.SACWrappers)
		if err != nil {
			return nil, fmt.Errorf("sac_balances observer: %w", err)
		}
		disp.AddEntryDecoder(sac)
		registered = append(registered, sac_balances.SourceName)
	}
	return registered, nil
}

// RegisterSupplyEventDecoders adds the dispatcher's copy of every
// [SourceSpec.Watched] source (sep41_supply, then sep41_transfers; their
// topic[0] sets are disjoint) built from supply.watched_sep41_contracts.
// An empty watched set registers nothing. Returns the registered names.
func RegisterSupplyEventDecoders(disp *dispatcher.Dispatcher, sup config.SupplyConfig) ([]string, error) {
	var registered []string
	a := BuildArgs{WatchedSEP41: sup.WatchedSEP41Contracts}
	for i := range specs {
		if !specs[i].Watched {
			continue
		}
		dec, err := specs[i].NewDecoder(a)
		if err != nil {
			return nil, fmt.Errorf("%s decoder: %w", specs[i].Name, err)
		}
		if dec == nil {
			continue
		}
		disp.AddDecoder(dec)
		registered = append(registered, specs[i].Name)
	}
	return registered, nil
}
