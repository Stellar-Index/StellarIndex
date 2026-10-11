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

// TokenSupplyReader is the seam GET /v1/assets/{asset_id}/supply reads through: the ClickHouse
// supply_flows lake, whose decoded per-event mint/burn/clawback amounts make supply a live SQL sum
// with no rollup. Nil disables the endpoint (503). Distinct from [SupplyLooker] (circulating-vs-max
// policy): this is the raw on-chain sum(mint)-sum(burn)-sum(clawback) for EVERY token.
type TokenSupplyReader interface {
	TokenSupply(ctx context.Context, contractID string) (clickhouse.TokenSupply, error)
	NativeTotalCoins(ctx context.Context) (totalCoins int64, ledger uint32, err error)
}

// isNativeSupplyAlias reports whether a raw {asset_id} path segment names
// XLM in any of its canonical spellings.
//
// It parses rather than string-matching: a literal comparison is case-SENSITIVE, so
// /v1/assets/xlm/supply would 404 while every other asset route accepts `xlm`, and a client reads
// that 404 as "no supply data".
//
// The XLM SAC contract address is deliberately NOT routed here even though canonical.AssetAliases
// lists it: the SAC's supply is the amount currently WRAPPED, a different quantity from the ledger
// header's total_coins, and is served by the contract branch below.
func isNativeSupplyAlias(assetID string) bool {
	parsed, err := canonical.ParseAsset(assetID)
	if err != nil {
		return false
	}
	return parsed.Type == canonical.AssetNative ||
		(parsed.Type == canonical.AssetCrypto && strings.EqualFold(parsed.Code, "XLM"))
}

// AssetSupply is the wire response for GET /v1/assets/{asset_id}/supply.
// Amounts are decimal strings in the asset's smallest unit (ADR-0003: an i128
// is never a JSON number); the client applies per-asset decimals for display.
type AssetSupply struct {
	AssetID       string  `json:"asset_id"`
	ContractID    string  `json:"contract_id,omitempty"`
	TotalSupply   string  `json:"total_supply"`
	MintTotal     *string `json:"mint_total,omitempty"`
	BurnTotal     *string `json:"burn_total,omitempty"`
	ClawbackTotal *string `json:"clawback_total,omitempty"`
	FlowCount     uint64  `json:"flow_count"`
	// Source is how TotalSupply was derived: "mint_burn_flows" (from supply_flows),
	// "ledger_total_coins" (XLM, from the ledger header, as it has no SAC mint/burn events), or
	// "contract_storage_balances" (sum of the token's own contract-storage balances, a DIFFERENT
	// BASIS, see supply.BasisContractStorageBalances, served only for tokens emitting no supply
	// events).
	Source string `json:"source"`

	// SupplyBasis names the reading behind a "mint_burn_flows" TotalSupply, in
	// the supply.Basis vocabulary the listings use. A classic asset's figure
	// is classic_lake_flows: its derived SAC's event net, not a figure the
	// issuer publishes, and an upper reading rather than a certified one.
	SupplyBasis string `json:"supply_basis,omitempty"`

	// CirculatingSupplyLowerBound is true when TotalSupply is a PROVABLE FLOOR, not the figure
	// itself, and must not be presented as an exact supply.
	//
	// Set for "contract_storage_balances": that reading sees only entries the current-state
	// projection captured, so one dormant since before coverage began is absent. Where the contract
	// publishes a holder count, SupplyConsistent disagreeing is what a missing entry looks like.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`

	// BalanceEntries is how many per-holder balance entries were summed.
	// Only set for "contract_storage_balances".
	BalanceEntries int `json:"balance_entries,omitempty"`

	// ArchivedBalanceEntries and ArchivedBalanceTotal are the part of
	// BalanceEntries and TotalSupply held in persistent entries whose TTL had
	// lapsed at the lake tip: archived, still owned and restorable, so counted.
	// Only set for "contract_storage_balances", and only when non-zero.
	ArchivedBalanceEntries int     `json:"archived_balance_entries,omitempty"`
	ArchivedBalanceTotal   *string `json:"archived_balance_total,omitempty"`

	// SupplyConsistent, when non-nil, reports whether the contract's own `TotalSupply` and
	// `HolderCount` agreed with what we summed and saw. Only set for "contract_storage_balances";
	// nil means no cross-checks were offered, which is not a failed one.
	SupplyConsistent *bool `json:"supply_consistent,omitempty"`

	// Decimals is the scale the CONTRACT ITSELF declares, set only for "contract_storage_balances".
	// Omitted when the chain declares none, in which case a consumer MUST NOT invent one.
	Decimals *uint32 `json:"decimals,omitempty"`
	// AsOfLedger is the lake watermark this read is fresh to: the highest ledger captured in the
	// ClickHouse lake at serve time (for native, the ledger the total_coins row came from). Omitted
	// without a watermark reader. Pairs with `flags.stale`, set when its close time trails now
	// beyond the staleness threshold.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

// handleAssetSupply serves GET /v1/assets/{asset_id}/supply — a token's live
// supply from the decode-at-ingest supply_flows lake. The literal "supply"
// segment takes precedence over the {asset_id}/{network} wildcard route.
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

	// Per-request DB ceiling: the supply_flows sum and native header read share the explorer pool,
	// so a bounded context releases the connection on a slow scan. 8s matches the sibling raw-scan
	// endpoints and fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// XLM: supply is the ledger header's total_coins (XLM is not minted/burned
	// via SAC mint/burn events, so it has no supply_flows). Handle every alias.
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
			// The native read already carries its exact source ledger —
			// more precise than the cached watermark, and free.
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
		// A negative net total (burn+clawback > mint) means supply_flows is incompletely seeded
		// (e.g. pre-Soroban SAC-wrapper mints not yet replayed), NOT that supply is negative.
		// TotalSupply is a non-nullable wire string, so REFUSE the snapshot (404 "Supply not
		// available") rather than publish an impossible negative; total_supply >= 0 is also enforced
		// in the schema. Seed the baseline with `stellarindex-ops supply seed-sep41-genesis`.
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-incomplete",
			"Supply not available", http.StatusNotFound,
			"This token's on-chain supply flows are incompletely seeded (recorded burns exceed recorded mints), so a total supply isn't available for it yet.")
		return
	}
	// A token with NO flows is the one case the event log cannot speak to: supply_flows scans an
	// unseen contract to zeros, and zero is a claim ("fully burned"), not an absence. Ask the
	// contract's own storage; if it declines too, there is nothing to publish.
	//
	// Gated on FlowCount == 0, not a magnitude comparison, so the two readings never both feed one
	// figure: they measure the same tokens from opposite ends and summing would double-count.
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

// ContractStorageSupplyReader is the seam the storage-derived supply reading is
// read through. Declared HERE rather than added to [TokenSupplyReader] so every
// existing stub implementing that interface keeps compiling, and so a
// deployment can wire one source without the other.
type ContractStorageSupplyReader interface {
	ContractStorageSupply(ctx context.Context, contractID string) (clickhouse.ContractStorageSupply, error)
}

// storageSupplyResponse builds the storage-derived answer for a token the event log has nothing to
// say about. ok=false means no defensible answer and the caller answers 404, since a flowless
// token's event reading is an unfounded zero.
//
// Every refusal below is silent to the client by design: a fallback that turned a 200 into a 502
// because its own optional source declined would be worse than the gap it closes.
func (s *Server) storageSupplyResponse(ctx context.Context, assetID, contractID string) (AssetSupply, bool, bool) {
	if s.ContractStorageSupply == nil {
		return AssetSupply{}, false, false
	}
	st, err := s.ContractStorageSupply.ContractStorageSupply(ctx, contractID)
	if err != nil {
		// Both refusals (a SAC, an oversized holder set) and genuine read
		// errors land here. A SAC is the expected case — every classic asset
		// routed through resolveSupplyContractID derives one — so this is
		// logged at debug volume, not warned.
		s.logger.Debug("supply: contract storage fallback declined",
			"contract_id", contractID, "err", err)
		return AssetSupply{}, false, false
	}
	if st.BalanceEntries == 0 || st.Total == nil || st.Total.Sign() == 0 {
		// No balances is not a supply of zero — it is the absence of a
		// reading. Publishing it would replace one unfounded zero with
		// another.
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
	// The same watermark stamps as_of_ledger and decides flags.stale, as on
	// the mint/burn arm: a reading fresh to the lake tip is fresh to that
	// ledger, not to the last ledger any one balance entry happened to move.
	wmLedger, stale, _ := s.lakeWatermark(ctx)
	resp.AsOfLedger = wmLedger
	return resp, stale, true
}

// resolveSupplyContractID maps an asset_id to the contract_id supply_flows is keyed by: a Soroban
// C-strkey is itself; a classic asset ("CODE-ISSUER") resolves to its SAC by deterministic
// derivation, a pure function of (asset, passphrase) valid even before deployment. sac_wrappers is
// deliberately NOT consulted: an entry can only equal the derived SAC or point supply at another
// contract. Only unparseable ids and shapes with no SAC (fiat:*) fail to resolve.
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
