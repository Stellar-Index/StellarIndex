// Package explorer holds the network-explorer endpoints (ADR-0038): ledgers, operations, network
// throughput, tx, search, contracts, accounts and asset holders under /v1.
//
// Handlers read the certified ClickHouse lake through ExplorerReader and otherwise depend only on
// narrow injected seams (Handler below). This package cannot import internal/api/v1, whose
// v1.Server wires a *Handler into its routes (cycle); v1 keeps aliases for the exported types.
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

// explorerReadTimeout is the per-request ceiling on every handler's ClickHouse lake reads: the pool has 8 connections and WriteTimeout
// does not cancel an in-flight query. It fires before the 15s request-timeout middleware.
const explorerReadTimeout = 8 * time.Second

// readTimedOut reports whether a handler-scoped read ctx hit its deadline. Pass the PER-CALL ctx, not r.Context(): the driver may return
// an error not unwrapping to DeadlineExceeded. Keep in sync with v1's handlerTimedOut (import cycle), incl. the usage-metering verdict.
func readTimedOut(callCtx context.Context, err error) bool {
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		callCtx.Err() == context.DeadlineExceeded
	if timedOut {
		middleware.MarkReadDeadline(callCtx)
	}
	return timedOut
}

// retryableColdMiss reports whether a cold-path SWR error is the capacity class mapped to the retryable 503: deadline expired, or the
// refresh gate saturated (both this package's sentinel and the clickhouse layer's).
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

// lakeUnreachable reports whether err is a transport failure below the query layer (refused, reset, DNS): "dependency down, retry".
// A hard-down lake gives a non-timeout *net.OpError that would read as a 500 in the 5xx SLA probe. Keep in step with IsCacheUnavailable.
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

// writeReadTimeout writes the 503 problem+json, with an endpoint-specific `...-timeout` type URL, for a read that blew explorerReadTimeout.
// 503 not 504 (the API's server-side read deadline code); a 500 would make a capacity problem look like a bug.
func (h *Handler) writeReadTimeout(w http.ResponseWriter, r *http.Request, typeURL, title string) {
	// Also reached by a driver i/o timeout, which readTimedOut does not see.
	middleware.MarkReadDeadline(r.Context())
	h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
		"the ClickHouse lake read didn't return within the "+explorerReadTimeout.String()+
			" explorer read budget; retry shortly")
}

// writeRetryable is writeReadTimeout with an honest split by cause: a saturation reject is not a timeout (the query never ran).
// Same 503 + type URL either way; only the detail differs.
func (h *Handler) writeRetryable(w http.ResponseWriter, r *http.Request, err error, typeURL, title string) {
	if errors.Is(err, errRefreshSaturated) || errors.Is(err, clickhouse.ErrRefreshSaturated) {
		w.Header().Set("Retry-After", retryAfterBusy)
		h.WriteProblem(w, r, typeURL, title, http.StatusServiceUnavailable,
			"the server's shared cold-page refresh capacity is saturated (many distinct cold pages "+
				"are being computed right now); this page's data was NOT computed on this request — "+
				"retry in a few seconds")
		return
	}
	// Third cause, same 503: the lake is unreachable. Saying "timed out" would send an operator
	// hunting a slow query that never ran. Logged here because call sites omit err.
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

// Retry-After hints on the retryable 503s; without one clients retry at once, a storm against a saturated gate or downed dependency.
// Mirrors package v1: 30s for an outage (a Redis/CH fail-over window), a few seconds for in-process busy-ness.
const (
	retryAfterBusy           = "5"
	retryAfterDependencyDown = "30"
)

// ExplorerReader is the seam the explorer endpoints read the certified ClickHouse lake through, satisfied by *clickhouse.ExplorerReader;
// nil disables the endpoints (503).
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
	// the FINAL scan on the caller's deadline. An entry past its TTL is still served (flagged
	// degraded), so failed refreshes yield old-but-real data, not 503s. ok=false means never
	// computed: render a warming state, never fall back to AccountsByWealth on the request path
	// (the scan needs 11-20s against an 8s deadline). Basis ("usd" | "native_xlm") labels the
	// ranking's unit when no USD price map was available.
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
	// EntryIndexWatermark is the ledger ch-entry-index has built the entry indexes through (0 = not built).
	EntryIndexWatermark(ctx context.Context) (uint32, error)
	// EntryHistoryWatermark is the ledger ch-entry-history has derived the sponsorship lifecycles through (0 = never derived).
	EntryHistoryWatermark(ctx context.Context) (uint32, error)
	// AccountEntryLedgers lists, newest first, the indexed ledgers at or below maxLedger for the raw account key.
	AccountEntryLedgers(ctx context.Context, account string, roles []string, maxLedger uint32, limit int) ([]uint32, error)
	// LiveClaimableBalancesInLedgers returns the still-unclaimed balances naming the raw account changed in the ledgers.
	LiveClaimableBalancesInLedgers(ctx context.Context, account string, ledgers []uint32, maxKeys int) (clickhouse.ClaimableScan, error)
	// ClaimableIndexWatermark is the ledger the claimable index is complete through (0 = not built).
	ClaimableIndexWatermark(ctx context.Context) (uint32, error)
	// IndexedClaimableBalances returns at most maxKeys unclaimed balances naming the raw account, from the claimable index.
	IndexedClaimableBalances(ctx context.Context, account string, maxKeys int) (clickhouse.ClaimableScan, error)
	// SignerIndexWatermark is the ledger the entry index's signer role is complete through (0 = not built).
	SignerIndexWatermark(ctx context.Context) (uint32, error)
	// AccountsSignedBy returns the accounts changed in the ledgers whose current signers hold the raw ed25519 key.
	AccountsSignedBy(ctx context.Context, key string, ledgers []uint32, maxAccounts int) (clickhouse.SignerScan, error)
	// AccountSponsorships pages the sponsorship arrangements in which the account is the side's party (sponsor or
	// sponsored), each with its events through maxLedger; inForceOnly keeps the ones whose last event is a start.
	AccountSponsorships(ctx context.Context, account, side string, inForceOnly bool, after clickhouse.SponsorshipKey, maxLedger uint32, limit int) ([]clickhouse.SponsorshipArrangement, clickhouse.SponsorshipKey, error)
	// SponsorshipTotals counts one side of the account's arrangements through maxLedger.
	SponsorshipTotals(ctx context.Context, account, side string, maxLedger uint32) (clickhouse.SponsorshipTotals, error)
	// AssetEntryLedgers lists, newest first, the indexed ledgers at or below maxLedger for the asset.
	AssetEntryLedgers(ctx context.Context, asset string, entryTypes []string, maxLedger uint32, limit int) ([]uint32, error)
	// AssetTrustlineDaily returns, oldest first, the UTC days through maxLedger with trustline creations or removals.
	AssetTrustlineDaily(ctx context.Context, asset string, maxLedger uint32) ([]clickhouse.AssetTrustlineDay, error)
	// StreamEntryChangesForLedgers streams the listed ledgers' entry-change source rows in the projector's order.
	StreamEntryChangesForLedgers(ctx context.Context, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error
	// StreamAccountEntryChangesForLedgers streams the subset of those rows whose (op, entry key) group carries the raw
	// 32-byte key: an account's, an issuer's or a pool id.
	StreamAccountEntryChangesForLedgers(ctx context.Context, account string, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error
	// Cap67MovementsWatermark is the highest ledger the cap67 movement derive has completed
	// through (0 when not provisioned). The movements merge floors its Postgres arm at watermark+1
	// and ceilings the ClickHouse arm at the watermark: gap-free, no double count. Cached (~60s).
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
	ContractActivitySummaryFor(ctx context.Context, contractID string, days int, sinceLedger uint32) (clickhouse.ContractActivitySummary, bool, error)
}

// ContractsReader is the narrow read seam onto the protocol_contracts registry (ADR-0035): contract_id to owning protocol.
type ContractsReader interface {
	ProtocolContractIndex(ctx context.Context) (map[string]string, error)
}

// Handler holds every explorer endpoint's dependencies; response writing and cross-cutting reads are injected function values, since
// importing package v1 would cycle.
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

	// TradesArchive, when non-nil, serves GET /v1/accounts/{g}/trades below the Postgres compression horizon from
	// stellar.trades_by_account. Nil keeps the horizon floor (lower_bound=true).
	TradesArchive TradesArchiveReader

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

	// opTypeStats caches the trailing-24h op-type breakdown separately from the directory cache:
	// it scans a huge table, so the directory's few-second cadence would blow the read deadline.
	opTypeStats opTypeStatsCache

	// assetHolders and contractsDir are bounded-TTL, single-flighted caches in front of two unauthenticated reads whose cost scales with
	// the lake (FINAL scans; a multi-day GROUP BY). See hot_reads.go.
	assetHolders assetHoldersCache
	contractsDir contractsDirCache
	// contractStats is the single-entry snapshot behind GET /v1/contracts/stats.
	contractStats contractStatsCache

	// contractDetail is the single-flighted cache in front of the per-contract reads; inline, a
	// busy contract would time out every time and no retry could land warm.
	contractDetail contractDetailCache

	// sacNames memoises proven SAC resolutions across requests (movements.go).
	sacNames sacNameMemo

	// throughput is the single-flighted cache in front of GET /v1/network/throughput, a FINAL scan
	// over up to a year of stellar.ledgers that would miss the 8s budget.
	throughput networkThroughputCache

	// attribution memoises the contract → protocol registry map read by the
	// contracts directory, interactions and detail routes. Zero value ready.
	attribution contractAttributionCache

	// refreshGate bounds DETACHED cache refreshes globally across keys AND cache kinds: per-key single-flight alone leaves the key space
	// attacker-chosen on unauthenticated routes. Resolved lazily by detachedGate(), shared with the lake reader's account-state gate.
	refreshGateOnce sync.Once
	refreshGate     *clickhouse.RefreshGate
}

// detachedGate returns the shared bound for detached refreshes: the lake reader's own gate when it exposes one, else a handler-local gate
// with the same limit (test stubs).
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

// writeJSONAt writes data with an explicit envelope as_of when the seam is wired, else plain WriteJSON (as_of = now), so test handlers that
// only wire WriteJSON keep working.
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

// parseUint32Query parses an optional uint32 query param (e.g. ?before=): 0 when absent; ok=false (after a problem+json) on a malformed
// value.
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

// encodeCursor renders a keyset position as an opaque dotted-decimal ?cursor= ("63000000.4.7"); the component count matches the listing's
// ORDER BY arity (2 account txs; 3 account ops and contract events).
func encodeCursor(parts ...uint32) string {
	ss := make([]string, len(parts))
	for i, p := range parts {
		ss[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(ss, ".")
}

// parseExplorerCursor decodes the optional ?cursor= into an ExplorerCursor with exactly `parts` components; absent is the zero cursor.
// ok=false (after a problem+json) on a malformed value or zero ledger, since a real cursor points past an actual row.
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
