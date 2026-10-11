// Package explorer holds the network-explorer endpoints (ADR-0038):
// GET /v1/ledgers, /v1/ledgers/{seq}, /v1/ledgers/{seq}/transactions,
// /v1/operations, /v1/network/throughput, /v1/tx/{hash}, /v1/search,
// /v1/contracts*, /v1/accounts*, /v1/assets/{asset_id}/holders.
//
// Handlers read the certified ClickHouse lake through ExplorerReader and otherwise depend only on
// narrow injected seams (Handler below). They must not reference v1.Server, so this package
// cannot import internal/api/v1: v1.Server wires a *Handler into its routes, which would cycle.
// Package v1 keeps type aliases for every exported type here (see explorer.go there).
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
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// explorerReadTimeout is the per-request ceiling on every handler's ClickHouse lake reads. The
// shared explorer pool has 8 connections, and the server WriteTimeout does not cancel an in-flight
// query, so a few slow unauthenticated requests would otherwise hold every connection. 8s matches
// the sibling raw-scan endpoints in package v1 and fires before the 15s request-timeout middleware.
const explorerReadTimeout = 8 * time.Second

// readTimedOut reports whether a handler-scoped read context hit its deadline. Pass the PER-CALL
// ctx, not r.Context(): the driver often returns its own cancellation error that does not unwrap
// to context.DeadlineExceeded, so the context itself is the reliable signal.
//
// A 1:1 mirror of v1's handlerTimedOut (envelope.go), which this package cannot import (cycle).
// Keep the two in sync, including recording a true verdict for usage metering, which makes the
// read billable.
func readTimedOut(callCtx context.Context, err error) bool {
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		callCtx.Err() == context.DeadlineExceeded
	if timedOut {
		middleware.MarkReadDeadline(callCtx)
	}
	return timedOut
}

// retryableColdMiss reports whether a cold-path SWR-cache error is the capacity class mapped to
// the retryable 503: the deadline expired waiting for the detached compute, or the shared refresh
// gate was saturated. Both saturation sentinels are matched (this package's errRefreshSaturated
// and the clickhouse layer's, which it cannot share because that layer must return its own).
func retryableColdMiss(callCtx context.Context, err error) bool {
	if readTimedOut(callCtx, err) ||
		errors.Is(err, errRefreshSaturated) ||
		errors.Is(err, clickhouse.ErrRefreshSaturated) {
		return true
	}
	// Driver-level i/o timeouts are the same transient capacity class but arrive as net.Error,
	// not context.DeadlineExceeded, and would otherwise fall through to a 500.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return lakeUnreachable(err)
}

// lakeUnreachable reports whether err is a TRANSPORT-layer failure below the query layer
// (connection refused, socket died mid-query, host stopped resolving). A query cannot provoke
// these, so they mean "dependency down, retry", not a bug in this process.
//
// Matching only net.Error with Timeout() true is not enough: a hard-down lake answers instantly
// with a *net.OpError whose Timeout() is false. Unmatched, a ClickHouse outage would read as a
// code bug (500) in logs, the 5xx SLA probe and every alert built on them.
//
// Same rule as the other storage seams: IsCacheUnavailable (cache_errors.go) for Redis and
// transientStorageErr (envelope.go) for Postgres. Keep the three in step.
//
// *net.DNSError arrives bare, not wrapped in an OpError; the errno arms catch a driver that
// re-wrapped only the syscall error.
func lakeUnreachable(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// writeReadTimeout writes the standard 503 problem+json, with an endpoint-specific `...-timeout`
// type URL, for a lake read that blew explorerReadTimeout. A 500 would make a capacity problem
// indistinguishable from a bug in the logs and the 5xx SLA probe.
//
// 503, not 504: the rest of the API uses 503 for a server-side read deadline (see v1's
// clientAborted) and every explorer route already declares 503 in the OpenAPI spec.
func (h *Handler) writeReadTimeout(w http.ResponseWriter, r *http.Request, typeURL, title string) {
	// Also reached by a driver i/o timeout, which readTimedOut does not see.
	middleware.MarkReadDeadline(r.Context())
	h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
		"the ClickHouse lake read didn't return within the "+explorerReadTimeout.String()+
			" explorer read budget; retry shortly")
}

// writeRetryable is writeReadTimeout with an honest split by cause. A saturation reject is not a
// timeout (the query never ran), and labeling it one sends investigations down the wrong path.
// Same 503 + type URL either way; only the detail says which condition occurred.
func (h *Handler) writeRetryable(w http.ResponseWriter, r *http.Request, err error, typeURL, title string) {
	if errors.Is(err, errRefreshSaturated) || errors.Is(err, clickhouse.ErrRefreshSaturated) {
		w.Header().Set("Retry-After", retryAfterBusy)
		h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
			"the server's shared cold-page refresh capacity is saturated (many distinct cold pages "+
				"are being computed right now); this page's data was NOT computed on this request — "+
				"retry in a few seconds")
		return
	}
	// Third cause, same 503: the lake is unreachable. A refused dial answers in microseconds, so
	// "timed out" would send an operator hunting a slow query that never ran. The error is logged
	// here because call sites log a fixed "deadline exceeded" line without err.
	if lakeUnreachable(err) {
		h.Logger.Warn("explorer lake unreachable", "err", err, "path", r.URL.Path)
		w.Header().Set("Retry-After", retryAfterDependencyDown)
		h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
			"the ClickHouse lake is unreachable from the API right now (transport-level failure, "+
				"not a slow query); this is a dependency outage, not a bad request — retry shortly. "+
				"See https://api.stellarindex.io/v1/readyz for live health.")
		return
	}
	w.Header().Set("Retry-After", retryAfterBusy)
	h.writeReadTimeout(w, r, typeURL, title)
}

// Retry-After hints on the retryable 503s. Without one, clients retry immediately, which is the
// retry storm a saturated refresh gate or a downed dependency must not receive. Values mirror
// package v1: 30s for a dependency outage (sized to a Redis/CH fail-over window), a few seconds
// for in-process busy-ness.
const (
	retryAfterBusy           = "5"
	retryAfterDependencyDown = "30"
)

// ExplorerReader is the seam the explorer endpoints (ADR-0038) read the certified ClickHouse lake
// through (the full chain to genesis). *clickhouse.ExplorerReader satisfies it; nil disables the
// endpoints (503).
//
// It is also the read seam behind a few v1 handlers outside this package (SAC resolution, lending
// TVL, liquidity-pool reserves); v1 aliases its own ExplorerReader to this type.
type ExplorerReader interface {
	RecentLedgers(ctx context.Context, limit int, beforeSeq uint32) ([]clickhouse.LedgerHeader, error)
	LedgerBySeq(ctx context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error)
	LedgerTransactions(ctx context.Context, seq uint32, limit int) ([]clickhouse.TxSummary, error)
	OperationsByLedger(ctx context.Context, seq uint32, limit int) ([]clickhouse.OpRow, error)
	RecentOperations(ctx context.Context, limit int, cur clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error)
	RecentOperationsOfType(ctx context.Context, limit int, cur clickhouse.ExplorerCursor, opTypes []string) (clickhouse.OpTypePage, error)
	OperationTypeStats(ctx context.Context, windowLedgers uint32) ([]clickhouse.OpTypeCount, error)
	NetworkThroughput(ctx context.Context, windowDays int) ([]clickhouse.ThroughputBucket, error)
	BlendPoolReserves(ctx context.Context, pool string, version blend.PoolVersion, assets []string, configs map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error)
	TransactionByHash(ctx context.Context, hash string) (clickhouse.TxSummary, bool, error)
	OperationsByTx(ctx context.Context, seq uint32, hash string) ([]clickhouse.OpRow, error)
	OperationResultsByTx(ctx context.Context, seq uint32, hash string) (map[uint32]clickhouse.OpResult, error)
	// TxOutcomesByHash batch-reads parent-transaction outcomes keyed by tx_hash, so operation lists
	// can mark a failed transaction's operations FAILED. `ledgers` is the EXACT set of ledgers the
	// page's operations sit in, not a span across them (see the implementation).
	TxOutcomesByHash(ctx context.Context, ledgers []uint32, hashes []string) (map[string]clickhouse.TxOutcome, error)
	EventsByTx(ctx context.Context, seq uint32, hash string) ([]clickhouse.EventSummary, error)
	ContractEventsRecent(ctx context.Context, contractID string, limit int, cur clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error)
	ContractWasm(ctx context.Context, contractID string) (clickhouse.ContractWasmInfo, error)
	// ContractInstanceState is the instance entry's existence evidence and
	// newest TTL live_until, backing the contract routes' exists + ttl.
	ContractInstanceState(ctx context.Context, contractID string) (clickhouse.ContractInstanceState, error)
	RecentContracts(ctx context.Context, limit int, sinceLedger uint32) ([]clickhouse.ContractDirectoryRow, error)
	ContractStats(ctx context.Context) (clickhouse.ContractStats, error)
	// ContractTypes maps contract ids to isSAC; ids with no instance row are absent.
	ContractTypes(ctx context.Context, contractIDs []string) (map[string]bool, error)
	// ContractInteractions returns the edges plus the EFFECTIVE window floor, which may be newer
	// than sinceLedger (a busy contract's window narrows to keep the read sub-second). Callers
	// must serve the returned floor as since_ledger, not the one they asked for.
	ContractInteractions(ctx context.Context, contractID string, limit int, sinceLedger uint32) ([]clickhouse.ContractEdgeRow, uint32, error)
	ContractCodeHistory(ctx context.Context, contractID string) ([]clickhouse.ContractCodeVersion, error)
	// AccountTransactions' resume, when set, is the next page's cursor: the
	// page stopped at a scan frontier and may be short without being last.
	AccountTransactions(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) (_ []clickhouse.TxSummary, resume clickhouse.ExplorerCursor, _ error)
	AccountOperations(ctx context.Context, account string, limit int, cur clickhouse.ExplorerCursor) (_ []clickhouse.OpRow, resume clickhouse.ExplorerCursor, _ error)
	// AccountOperationTypeCounts is the whole-history GROUP BY op_type variant of AccountOperations.
	// It is scan-shaped, so run it under a detached budget (the activity SWR cache), never a
	// request deadline.
	AccountOperationTypeCounts(ctx context.Context, account string) ([]clickhouse.OpTypeCount, error)
	AccountState(ctx context.Context, account string) (clickhouse.AccountState, error)
	// AccountStateCached serves account state from a bounded TTL cache. A cold miss waits on a
	// DETACHED fill bounded by the caller's deadline; an EXPIRED entry is served at once with
	// stale=true while one detached refresh runs. Pair stale with flags.stale in the envelope.
	AccountStateCached(ctx context.Context, account string) (clickhouse.AccountState, bool, error)
	AssetHolders(ctx context.Context, asset string, limit int) ([]clickhouse.AssetHolder, int64, error)
	AccountsByWealth(ctx context.Context, assets, prices []string, limit int) ([]clickhouse.AccountWealth, error)
	// AccountsByWealthCached serves the ranking from a background-refreshed cache and NEVER runs
	// the underlying FINAL scan on the caller's deadline. AsOf/AsOfLedger are the ranking's
	// vintage; an entry past its TTL is still served (the handler flags it degraded) so failed
	// refreshes degrade to old-but-real data, not 503s.
	// ok=false means never computed: render a warming state and do not fall back to
	// AccountsByWealth on the request path (the scan needs 11-20s against an 8s deadline).
	// Basis ("usd" | "native_xlm") is the unit of the cached ranking, so the handler labels the
	// numbers correctly where no USD price map was available.
	AccountsByWealthCached(ctx context.Context, assets, prices []string, limit int) (clickhouse.AccountWealthSnapshot, bool)
	SoroswapPairReserves(ctx context.Context, pairs []string) (map[string]clickhouse.SoroswapPairState, error)
	NativeLiquidityPoolReserves(ctx context.Context, poolIDs []string) (map[string]clickhouse.NativeLiquidityPoolState, error)
	NativeLiquidityPoolsRanked(ctx context.Context, limit int) ([]clickhouse.NativeLiquidityPoolState, error)
	TokenDisplays(ctx context.Context, tokens []string) (map[string]clickhouse.TokenDisplayMeta, error)
	SACClassicAssetName(ctx context.Context, contractID string) (string, bool, error)
	SACAssetFromEvents(ctx context.Context, contractID string) (string, bool, error)
	AccountsUnspendable(ctx context.Context, accountIDs []string) (map[string]bool, error)
	AccountMovements(ctx context.Context, address string, limit int, cur clickhouse.AccountMovementCursor, filter clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error)
	AssetMovements(ctx context.Context, asset string, limit int, cur clickhouse.AccountMovementCursor, maxLedger uint32) ([]clickhouse.AssetMovementRow, error)
	// AssetMovementsBackfilledThru is the ledger movements_by_asset's history
	// copy is verified through; 0 = only rows since its MV was created.
	AssetMovementsBackfilledThru(ctx context.Context) (uint32, error)
	AssetEntryChanges(ctx context.Context, asset string, limit int, cur clickhouse.AssetEntryChangeCursor, maxLedger uint32) ([]clickhouse.AssetEntryChange, error)
	// EntryHistoryCoverage is the ch-entry-history derive watermark and the
	// from-genesis backfill marker (0 = not derived / not verified).
	EntryHistoryCoverage(ctx context.Context) (watermark, backfilledThru uint32, err error)
	// Cap67MovementsWatermark is the highest ledger the cap67 movement derive has completed
	// through (0 when the feed isn't provisioned). The movements handler floors its Postgres tail
	// arm at watermark+1 and ceilings the ClickHouse arm at the watermark, keeping the merge
	// gap-free and double-count-free. Implementations cache (~60s).
	Cap67MovementsWatermark(ctx context.Context) (uint32, error)
	// Cap67SupplyCoverage is the ledger range, at or below the watermark,
	// the derive has also covered with mint, burn and clawback (ok=false:
	// none yet). Same cache as the watermark.
	Cap67SupplyCoverage(ctx context.Context) (from, thru uint32, ok bool, err error)
	// AccountsStats is the /accounts hub analytics snapshot (rollup-
	// backed; ok=false while the rollup hasn't completed a cycle).
	AccountsStats(ctx context.Context) (clickhouse.AccountsStats, bool, error)
	// AccountCreators is the account-creator league table plus the ledger span its building cycle
	// aggregated. ok=false while no cycle has completed, or when the board has no span to
	// qualify it. A non-empty `account` narrows the board to that keyed row, so a caller can read
	// a rank the top-N page does not reach.
	AccountCreators(ctx context.Context, limit int, account string) (clickhouse.AccountCreators, bool, error)
	// AccountSponsors is the sponsor league table plus the ledger span its building cycle
	// aggregated. History only, never a live sponsored set. ok=false and `account` behave as in
	// AccountCreators.
	AccountSponsors(ctx context.Context, limit int, account string) (clickhouse.AccountSponsors, bool, error)
	// AccountGraph is one account's neighbourhood in the sponsorship and account-creation graph:
	// bounded inbound edges, both outbound summaries and, when `relation` names one, a keyset page
	// of that direction. ok=false while either graph arm is unbuilt; the handler serves that as a
	// warming 503, not as "no relationships".
	AccountGraph(ctx context.Context, account, relation string, limit int, cursor string) (clickhouse.AccountGraph, bool, error)
	// AccountGraphHistory is the same graph bucketed into calendar months. Events the served tier
	// cannot place in a month are counted separately, not smeared. ok=false as in AccountGraph.
	AccountGraphHistory(ctx context.Context, account string) (clickhouse.AccountGraphHistory, bool, error)
	// AccountCohort is the cohort rollup's snapshot of what the accounts a root created or
	// sponsored went on to hold and do. ok=false means no cycle has completed on this deployment;
	// a completed cycle that does not cover the root returns ok=true with Covered=false.
	AccountCohort(ctx context.Context, account, relation string) (clickhouse.AccountCohort, bool, error)
	// ContractActivitySummaryFor is the per-contract liveness card
	// (first/last seen + daily active-ledger series; ok=false when the
	// active-ledgers index isn't usable — callers omit the card).
	ContractActivitySummaryFor(ctx context.Context, contractID string, days int) (clickhouse.ContractActivitySummary, bool, error)
}

// ContractsReader is the narrow read seam onto the protocol_contracts registry (ADR-0035):
// contract_id to owning-protocol attribution. v1.ProtocolContractsReader satisfies it structurally.
type ContractsReader interface {
	ProtocolContractIndex(ctx context.Context) (map[string]string, error)
}

// Handler holds every explorer endpoint's dependencies. Package v1 constructs one at startup and
// registers its exported methods on the mux.
//
// Response writing and the cross-cutting reads (LookupUSDPrice, IsKnownSAC, LakeWatermark,
// ParseWindowDays) are injected function values, not imports of package v1, which would cycle.
// Each mirrors a v1 helper 1:1; see server.go for the wiring.
type Handler struct {
	Reader             ExplorerReader
	Logger             *slog.Logger
	VerifiedCurrencies *currency.Catalogue
	ProtocolContracts  ContractsReader
	// PricingEnabled mirrors v1's `s.prices != nil` — the wealth-ranking
	// endpoint (GET /v1/accounts) needs a priced asset set and 503s
	// without one, independent of whether the lake reader is wired.
	PricingEnabled bool
	// SEP41Movements, when non-nil, backs the Postgres recent-tail half of the movements merge
	// (ADR-0048 D5). Nil serves the ClickHouse pre-P23 archive alone, with an honest coverage_note.
	SEP41Movements SEP41MovementsReader

	// Positions, when non-nil, backs GET /v1/accounts/{g}/positions
	// (the "DeFi positions" view) — six per-protocol Postgres folds.
	// Nil 503s the endpoint (positions.go).
	Positions PositionsReader

	// Activity, when non-nil, backs the Postgres segments of GET /v1/accounts/{g}/activity; the
	// ops_by_type segment reads the lake via Reader. Either seam may be nil: the endpoint degrades
	// per segment with an honest coverage_note and 503s only when BOTH are nil.
	Activity ActivityReader

	// Trades, when non-nil, backs GET /v1/accounts/{g}/trades over the Postgres `trades`
	// hypertable (taker/maker attribution). Nil 503s the endpoint.
	Trades TradesReader

	// PoolTokens, when non-nil, best-effort resolves a pool-based protocol's venue to an asset-pair
	// label on the positions endpoint. Nil or a read error omits venue_label, never fails the request.
	PoolTokens PoolTokensReader

	// Directory, when non-nil, resolves curated third-party address labels for the account and
	// contract detail views' `directory` field and GET /v1/directory. Nil omits the field and 503s
	// the batch endpoint.
	Directory DirectoryReader

	// ContractProtocol names the protocol a C… contract belongs to, for
	// labelling the contracts a cohort moved value through. Nil leaves
	// every contract unlabelled; a miss leaves that one unlabelled.
	ContractProtocol func(ctx context.Context, contractID string) (string, bool)

	// TokenDecimals resolves a Soroban token contract's decimals();
	// ok=false means unknown. Nil leaves every token's scale unknown.
	TokenDecimals func(ctx context.Context, contractID string) (decimals int, ok bool)

	LookupUSDPrice  func(ctx context.Context, asset canonical.Asset) (string, bool)
	IsKnownSAC      func(contractID string) bool
	LakeWatermark   func(ctx context.Context) (ledger uint32, stale bool, ok bool)
	ParseLimit      func(w http.ResponseWriter, r *http.Request, def, maxN int) (int, bool)
	ParseWindowDays func(w http.ResponseWriter, r *http.Request, def int) (int, bool)

	WriteJSON func(w http.ResponseWriter, data any, stale bool)
	// WriteJSONAt is WriteJSON with an explicit envelope as_of, so a degraded stale-snapshot
	// response carries the snapshot's REAL computation time, not now(). degraded marks a
	// carried-forward body that no shared cache may keep. Optional: nil falls back to WriteJSON.
	WriteJSONAt   func(w http.ResponseWriter, data any, stale, degraded bool, asOf time.Time)
	WriteProblem  func(w http.ResponseWriter, r *http.Request, typeURL, title string, status int, detail string)
	ClientAborted func(r *http.Request, err error) bool

	// opsDir is the short-TTL, stale-serving cache for the /v1/operations directory first page
	// (rationale on opsDirCache in operations.go). Zero value is ready to use.
	opsDir opsDirCache

	// opTypeStats caches the trailing-24h op-type breakdown SEPARATELY from the directory cache:
	// it summarises a 24-hour window over a 34B-row table, so recomputing it on the directory's
	// few-second cadence blew the /v1/operations read deadline under load.
	opTypeStats opTypeStatsCache

	// assetHolders and contractsDir are the bounded-TTL, single-flighted caches in front of the two
	// UNAUTHENTICATED reads whose cost scales with the lake, not the request (holders: two
	// current-state FINAL scans; contracts directory: a multi-day GROUP BY over contract_events).
	// Zero values are ready; see hot_reads.go.
	assetHolders assetHoldersCache
	contractsDir contractsDirCache
	// contractStats is the single-entry snapshot behind GET /v1/contracts/stats.
	contractStats contractStatsCache

	// contractDetail is the shared bounded-TTL, single-flighted cache in front of the per-contract
	// events / interactions / code-history reads: inline on the request deadline, a busy contract
	// would time out on every request and no retry could land warm. See contract_detail_cache.go.
	contractDetail contractDetailCache

	// sacNames memoises proven SAC resolutions across requests (movements.go).
	sacNames sacNameMemo

	// throughput is the single-entry, single-flighted cache in front of GET /v1/network/throughput,
	// a FINAL scan over up to a year of stellar.ledgers that would miss the 8s request budget.
	// See network_throughput_cache.go.
	throughput networkThroughputCache

	// attribution memoises the contract → protocol registry map read by the
	// contracts directory, interactions and detail routes. Zero value ready.
	attribution contractAttributionCache

	// refreshGate bounds DETACHED cache refreshes globally across keys AND cache kinds. Per-key
	// single-flight alone leaves the key space attacker-chosen on these unauthenticated routes,
	// so fabricated keys could queue one unbounded lake scan each. Resolved lazily by
	// detachedGate(), shared with the lake reader's account-state gate so the whole explorer
	// surface has ONE bound.
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
func (h *Handler) writeJSONAt(w http.ResponseWriter, data any, stale, degraded bool, asOf time.Time) {
	if h.WriteJSONAt != nil && (!asOf.IsZero() || degraded) {
		h.WriteJSONAt(w, data, stale, degraded, asOf)
		return
	}
	h.WriteJSON(w, data, stale)
}

// lakeTip is LakeWatermark with an unwired func reading as no lake to judge
// (not stale, no ledger), the same verdict the server gives an unwired reader.
func (h *Handler) lakeTip(ctx context.Context) (ledger uint32, stale, ok bool) {
	if h.LakeWatermark == nil {
		return 0, false, false
	}
	return h.LakeWatermark(ctx)
}

// unavailable writes the standard 503 when no explorer reader is wired (no ClickHouse, or it was
// unreachable at startup). Mirrors v1's Server.explorerUnavailable, which other v1 handlers use.
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

// encodeCursor renders a keyset position as an opaque dotted-decimal ?cursor= ("63000000.4.7").
// The component count matches the listing's ORDER BY arity (2 account txs; 3 account ops and
// contract events).
func encodeCursor(parts ...uint32) string {
	ss := make([]string, len(parts))
	for i, p := range parts {
		ss[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(ss, ".")
}

// parseExplorerCursor decodes the optional ?cursor= into an ExplorerCursor with exactly `parts`
// components; absent is the zero cursor (first page). ok=false (after a problem+json) on a
// malformed value or a zero ledger, since a real cursor always points past an actual row.
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
