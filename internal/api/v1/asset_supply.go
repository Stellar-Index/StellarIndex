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

// TokenSupplyReader is the seam GET /v1/assets/{asset_id}/supply reads through:
// the ClickHouse supply_flows lake (decode-at-ingest, ADR-0034), which carries
// the decoded mint/burn/clawback amount per event so per-token supply is a live
// SQL sum with no rollup refresh. *clickhouse.SupplyReader satisfies it. Nil
// disables the endpoint (503). Distinct from the F2 [SupplyLooker]
// (asset_supply_history / circulating-vs-max policy, ADR-0011) — this is the
// raw on-chain Σmint−Σburn−Σclawback total for EVERY token.
type TokenSupplyReader interface {
	TokenSupply(ctx context.Context, contractID string) (clickhouse.TokenSupply, error)
	NativeTotalCoins(ctx context.Context) (totalCoins int64, ledger uint32, err error)
}

// isNativeSupplyAlias reports whether a raw {asset_id} path segment names
// XLM in any of its canonical spellings.
//
// It parses rather than string-matching. The literal comparison this
// replaced (`assetID == "native" || "XLM" || "crypto:XLM"`) was
// case-SENSITIVE, so /v1/assets/xlm/supply 404'd while
// /v1/assets/XLM/supply returned 200 — even though canonical.ParseAsset
// is case-insensitive and every other asset route on the server accepts
// `xlm`. A 404 on a legitimate spelling reads to a client as "this asset
// has no supply data", not "try different capitalisation" (cold audit
// 2026-08-04).
//
// The XLM SAC contract address is deliberately NOT routed here even
// though canonical.AssetAliases lists it: the SAC's token supply is how
// much XLM is currently WRAPPED, a genuinely different quantity from the
// ledger header's total_coins, and it is served by the contract branch
// below.
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
	// Source is how TotalSupply was derived: "mint_burn_flows" (Σmint−Σburn−
	// Σclawback from supply_flows), "ledger_total_coins" (XLM, from the ledger
	// header — XLM has no SAC mint/burn events), or
	// "contract_storage_balances" (Σ of the per-holder balance entries in the
	// token's own Soroban contract storage — a DIFFERENT BASIS, see
	// supply.BasisContractStorageBalances, served only for tokens that emit no
	// supply events at all).
	Source string `json:"source"`

	// CirculatingSupplyLowerBound is true when TotalSupply is a PROVABLE FLOOR
	// rather than the figure itself, and the consumer must not present it as an
	// exact supply.
	//
	// It is set for "contract_storage_balances": that reading sees only
	// balances that exist as ledger entries right now, and Soroban state expiry
	// archives contract-data entries, so a real and restorable balance can be
	// invisible to it. Where the contract publishes its own holder count,
	// SupplyConsistent reports whether the two agreed — a disagreement is what
	// an archived balance looks like from here.
	CirculatingSupplyLowerBound bool `json:"circulating_supply_lower_bound,omitempty"`

	// BalanceEntries is how many per-holder balance entries were summed.
	// Only set for "contract_storage_balances".
	BalanceEntries int `json:"balance_entries,omitempty"`

	// SupplyConsistent, when non-nil, reports whether every cross-check the
	// contract itself published agreed with what we summed — its own
	// `TotalSupply` against our sum, and its own `HolderCount` against the
	// number of entries we could see. Only set for
	// "contract_storage_balances"; nil means the contract offered no
	// cross-checks, which is not the same as a failed one.
	SupplyConsistent *bool `json:"supply_consistent,omitempty"`

	// Decimals is the scale the CONTRACT ITSELF declares, when it declares one.
	// Only set for "contract_storage_balances", where it is the difference
	// between a decimalised figure resting on our own measurement and one
	// resting on a borrowed exponent. Omitted when the chain declares no scale,
	// in which case a consumer MUST NOT invent one.
	Decimals *uint32 `json:"decimals,omitempty"`
	// AsOfLedger is the lake watermark this read is fresh to (ADR-0041
	// Decision 4): the highest ledger captured in the ClickHouse lake at
	// serve time (for native, the exact ledger the total_coins row came
	// from). Omitted when no watermark reader is wired. Pairs with
	// `flags.stale`, which fires when the watermark's close time trails
	// now beyond the staleness threshold.
	AsOfLedger uint32 `json:"as_of_ledger,omitempty"`
}

// handleAssetSupply serves GET /v1/assets/{asset_id}/supply — a token's live
// supply from the decode-at-ingest supply_flows lake. The literal "supply"
// segment takes precedence over the {asset_id}/{network} wildcard route.
func (s *Server) handleAssetSupply(w http.ResponseWriter, r *http.Request) {
	if s.tokenSupply == nil {
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

	// Per-request DB ceiling (P1, audit-2026-07-16): the ClickHouse
	// supply_flows sum (and the native ledger-header read) run against
	// the shared explorer pool; a bounded context releases the pool
	// connection on a slow scan. 8s matches the sibling raw-scan
	// endpoints and fires before the blanket request-timeout middleware.
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	// XLM: supply is the ledger header's total_coins (XLM is not minted/burned
	// via SAC mint/burn events, so it has no supply_flows). Handle every alias.
	if isNativeSupplyAlias(assetID) {
		coins, ledger, err := s.tokenSupply.NativeTotalCoins(ctx)
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

	sup, err := s.tokenSupply.TokenSupply(ctx, contractID)
	if err != nil {
		s.logger.Warn("supply: token supply", "contract_id", contractID, "err", err)
		writeProblemErr(w, r, err, "https://api.stellarindex.io/errors/supply-error",
			"Supply read failed", http.StatusBadGateway, "Could not read token supply.")
		return
	}
	if sup.Incomplete {
		// A negative lake-flows net total (Σ(burn+clawback) > Σmint) means
		// this token's supply_flows are incompletely seeded — e.g. pre-Soroban
		// SAC-wrapper mints not yet CAP-67-replayed — NOT that supply is
		// negative. TotalSupply is a non-nullable wire string, so we can't
		// omit it; REFUSE to serve the snapshot (the endpoint's existing 404
		// "Supply not available" path) rather than publish a physically-
		// impossible negative (ADR-0003; migration 0005 enforces
		// total_supply >= 0). Mirrors SEP41Computer.Compute refusing a
		// negative total. Seed the pre-Soroban baseline with
		// `stellarindex-ops supply seed-sep41-genesis` to make it available.
		writeProblem(w, r, "https://api.stellarindex.io/errors/supply-incomplete",
			"Supply not available", http.StatusNotFound,
			"This token's on-chain supply flows are incompletely seeded (recorded burns exceed recorded mints), so a total supply isn't available for it yet.")
		return
	}
	// A token with NO flows at all is the one case the event log cannot speak
	// to: supply_flows scans a contract it has never seen to zeros, and zero is
	// a claim ("fully burned") rather than an absence of one. Before publishing
	// that claim, ask the contract's own storage.
	//
	// Gated on FlowCount == 0 rather than on a magnitude comparison, so the two
	// readings can never both contribute to one figure — they measure the same
	// tokens from opposite ends (issuance vs distribution) and summing them
	// would double-count every holder.
	if sup.FlowCount == 0 {
		if resp, storageStale, ok := s.storageSupplyResponse(ctx, assetID, contractID); ok {
			writeJSON(w, resp, Flags{Stale: storageStale})
			return
		}
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

// storageSupplyResponse builds the storage-derived answer for a token the event
// log has nothing to say about. ok=false means "no defensible answer" and the
// caller falls back to the event reading (which, for a token with no flows, is
// the zero it has always published).
//
// Every refusal below is silent to the client by design: this path is a
// fallback, and a fallback that turns a 200 into a 502 because its own optional
// source declined would be worse than the gap it closes.
func (s *Server) storageSupplyResponse(ctx context.Context, assetID, contractID string) (AssetSupply, bool, bool) {
	if s.storageSupply == nil {
		return AssetSupply{}, false, false
	}
	st, err := s.storageSupply.ContractStorageSupply(ctx, contractID)
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
		AsOfLedger:                  st.AsOfLedger,
	}
	if st.HasSelfChecks() {
		consistent := st.SelfConsistent()
		resp.SupplyConsistent = &consistent
	}
	if st.DecimalsFound {
		d := st.Decimals
		resp.Decimals = &d
	}
	_, stale, _ := s.lakeWatermark(ctx)
	return resp, stale, true
}

// resolveSupplyContractID maps an asset_id to the contract_id supply_flows is
// keyed by: a Soroban C-strkey is itself; a classic asset ("CODE-ISSUER") is
// resolved to its Stellar-Asset-Contract. The operator's sac_wrappers map is
// consulted first (an explicit override / fast path); any OTHER classic asset
// falls through to deterministic SAC derivation — the SAC address is a pure
// function of (asset, pubnet passphrase), valid even before the SAC is
// deployed (canonical.Asset.SacContractID, board #40). Only unparseable ids
// and shapes with no SAC (fiat:*) fail to resolve.
func (s *Server) resolveSupplyContractID(assetID string) (string, bool) {
	if canonical.IsContractID(assetID) {
		return assetID, true
	}
	for sac, assetKey := range s.sacWrappers {
		if assetKey == assetID {
			return sac, true
		}
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
