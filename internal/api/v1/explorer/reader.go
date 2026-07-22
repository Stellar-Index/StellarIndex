// Package explorer holds the network-explorer endpoints (ADR-0038):
// GET /v1/ledgers, /v1/ledgers/{seq}, /v1/ledgers/{seq}/transactions,
// /v1/operations, /v1/network/throughput, /v1/tx/{hash}, /v1/search,
// /v1/contracts*, /v1/accounts*, /v1/assets/{asset_id}/holders.
//
// Extracted from internal/api/v1 (maintainability audit 2026-07-01,
// D1 finding M1-7: "internal/api/v1 is 76 flat non-test files; the
// explorer_* cluster is the obvious next extraction"). The handlers
// read the certified ClickHouse lake directly through ExplorerReader
// and otherwise depend only on a handful of narrow, injected seams
// (Handler below) — they do NOT hold a reference to v1.Server, so
// this package does not import internal/api/v1 (that would cycle,
// since v1.Server wires a *Handler into its route table). Package
// v1 keeps type aliases for every exported type here so its existing
// (pre-extraction) tests keep compiling unchanged — see explorer.go
// in internal/api/v1.
package explorer

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// explorerReadTimeout is the per-request ceiling every explorer handler
// wraps its ClickHouse lake reads in (C3-1, audit-2026-07-16). The
// explorer endpoints read the shared explorer pool (MaxOpenConns:8);
// without a bounded context a handful of slow unauthenticated requests
// (e.g. AssetHolders' two ledger_entries_current FINAL scans) hold every
// connection open and block every lake-backed endpoint — the server
// WriteTimeout does NOT cancel an in-flight query. A request-scoped
// deadline lets the reads observe cancellation and release the
// connection. 8s matches the sibling raw-scan endpoints in package v1
// (liquidity_pools.go, markets.go) and fires before the blanket
// request-timeout middleware (defaultRequestTimeout, 15s).
const explorerReadTimeout = 8 * time.Second

// ExplorerReader is the seam the network-explorer endpoints (ADR-0038) read
// through: the certified Tier-1 ClickHouse lake (the full chain to genesis —
// ledgers / transactions / operations / contract events). *clickhouse.ExplorerReader
// satisfies it. Nil disables the explorer endpoints (503). The interface grows
// per ADR-0038 phase (A: ledgers/tx/ops/contracts; B: account history; C: state).
//
// NOTE: this interface is also the read seam behind a few v1 handlers
// OUTSIDE this package (asset SAC resolution, lending TVL, liquidity-pool
// reserves) — internal/api/v1 keeps its own `ExplorerReader` as a type
// alias to this one so those callers are unaffected by the extraction.
type ExplorerReader interface {
	RecentLedgers(ctx context.Context, limit int, beforeSeq uint32) ([]clickhouse.LedgerHeader, error)
	LedgerBySeq(ctx context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error)
	LedgerTransactions(ctx context.Context, seq uint32, limit int) ([]clickhouse.TxSummary, error)
	OperationsByLedger(ctx context.Context, seq uint32, limit int) ([]clickhouse.OpRow, error)
	RecentOperations(ctx context.Context, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error)
	OperationTypeStats(ctx context.Context, windowLedgers uint32) ([]clickhouse.OpTypeCount, error)
	NetworkThroughput(ctx context.Context, windowDays int) ([]clickhouse.ThroughputBucket, error)
	BlendPoolReserves(ctx context.Context, pool string, assets []string, configs map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error)
	TransactionByHash(ctx context.Context, hash string) (clickhouse.TxSummary, bool, error)
	OperationsByTx(ctx context.Context, seq uint32, hash string) ([]clickhouse.OpRow, error)
	OperationResultsByTx(ctx context.Context, seq uint32, hash string) (map[uint32]int32, error)
	EventsByTx(ctx context.Context, seq uint32, hash string) ([]clickhouse.EventSummary, error)
	ContractEventsRecent(ctx context.Context, contractID string, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.ContractActivityRow, error)
	ContractWasm(ctx context.Context, contractID string) (clickhouse.ContractWasmInfo, error)
	RecentContracts(ctx context.Context, limit int, sinceLedger uint32) ([]clickhouse.ContractDirectoryRow, error)
	ContractInteractions(ctx context.Context, contractID string, limit int, sinceLedger uint32) ([]clickhouse.ContractEdgeRow, error)
	ContractCodeHistory(ctx context.Context, contractID string) ([]clickhouse.ContractCodeVersion, error)
	AccountTransactions(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error)
	AccountOperations(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error)
	AccountState(ctx context.Context, account string) (clickhouse.AccountState, error)
	AssetHolders(ctx context.Context, asset string, limit int) ([]clickhouse.AssetHolder, int64, error)
	AccountsByWealth(ctx context.Context, assets []string, prices []float64, limit int) ([]clickhouse.AccountWealth, error)
	// AccountsByWealthCached serves the ranking from a background-refreshed
	// cache and NEVER runs the underlying FINAL scan on the caller's
	// deadline. ok=false means "not warm yet" — render a warming state, do
	// not fall back to AccountsByWealth on the request path (site-audit S3:
	// that scan needs 11-20s against an 8s handler deadline, so it 500'd
	// 100% of the time).
	AccountsByWealthCached(ctx context.Context, assets []string, prices []float64, limit int) ([]clickhouse.AccountWealth, bool)
	SoroswapPairReserves(ctx context.Context, pairs []string) (map[string]clickhouse.SoroswapPairState, error)
	NativeLiquidityPoolReserves(ctx context.Context, poolIDs []string) (map[string]clickhouse.NativeLiquidityPoolState, error)
	NativeLiquidityPoolsRanked(ctx context.Context, limit int) ([]clickhouse.NativeLiquidityPoolState, error)
	TokenDisplays(ctx context.Context, tokens []string) (map[string]clickhouse.TokenDisplayMeta, error)
	SACClassicAssetName(ctx context.Context, contractID string) (string, bool, error)
	SACAssetFromEvents(ctx context.Context, contractID string) (string, bool, error)
	AccountsUnspendable(ctx context.Context, accountIDs []string) (map[string]bool, error)
	AccountMovements(ctx context.Context, address string, limit int, cur clickhouse.AccountMovementCursor, filter clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error)
}

// ContractsReader is the narrow read seam onto the protocol_contracts
// registry (ADR-0035) this package needs: contract_id → owning-protocol
// attribution. v1.ProtocolContractsReader (a wider interface used
// elsewhere in package v1) satisfies this structurally — v1 passes its
// reader straight through when constructing a Handler, no adapter needed.
type ContractsReader interface {
	ProtocolContractIndex(ctx context.Context) (map[string]string, error)
}

// Handler holds every explorer endpoint's dependencies. Package v1
// constructs one at startup (internal/api/v1/server.go) and registers
// its exported methods directly on the mux — the "thin router" side
// of the split lives in v1; this package holds the endpoint logic.
//
// The response-writing (WriteJSON/WriteProblem/ClientAborted) and a
// handful of cross-cutting reads (LookupUSDPrice/IsKnownSAC/LakeWatermark/
// ParseWindowDays) are injected function values rather than pulled in via
// an import of package v1, because v1.Server itself embeds a *Handler —
// an import the other way would cycle. Each of these mirrors a v1
// package-level helper or Server method 1:1; see server.go's Handler
// construction for the wiring.
type Handler struct {
	Reader             ExplorerReader
	Logger             *slog.Logger
	VerifiedCurrencies *currency.Catalogue
	ProtocolContracts  ContractsReader
	// PricingEnabled mirrors v1's `s.prices != nil` — the wealth-ranking
	// endpoint (GET /v1/accounts) needs a priced asset set and 503s
	// without one, independent of whether the lake reader is wired.
	PricingEnabled bool
	// SEP41Movements, when non-nil, backs the Postgres "recent tail"
	// half of GET /v1/accounts/{g}/movements' merge (ADR-0048 D5). Nil
	// degrades that endpoint to serving the ClickHouse pre-P23 archive
	// alone, with an honest coverage_note — see movements.go.
	SEP41Movements SEP41MovementsReader

	// Positions, when non-nil, backs GET /v1/accounts/{g}/positions
	// (the "DeFi positions" view) — six per-protocol Postgres folds.
	// Nil 503s the endpoint (positions.go).
	Positions PositionsReader

	// PoolTokens, when non-nil, best-effort resolves a pool-based
	// protocol's venue to a human asset-pair label on the positions
	// endpoint (blend / aquarius; see positions.go's poolTokensFor).
	// Nil / a read error just omits venue_label — never fails the
	// request.
	PoolTokens PoolTokensReader

	LookupUSDPrice  func(ctx context.Context, asset canonical.Asset) (string, bool)
	IsKnownSAC      func(contractID string) bool
	LakeWatermark   func(ctx context.Context) (ledger uint32, stale bool, ok bool)
	ParseLimit      func(w http.ResponseWriter, r *http.Request, def, maxN int) (int, bool)
	ParseWindowDays func(r *http.Request, def int) int

	WriteJSON     func(w http.ResponseWriter, data any, stale bool)
	WriteProblem  func(w http.ResponseWriter, r *http.Request, typeURL, title string, status int, detail string)
	ClientAborted func(r *http.Request, err error) bool

	// opsDir is the short-TTL cache for the /v1/operations directory
	// first page (opsDirCache doc comment in operations.go has the
	// full rationale). Zero value is ready to use.
	opsDir opsDirCache

	// opTypeStats caches the trailing-24h op-type breakdown SEPARATELY from
	// the 3s directory cache. It summarises a 24-HOUR window over a 34B-row
	// table, so recomputing it on the directory's 3s cadence was ~1,200
	// pointless recomputes/hour and the dominant cause of /v1/operations
	// blowing its read deadline under load.
	opTypeStats opTypeStatsCache
}

// unavailable writes the standard 503 when no explorer reader is wired
// (deployment without ClickHouse, or ClickHouse unreachable at startup).
// Mirrors v1's Server.explorerUnavailable, which stays in package v1 —
// three other v1 handlers (lending/liquidity-pools/pool-reserves) that
// also read through ExplorerReader call that copy directly.
func (h *Handler) unavailable(w http.ResponseWriter, r *http.Request) {
	h.WriteProblem(w, r,
		"https://api.stellarindex.io/errors/explorer-unavailable",
		"Explorer unavailable", http.StatusServiceUnavailable,
		"This deployment hasn't wired the ClickHouse explorer reader (ADR-0038).")
}

// parseUint32Query parses an optional uint32 query param (e.g. ?before=).
// Returns 0 when absent. ok=false (after writing a problem+json) on a
// malformed value.
func (h *Handler) parseUint32Query(w http.ResponseWriter, r *http.Request, name string) (uint32, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-parameter",
			"Invalid parameter", http.StatusBadRequest,
			name+" must be a non-negative 32-bit integer")
		return 0, false
	}
	return uint32(n), true
}

// encodeCursor renders a composite keyset position as an opaque dotted-decimal
// cursor string ("63000000.4.7") for ?cursor=. The component count matches the
// listing's ORDER BY arity (2 for account txs, 3 for account ops + contract
// events). Clients treat it as opaque and echo it back verbatim.
func encodeCursor(parts ...uint32) string {
	ss := make([]string, len(parts))
	for i, p := range parts {
		ss[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(ss, ".")
}

// parseExplorerCursor reads the optional ?cursor= opaque keyset cursor and
// decodes it into an ExplorerCursor with exactly `parts` components (2 → account
// txs; 3 → account ops + contract events). Absent → zero cursor (first page).
// ok=false (after a problem+json) on a malformed value or a zero ledger (a real
// cursor always points past an actual row).
func (h *Handler) parseExplorerCursor(w http.ResponseWriter, r *http.Request, parts int) (clickhouse.ExplorerCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return clickhouse.ExplorerCursor{}, true
	}
	bad := func() (clickhouse.ExplorerCursor, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return clickhouse.ExplorerCursor{}, false
	}
	segs := strings.Split(raw, ".")
	if len(segs) != parts {
		return bad()
	}
	vals := make([]uint32, parts)
	for i, s := range segs {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return bad()
		}
		vals[i] = uint32(n)
	}
	if vals[0] == 0 {
		return bad()
	}
	cur := clickhouse.ExplorerCursor{Ledger: vals[0], A: vals[1]}
	if parts == 3 {
		cur.B = vals[2]
	}
	return cur, true
}

// compile-time assertion that the lake reader satisfies the explorer seam.
var _ = func(r *clickhouse.ExplorerReader) ExplorerReader { return r }
