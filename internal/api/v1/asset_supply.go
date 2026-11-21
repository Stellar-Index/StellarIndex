package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TokenSupplyReader is the seam for GET /v1/assets/{asset_id}/supply: a live ClickHouse supply_flows sum of mint/burn/clawback; nil disables the endpoint (503).
type TokenSupplyReader interface {
	TokenSupply(ctx context.Context, contractID string) (clickhouse.TokenSupply, error)
	NativeTotalCoins(ctx context.Context) (totalCoins int64, ledger uint32, err error)
}

// isNativeSupplyAlias reports whether {asset_id} names XLM in any canonical spelling (parsed, since a literal compare is
// case-sensitive and /v1/assets/xlm/supply would 404). The XLM SAC is deliberately NOT routed here: its supply is the amount
// WRAPPED, not the header's total_coins.
func isNativeSupplyAlias(assetID string) bool {
	parsed, err := canonical.ParseAsset(assetID)
	if err != nil {
		return false
	}
	return parsed.Type == canonical.AssetNative ||
		(parsed.Type == canonical.AssetCrypto && strings.EqualFold(parsed.Code, "XLM"))
}

// AssetSupply is the GET /v1/assets/{asset_id}/supply response; amounts are smallest-unit decimal strings, never JSON numbers.
type AssetSupply struct {
	AssetID       string  `json:"asset_id"`
	ContractID    string  `json:"contract_id,omitempty"`
	TotalSupply   string  `json:"total_supply"`
	MintTotal     *string `json:"mint_total,omitempty"`
	BurnTotal     *string `json:"burn_total,omitempty"`
	ClawbackTotal *string `json:"clawback_total,omitempty"`
	FlowCount     uint64  `json:"flow_count"`
	// Source is how TotalSupply was derived: "mint_burn_flows" (supply_flows), "ledger_total_coins"
	// (XLM, from the ledger header; it has no SAC mint/burn events), or "contract_storage_balances"
	// (the token's own storage balances, a DIFFERENT BASIS, served when the event log is empty or
	// its net is below the storage level).
	Source string `json:"source"`

	// SupplyBasis names the reading behind a "mint_burn_flows" TotalSupply in the supply.Basis
	// vocabulary. A classic asset's classic_lake_flows is its derived SAC's event net: an upper
	// reading, not a figure the issuer publishes.
	SupplyBasis string `json:"supply_basis,omitempty"`

	// CirculatingSupplyLowerBound is true when TotalSupply is a PROVABLE FLOOR and must not be
	// presented as exact. Set for "contract_storage_balances": that reading sees only entries the
	// current-state projection captured, so one dormant since before coverage is absent. Set for
	// "mint_burn_flows" when storage holds a balance entry the reader cannot decode, so the check
	// that would prove the event net short could not run.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`

	// BalanceEntries is how many per-holder entries were summed ("contract_storage_balances" only).
	BalanceEntries int `json:"balance_entries,omitempty"`

	// ArchivedBalanceEntries and ArchivedBalanceTotal are the part of BalanceEntries and TotalSupply
	// in persistent entries whose TTL lapsed at the lake tip: still owned and restorable, so
	// counted. Set only for "contract_storage_balances", and only when non-zero.
	ArchivedBalanceEntries int     `json:"archived_balance_entries,omitempty"`
	ArchivedBalanceTotal   *string `json:"archived_balance_total,omitempty"`

	// SupplyConsistent, when non-nil, reports whether the contract's own TotalSupply and
	// HolderCount agreed with what we summed. Nil means no cross-check was offered, not a failed one.
	SupplyConsistent *bool `json:"supply_consistent,omitempty"`

	// Decimals is the scale the CONTRACT ITSELF declares ("contract_storage_balances" only). When
	// omitted the chain declares none and a consumer MUST NOT invent one.
	Decimals *uint32 `json:"decimals,omitempty"`
	// AsOfLedger is the lake watermark this read is fresh to (for native, the ledger the
	// total_coins row came from). Omitted without a watermark reader; pairs with flags.stale.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

// handleAssetSupply serves GET /v1/assets/{asset_id}/supply from the supply_flows lake. The literal
// "supply" segment takes precedence over the {asset_id}/{network} wildcard route.
func (s *Server) handleAssetSupply(w http.ResponseWriter, r *http.Request) {
	if s.TokenSupply == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/supply-unavailable",
			"Supply unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the ClickHouse supply reader yet.")
		return
	}
	assetID := r.PathValue("asset_id")
	if assetID == "" {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-asset",
			"Invalid asset", http.StatusBadRequest, "asset_id path segment is required.")
		return
	}

	// Per-request DB ceiling: the reads share the explorer pool, so a bounded context releases the
	// connection on a slow scan. 8s fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	// The supply caches report a reading their refreshes have not replaced, so it is not served as current.
	ctx, reads := withReadReport(ctx)

	// XLM supply is the ledger header's total_coins (no SAC mint/burn events); handle every alias.
	if isNativeSupplyAlias(assetID) {
		coins, ledger, err := s.TokenSupply.NativeTotalCoins(ctx)
		if err != nil {
			s.logger.Warn("supply: native total_coins", "err", err)
			writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
				"Supply read failed", http.StatusBadGateway, "Could not read native supply.")
			return
		}
		_, stale, _ := s.lakeWatermark(ctx)
		writeJSON(w, AssetSupply{
			AssetID:     assetID,
			TotalSupply: strconv.FormatInt(coins, 10),
			Source:      "ledger_total_coins",
			// The native read carries its exact source ledger, more precise than the watermark.
			AsOfLedger: ledger,
		}, Flags{Stale: stale})
		return
	}

	contractID, ok := s.resolveSupplyContractID(assetID)
	if !ok {
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-not-mapped",
			"Supply not available", http.StatusNotFound,
			"Supply is keyed by contract: Soroban tokens (C…) resolve directly and classic assets (CODE-ISSUER) derive their Stellar-Asset-Contract; this id has no contract (fiat:* and malformed ids do not).")
		return
	}

	sup, err := s.TokenSupply.TokenSupply(ctx, contractID)
	if err != nil {
		s.logger.Warn("supply: token supply", "contract_id", contractID, "err", err)
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
			"Supply read failed", http.StatusBadGateway, "Could not read token supply.")
		return
	}
	if sup.Incomplete {
		// A negative net (burn+clawback > mint) means supply_flows is incompletely seeded (e.g.
		// pre-Soroban SAC-wrapper mints not replayed), not that supply is negative. TotalSupply is
		// a non-nullable string, so refuse with 404 rather than publish an impossible negative.
		// Seed with `stellarindex-ops supply seed-sep41-genesis`.
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-incomplete",
			"Supply not available", http.StatusNotFound,
			"This token's on-chain supply flows are incompletely seeded (recorded burns exceed recorded mints), so a total supply isn't available for it yet.")
		return
	}
	// A classic asset's SAC keeps balances in trustlines, so its storage is only asked when the log is empty.
	verdict := storageNoVerdict
	if sup.FlowCount == 0 || canonical.IsContractID(assetID) {
		var st clickhouse.ContractStorageSupply
		st, verdict, err = s.storageReadingSupersedes(ctx, contractID, sup)
		if err != nil {
			writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
				"Supply read failed", http.StatusBadGateway,
				"Could not read the token's contract storage, so its event-log total cannot be shown complete.")
			return
		}
		if verdict == storageSupersedes {
			resp, storageStale := s.storageSupplyResponse(ctx, assetID, contractID, st)
			writeJSON(w, resp, Flags{Stale: storageStale || reads.stale.Load()})
			return
		}
	}
	// A spent budget may have cut the storage read short, so the event net is not shown complete.
	if err := ctx.Err(); err != nil {
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
			"Supply read failed", http.StatusBadGateway, "Could not read token supply.")
		return
	}
	if sup.FlowCount == 0 {
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-incomplete",
			"Supply not available", http.StatusNotFound,
			"No supply flows are recorded for this contract and its storage offers no balance reading, so a total supply isn't available for it.")
		return
	}

	resp, stale := s.flowsSupplyResponse(ctx, assetID, contractID, sup, verdict)
	writeJSON(w, resp, Flags{Stale: stale || reads.stale.Load()})
}

// flowsSupplyResponse renders the event net when storage did not supersede it; stale is the lake watermark's verdict.
func (s *Server) flowsSupplyResponse(
	ctx context.Context, assetID, contractID string, sup clickhouse.TokenSupply, verdict storageVerdict,
) (AssetSupply, bool) {
	basis := supply.BasisSEP41LakeFlows
	if !canonical.IsContractID(assetID) {
		basis = supply.BasisClassicLakeFlows
	}
	mint, burn, clawback := sup.Mint.String(), sup.Burn.String(), sup.Clawback.String()
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	return AssetSupply{
		AssetID:       assetID,
		ContractID:    contractID,
		TotalSupply:   sup.Total.String(),
		MintTotal:     &mint,
		BurnTotal:     &burn,
		ClawbackTotal: &clawback,
		FlowCount:     sup.FlowCount,
		Source:        "mint_burn_flows",
		SupplyBasis:   basis.String(),
		// Storage the reader could not sum cannot prove the net short, so the net is only a floor.
		CirculatingSupplyLowerBound: verdict == storageUnchecked,
		AsOfLedger:                  wmLedger,
	}, stale
}

// ContractStorageSupplyReader is the seam for the storage-derived supply reading, separate from [TokenSupplyReader] so either can be wired alone.
type ContractStorageSupplyReader interface {
	ContractStorageSupply(ctx context.Context, contractID string) (clickhouse.ContractStorageSupply, error)
}

// storageVerdict is what a contract's storage says about its event net.
type storageVerdict int

const (
	// storageNoVerdict: no storage reading by design, or one that does not exceed the net; the net stands.
	storageNoVerdict storageVerdict = iota
	// storageSupersedes: the storage level replaces the net.
	storageSupersedes
	// storageUnchecked: the lake state stopped the check that would prove the net short ([storageSupplyUnchecked]), or a level
	// above the net failed its own cross-check, so a net served beside it is a floor. A fixed lake property, so not unread.
	storageUnchecked
)

// storageReadingSupersedes is the one event-vs-storage choice for a contract's supply: the storage level wins for a flowless
// log (whose zero is unfounded), and otherwise only when it exceeds the event net. Storage is a floor, so a level above the
// net proves the log partial and is the tighter floor, unless it fails the contract's own cross-check. The two are never
// summed: they count the same tokens. A failed read is always an error: with flows it may have proved the net partial, and
// without flows it is the only reading there is, so its absence is unread, not "no figure".
func (s *Server) storageReadingSupersedes(
	ctx context.Context, contractID string, sup clickhouse.TokenSupply,
) (clickhouse.ContractStorageSupply, storageVerdict, error) {
	st, ok, err := s.contractStorageReading(ctx, contractID)
	if storageSupplyUnchecked(err) {
		return clickhouse.ContractStorageSupply{}, storageUnchecked, nil
	}
	if err != nil {
		return clickhouse.ContractStorageSupply{}, storageNoVerdict, err
	}
	if !ok {
		return clickhouse.ContractStorageSupply{}, storageNoVerdict, nil
	}
	if sup.FlowCount > 0 && sup.Total != nil {
		if st.Total.Cmp(sup.Total) <= 0 {
			return clickhouse.ContractStorageSupply{}, storageNoVerdict, nil
		}
		// A level above the net that fails its own cross-check neither proves the log short nor clears it.
		if st.HasSelfChecks() && !st.SelfConsistent() {
			return clickhouse.ContractStorageSupply{}, storageUnchecked, nil
		}
	}
	return st, storageSupersedes, nil
}

// contractStorageReading is the contract's storage level. ok=false with a nil error is a decline by design; an error is a read
// that did not answer, or a check the lake state left unrun ([storageSupplyUnchecked]), which [Server.storageReadingSupersedes] owns.
func (s *Server) contractStorageReading(ctx context.Context, contractID string) (clickhouse.ContractStorageSupply, bool, error) {
	if s.ContractStorageSupply == nil {
		return clickhouse.ContractStorageSupply{}, false, nil
	}
	st, err := s.ContractStorageSupply.ContractStorageSupply(ctx, contractID)
	if err != nil {
		if storageSupplyDeclined(err) {
			// A SAC is the expected case for every classic asset, so log at debug, not warn.
			s.logger.Debug("supply: contract storage reading declined",
				"contract_id", contractID, "err", err)
			return clickhouse.ContractStorageSupply{}, false, nil
		}
		if storageSupplyUnchecked(err) {
			s.logger.Debug("supply: contract storage check could not run",
				"contract_id", contractID, "err", err)
			return clickhouse.ContractStorageSupply{}, false, err
		}
		if ctx.Err() == nil {
			s.logger.Warn("supply: contract storage reading failed", "contract_id", contractID, "err", err)
		}
		return clickhouse.ContractStorageSupply{}, false, err
	}
	if st.BalanceEntries == 0 || st.Total == nil || st.Total.Sign() == 0 {
		// No balances is the absence of a reading, not a supply of zero.
		return clickhouse.ContractStorageSupply{}, false, nil
	}
	return st, true, nil
}

// storageSupplyDeclined reports the reader's one verdict-free refusal: a SAC, whose storage holds only the wrapped slice of a
// classic asset and so has nothing to add to a supply.
func storageSupplyDeclined(err error) bool {
	return errors.Is(err, clickhouse.ErrStorageSupplyIsStellarAsset)
}

// storageSupplyUnchecked reports a storage read the lake state stopped short of a sum: an undecodable entry, a holder set past
// the reader's cap, or no instance entry to rule out a SAC. Storage could still prove the net short, so a net beside it is a floor.
func storageSupplyUnchecked(err error) bool {
	return errors.Is(err, clickhouse.ErrStorageSupplyUndecodable) ||
		errors.Is(err, clickhouse.ErrStorageSupplyTooManyEntries) ||
		errors.Is(err, clickhouse.ErrStorageSupplyNoInstance)
}

// storageSupplyResponse renders a storage reading [Server.storageReadingSupersedes] chose; stale is the lake watermark's verdict.
func (s *Server) storageSupplyResponse(
	ctx context.Context, assetID, contractID string, st clickhouse.ContractStorageSupply,
) (AssetSupply, bool) {
	resp := AssetSupply{
		AssetID:                     assetID,
		ContractID:                  contractID,
		TotalSupply:                 st.Total.String(),
		Source:                      string(supply.BasisContractStorageBalances),
		CirculatingSupplyLowerBound: true,
		BalanceEntries:              st.BalanceEntries,
	}
	if st.ArchivedEntries > 0 && st.ArchivedTotal != nil {
		archived := st.ArchivedTotal.String()
		resp.ArchivedBalanceEntries, resp.ArchivedBalanceTotal = st.ArchivedEntries, &archived
	}
	if st.HasSelfChecks() {
		consistent := st.SelfConsistent()
		resp.SupplyConsistent = &consistent
	}
	if st.DecimalsFound {
		d := st.Decimals
		resp.Decimals = &d
	}
	// The lake watermark stamps as_of_ledger and decides flags.stale, as on the mint/burn arm.
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	resp.AsOfLedger = wmLedger
	return resp, stale
}

// resolveSupplyContractID maps an asset_id to the contract_id supply_flows is keyed by: a C-strkey is itself, a classic asset
// derives its SAC (valid before deployment). sac_wrappers is deliberately NOT consulted.
func (s *Server) resolveSupplyContractID(assetID string) (string, bool) {
	if canonical.IsContractID(assetID) {
		return assetID, true
	}
	parsed, err := canonical.ParseAsset(assetID)
	if err != nil || parsed.Type != canonical.AssetClassic {
		return "", false
	}
	sac, err := parsed.SacContractID()
	if err != nil {
		return "", false
	}
	return sac, true
}
