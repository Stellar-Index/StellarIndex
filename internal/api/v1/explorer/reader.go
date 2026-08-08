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
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

// readTimedOut reports whether a handler-scoped read context — the
// context.WithTimeout(r.Context(), explorerReadTimeout) every handler in this
// package wraps its lake reads in — hit its deadline. Pass the PER-CALL ctx,
// not r.Context(): the driver often beats the caller to noticing and returns
// its own cancellation error rather than something that unwraps to
// context.DeadlineExceeded, so the context itself is the reliable signal.
//
// A 1:1 mirror of v1's handlerTimedOut (internal/api/v1/envelope.go), which
// this package cannot import — v1.Server embeds a *Handler from here, so the
// import would cycle (see the package doc). Keep the two in sync.
func readTimedOut(callCtx context.Context, err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return callCtx.Err() == context.DeadlineExceeded
}

// retryableColdMiss reports whether a cold-path SWR-cache error is the
// capacity class the handlers map to the retryable 503: the request
// deadline expired waiting for the detached compute, or the shared
// detached-refresh gate was saturated and the refresh was skipped — both
// "try again shortly", neither a bug. Two saturation sentinels are matched:
// this package's errRefreshSaturated (asset-holders / contracts SWR caches)
// and clickhouse.ErrRefreshSaturated, raised by the lake reader's own
// account-state cache (AccountStateCached) — same condition, different
// package (this one can't own a sentinel the clickhouse layer must return).
func retryableColdMiss(callCtx context.Context, err error) bool {
	if readTimedOut(callCtx, err) ||
		errors.Is(err, errRefreshSaturated) ||
		errors.Is(err, clickhouse.ErrRefreshSaturated) {
		return true
	}
	// Driver-level network timeouts (`read tcp …: i/o timeout` from the
	// ClickHouse conn under load) are the same transient capacity class:
	// they arrive as net.Error, not context.DeadlineExceeded, and were
	// falling through to 500 "Internal error" (site audit 2026-08-07 —
	// /accounts/{g}/operations served exactly that during a refresh
	// storm).
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// writeReadTimeout writes the standard response for a lake read that blew
// explorerReadTimeout: 503 + problem+json with an endpoint-specific
// `…-timeout` type URL (audit-2026-07-23 C-F1). Pre-fix every explorer handler
// mapped a deadline to `errors/internal` 500, which tells the caller "we broke"
// when the truthful answer is "this exceeded our time budget, retry" — and made
// a capacity problem indistinguishable from a bug in the logs and the 5xx SLA
// probe. /v1/contracts/{id}/code-history served that 500 for EVERY contract.
//
// 503, not 504: this is the convention the rest of the API already uses for a
// server-side read deadline (14 call sites in package v1 pair handlerTimedOut
// with a `…-timeout` 503 — markets, lending, history, chart, ohlc, oracle,
// issuers, observations, sep41-transfers, cursors), it is the decision rule
// documented on v1's clientAborted ("the client is still waiting and deserves a
// 503"), and every explorer route ALREADY declares 503 in
// openapi/stellar-index.v1.yaml — so this needs no new wire shape.
func (h *Handler) writeReadTimeout(w http.ResponseWriter, r *http.Request, typeURL, title string) {
	h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
		"the ClickHouse lake read didn't return within the "+explorerReadTimeout.String()+
			" explorer read budget; retry shortly")
}

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
	ContractEventsRecent(ctx context.Context, contractID string, limit int, cur clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error)
	ContractWasm(ctx context.Context, contractID string) (clickhouse.ContractWasmInfo, error)
	RecentContracts(ctx context.Context, limit int, sinceLedger uint32) ([]clickhouse.ContractDirectoryRow, error)
	ContractInteractions(ctx context.Context, contractID string, limit int, sinceLedger uint32) ([]clickhouse.ContractEdgeRow, error)
	ContractCodeHistory(ctx context.Context, contractID string) ([]clickhouse.ContractCodeVersion, error)
	AccountTransactions(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error)
	AccountOperations(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error)
	// AccountOperationTypeCounts is the whole-history aggregate variant
	// of AccountOperations (same two UNION arms, GROUP BY op_type) —
	// scan-shaped, so callers must run it under a detached budget (the
	// activity endpoint's SWR cache), never a request deadline.
	AccountOperationTypeCounts(ctx context.Context, account string) ([]clickhouse.OpTypeCount, error)
	AccountState(ctx context.Context, account string) (clickhouse.AccountState, error)
	// AccountStateCached serves account state from a bounded TTL cache;
	// a cold miss waits on a DETACHED fill bounded by the caller's
	// deadline, and an EXPIRED entry is served immediately with
	// stale=true while one detached refresh runs (route-sweep
	// 2026-07-30) — pair stale with flags.stale in the envelope.
	AccountStateCached(ctx context.Context, account string) (clickhouse.AccountState, bool, error)
	AssetHolders(ctx context.Context, asset string, limit int) ([]clickhouse.AssetHolder, int64, error)
	AccountsByWealth(ctx context.Context, assets []string, prices []float64, limit int) ([]clickhouse.AccountWealth, error)
	// AccountsByWealthCached serves the ranking from a background-refreshed
	// cache and NEVER runs the underlying FINAL scan on the caller's
	// deadline. asOf is the ranking's fetch time — an entry past its TTL is
	// STILL served (with its honest asOf; the handler flags it degraded)
	// rather than treated as a miss, so a window of failed refreshes
	// degrades to old-but-real data, not to 503s (route-sweep 2026-07-29).
	// ok=false means "never computed yet" — render a warming state, do
	// not fall back to AccountsByWealth on the request path (site-audit S3:
	// that scan needs 11-20s against an 8s handler deadline, so it 500'd
	// 100% of the time).
	AccountsByWealthCached(ctx context.Context, assets []string, prices []float64, limit int) ([]clickhouse.AccountWealth, time.Time, bool)
	SoroswapPairReserves(ctx context.Context, pairs []string) (map[string]clickhouse.SoroswapPairState, error)
	NativeLiquidityPoolReserves(ctx context.Context, poolIDs []string) (map[string]clickhouse.NativeLiquidityPoolState, error)
	NativeLiquidityPoolsRanked(ctx context.Context, limit int) ([]clickhouse.NativeLiquidityPoolState, error)
	TokenDisplays(ctx context.Context, tokens []string) (map[string]clickhouse.TokenDisplayMeta, error)
	SACClassicAssetName(ctx context.Context, contractID string) (string, bool, error)
	SACAssetFromEvents(ctx context.Context, contractID string) (string, bool, error)
	AccountsUnspendable(ctx context.Context, accountIDs []string) (map[string]bool, error)
	AccountMovements(ctx context.Context, address string, limit int, cur clickhouse.AccountMovementCursor, filter clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error)
	// Cap67MovementsWatermark is the highest ledger the cap67 movement
	// derive (inventory #1) has completed through — 0 when the feed
	// isn't provisioned. The movements handler floors its Postgres tail
	// arm at watermark+1 and ceilings the ClickHouse arm at the
	// watermark, keeping the merge gap-free and double-count-free at
	// any derive progress. Implementations cache (~60s).
	Cap67MovementsWatermark(ctx context.Context) (uint32, error)
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

	// Activity, when non-nil, backs the Postgres segments of GET
	// /v1/accounts/{g}/activity (trades_total / defi_actions /
	// bridge_transfers); the ops_by_type segment reads the ClickHouse
	// lake via Reader. Either seam may be nil independently — the
	// endpoint degrades segment-wise with an honest coverage_note and
	// only 503s when BOTH are nil (activity.go).
	Activity ActivityReader

	// Trades, when non-nil, backs GET /v1/accounts/{g}/trades — the
	// per-address historic-trades listing over the Postgres `trades`
	// hypertable (taker/maker attribution). Nil 503s the endpoint
	// (account_trades.go).
	Trades TradesReader

	// PoolTokens, when non-nil, best-effort resolves a pool-based
	// protocol's venue to a human asset-pair label on the positions
	// endpoint (blend / aquarius; see positions.go's poolTokensFor).
	// Nil / a read error just omits venue_label — never fails the
	// request.
	PoolTokens PoolTokensReader

	// Directory, when non-nil, resolves curated third-party address
	// labels (stellar-expert/public-directory via account_directory,
	// migration 0136) for the account + contract detail views'
	// `directory` field and GET /v1/directory. Nil omits the field
	// and 503s the batch endpoint (directory.go).
	Directory DirectoryReader

	LookupUSDPrice  func(ctx context.Context, asset canonical.Asset) (string, bool)
	IsKnownSAC      func(contractID string) bool
	LakeWatermark   func(ctx context.Context) (ledger uint32, stale bool, ok bool)
	ParseLimit      func(w http.ResponseWriter, r *http.Request, def, maxN int) (int, bool)
	ParseWindowDays func(r *http.Request, def int) int

	WriteJSON func(w http.ResponseWriter, data any, stale bool)
	// WriteJSONAt is WriteJSON with an explicit envelope as_of — used by the
	// snapshot-backed listings so a degraded (stale-snapshot) response
	// carries the snapshot's REAL computation time instead of now().
	// Optional: nil falls back to WriteJSON (same wire shape; as_of is
	// always present on the envelope either way).
	WriteJSONAt   func(w http.ResponseWriter, data any, stale bool, asOf time.Time)
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

	// assetHolders + contractsDir are the bounded-TTL, single-flighted
	// caches in front of the two UNAUTHENTICATED reads whose cost scales
	// with the lake rather than the request — GET /v1/assets/{id}/holders
	// (two current-state FINAL scans) and the GET /v1/contracts directory
	// (a multi-day GROUP BY over contract_events). Both zero values are
	// ready to use; see hot_reads.go for the full rationale (C3-002 +
	// C3-009, audit-2026-07-23).
	assetHolders assetHoldersCache
	contractsDir contractsDirCache

	// contractDetail is the shared bounded-TTL, single-flighted cache in
	// front of the three per-contract detail reads (recent events /
	// interactions / code-history) — route-sweep 2026-07-30: all three
	// ran inline on the request deadline, so a busy contract timed out on
	// every request and no retry could land warm. Zero value ready; see
	// contract_detail_cache.go.
	contractDetail contractDetailCache

	// refreshGate bounds this handler's DETACHED cache refreshes globally
	// across keys AND cache kinds (audit 2026-07-31): per-key
	// single-flight alone leaves the key space attacker-chosen on these
	// unauthenticated routes, so fabricated-key churn could queue one
	// unbounded lake scan per key on the shared 8-conn pool. Resolved
	// lazily by detachedGate() — shared with the lake reader's own
	// account-state gate when the Reader exposes one, so the whole
	// explorer surface has ONE bound.
	refreshGateOnce sync.Once
	refreshGate     *clickhouse.RefreshGate
}

// detachedGate returns the shared bound for detached refreshes: the lake
// reader's own gate when it exposes one (production — one global bound
// across account-state + holders + contracts-dir + contract-detail
// refreshes), else a handler-local gate with the same limit (test stubs).
func (h *Handler) detachedGate() *clickhouse.RefreshGate {
	h.refreshGateOnce.Do(func() {
		if p, ok := h.Reader.(interface {
			DetachedRefreshGate() *clickhouse.RefreshGate
		}); ok {
			h.refreshGate = p.DetachedRefreshGate()
		}
		if h.refreshGate == nil {
			h.refreshGate = clickhouse.NewRefreshGate(clickhouse.DefaultDetachedRefreshLimit)
		}
	})
	return h.refreshGate
}

// writeJSONAt writes data with an explicit envelope as_of when the seam is
// wired, degrading to the plain WriteJSON (as_of = now) when not — test
// handlers that only wire WriteJSON keep working unchanged.
func (h *Handler) writeJSONAt(w http.ResponseWriter, data any, stale bool, asOf time.Time) {
	if h.WriteJSONAt != nil && !asOf.IsZero() {
		h.WriteJSONAt(w, data, stale, asOf)
		return
	}
	h.WriteJSON(w, data, stale)
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
