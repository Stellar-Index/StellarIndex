package v1

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
)

// ExplorerReader aliases the explorer package's read seam, which v1 handlers outside it (SAC resolution, lending TVL, pool reserves) also use; the explorer endpoints live in internal/api/v1/explorer (ADR-0038).
type ExplorerReader = explorerpkg.ExplorerReader

type (
	DirectoryInfoV           = explorerpkg.DirectoryInfoV
	DirectoryLookupView      = explorerpkg.DirectoryLookupView
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
	AccountTradesView        = explorerpkg.AccountTradesView
	AccountTradeEntry        = explorerpkg.AccountTradeEntry
	AccountActivityView      = explorerpkg.AccountActivityView
	OpTypeCountView          = explorerpkg.OpTypeCountView
	DefiActionView           = explorerpkg.DefiActionView
	BridgeTransfersView      = explorerpkg.BridgeTransfersView
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

// explorerUnavailable writes the standard 503 when no explorer reader is wired. Kept in package v1 because lending TVL,
// liquidity-pool reserves and pool-reserves also read through the ExplorerReader seam; explorer.Handler has its own equivalent.
func (s *Server) explorerUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the ClickHouse explorer reader (ADR-0038).")
}

// parseExplorerLimit parses ?limit= with a default and inclusive cap; ok=false (after writing problem+json) when invalid. Kept
// here because anomalies.go, mev.go and liquidity_pools.go call it; explorer.Handler gets it injected as ParseLimit.
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

// explorerHandlerFor builds the explorer *Handler from the Server and Options, injecting cross-cutting seams (logger, USD
// pricing, SAC detection, lake watermark cache, response helpers) as funcs: package explorer must not import v1 (import cycle).
func explorerHandlerFor(s *Server, opts Options, logger *slog.Logger) *explorerpkg.Handler {
	return &explorerpkg.Handler{
		Reader:             opts.Explorer,
		Logger:             logger,
		VerifiedCurrencies: opts.VerifiedCurrencies,
		ProtocolContracts:  opts.ProtocolContracts,
		PricingEnabled:     opts.Prices != nil,
		SEP41Movements:     opts.SEP41Movements,
		Positions:          opts.Positions,
		Trades:             opts.AccountTrades,
		Activity:           opts.AccountActivity,
		PoolTokens:         s.protocolPoolTokens,
		Directory:          opts.Directory,
		ContractProtocol:   s.contractProtocol,
		TokenDecimals:      s.resolveTokenDecimals,
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
		WriteJSONAt: func(w http.ResponseWriter, data any, stale, degraded bool, asOf time.Time) {
			writeEnvelope(w, Envelope{Data: data, AsOf: WireTime(asOf.UTC()), Flags: Flags{Stale: stale, Degraded: degraded}})
		},
	}
}

// PrewarmAccountsWealth primes the /v1/accounts wealth ranking for cmd/stellarindex-api's prewarm loop; see [explorer.Handler.PrewarmAccountsWealth].
func (s *Server) PrewarmAccountsWealth(ctx context.Context) {
	if s.explorerHandler == nil {
		return
	}
	s.explorerHandler.PrewarmAccountsWealth(ctx)
}

// PrewarmOpTypeStats primes the /v1/operations trailing-24h op-type panel for the prewarm loop; see [explorer.Handler.PrewarmOpTypeStats].
func (s *Server) PrewarmOpTypeStats(ctx context.Context) {
	if s.explorerHandler == nil {
		return
	}
	s.explorerHandler.PrewarmOpTypeStats(ctx)
}

// PrewarmContractsDirectory primes the /v1/contracts directory's default window rung for the prewarm loop; see [explorer.Handler.PrewarmContractsDirectory].
func (s *Server) PrewarmContractsDirectory(ctx context.Context) {
	if s.explorerHandler == nil {
		return
	}
	s.explorerHandler.PrewarmContractsDirectory(ctx)
}

// PrewarmContractStats primes the /v1/contracts/stats snapshot; see [explorer.Handler.PrewarmContractStats].
func (s *Server) PrewarmContractStats(ctx context.Context) {
	if s.explorerHandler == nil {
		return
	}
	s.explorerHandler.PrewarmContractStats(ctx)
}

// PrewarmNetworkThroughput primes the /v1/network/throughput daily series for the prewarm loop; see [explorer.Handler.PrewarmNetworkThroughput].
func (s *Server) PrewarmNetworkThroughput(ctx context.Context) {
	if s.explorerHandler == nil {
		return
	}
	s.explorerHandler.PrewarmNetworkThroughput(ctx)
}
