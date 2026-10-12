package v1

import (
	"context"
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
	// (the token's own storage balances, a DIFFERENT BASIS, served only for tokens emitting no
	// supply events).
	Source string `json:"source"`

	// SupplyBasis names the reading behind a "mint_burn_flows" TotalSupply in the supply.Basis
	// vocabulary. A classic asset's classic_lake_flows is its derived SAC's event net: an upper
	// reading, not a figure the issuer publishes.
	SupplyBasis string `json:"supply_basis,omitempty"`

	// CirculatingSupplyLowerBound is true when TotalSupply is a PROVABLE FLOOR and must not be
	// presented as exact. Set for "contract_storage_balances": that reading sees only entries the
	// current-state projection captured, so one dormant since before coverage is absent.
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
	// A token with NO flows is the one case the event log cannot speak to: an unseen contract scans
	// to zeros, and zero claims "fully burned" rather than absence. Ask the contract's storage; if
	// it declines too, publish nothing. Gated on FlowCount == 0, not a magnitude comparison, so the
	// two readings never feed one figure (they measure from opposite ends; summing double-counts).
	if sup.FlowCount == 0 {
		if resp, storageStale, ok := s.storageSupplyResponse(ctx, assetID, contractID); ok {
			writeJSON(w, resp, Flags{Stale: storageStale})
			return
		}
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-incomplete",
			"Supply not available", http.StatusNotFound,
			"No supply flows are recorded for this contract and its storage offers no balance reading, so a total supply isn't available for it.")
		return
	}

	basis := supply.BasisSEP41LakeFlows
	if !canonical.IsContractID(assetID) {
		basis = supply.BasisClassicLakeFlows
	}
	mint, burn, clawback := sup.Mint.String(), sup.Burn.String(), sup.Clawback.String()
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	writeJSON(w, AssetSupply{
		AssetID:       assetID,
		ContractID:    contractID,
		TotalSupply:   sup.Total.String(),
		MintTotal:     &mint,
		BurnTotal:     &burn,
		ClawbackTotal: &clawback,
		FlowCount:     sup.FlowCount,
		Source:        "mint_burn_flows",
		SupplyBasis:   basis.String(),
		AsOfLedger:    wmLedger,
	}, Flags{Stale: stale})
}

// ContractStorageSupplyReader is the seam for the storage-derived supply reading, separate from [TokenSupplyReader] so either can be wired alone.
type ContractStorageSupplyReader interface {
	ContractStorageSupply(ctx context.Context, contractID string) (clickhouse.ContractStorageSupply, error)
}

// storageSupplyResponse builds the storage-derived answer for a token the event log cannot speak to; ok=false means no
// defensible answer (404), since a flowless token's event reading is an unfounded zero. Refusals are silent: an optional
// fallback must not turn a 200 into a 502.
func (s *Server) storageSupplyResponse(ctx context.Context, assetID, contractID string) (AssetSupply, bool, bool) {
	if s.ContractStorageSupply == nil {
		return AssetSupply{}, false, false
	}
	st, err := s.ContractStorageSupply.ContractStorageSupply(ctx, contractID)
	if err != nil {
		// Refusals (a SAC, an oversized holder set) and read errors land here. A SAC is the expected
		// case for every classic asset, so log at debug, not warn.
		s.logger.Debug("supply: contract storage fallback declined",
			"contract_id", contractID, "err", err)
		return AssetSupply{}, false, false
	}
	if st.BalanceEntries == 0 || st.Total == nil || st.Total.Sign() == 0 {
		// No balances is the absence of a reading, not a supply of zero.
		return AssetSupply{}, false, false
	}

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
	return resp, stale, true
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
