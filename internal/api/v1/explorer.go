package v1

import (
	"log/slog"
	"net/http"
	"strconv"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
)

// The network-explorer endpoint implementations (ADR-0038) live in
// internal/api/v1/explorer (maintainability audit 2026-07-01, D1
// finding M1-7). This file only wires that package's *Handler into
// Server (see server.go's Handler construction + mountRoutes) and
// keeps type aliases for every type explorer_*.go used to export
// directly from package v1, so the existing (pre-extraction)
// explorer_*_test.go files keep compiling completely unchanged —
// they still write `v1.LedgersListView` etc.
//
// ExplorerReader is also the read seam behind a few v1 handlers
// OUTSIDE the explorer package (asset SAC resolution in assets.go,
// lending TVL, liquidity-pool + pool-reserve reads) — those keep
// referring to the bare `ExplorerReader` name in package v1
// unchanged via this alias.
type ExplorerReader = explorerpkg.ExplorerReader

type (
	AccountsListView         = explorerpkg.AccountsListView
	AccountWealthRow         = explorerpkg.AccountWealthRow
	AccountStateView         = explorerpkg.AccountStateView
	AccountThresholds        = explorerpkg.AccountThresholds
	AccountSignerV           = explorerpkg.AccountSignerV
	TrustlineV               = explorerpkg.TrustlineV
	OfferV                   = explorerpkg.OfferV
	AssetHoldersView         = explorerpkg.AssetHoldersView
	AssetHolderV             = explorerpkg.AssetHolderV
	AccountTransactionsView  = explorerpkg.AccountTransactionsView
	AccountOperationsView    = explorerpkg.AccountOperationsView
	AccountMovementsView     = explorerpkg.AccountMovementsView
	AccountMovementEntry     = explorerpkg.AccountMovementEntry
	AccountPositionsView     = explorerpkg.AccountPositionsView
	PositionEntry            = explorerpkg.PositionEntry
	PositionLastActivity     = explorerpkg.PositionLastActivity
	ContractEventView        = explorerpkg.ContractEventView
	ContractDetailView       = explorerpkg.ContractDetailView
	ContractDirectoryEntry   = explorerpkg.ContractDirectoryEntry
	ContractsDirectoryView   = explorerpkg.ContractsDirectoryView
	ContractEdge             = explorerpkg.ContractEdge
	ContractInteractionsView = explorerpkg.ContractInteractionsView
	ContractCodeVersionV     = explorerpkg.ContractCodeVersionV
	ContractCodeHistoryView  = explorerpkg.ContractCodeHistoryView
	LedgerView               = explorerpkg.LedgerView
	TxSummaryView            = explorerpkg.TxSummaryView
	LedgersListView          = explorerpkg.LedgersListView
	LedgerTransactionsView   = explorerpkg.LedgerTransactionsView
	SearchResultView         = explorerpkg.SearchResultView
	TxEventView              = explorerpkg.TxEventView
	TxDetailView             = explorerpkg.TxDetailView
	WasmExportView           = explorerpkg.WasmExportView
	ContractWasmView         = explorerpkg.ContractWasmView
)

// explorerUnavailable writes the standard 503 when no explorer reader is wired
// (deployment without ClickHouse, or ClickHouse unreachable at startup). Kept
// in package v1 (rather than moved into internal/api/v1/explorer) because
// three non-explorer-cluster handlers — lending TVL, liquidity-pool reserves,
// and pool-reserves — also read through the same ExplorerReader seam and call
// this directly; explorer.Handler has its own equivalent (unavailable) so
// this package doesn't need to import that package's unexported method.
func (s *Server) explorerUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the ClickHouse explorer reader (ADR-0038).")
}

// parseExplorerLimit parses ?limit= with a default and an inclusive cap.
// ok=false (after writing a problem+json) on parse error / out of range.
// Kept in package v1 (rather than moved) because anomalies.go, mev.go, and
// liquidity_pools.go also call it directly; explorer.Handler receives it as
// the injected ParseLimit func (see explorerHandlerFor below) so the moved
// endpoints share the exact same limit-parsing behavior.
func parseExplorerLimit(w http.ResponseWriter, r *http.Request, def, maxN int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxN {
		writeProblem(w, r, "https://api.stellarindex.io/errors/invalid-limit",
			"Invalid limit", http.StatusBadRequest,
			"limit must be an integer in [1, "+strconv.Itoa(maxN)+"]")
		return 0, false
	}
	return n, true
}

// explorerHandlerFor constructs the explorer package's *Handler from a
// fully-initialized Server + its constructor Options, wiring the small
// set of cross-cutting seams (logger, USD pricing, SAC detection, the
// lake watermark cache, and the shared response-writing helpers) as
// injected function values — the explorer package must not import
// package v1 (Server already imports explorer to register routes;
// the reverse import would cycle), so these seams cross the boundary
// as plain funcs/interfaces instead of *Server itself.
func explorerHandlerFor(s *Server, opts Options, logger *slog.Logger) *explorerpkg.Handler {
	return &explorerpkg.Handler{
		Reader:             opts.Explorer,
		Logger:             logger,
		VerifiedCurrencies: opts.VerifiedCurrencies,
		ProtocolContracts:  opts.ProtocolContracts,
		PricingEnabled:     opts.Prices != nil,
		SEP41Movements:     opts.SEP41Movements,
		Positions:          opts.Positions,
		PoolTokens:         opts.ProtocolPoolTokens,
		LookupUSDPrice:     s.lookupUSDPrice,
		IsKnownSAC:         s.isKnownSAC,
		LakeWatermark:      s.lakeWatermark,
		ParseLimit:         parseExplorerLimit,
		ParseWindowDays:    parseWindowDays,
		WriteProblem:       writeProblem,
		ClientAborted:      clientAborted,
		WriteJSON: func(w http.ResponseWriter, data any, stale bool) {
			writeJSON(w, data, Flags{Stale: stale})
		},
	}
}
