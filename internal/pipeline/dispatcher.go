// Package pipeline holds the ingest glue shared by the indexer and `stellarindex-ops backfill`: BuildDispatcher, ProcessLedger, PersistEvents, LedgerstreamConfig.
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

// BuildDispatcher registers a decoder per name in names per its SourceSpec, erroring on unknown names or a missing oracle contract ID; sep41 is never enabled by name.
func BuildDispatcher(names []string, oracle config.OracleConfig, gated map[string][]contractid.Option, soroswapOpts ...soroswap.DecoderOption) (*dispatcher.Dispatcher, error) {
	// Install the oracle-staleness policy BEFORE any decoder is built so the first persisted update publishes the right budget.
	// Overrides are keyed by (source, asset), so one naming a source this replica does not run is inert; per-source defaults come from each spec's OnDispatch.
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

// oracleStalenessOverrides translates `[[oracle.staleness_overrides]]` rows into obs's shape, keeping internal/obs free of an internal/config import.
// config.Validate has already rejected unknown sources, non-canonical assets, non-positive budgets and duplicates.
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

// AccountObserverWatchSet is the accounts observer's watch set (SDF reserve accounts plus metadata-watched issuers, deduplicated); empty means unregistered.
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

// RegisterSupplyEntryDecoders attaches the opt-in LCM supply observers (an empty watched set leaves one unregistered) and returns their names.
// Accounts use AccountObserverWatchSet (readers query by account id, so issuers do not leak into the reserve sum); persistence is the sink.go type-switch;
// call RegisterSupplyEventDecoders too for the sep41_supply event observer.
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
