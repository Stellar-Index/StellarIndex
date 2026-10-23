package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/holds"
	"github.com/Stellar-Index/StellarIndex/internal/incidents"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/version"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// ReadyChecker is what /readyz polls; Ping MUST respect ctx (shared 2 s deadline) and Critical() separates "cannot serve" (Postgres) from "degrades" (Redis), so only a critical failure returns 503.
type ReadyChecker interface {
	Ping(ctx context.Context) error
	Name() string
	Critical() bool
}

// ExpectedSchemaVersion is the migrations/ head this binary was built against; readiness fails when applied < expected (a NEWER schema is safe), and a test fails CI if a migration is added unbumped.
const ExpectedSchemaVersion uint = 222

// nonAtomicMigrationVersions lists migrations whose up.sql commits mid-file. A dirty flag at any other version means a clean rollback
// (schema intact at version-1).
var nonAtomicMigrationVersions = map[uint]bool{30: true}

// SchemaVersionReader reports the applied golang-migrate state (version + dirty), adapted from *timescale.Store in cmd/stellarindex-api.
type SchemaVersionReader interface {
	// SchemaMigrationVersion returns the highest applied migration and whether its row is dirty.
	// version==0 means none applied.
	SchemaMigrationVersion(ctx context.Context) (version uint, dirty bool, err error)
}

// schemaVersionChecker is the critical ReadyChecker that asserts the
// applied schema is at least [ExpectedSchemaVersion] and not dirty.
type schemaVersionChecker struct {
	reader   SchemaVersionReader
	expected uint
}

// NewSchemaVersionChecker builds the critical schema-head readiness check, so a binary/schema mismatch drains the backend instead of serving partial data.
func NewSchemaVersionChecker(reader SchemaVersionReader) ReadyChecker {
	return schemaVersionChecker{reader: reader, expected: ExpectedSchemaVersion}
}

func (c schemaVersionChecker) Name() string   { return "schema" }
func (c schemaVersionChecker) Critical() bool { return true }

func (c schemaVersionChecker) Ping(ctx context.Context) error {
	version, dirty, err := c.reader.SchemaMigrationVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema_migrations version: %w", err)
	}
	if dirty && nonAtomicMigrationVersions[version] {
		return readyzPublicError{fmt.Sprintf("schema_migrations is dirty at version %d: this migration is not atomic (explicit mid-file COMMIT) and its schema state cannot be inferred — the binary must not serve against a half-migrated schema", version)}
	}
	// Other migrations are one transaction, so dirty means a clean rollback to version-1; the row
	// still needs an operator `force`, reported non-critically by schemaDirtyChecker.
	effective := version
	if dirty && version > 0 {
		effective = version - 1
	}
	if effective < c.expected {
		return readyzPublicError{fmt.Sprintf("schema/binary mismatch: binary expects migrations head >= %d but only %d is applied (dirty=%v) — a deploy skipped migrations or the binary was swapped ahead of them", c.expected, effective, dirty)}
	}
	return nil
}

// schemaDirtyChecker flags a dirty schema_migrations row as readyz "degraded" without draining the backend: for atomic migrations dirty
// means a clean rollback.
type schemaDirtyChecker struct {
	reader SchemaVersionReader
}

// NewSchemaDirtyChecker builds the non-critical dirty-row visibility check; register it beside [NewSchemaVersionChecker] on the same reader.
func NewSchemaDirtyChecker(reader SchemaVersionReader) ReadyChecker {
	return schemaDirtyChecker{reader: reader}
}

func (c schemaDirtyChecker) Name() string   { return "schema-dirty" }
func (c schemaDirtyChecker) Critical() bool { return false }

func (c schemaDirtyChecker) Ping(ctx context.Context) error {
	version, dirty, err := c.reader.SchemaMigrationVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema_migrations version: %w", err)
	}
	if dirty {
		return readyzPublicError{fmt.Sprintf("schema_migrations is dirty at version %d — run `migrate force` once confirmed; the applied schema is otherwise intact and serving continues", version)}
	}
	return nil
}

// OpenBucketCAGGReader lists the continuous aggregates that serve their in-progress bucket outside the real-time allowlist; *timescale.Store satisfies it.
type OpenBucketCAGGReader interface {
	OpenBucketCAGGs(ctx context.Context) ([]string, error)
}

// closedBucketChecker is the critical ReadyChecker for ADR-0015: an out-of-band ALTER of materialized_only would serve the open bucket
// beside a guarded /v1/price.
type closedBucketChecker struct {
	reader OpenBucketCAGGReader
}

// NewClosedBucketChecker builds the critical materialized_only readiness check, since two money surfaces disagreeing on one pair must drain the backend.
func NewClosedBucketChecker(reader OpenBucketCAGGReader) ReadyChecker {
	return closedBucketChecker{reader: reader}
}

func (c closedBucketChecker) Name() string   { return "closed_buckets" }
func (c closedBucketChecker) Critical() bool { return true }

func (c closedBucketChecker) Ping(ctx context.Context) error {
	views, err := c.reader.OpenBucketCAGGs(ctx)
	if err != nil {
		return fmt.Errorf("read continuous aggregate materialized_only: %w", err)
	}
	if len(views) > 0 {
		return fmt.Errorf("continuous aggregates serving the open bucket (materialized_only = false): %s — restore with ALTER MATERIALIZED VIEW <view> SET (timescaledb.materialized_only = true)", strings.Join(views, ", "))
	}
	return nil
}

// Server is the thread-safe HTTP handler for the v1 API; [New] returns it with routes mounted and [Server.Handler] yields the http.Handler.
type Server struct {
	// Options is the construction-time wiring; the fields below are state derived from it.
	Options
	logger         *slog.Logger
	rwaCuratedSnap rwaCuratedCache
	// assetListings memoises one read of the listing directory for the /v1/assets listing-priced
	// valuation arm; not shared with rwaListings, which serves contract rows only.
	assetListings assetListingCache
	// holds is the operator under-review list (SetHolds).
	holds atomic.Pointer[[]holds.Hold]
	// Cached lake watermark, see lakeWatermark(). lakeWMMu is never held across the lake read (a
	// non-context-aware mutex would queue callers past their deadlines); lakeWMNextTry is the
	// negative cache, so a wedged lake is retried once per lakeWatermarkRetryGap, not per request.
	lakeWMMu       sync.Mutex
	lakeWMLedger   uint32
	lakeWMClosedAt time.Time
	lakeWMFetched  time.Time
	lakeWMNextTry  time.Time
	lakeWMFlight   singleflight.Group
	// Shared /v1/ledger/stream tip read — see ledgerStreamTip.
	ledgerStreamMu     sync.Mutex
	ledgerStreamView   LedgerTipView
	ledgerStreamOK     bool
	ledgerStreamAt     time.Time
	ledgerStreamFlight singleflight.Group
	// priceReadFlight coalesces concurrent readPriceWithAliasesServed calls for the same
	// (asset, quote) pair, so a burst of identical price requests for a hot pair costs one DB read.
	priceReadFlight singleflight.Group
	// batchRowFlight coalesces identical in-flight /v1/price/batch rows across requests (see resolveBatchRowShared).
	batchRowFlight singleflight.Group
	// Cached top-N native (CAP-38) LP listing: a whole-prefix lake scan (~40k pools) ranked in Go.
	// nativeLPMu is NEVER held across the scan, or callers queue behind it after each TTL lapse.
	// nativeLPFillMu collapses COLD fills onto one scan; nativeLPRefreshing admits one rescan.
	nativeLPMu         sync.Mutex
	nativeLPCached     []LiquidityPoolReservesRow
	nativeLPAll        []clickhouse.NativeLiquidityPoolState
	nativeLPFetched    time.Time
	nativeLPFillMu     sync.Mutex
	nativeLPRefreshing atomic.Bool
	// protocolFast{Mu,Settled,OK} cache the daily-pre-aggregation probe only once definitive (see
	// fastActivity); a sync.Once would latch a transient first-probe error for the process lifetime.
	protocolFastMu      sync.Mutex
	protocolFastSettled bool
	protocolFastOK      bool
	protocolPoolTokens  ProtocolPoolTokensReader
	// TTL + single-flight cache for the expensive /v1/protocols/{name} detail (see cachedProtocolDetail).
	protoDetailMu     sync.Mutex
	protoDetailCache  map[string]protoDetailEntry
	protoDetailFlight map[string]chan struct{}
	// Last-good cache for the bespoke analytics block of that detail view, which is built last and
	// would otherwise vanish when the rebuild's budget runs out. See protocol_bespoke_cache.go.
	protocolBespokeCache bespokeCache
	// Prewarmed SWR cache for the per-source contract_count on GET /v1/protocols. The roster is a
	// served-tier `SELECT DISTINCT ... LIMIT 5000` scan on an unauthenticated route, so scanning on
	// every origin miss would be a DoS surface.
	protocolRosterCache rosterCache
	// Per-pool built reserves views for GET /v1/lending/pools/{pool}/reserves; see buildPoolReserves.
	lendingReserves *swrCache[poolReserves]
	// SAC contract id → the asset_id it wraps, and → expiry of a "not a SAC" answer; see sacWrappedAssetID.
	sacWrapped    sync.Map
	sacNotWrapped sync.Map
	// TTL + single-flight cache for the classic circulating-supply map (one ~0.5s ClickHouse
	// GROUP BY over the trustline slice, see cachedClassicSupply); backs market-cap fill on /v1/assets.
	classicSupplyMu     sync.Mutex
	classicSupplyCache  map[string]string
	classicSupplyAt     time.Time
	classicSupplyFlight chan struct{}
	// classicSupplyAttemptAt advances on every attempt, unlike classicSupplyAt (success only), so a
	// failing refresh retries once per classicSupplyRetryGap, not per request at full timeout.
	classicSupplyAttemptAt time.Time
	// Per-asset TTL cache for the lake-flows classic supply, which includes the claimable-balance,
	// LP-reserve and SAC-held supply the trustline sum cannot see. Filled in bounded batches by a
	// detached refresh; see classic_lake_supply.go.
	lakeSupplyMu        sync.Mutex
	lakeSupply          map[string]lakeSupplyEntry
	lakeSupplyFlight    chan struct{}
	lakeSupplyAttemptAt time.Time
	// SWR cache for the SEP-1 logo map ([buildSep1ImageMap]), built from every verified issuer's
	// sep1_payload in one scan; see cachedSep1Images. sep1ImagesAt drives freshness;
	// sep1ImagesAttemptAt rate-limits a failing scan.
	sep1ImagesMu        sync.Mutex
	sep1ImagesCache     map[string]string
	sep1ImagesAt        time.Time
	sep1ImagesAttemptAt time.Time
	sep1ImagesFlight    chan struct{}

	// RWA membership set: the (code, issuer) pairs meeting the internal/rwa definition, rebuilt
	// off the request path behind a TTL + single-flight. It holds no valuation.
	//
	// rwaAttemptAt rate-limits a failing rebuild (see [Server.readRWAMembership]); rwaFailedAt,
	// cleared by the next success, tells a lapsed set a rebuild will replace from one nothing is.
	rwaMu        sync.Mutex
	rwaCache     *rwaMembership
	rwaAt        time.Time
	rwaAttemptAt time.Time
	rwaFailedAt  time.Time
	rwaFlight    chan struct{}
	// Oracle reference snapshot the premium/discount is measured against. Shorter-lived than the
	// membership cache because a stale NAV against a live price reports a premium neither supports.
	rwaRefMu     sync.Mutex
	rwaRefCache  *rwaReferences
	rwaRefAt     time.Time
	rwaRefFlight chan struct{}
	// Assembled /v1/rwa/assets view: the per-issuer listing reads and valuation behind it take
	// seconds, so readers get the last build while the next one runs.
	rwaViewMu      sync.Mutex
	rwaViewCache   *rwaAssetsBuild
	rwaViewExpires time.Time
	rwaViewErr     error
	rwaViewFlight  chan struct{}
	// RWA value-over-time assembly (/v1/rwa/history). Cached on the membership cadence: a daily
	// series cannot move faster than the oracle publishes, and both legs are scans.
	rwaHistMu     sync.Mutex
	rwaHistCache  *rwaValueHistory
	rwaHistAt     time.Time
	rwaHistFlight chan struct{}
	// RWA premium-over-time assembly (/v1/rwa/premium). Separate from the value history: they do
	// not share the market leg, and one failing must not blank the other.
	rwaPremMu     sync.Mutex
	rwaPremCache  *rwaPremiumHistory
	rwaPremAt     time.Time
	rwaPremFlight chan struct{}

	// contractIndex caches contract → protocol for cohort labels; see contract_protocol_index.go.
	contractIndex contractProtocolIndex
	// fxCrossMaxAge bounds how old the forex snapshot's matched rate may be before
	// [Server.tryFiatCrossRate] / [Server.tryUSDAnchoredFiatCross] refuse it; see fxCrossStale.
	fxCrossMaxAge time.Duration
	// fxFixings binds closed-bucket FX legs; nil leaves those crosses unserved.
	fxFixings       *fxFixingCache
	explorerHandler *explorerpkg.Handler // network-explorer endpoints (ADR-0038); see explorer.go

	// readyz single-flight cache (see handleReadyz). readyzFlight is non-nil while a round runs;
	// waiters select on it or their own ctx, so readyzMu is never held across probe IO.
	readyzMu     sync.Mutex
	readyzAt     time.Time
	readyzCode   int
	readyzBody   []byte
	readyzFlight chan struct{}

	// livez/lake single-flight cache — see handleLivezLake.
	// livezLakeFlight mirrors readyzFlight.
	livezLakeMu     sync.Mutex
	livezLakeAt     time.Time
	livezLakeCode   int
	livezLakeBody   []byte
	livezLakeFlight chan struct{}
	incidents       []incidents.Incident
	// streamDrain is the shutdown broadcast every SSE writer watches beside its request context;
	// the http.Server owner hands it to RegisterOnShutdown, see [Server.BeginStreamDrain].
	streamDrain *streaming.Drain
	// streamRevalidate re-runs the auth/policy/quota gates for an open
	// stream at each heartbeat; see [Server.buildStreamRevalidator].
	streamRevalidate func(*http.Request) bool
	// tipProducers is the shared tip-stream producer registry: one compute loop per distinct
	// (asset, quote, window), refcounted by open /v1/price/tip/stream connections.
	tipProducers tipProducerRegistry
	// tipDivergenceStalls rate-limits the warning for a tip-stream divergence lookup that exceeds
	// its sub-budget. Process-wide because the stalling store is shared by every stream.
	tipDivergenceStalls tipDivergenceStallLog
	// streamSecondFor overrides the length of one second of a stream's
	// window_seconds / interval_seconds cadence when > 0 (tests).
	streamSecondFor time.Duration
	// tipDivergenceBudgetFor overrides [tipStreamDivergenceBudget] when
	// > 0 (tests).
	tipDivergenceBudgetFor time.Duration
	backupMetrics          backupMetricsSource
	backups                backupsCache
	regionName             string
	regionDeployment       string
	statusServices         []string
	// sacReserveAssets maps a SAC contract C-strkey to the canonical classic/native asset it
	// wraps, built lazily from verifiedCurrencies (ADR-0039 lending TVL: a Blend reserve's
	// underlying is the asset's SAC, so pricing goes through this).
	sacReserveAssets   map[string]string
	sacReserveOnce     sync.Once
	coverageFloorCache *coverageFloorCache
	// globalPrice + globalPriceOpts power the /v1/assets/{slug} global view's three-tier fallback
	// chain. Nil-safe: unwired, handleGlobalAsset omits the price block but the slug still
	// resolves and networks[] still populates.
	globalPrice     aggregate.GlobalPriceReader
	globalPriceOpts aggregate.GlobalPriceOptions
	// knownSACs is the cached union of sac_wrappers + computed SAC ids
	// (native + verified catalogue), built once via knownSACsOnce.
	knownSACsOnce sync.Once
	knownSACs     map[string]struct{}
	// verifiedSACs maps computed SAC contract id to the native/verified asset it wraps. It excludes
	// operator sac_wrappers (which may name unverified assets) because it backs an identity trust
	// decision, the oracle ticker gate in oracle.go.
	verifiedSACsOnce sync.Once
	verifiedSACs     map[string]canonical.Asset
	// assetDetailCache is the short-TTL response cache for /v1/assets/{id}. A hit skips the handler
	// chain, including the uncached DB calls in applyF2Fields, and cannot drift because the entry
	// is what the handler produced. Nil or ttl=0 always misses.
	assetDetailCache *assetDetailResponseCache
	// assetListCache is the response-level cache for the default
	// /v1/assets listing; see [assetListResponseCache].
	assetListCache *assetListResponseCache
	// pegDeclaredAt is the observed_at [Server.declaredPegSnapshot] stamps on the flat $1 answer: a
	// declaration is a constant, not an observation, so adoption time is the only honest time.
	pegDeclaredAt time.Time
	// ingestionSnapshot caches IngestionDiagnostics (refreshed ~15s by
	// [Server.StartIngestionSnapshotRefresh]); nil before the first refresh, then built inline.
	ingestionSnapshot atomic.Pointer[ingestionSnapshotEntry]
	mux               *http.ServeMux
	// publicRoutes records every route mounted via handlePublic; Handler
	// wires its Mark middleware outside Auth.
	publicRoutes *middleware.PublicRoutes
	started      time.Time
	// requestTimeout bounds every non-streaming request's context via the RequestTimeout
	// middleware, the chokepoint giving every handler a deadline; zero only under
	// Options.DisableRequestTimeout.
	requestTimeout time.Duration
}

// defaultRequestTimeout is the per-request deadline when Options.RequestTimeout is unset: above the per-read 8s ceilings, under the 30s
// WriteTimeout.
const defaultRequestTimeout = 15 * time.Second

// maxHandlerBudget caps a per-handler context.WithTimeout: at or above the global deadline it never fires, making the handler's 503
// branch unreachable.
// Pinned by TestSourceShapeRules/handler_budgets_stay_inside_request_timeout and TestMaxHandlerBudgetMatchesConfigBound.
const maxHandlerBudget = defaultRequestTimeout - 3*time.Second

// DashboardAuthMounter is satisfied by main.go's dashboardauth.Handlers, defined here so this package does not import dashboardauth.
type DashboardAuthMounter interface {
	// Mount registers the login entry points through public so they
	// answer without an API key or SEP-10 JWT under every auth_mode.
	Mount(mux *http.ServeMux, public *middleware.PublicRoutes)
}

// Options configures a [Server] at construction.
type Options struct {
	Logger *slog.Logger
	// Network is the Stellar network served (pubnet / testnet / futurenet; empty = pubnet).
	// /v1/coverage uses it to report sources that do not exist here instead of counting them
	// incomplete; reference listings use it to omit pubnet-only entries.
	Network string
	// ReadyChecks are polled by /readyz. Order matters only for
	// log output (first-failed wins).
	ReadyChecks []ReadyChecker
	// Assets, when non-nil, backs /v1/assets and /v1/assets/{id}; nil serves an empty list and
	// echoes the canonical id on single lookups.
	Assets AssetReader
	// Prices, when non-nil, backs /v1/price. Nil returns 503; the handler is mounted either way
	// so clients can integrate against the wire contract before a reader is wired.
	Prices PriceReader

	// History, when non-nil, backs /v1/history. Leave nil to return
	// 503 on that path.
	History HistoryReader

	// CoverageFloor, when non-nil, lets the empty-window surfaces (/v1/ohlc, /v1/history,
	// /v1/chart, /v1/price/at) say when this deployment's history for a pair begins, so an empty
	// answer can be told from a quiet market. Nil omits `coverage_from` and
	// `flags.outside_coverage` (both omitempty).
	CoverageFloor CoverageFloorReader

	// Markets, when non-nil, backs /v1/markets; nil serves an empty list.
	Markets MarketsReader

	// Oracle, when non-nil, backs /v1/oracle/latest. Leave nil to
	// return 503 on that path.
	Oracle OracleReader
	// OracleHistory, when non-nil, backs the price leg of /v1/rwa/history. Nil publishes no series
	// and says so in `basis`, since an empty chart would read as a finding about the sector.
	OracleHistory RWAOracleHistoryReader
	// MarketHistory, when non-nil, backs the market leg of /v1/rwa/premium (the observed daily
	// dollar VWAP). Nil publishes no series, for [OracleHistory]'s reason: a premium missing a
	// leg is no series.
	MarketHistory RWAMarketHistoryReader
	// RWAPremiumSubstance is the thin-market floor /v1/rwa/premium's day-clearing check uses; it
	// must be the same policy as the live [pricingguard.SubstanceGate] (pass
	// substanceGate.Policy()). Zero falls back to pricingguard defaults.
	RWAPremiumSubstance pricingguard.SubstancePolicy
	// Sep1Cache, when non-nil, enables the SEP-1 overlay on /v1/assets/{id} from
	// `issuers.sep1_payload`, populated by `stellarindex-ops sep1-refresh`. Cron-populated, not
	// fetched live, because a live HTTPS fetch dominated the endpoint's p95.
	Sep1Cache Sep1CachedReader

	// CORS, when non-nil, sits above RateLimit so preflight OPTIONS requests short-circuit before
	// the rate-limit counter increments. Built via middleware.CORS with cfg.API.AllowedOrigins.
	CORS middleware.Middleware

	// Accounts, when non-nil, backs POST /v1/account/keys (key issuance); nil returns 503. The GET
	// endpoints (/me, /usage) only read the request-context Subject. Wire only when Redis is
	// reachable; auth.NewRedisAPIKeyStore enforces that.
	Accounts AccountStore

	// AccountKeyQuota caps un-revoked API keys one caller may hold via POST /v1/account/keys, so
	// minting cannot fill Redis. Zero selects [defaultAccountKeyQuota]; negative disables it. Flat,
	// not tiered: Redis-validated records have no account row to read a tier from.
	AccountKeyQuota int

	// PlatformAccounts, when non-nil, backs the operator tier-override endpoints
	// (GET/PATCH /v1/admin/accounts/{id}); it is the store the key validator reads overrides from,
	// so an override applies on the next key lookup. Nil returns 503.
	PlatformAccounts PlatformAccountStore

	// PlatformUsers, when non-nil, lets PATCH /v1/admin/accounts/{id} revoke every dashboard
	// session of an account it closes; nil leaves that to the dashboard middleware's status gate.
	PlatformUsers AccountSessionRevoker

	// RegisterAccounts, when non-nil, backs POST /v1/register, which creates a free-tier account
	// and mints its first Postgres-backed key (APIKeyBudgets.Platform too, or the endpoint 503s).
	// It is the PlatformAccounts store narrowed to create-only. SignupIPThrottle gates it.
	RegisterAccounts RegisterAccountCreator

	// StatusNotices, when non-nil, backs the operator status-banner endpoints
	// (/v1/admin/status-notices) and the public GET /v1/status/notices. Nil makes the public list
	// return `[]` and the admin endpoints 503.
	StatusNotices StatusNoticeStore

	// Audit, when non-nil, receives persisted audit rows for admin actions (key mint, account
	// overrides, status-notice mutations). Nil degrades to log-only audit; the mutation always logs.
	Audit AuditSink

	// SignupIPThrottle, when non-nil, applies a per-IP cap to /v1/register; the anonymous global
	// bucket (60/min) would let one IP mint ~3,600 keys an hour. Typically 5/hour per IP in Redis.
	SignupIPThrottle SignupIPThrottle

	// APIKeyBudgets are the credential stores a tier change must clamp: the enforced budget lives
	// on the key record, so a tier-only write would leave a demoted account's keys at the old limit.
	// Zero value disables the clamp; the endpoint then reports keys_clamped=0.
	APIKeyBudgets APIKeyBudgetStores

	// Divergence, when non-nil, is consulted by /v1/price after a successful LatestPrice lookup;
	// a fired warning sets flags.divergence_warning=true. Nil leaves the flag false.
	Divergence DivergenceLooker

	// Freeze, when non-nil, is consulted by /v1/price (and batch) after a successful LatestPrice
	// lookup; a frozen pair gets flags.frozen and flags.single_source (anomaly.ActionFreeze). Nil
	// leaves frozen false and derives single_source from the observation count.
	Freeze FrozenLooker

	// Substance, when non-nil, is the serving-side thin-market gate ([PriceSubstanceGate]).
	// The server consults it on price paths that bypass PriceReader (the tip rolling-window
	// VWAP); reader-backed paths carry the gate inside the readers. Nil disables handler-side
	// gating only.
	Substance PriceSubstanceGate

	// Scam, when non-nil, withholds the aggregated price for directory-scam-flagged issuers on the
	// paths the server gates directly (tip VWAP, VWAP, TWAP, chart, history, price stream);
	// reader-backed paths gate inside the readers. Production impl is pricingguard.ScamGate.
	Scam PriceScamGate

	// Supply, when non-nil, populates the supply fields (total, circulating, max, market_cap_usd,
	// fdv_usd, supply_basis) on /v1/assets/{id}; nil leaves them null. It still depends on another
	// process populating asset_supply_history.
	Supply SupplyLooker

	// TokenSupply, when non-nil, backs GET /v1/assets/{asset_id}/supply from the decode-at-ingest
	// supply_flows lake (ADR-0034): the raw mint-burn-clawback total for every token, unlike
	// Supply's ADR-0011 circulating/max policy over asset_supply_history. Nil returns 503.
	TokenSupply TokenSupplyReader

	// ContractStorageSupply, when non-nil, gives /v1/assets/{asset_id}/supply a second source for
	// tokens the event log cannot see: the sum of per-holder contract-storage balances. Consulted
	// only when the event-derived reading found no flows, never to adjust one (it would
	// double-count). Nil leaves a flowless token at zero.
	ContractStorageSupply ContractStorageSupplyReader

	// TokenDecimals, when non-nil, overlays on-chain `decimals()` onto /v1/assets/{id} for Soroban
	// tokens, from the lake's captured contract-instance metadata. Classic and native assets are
	// always 7 by protocol and never consult it. Nil keeps the 7 default.
	TokenDecimals TokenDecimalsReader

	// TokenSymbol, when non-nil, resolves a contract's on-chain SEP-41 symbol. Consulted ONLY by
	// the RWA contract arm, after the curated directory named that exact contract: the value is
	// contract-authored, so it says WHICH instrument an address holds, never WHETHER it is vouched
	// for. Nil admits only what the curated set binds.
	TokenSymbol TokenSymbolReader

	// RWAContracts, when non-nil, backs the contract arm of GET /v1/rwa/assets (curated-directory
	// candidates and the tokenless-entity census). Nil serves the classic arm alone and the funnel
	// says the contract population was NOT MEASURED, not empty.
	RWAContracts RWADirectoryContractReader

	// RWAListings, when non-nil, backs the second arm on GET /v1/rwa/assets: the independent
	// listing directory (`listing-sync`) corroborating a curated binding. Nil reports NOT MEASURED
	// and refuses bindings as independent_listing_unavailable, never as a finding that no
	// independent party names the addresses.
	RWAListings RWAListingDirectoryReader
	// RWACurated, when non-nil, backs the curated arm on GET /v1/rwa/assets: a named third
	// party's asset list and its own prices, served apart from the verified set.
	RWACurated RWACuratedDirectoryReader

	// Listings, when non-nil, backs the listing-priced valuation arm on GET /v1/assets and
	// /v1/assets/{asset_id} (the RWAListings directory, both address forms). Nil marks assets with
	// a withheld market cap `listing_valuation.status = "listing_unavailable"`: nobody looked, not
	// nobody lists the address.
	Listings AssetListingDirectoryReader

	// ContractCatalogue, when non-nil, reads catalogue rows for an explicit contract set, without
	// the volume gate: a tokenized fund can carry a nine-figure supply and never appear in a 24h
	// volume rollup. Nil reports admitted contracts as unobserved rather than serving them.
	ContractCatalogue ContractCatalogueReader

	// LakeWatermark, when non-nil, stamps lake-backed responses (supply, accounts, holders) with
	// `as_of_ledger` and sets `flags.stale` when the lake's captured tip trails now beyond
	// lakeStaleThreshold (ADR-0041 Decision 4). Nil omits the field and the flag never fires.
	LakeWatermark LakeWatermarkReader

	// Volume, when non-nil, populates `volume_24h_usd` on /v1/assets/{id}: trailing-24h USD
	// trade volume across every pair the asset participates in. Nil leaves it null,
	// independent of Supply.
	Volume VolumeReader

	// Change24h, when non-nil, populates `change_24h_pct` on /v1/assets/{id} (signed change vs
	// the USD price ~24h ago). Nil leaves it null. Independent of Supply and Volume.
	Change24h Change24hReader

	// PriceAt, when non-nil, backs GET /v1/price/at, the point-in-time closed-bucket VWAP.
	PriceAt PriceAtReader

	// ChangeSummary, when non-nil, backs GET /v1/changes/{entity_type}/{id} from the
	// change_summary_5m hypertable the changesummary worker populates; it powers the explorer's
	// multi-window delta strips. Nil returns 503.
	ChangeSummary ChangeSummaryReader

	// AssetsReader, when non-nil, supplies the catalogue overlay (price, volume, market_cap,
	// sparkline, ATH, top_markets) every /v1/assets row sources through this seam. Nil makes the
	// affected fields 503.
	AssetsReader AssetsReader

	// TransitivePricer, when non-nil, supplies a one-hop USD price for assets the catalogue's two
	// shapes (direct_usd, asset_vs_xlm) cannot reach, notably Soroban-native contract assets,
	// which never appear in `classic_assets`. Nil leaves pricing unchanged.
	TransitivePricer TransitivePricer

	// Issuers, when non-nil, backs GET /v1/issuers/{g_strkey}; nil returns 503.
	Issuers IssuersReader

	// SEP41Transfers, when non-nil, backs GET /v1/contracts/{contract_id}/transfers, which in
	// turn backs per-account net-position queries. Nil returns 503.
	SEP41Transfers SEP41TransfersReader

	// Cursors, when non-nil, backs GET /v1/diagnostics/cursors, the operator diagnostic behind
	// the explorer /diagnostics page. Nil returns 503.
	Cursors CursorsReader

	// CoverageReader, when non-nil, backs the shadow data-derived density on
	// /v1/diagnostics/ingestion from source_coverage_snapshots. Nil leaves DensityPctV2 /
	// GapFreePct zero; cursor-derived density stays the authoritative signal.
	CoverageReader SourceCoverageReader

	// CompletenessReader, when non-nil, backs the completeness_* fields on
	// /v1/diagnostics/ingestion; nil leaves them absent.
	CompletenessReader CompletenessReader

	// AuditedSources is completeness.AuditedSources(cfg), the sources expected to have a verdict.
	// /v1/coverage counts an applicable one with no verdict row as unverified_sources. Nil
	// disables that join.
	AuditedSources []string

	// ProtocolContracts, when non-nil, backs the contract registry (instance lists and counts) on
	// /v1/protocols*. Nil keeps the directory serving with empty lists and zero counts.
	ProtocolContracts ProtocolContractsReader

	// ProtocolStats, when non-nil, backs the per-protocol events_24h column on /v1/protocols*;
	// nil serves zeros.
	ProtocolStats ProtocolStatsReader

	// ProtocolActivity, when non-nil, backs the per-protocol lake analytics on
	// /v1/protocols/{name} (event-type breakdown, daily series, per-contract counts). Nil serves
	// the detail view without them.
	ProtocolActivity ProtocolActivityReader

	// ProtocolBespoke, when non-nil, backs the per-category analytics block on
	// /v1/protocols/{name} (TVL/volume/AUM/flows/feeds) from the served-tier projected tables.
	// Nil serves the detail view without it.
	ProtocolBespoke ProtocolBespokeReader

	// SoroswapPairs, when non-nil, supplies soroswap's contract list on /v1/protocols* from the
	// soroswap_pairs registry (whose pair set carries token identities). Nil serves an empty list.
	SoroswapPairs SoroswapPairsReader

	// ProtocolPoolTokens, when non-nil, maps each pool-based protocol's contracts to the token
	// C-strkeys they hold, so the roster shows a pair ("XLM/USDC") instead of raw C-strkeys. Nil
	// omits the label (soroswap still labels rows from its own token0/token1).
	ProtocolPoolTokens ProtocolPoolTokensReader

	// NetworkStats, when non-nil, backs GET /v1/network/stats, the home-page aggregate (24h
	// volume, markets, assets indexed, latest ledger). Nil returns 503.
	NetworkStats NetworkStatsReader

	// Aggregators, when non-nil, backs GET /v1/aggregators: the routers registry with per-router
	// routed-via 24h rollups. Nil returns 503.
	Aggregators AggregatorsReader

	// MarketSources backs GET /v1/markets/sources (per-source 24h volume); nil returns an empty list.
	MarketSources MarketSourceReader

	// SourcesStats, when non-nil, populates trade_count_24h on /v1/sources?include=stats. Without
	// it the include flag is silently ignored and the response is the static registry projection.
	SourcesStats SourcesStatsReader

	// Lending, when non-nil, backs /v1/lending/pools (per-Blend-pool summaries). Nil serves an
	// empty array.
	Lending LendingReader

	// MEV, when non-nil, backs /v1/mev (the auto-flagged MEV-event feed); nil serves an empty array.
	MEV MEVReader

	// MEVPulse, when non-nil, reads the MEV worker's run heartbeat for /v1/mev; nil, a missing beat
	// or a partial one all set flags.detectors_partial.
	MEVPulse MEVPulseLooker

	// Anomalies, when non-nil, backs /v1/anomalies (the durable freeze-event timeline); nil is empty.
	Anomalies AnomalyReader

	// Divergences, when non-nil, backs /v1/divergence and /v1/divergence/series; nil is empty.
	Divergences DivergenceReader

	// DivergenceThresholdPct is the operator's divergence alert threshold, surfaced on
	// /v1/divergence/series so charts shade the band from the same number the worker fires on.
	// Zero omits the field; serving a made-up default would fabricate policy.
	DivergenceThresholdPct float64

	// MinMarketCapVolumeUSD is the valuation-integrity floor: market_cap_usd / fdv_usd is
	// suppressed (null, market_cap_low_liquidity=true) when the price came from a single venue AND
	// trailing-24h USD volume is below it, so one dust trade cannot make an obscure asset worth
	// billions. The AND is load-bearing, see [dustLiquiditySuppressed]. Zero disables the guard.
	//floatmoney:ok operator config constant, not a stored/served amount — see capExceedsObservedTurnover/dustLiquiditySuppressed (assets.go), which compare it via big.NewFloat against ADR-0003 decimal-string money
	MinMarketCapVolumeUSD float64

	// MaxMarketCapVolumeRatio is the valuation-integrity CEILING: a market cap or FDV is suppressed
	// above this multiple of trailing-24h USD volume. Not a substitute for MinMarketCapVolumeUSD
	// (is trading small vs is the claim large against it). See [capExceedsObservedTurnover].
	//floatmoney:ok dimensionless turnover-ratio ceiling (days-to-turn-over multiple), same non-money class as MinMarketCapVolumeUSD above despite the "cap" substring
	MaxMarketCapVolumeRatio float64

	// Currencies, when non-nil, supplies the fiat-rate snapshot that /v1/assets fiat rows and the
	// /v1/chart fiat:fiat fallback both consume. Nil falls back to an empty currencies state.
	Currencies CurrenciesReader

	// FXCrossMaxAgeHours bounds the age of the forex rate [Server.tryFiatCrossRate] /
	// [Server.tryUSDAnchoredFiatCross] will serve; older is refused rather than served with a
	// fresh-looking observed_at. 0 falls back to [defaultFXCrossMaxAge] (76h).
	FXCrossMaxAgeHours int

	// DisableFiatBasis serves a single-venue fiat book as read instead of
	// the ADR-0053 USD-anchored derivation (pricing_guard.disable_fiat_basis).
	DisableFiatBasis bool

	// FXFixings binds the closed surfaces' FX legs to the vendor's time
	// series (fx_fixings). Nil: closed fiat crosses are not served.
	FXFixings FXFixingReader

	// Explorer, when non-nil, backs the network-explorer endpoints (/v1/ledgers, /v1/tx,
	// /v1/operations, /v1/contracts, /v1/search) from the ClickHouse lake. Nil 503s them.
	Explorer ExplorerReader

	// IssuerAuthFlags, when non-nil, is the point lookup of the issuer's live AccountEntry, instead
	// of Explorer's AccountStateCached fan-out to trustlines and offers.
	IssuerAuthFlags IssuerAuthFlagsReader

	// StaticHomeDomain, when non-nil, is the operator-static [metadata.issuer_home_domains] map,
	// consulted AFTER the live on-chain read and before the curated knownIssuers map.
	StaticHomeDomain func(ctx context.Context, issuer string) (string, bool)

	// SEP41Movements, when non-nil, backs the Postgres recent-tail half of the
	// /v1/accounts/{g_strkey}/movements merge (address-scoped, unlike contract-scoped
	// SEP41Transfers). Nil degrades to the ClickHouse pre-P23 archive alone with a coverage_note.
	SEP41Movements explorerpkg.SEP41MovementsReader

	// Positions, when non-nil, backs GET /v1/accounts/{g}/positions (six per-protocol Postgres
	// folds). Nil 503s. Venue labels reuse ProtocolPoolTokens.
	Positions explorerpkg.PositionsReader

	// AccountTrades, when non-nil, backs GET /v1/accounts/{g}/trades, the per-address listing over
	// the Postgres `trades` hypertable (see timescale/account_trades.go for what it can and
	// cannot attribute to taker/maker). Nil 503s the endpoint.
	AccountTrades explorerpkg.TradesReader

	// AccountActivity, when non-nil, backs the Postgres segments of GET /v1/accounts/{g}/activity
	// (trades_total, defi_actions, bridge_transfers); ops_by_type reads the lake via Explorer.
	// Nil degrades those segments with a coverage_note; the endpoint 503s only if Explorer is
	// also nil.
	AccountActivity explorerpkg.ActivityReader

	// Directory, when non-nil, resolves curated third-party address labels (account_directory,
	// synced from the MIT-licensed stellar-expert/public-directory) for account and contract
	// views and GET /v1/directory. Nil omits the `directory` field and 503s the batch endpoint.
	Directory explorerpkg.DirectoryReader

	// VolumeCharacter, when non-nil, populates volume_character and its account-structure signals
	// on /v1/assets/{id}; maker/taker trades live in Timescale, not the lake. Nil omits them.
	// Analytics only: never feeds pricing or verification and never re-ranks.
	VolumeCharacter VolumeCharacterReader

	// FXHistory, when non-nil, lets /v1/chart serve fiat:fiat ranges beyond 7d from fx_quotes.
	FXHistory FXHistoryReader

	// SessionPeeker, when non-nil, lets handlers read the magic-link session on the request
	// context, for /v1/account/me with cookie-auth callers. When a request carries both a session
	// and an API-key Subject, the session takes precedence.
	SessionPeeker SessionPeeker

	// SEP10, when non-nil, backs GET /v1/auth/sep10/challenge and POST /v1/auth/sep10/token. Nil
	// returns 404, typically because the signing seed / JWT secret config is absent.
	SEP10 auth.SEP10Validator

	// Auth, when non-nil, is inserted between CORS and RateLimit and sets a Subject in the
	// request context, readable via [auth.SubjectFrom]. Nil means anonymous-only; the
	// rate-limit middleware then keys on RemoteIP only.
	Auth middleware.Middleware

	// KeyPolicy, when non-nil, runs after Auth and before RateLimit, enforcing per-key policy (IP
	// and Referer allowlists, per-endpoint permissions); without it they are accepted at creation
	// but never enforced. Anonymous subjects pass through.
	KeyPolicy middleware.Middleware

	// RateLimit, when non-nil, sits inside Logger, Auth, KeyPolicy, RequireEmailVerified,
	// UsageTracker and MonthlyQuota, so remote_ip + Subject are on the context (order:
	// middleware/doc.go). Production uses middleware.RateLimitBySubject so anon and per-tier keyed
	// limits both apply; the single-bucket middleware.RateLimit serves tests.
	RateLimit middleware.Middleware

	// UsageTracker, when non-nil, sits at the end of the middleware chain and records per-day
	// counters feeding /v1/account/usage. Best-effort: never blocks a request. Pair with
	// UsageReader to expose the data.
	UsageTracker middleware.Middleware

	// MonthlyQuota, when non-nil, sits before rate-limit so a request over the per-key monthly
	// cap returns 429 without spending a rate-limit token. Opt-in: only Postgres-backed keys
	// carry `Subject.MonthlyQuota`.
	MonthlyQuota middleware.Middleware

	// TouchUsage, when non-nil, sits inside rate-limit so a denied (429) request does not update
	// the dashboard's "last seen" column. It fires post-handler with a Redis SETNX debounce.
	// Opt-in: requires both the Postgres key store and Redis.
	TouchUsage middleware.Middleware

	// RequireEmailVerified, when non-nil, sits after auth and before rate-limit and rejects
	// API-key callers with zero `EmailVerifiedAt` whose identifier indicates a `/v1/signup`
	// origin. Opt-in via `cfg.API.SignupRequireEmailVerification` so existing keys keep working.
	RequireEmailVerified middleware.Middleware

	// UsageReader, when non-nil, backs /v1/account/usage with real per-day counts; nil keeps the
	// empty list with the locked wire shape.
	UsageReader UsageReader

	// UsageRollupReader, when non-nil, backs /v1/account/usage with per-day x per-endpoint rows
	// from the `usage_daily` rollups. It takes precedence over UsageReader; the handler falls
	// back to the per-day Redis totals when the rollup read errors or has no rows yet.
	UsageRollupReader UsageRollupReader

	// Hub, when non-nil, backs the closed-bucket SSE endpoint (`/v1/price/stream`); nil returns 503
	// for it alone. /v1/price/tip/stream also uses it; internal/api/streaming/doc.go says which
	// streams replay from it.
	Hub *streaming.Hub

	// Confidence, when non-nil, populates the confidence + factors fields on `/v1/price`
	// (ADR-0019), read from the Redis cache the aggregator writes. Nil, or a cache miss at lookup,
	// leaves the field unset while the rest of the envelope serves.
	Confidence ConfidenceLooker

	// Triangulated, when non-nil, is the fallback /v1/price consults after a Timescale miss: the
	// aggregator's triangulated implied VWAPs plus the provenance marker that gates
	// `flags.triangulated`. Nil leaves triangulated-only pairs 404ing.
	Triangulated TriangulatedPriceLooker

	// CDNEnabled controls whether cacheable routes emit `s-maxage` beside `max-age`. Default true;
	// false avoids a CDN caching anything later made auth-tied. See [middleware.CacheControlWithCDN].
	CDNEnabled bool

	// StatusBackend, when non-nil, backs /v1/status with Prometheus-derived heartbeats, latency
	// percentiles, freshness and Alertmanager incident counts. Nil serves an in-process surface
	// (uptime and region only) for deployments without a local Prometheus.
	StatusBackend StatusBackend

	// ArchiveReportPath, when non-empty, backs GET /v1/diagnostics/archive (the archive-completeness
	// daemon's JSON report). Empty returns 503; a configured but missing file returns 404.
	ArchiveReportPath string

	// BackupMetrics, when non-nil, backs GET /v1/diagnostics/backups. Nil derives it from a
	// *PrometheusStatusBackend StatusBackend; with no Prometheus it is 503.
	BackupMetrics BackupMetricsSource

	// RegionName + RegionDeployment label /v1/status ("unknown" / "production" when unset).
	RegionName       string
	RegionDeployment string

	// StatusServices names the background services this deployment runs, the only ones /v1/status
	// reports a heartbeat for and rolls `overall` up from. Empty means {"indexer","aggregator"}.
	// A net without an aggregator would otherwise pin overall at "degraded" forever.
	StatusServices []string

	// DashboardAuth, when non-nil, mounts the dashboard magic-link auth flow (POST
	// /v1/auth/login, GET /v1/auth/callback, POST /v1/auth/logout). main.go builds it only when
	// cfg.API.Dashboard.BaseURL is set.
	DashboardAuth DashboardAuthMounter

	// DashboardKeys, when non-nil, mounts the key-management surface (/v1/dashboard/keys), gated
	// on the session cookie DashboardAuth sets.
	DashboardKeys DashboardAuthMounter

	// DashboardWebhooks, when non-nil, mounts the customer-webhook CRUD surface; the delivery
	// worker (internal/customerwebhook) is independent of these handlers.
	DashboardWebhooks DashboardAuthMounter

	// DashboardPriceAlerts, when non-nil, mounts the price-alert CRUD surface; the evaluator
	// (internal/pricealerts) runs in the aggregator, independent of these handlers.
	DashboardPriceAlerts DashboardAuthMounter

	// SACWrappers maps SAC C-strkey to classic asset key ("CODE:ISSUER") and backs
	// /v1/sac-wrappers, which the explorer joins client-side to label Soroban DEX pools. Empty
	// returns an empty map and the explorer shows the raw C-strkey.
	SACWrappers map[string]string

	// NetworkPassphrase derives deterministic SAC contract ids so the WASM endpoint can answer
	// "SAC, no WASM" for contracts whose instance predates the lake's capture window. Empty
	// disables the computed half.
	NetworkPassphrase string

	// USDPeggedClassics is the operator's allow-list of classic assets trusted as 1:1 USD
	// stablecoins. /v1/chart falls back from a literal X/fiat:USD lookup (no prices_1m rows) to
	// X/<peg> when that returns 0 points. Empty disables the fallback.
	USDPeggedClassics []canonical.Asset

	// PegDeclaredAt is the observed_at /v1/price stamps on the flat $1 for a declared peg no market
	// prices. The declaration has no timestamp, so zero uses construction time and the stamp resets
	// on every restart; set it to pin the wire value.
	PegDeclaredAt time.Time
	// Clock is the chart handlers' clock; nil uses time.Now. Tests pin it so a fixture built
	// before UTC midnight and a request served after it name the same day.
	Clock func() time.Time

	// FiatPeggedClassics maps a classic asset_id ("CODE-ISSUER") to the fiat asset the operator
	// declares it 1:1-pegged to. Rows whose price_usd is nil after the substance gate are filled
	// from the fiat-to-USD rate (fiatUSDPriceFor chain), stamped price_basis="declared_peg".
	FiatPeggedClassics map[string]canonical.Asset

	// SessionAuth, when non-nil, wraps every handler so a dashboard session cookie populates a
	// SessionContext; anonymous and bearer requests pass through. /v1/dashboard/* 401s without it.
	SessionAuth middleware.Middleware

	// VerifiedCurrencies, when non-nil, enables the verified-currency overlay on /v1/assets/{id}:
	// an `unverified_warning` + flags.unverified_ticker_collision when the asset's code matches a
	// verified ticker but the issuer does not (code alone is never identity). It also enables slug
	// dispatch to the global view for /v1/assets/{slug}.
	VerifiedCurrencies *currency.Catalogue

	// BackfillCoverage, when non-nil, is the cache behind the coverage section of
	// `/v1/diagnostics/ingestion`; nil omits it.
	BackfillCoverage *CoverageCache

	// UsdVolumePricing feeds /v1/coverage's usd_volume_pricing; nil renders null.
	UsdVolumePricing *UsdVolumePricingCache

	// SDEXOrderBook, when non-nil, is the in-process classic offer book behind GET
	// /v1/sdex/orderbook, loaded once from the lake then advanced every
	// [SDEXOrderBookAdvanceInterval]. Nil or not yet loaded serves an honest 503.
	SDEXOrderBook *SDEXOrderBookCache

	// DEXTVL, when non-nil, is the process-local per-protocol DEX TVL snapshot, refreshed every
	// [DEXTVLRefreshInterval]; per-request aggregation over the reserve tables is never acceptable
	// on the read path. Nil or cold leaves the `tvl` field absent.
	DEXTVL *DEXTVLCache

	// NonstandardDecimals, when non-nil, backs read-time normalization: every price-shaped surface
	// scales its served ratio by 10^(dec_base-dec_quote) for a pair with a leg confirmed as
	// non-7-decimal (aggregate.AdjustPrice). Nil serves the raw ratio. See
	// [NonstandardDecimalsCache] and docs/operations/runbooks/dex.md.
	NonstandardDecimals *NonstandardDecimalsCache

	// GlobalPrice, when non-nil, powers the price block on `/v1/assets/{slug}` global views via
	// the three-tier chain (vwap_native, aggregator_avg, triangulated). Nil leaves the block empty.
	GlobalPrice aggregate.GlobalPriceReader

	// GlobalPriceOpts tunes the three-tier policy; zero uses [aggregate.DefaultGlobalPriceOptions]
	// except the aggregator source list, which the defaults cannot know without importing the
	// registry.
	GlobalPriceOpts aggregate.GlobalPriceOptions

	// RequestTimeout is the per-request deadline for non-streaming requests; zero or negative
	// falls back to [defaultRequestTimeout]. SSE endpoints are exempt.
	RequestTimeout time.Duration
	// DisableRequestTimeout omits the RequestTimeout middleware. Separate because a zero
	// RequestTimeout means "unset", while an explicit api.request_timeout = 0 disables it.
	DisableRequestTimeout bool
}

// New constructs a Server and mounts all v1 routes.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		Options:            opts,
		logger:             logger,
		coverageFloorCache: &coverageFloorCache{entries: map[string]coverageFloorEntry{}},
		lendingReserves:    newSWRCache[poolReserves](logger, "lending_reserves", "api-lending-reserves-fill", lendingReservesTTL, lendingReservesMaxEntries),
		fxCrossMaxAge:      fxCrossMaxAgeOrDefault(opts.FXCrossMaxAgeHours),
		fxFixings:          newFXFixingCache(opts.FXFixings, logger, fxCrossMaxAgeOrDefault(opts.FXCrossMaxAgeHours)),
		streamDrain:        streaming.NewDrain(),
		backupMetrics:      backupMetricsFor(opts),
		regionName:         valueOr(opts.RegionName, "unknown"),
		regionDeployment:   valueOr(opts.RegionDeployment, "production"),
		statusServices:     statusServicesOr(opts.StatusServices),
		globalPrice:        newDecimalsCorrectedGlobalReader(opts.GlobalPrice, opts.NonstandardDecimals),
		globalPriceOpts:    globalPriceOptsWithDefaults(opts.GlobalPriceOpts),
		// 120s TTL on /v1/assets/{id}. MUST exceed the selfPrewarmAssetEndpoints cadence (60s)
		// with margin, or the cache expires between prewarm passes and probes pay the full cold
		// rebuild. Data moves per-minute at fastest, so 120s fits the closed-bucket-only contract.
		assetDetailCache: newAssetDetailResponseCache(120 * time.Second),
		assetListCache:   newAssetListResponseCache(assetListCacheTTL, assetListCacheMaxAge),
		mux:              http.NewServeMux(),
		publicRoutes:     middleware.NewPublicRoutes(),
		started:          time.Now().UTC(),
		pegDeclaredAt:    timeOr(opts.PegDeclaredAt, time.Now().UTC()),
		requestTimeout:   requestTimeoutFor(opts),
	}
	applyProtocolOptions(s, opts)
	s.explorerHandler = explorerHandlerFor(s, opts, logger)
	loadIncidents(s, logger)
	warnIfSep1CacheLacksFetchState(s, logger)
	s.mountRoutes()
	s.streamRevalidate = s.buildStreamRevalidator()
	return s
}

// applyProtocolOptions builds the Protocols-pillar state derived from
// Options (the pool-tokens cache wrapper). Split from New for funlen.
func applyProtocolOptions(s *Server, opts Options) {
	if opts.ProtocolPoolTokens != nil {
		s.protocolPoolTokens = newPoolTokensCache(opts.ProtocolPoolTokens)
	}
}

// loadIncidents loads the embedded incident corpus once at startup; it ships with the binary,
// so new posts need a redeploy. Split from New for funlen.
func loadIncidents(s *Server, logger *slog.Logger) {
	if loaded, err := incidents.Load(logger); err != nil {
		logger.Warn("incidents: load failed; /v1/incidents returns empty",
			"err", err)
	} else {
		s.incidents = loaded
	}
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// defaultStatusServices is the pubnet shape of [Options.StatusServices]; /v1/status reports a
// heartbeat for exactly these (plus "api") and rolls `overall` up from them.
var defaultStatusServices = []string{"indexer", "aggregator"}

// statusServicesOr normalises Options.StatusServices (nil/empty falls back to the pubnet pair; never nil). Names are lower-cased
// because heartbeats key on lower-case Prometheus `job` labels, so "Indexer" would boot clean then pin `overall` degraded.
func statusServicesOr(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), defaultStatusServices...)
	}
	return out
}

// requestTimeoutFor resolves the blanket request deadline: 0 (middleware omitted) only when
// the operator explicitly disabled it, so a zero-value Options keeps the default bound on.
func requestTimeoutFor(opts Options) time.Duration {
	if opts.DisableRequestTimeout {
		return 0
	}
	return durationOr(opts.RequestTimeout, defaultRequestTimeout)
}

// durationOr returns d when it is positive, else fallback. Used to back
// an unset (zero-value) Options duration with a safe default.
func durationOr(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// now reads [Options.Clock], falling back to the wall clock.
func (s *Server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// timeOr returns t when it is set, else fallback. Used to back an unset
// (zero-value) Options instant with the server's own clock.
func timeOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t
}

// globalPriceOptsWithDefaults fills zero fields of `Options.GlobalPriceOpts` from
// [aggregate.DefaultGlobalPriceOptions], so callers may supply only the source list.
func globalPriceOptsWithDefaults(o aggregate.GlobalPriceOptions) aggregate.GlobalPriceOptions {
	defaults := aggregate.DefaultGlobalPriceOptions()
	if o.VWAPMinTradeCount == 0 {
		o.VWAPMinTradeCount = defaults.VWAPMinTradeCount
	}
	if o.TriangulationWindow == 0 {
		o.TriangulationWindow = defaults.TriangulationWindow
	}
	if o.MaxAggregatorAge == 0 {
		o.MaxAggregatorAge = defaults.MaxAggregatorAge
	}
	if o.MaxVWAPAge == 0 {
		o.MaxVWAPAge = defaults.MaxVWAPAge
	}
	return o
}

// BeginStreamDrain tells every SSE connection the process is shutting down so none holds http.Server.Shutdown open; idempotent, register with RegisterOnShutdown, see [streaming.Drain].
func (s *Server) BeginStreamDrain() { s.streamDrain.Begin() }

// streamOptions is the [streaming.StreamOptions] every stream handler passes, so a new stream gets the shutdown drain and heartbeat
// credential re-check.
func (s *Server) streamOptions() streaming.StreamOptions {
	return streaming.StreamOptions{Drain: s.streamDrain, Revalidate: s.streamRevalidate}
}

// streamRevalidationGates are the middlewareStack entries an open stream re-runs per heartbeat; RateLimit is absent (admission control,
// not revocation).
var streamRevalidationGates = map[string]bool{
	"PublicRoutes":         true,
	"Auth":                 true,
	"KeyPolicy":            true,
	"RequireEmailVerified": true,
	"MonthlyQuota":         true,
}

// buildStreamRevalidator returns the Revalidate hook: replays the request through the gates in stack order and fails if any answers 401,
// 403 or 429. Nil with no gate.
func (s *Server) buildStreamRevalidator() func(*http.Request) bool {
	var gates []middleware.Middleware
	for _, e := range s.middlewareStack() {
		if streamRevalidationGates[e.name] {
			gates = append(gates, e.mw)
		}
	}
	if len(gates) == 0 {
		return nil
	}
	return func(r *http.Request) bool {
		admitted := false
		var h http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { admitted = true })
		for i := len(gates) - 1; i >= 0; i-- {
			h = gates[i](h)
		}
		h.ServeHTTP(discardResponseWriter{header: http.Header{}}, r)
		return admitted
	}
}

// discardResponseWriter swallows a revalidation gate's rejection body.
type discardResponseWriter struct{ header http.Header }

func (d discardResponseWriter) Header() http.Header         { return d.header }
func (d discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardResponseWriter) WriteHeader(int)             {}

// Handler returns the mux wrapped in [Server.middlewareStack], outermost first.
func (s *Server) Handler() http.Handler {
	entries := s.middlewareStack()
	stack := make([]middleware.Middleware, len(entries))
	for i, e := range entries {
		stack[i] = e.mw
	}
	return middleware.Chain(s.mux, stack...)
}

// stackEntry is one middleware in [Server.middlewareStack], named as middleware/doc.go names it.
type stackEntry struct {
	name string
	mw   middleware.Middleware
}

// middlewareStack is the request-path middleware order, outermost first; TestMiddlewareStackMatchesPackageDoc pins it to
// middleware/doc.go.
// HTTPMetrics is outside Logger+Recoverer so panics are counted; CORS is outside RateLimit; RateLimit is inside Auth so remote IP and
// Subject are set.
func (s *Server) middlewareStack() []stackEntry {
	stack := []stackEntry{
		{"RequestID", middleware.RequestID},
		{"HTTPMetrics", obs.HTTPMetrics},
		{"Logger", middleware.Logger(s.logger)},
		{"Recoverer", middleware.Recoverer(s.logger)},
		// Inside Recoverer so a panic's 500 response carries nosniff; idempotent with edge proxies.
		{"SecurityHeaders", middleware.SecurityHeaders},
		// Cache-Control is set BEFORE handlers run so writeJSON / writeProblem inherit it; `s-maxage`
		// is gated on s.CDNEnabled.
		{"CacheControl", middleware.CacheControlWithCDN(s.CDNEnabled)},
		// Turns the mux's text/plain 404 / 405 into problem+json; AFTER CacheControl so it gets the
		// same directive a handler-side response would.
		{"Envelope404", middleware.Envelope404},
	}
	if s.CORS != nil {
		stack = append(stack, stackEntry{"CORS", s.CORS})
	}
	// 308-redirect trailing-slash paths to the no-slash form (308 keeps method and body). MUST sit
	// INSIDE CORS: outside it the redirect lacks Access-Control-Allow-Origin and a browser fetch dies.
	stack = append(stack, stackEntry{"TrailingSlashRedirect", middleware.TrailingSlashRedirect(s.mux)})
	// ResolveRoute pre-matches the route pattern BEFORE the gates can reject, because they run
	// outside obs.CaptureRoute; otherwise UsageTracker buckets a rejected request "unmatched".
	stack = append(stack, stackEntry{"ResolveRoute", middleware.ResolveRoute(s.mux)})
	// RequestTimeout gives every handler a deadline. It sits OUTSIDE the credential/quota/limit
	// block, whose Redis and Postgres round-trips would otherwise escape it, and INSIDE CORS and
	// TrailingSlashRedirect so a preflight short-circuits without a timer.
	//
	// The pre-handler seams (MonthlyQuota read, RateLimit take, Auth's failed-auth throttle) detach
	// via context.WithoutCancel so a client abort cannot arm their dwell clocks;
	// middleware.throttleContext re-applies this deadline. SSE is exempt; off when <= 0.
	if s.requestTimeout > 0 {
		stack = append(stack, stackEntry{"RequestTimeout", middleware.RequestTimeout(s.requestTimeout)})
	}
	// Auth runs INSIDE CORS (preflight skips credential checks) but OUTSIDE RateLimit (per-tier
	// limits need the Subject). publicRoutes.Mark sits directly outside it so handlePublic routes
	// are credential-optional under every auth_mode.
	if s.Auth != nil {
		stack = append(stack, stackEntry{"PublicRoutes", s.publicRoutes.Mark()}, stackEntry{"Auth", s.Auth})
	}
	// KeyPolicy runs after Auth (Subject on context) and before RateLimit (a policy-denied 403
	// never spends a token).
	if s.KeyPolicy != nil {
		stack = append(stack, stackEntry{"KeyPolicy", s.KeyPolicy})
	}
	// RequireEmailVerified runs after KeyPolicy (Subject resolved) and BEFORE rate-limit so an
	// unverified-key 403 spends no token. Opt-in via cfg.API.SignupRequireEmailVerification.
	if s.RequireEmailVerified != nil {
		stack = append(stack, stackEntry{"RequireEmailVerified", s.RequireEmailVerified})
	}
	// Usage tracker runs OUTSIDE both quota and rate-limit so it records both kinds of 429 as
	// `throttled`. The billable total (the MonthlyQuota input) excludes 429s and most 5xx, see
	// middleware.billableClass, so a counted 429 cannot feed back into the quota that denied it.
	// Best-effort; failures never block.
	if s.UsageTracker != nil {
		stack = append(stack, stackEntry{"UsageTracker", s.UsageTracker})
	}
	// MonthlyQuota runs AFTER auth/key-policy (Subject on context) and BEFORE rate-limit (a
	// quota-rejected request spends no per-minute token).
	if s.MonthlyQuota != nil {
		stack = append(stack, stackEntry{"MonthlyQuota", s.MonthlyQuota})
	}
	if s.RateLimit != nil {
		stack = append(stack, stackEntry{"RateLimit", s.RateLimit})
	}
	// TouchUsage runs INSIDE rate-limit so a denied (429) request does not bump the dashboard's
	// "last seen". The touch fires post-handler behind a SETNX debounce to bound per-request cost.
	if s.TouchUsage != nil {
		stack = append(stack, stackEntry{"TouchUsage", s.TouchUsage})
	}
	// Session resolver runs INSIDE rate-limit; the cookie is parsed once per request and the
	// result stays attached for the rest of the chain.
	if s.SessionAuth != nil {
		stack = append(stack, stackEntry{"SessionAuth", s.SessionAuth})
	}
	// Inside UsageTracker/TouchUsage: their AfterResponse flush would spill it untagged.
	stack = append(stack, stackEntry{"ETag", middleware.ETag})
	// CaptureRoute MUST be innermost, directly above the mux, so r.Pattern is populated before it
	// reads; it writes into the *routeCapture HTTPMetrics planted in the context, since Logger's
	// r.WithContext shadows the original request. See obs.HTTPMetrics.
	return append(stack, stackEntry{"CaptureRoute", obs.CaptureRoute})
}

// Uptime returns how long this server has been running.
func (s *Server) Uptime() time.Duration { return time.Since(s.started) }

// proxyForwardHeaders are the headers a reverse proxy adds for a remote client. Their PRESENCE
// is the signal, not their value; a direct local scrape sets none.
var proxyForwardHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Real-Ip",
	"Forwarded",
}

// loopbackOnly returns 404 unless the request is a local, un-proxied scrape (for /metrics). Requires loopback RemoteAddr AND no
// forwarding header, since a misconfigured Caddy presents loopback for public traffic. 404 not 403, which would confirm the route
// exists.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr // RemoteAddr without port (rare)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.NotFound(w, r)
			return
		}
		for _, h := range proxyForwardHeaders {
			if r.Header.Get(h) != "" {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handlePublic mounts a route unauthenticated by design under every auth_mode; its OpenAPI operation must declare `security: []`
// (TestPublicRoutes_MatchOpenAPISecurityNone).
func (s *Server) handlePublic(pattern string, h http.HandlerFunc) {
	s.publicRoutes.Handle(s.mux, pattern, h)
}

// handleAdmin mounts an operator-only route; the operator tier and, for writes, X-Reason are enforced here so a new /v1/admin/ handler
// cannot skip them.
func (s *Server) handleAdmin(pattern string, h http.HandlerFunc) {
	_, instance, _ := strings.Cut(pattern, " ")
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireOperator(w, r, instance); !ok {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if _, ok := s.requireReason(w, r); !ok {
				return
			}
		}
		h(w, r)
	})
}

func (s *Server) mountRoutes() { //nolint:funlen // route registration is intentionally one block for grep-ability; splitting into sub-functions makes "where is /v1/X served?" harder to answer.
	s.mux.HandleFunc("GET /v1/issuers", s.handleIssuersList)
	s.mux.HandleFunc("GET /v1/issuers/{g_strkey}", s.handleIssuer)

	// Per-contract SEP-41 transfer audit trail with optional ?from= / ?to= filters.
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", s.handleSEP41Transfers)

	s.mux.HandleFunc("GET /v1/changes/{entity_type}/{id}", s.handleChangeSummary)
	s.mux.HandleFunc("GET /v1/diagnostics/cursors", s.handleCursors)
	s.mux.HandleFunc("GET /v1/diagnostics/ingestion", s.handleDiagnosticsIngestion)
	// Latest archive-completeness report; 503 when unconfigured, 404 pre-first-run.
	s.mux.HandleFunc("GET /v1/diagnostics/archive", s.handleDiagnosticsArchive)
	// Backup + DR-evidence freshness vs. SLO, read from Prometheus; backs the status page's
	// Backups panel. 503 when no metrics backend is wired.
	s.mux.HandleFunc("GET /v1/diagnostics/backups", s.handleDiagnosticsBackups)
	s.mux.HandleFunc("GET /v1/coverage", s.handleCoverageVerdicts)

	// Protocols: the static registry always serves; dynamic joins degrade.
	s.mux.HandleFunc("GET /v1/protocols", s.handleProtocolsList)
	s.mux.HandleFunc("GET /v1/protocols/{name}", s.handleProtocolDetail)
	// Pools + legs behind the `tvl` block, from the same in-process snapshot.
	s.mux.HandleFunc("GET /v1/protocols/{name}/tvl", s.handleProtocolTVL)

	// SDEX order-book depth from the in-process book (503 until the initial lake load completes).
	s.mux.HandleFunc("GET /v1/sdex/orderbook", s.handleSDEXOrderbook)

	// Live-ingest frontier (latest ingested ledger and lag). /tip is a 2s-cached poll; /stream is
	// the SSE counterpart pushing one ledger_update per new ledger.
	s.mux.HandleFunc("GET /v1/ledger/tip", s.handleLedgerTip)
	s.mux.HandleFunc("GET /v1/ledger/stream", s.handleLedgerStream)

	// Network explorer (ADR-0038), read from the ClickHouse lake; handlers live in v1/explorer.
	s.mux.HandleFunc("GET /v1/ledgers", s.explorerHandler.LedgersList)
	s.mux.HandleFunc("GET /v1/ledgers/at", s.explorerHandler.LedgerAt)
	s.mux.HandleFunc("GET /v1/ledgers/{seq}", s.explorerHandler.LedgerDetail)
	s.mux.HandleFunc("GET /v1/ledgers/{seq}/transactions", s.explorerHandler.LedgerTransactions)
	s.mux.HandleFunc("GET /v1/ledgers/{seq}/operations", s.explorerHandler.LedgerOperations)
	s.mux.HandleFunc("GET /v1/operations", s.explorerHandler.Operations)
	s.mux.HandleFunc("GET /v1/tx/{hash}", s.explorerHandler.TxDetail)
	s.mux.HandleFunc("GET /v1/search", s.explorerHandler.Search)
	s.mux.HandleFunc("GET /v1/contracts", s.explorerHandler.ContractsList)
	s.mux.HandleFunc("GET /v1/contracts/stats", s.explorerHandler.ContractStats)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}", s.explorerHandler.ContractDetail)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/wasm", s.explorerHandler.ContractWasm)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/interactions", s.explorerHandler.ContractInteractions)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/code-history", s.explorerHandler.ContractCodeHistory)
	s.mux.HandleFunc("GET /v1/accounts", s.explorerHandler.AccountsList)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}", s.underReview(s.explorerHandler.AccountState))
	s.mux.HandleFunc("GET /v1/directory", s.explorerHandler.DirectoryLookup)
	s.mux.HandleFunc("GET /v1/accounts/stats", s.explorerHandler.AccountsStats)
	// Literal segment, so it wins over the {g_strkey} wildcard as /stats does.
	s.mux.HandleFunc("GET /v1/accounts/creators", s.explorerHandler.AccountCreators)
	s.mux.HandleFunc("GET /v1/accounts/sponsors", s.explorerHandler.AccountSponsors)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/transactions", s.explorerHandler.AccountTransactions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/operations", s.explorerHandler.AccountOperations)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/movements", s.explorerHandler.AccountMovements)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/positions", s.explorerHandler.AccountPositions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/entry-changes", s.explorerHandler.AccountEntryChanges)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/claimable-balances", s.explorerHandler.AccountClaimableBalances)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/signer-on", s.explorerHandler.AccountSignerOn)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/sponsorships", s.explorerHandler.AccountSponsorships)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/trades", s.explorerHandler.AccountTrades)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/activity", s.explorerHandler.AccountActivity)
	// Sponsorship and account-creation graph for one account.
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/graph", s.explorerHandler.AccountGraph)
	// The same graph on a time axis; a sub-resource so the snapshot payload grows no unasked series.
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/graph/history", s.explorerHandler.AccountGraphHistory)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/graph/cohort", s.explorerHandler.AccountGraphCohort)

	s.mux.HandleFunc("GET /v1/incidents", s.handleIncidents)
	s.mux.HandleFunc("GET /v1/incidents.atom", s.handleIncidentsAtom)
	s.mux.HandleFunc("GET /v1/network/stats", s.handleNetworkStats)
	s.mux.HandleFunc("GET /v1/network/throughput", s.explorerHandler.NetworkThroughput)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /v1/readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /v1/livez/lake", s.handleLivezLake)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
	s.handlePublic("GET /v1/status", s.handleStatus)
	// Public list of active operator-posted status banners, shown beside the Alertmanager-derived
	// /v1/status incidents. Empty (`{"notices":[]}`) when unwired.
	s.handlePublic("GET /v1/status/notices", s.handleStatusNotices)

	// Prometheus scrape endpoint, unversioned: operator-facing. Also gated in Go on loopback AND
	// un-proxied (see loopbackOnly) to catch a stale Caddyfile; a public hit fingerprints the
	// deployment.
	s.mux.Handle("GET /metrics", loopbackOnly(obs.Handler()))

	// Asset catalogue.
	s.mux.HandleFunc("GET /v1/assets", s.handleAssetList)
	// /v1/external/assets — non-Stellar assets (fiat + reference-only coins)
	// split off /v1/assets. /v1/assets is Stellar-only.
	s.mux.HandleFunc("GET /v1/external/assets", s.handleExternalAssetList)
	s.mux.HandleFunc("GET /v1/external/assets/{slug}", s.handleExternalAssetGet)
	// ServeMux already prefers the static /v1/assets/verified over
	// /v1/assets/{asset_id}; mounting it first keeps that obvious.
	s.mux.HandleFunc("GET /v1/assets/verified", s.handleAssetsVerified)
	s.mux.HandleFunc("GET /v1/assets/{asset_id}", s.underReview(s.handleAssetGet))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/metadata", s.handleAssetMetadata)
	// Live per-token supply from the decode-at-ingest supply_flows lake
	// (ADR-0034).
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/supply", s.underReview(s.handleAssetSupply))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/supply/flows", s.underReview(s.handleAssetSupplyFlows))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/holders", s.underReview(s.explorerHandler.AssetHolders))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/movements", s.underReview(s.explorerHandler.AssetMovements))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/entry-changes", s.underReview(s.explorerHandler.AssetEntryChanges))
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/trustline-history", s.underReview(s.explorerHandler.AssetTrustlineHistory))

	// Current price — last-trade fallback today; VWAP path when
	// the aggregator ships.
	s.mux.HandleFunc("GET /v1/price", s.handlePrice)

	// Point-in-time closed bucket at-or-before ts, and the 1h/24h/7d/30d change strip; both use
	// the same finest-CAGG reader.
	s.mux.HandleFunc("GET /v1/price/at", s.handlePriceAt)
	s.mux.HandleFunc("GET /v1/price/changes", s.handlePriceChanges)

	// Rolling-window tip (ADR-0018): VWAP over the last few seconds, falling back to last-good
	// price when empty. NOT cross-region consistent; use /v1/price for that.
	s.mux.HandleFunc("GET /v1/price/tip", s.handlePriceTip)

	// SSE counterpart of /v1/price/tip, pushed on a per-connection tick.
	s.mux.HandleFunc("GET /v1/price/tip/stream", s.handlePriceTipStream)

	// Raw per-source observations (ADR-0018): the most recent trade per source, no aggregation.
	s.mux.HandleFunc("GET /v1/observations", s.handleObservations)

	// SSE counterpart of /v1/observations; interval_seconds tunes cadence.
	s.mux.HandleFunc("GET /v1/observations/stream", s.handleObservationsStream)

	// Closed-bucket SSE, fed by the aggregator publishing into the Hub on each window close; same
	// ADR-0015 closed-bucket contract as /v1/price.
	s.mux.HandleFunc("GET /v1/price/stream", s.handlePriceStream)

	// Batch price lookup, up to 100 assets per request.
	s.mux.HandleFunc("GET /v1/price/batch", s.handlePriceBatch)

	// Batch price lookup via JSON body — same shape, raises the
	// per-request ceiling to 1000.
	s.mux.HandleFunc("POST /v1/price/batch", s.handlePriceBatchPost)

	// Trade history within a time window.
	s.mux.HandleFunc("GET /v1/history", s.handleHistory)

	// Aggregated history over the asset's full indexed range, served from prices_<granularity>
	// CAGGs; only closed buckets are returned (ADR-0015).
	s.mux.HandleFunc("GET /v1/history/since-inception", s.handleHistorySinceInception)

	// Rolling-window chart series matching the V1 chart contract
	// (timeframe, granularity, price_type). Per ADR-0020.
	s.mux.HandleFunc("GET /v1/chart", s.handleChart)

	// Single-bar OHLC over a time window.
	s.mux.HandleFunc("GET /v1/ohlc", s.handleOHLC)

	// Volume-weighted average price over a time window.
	s.mux.HandleFunc("GET /v1/vwap", s.handleVWAP)

	// Time-weighted average price over a time window.
	s.mux.HandleFunc("GET /v1/twap", s.handleTWAP)

	// Distinct trading pairs.
	s.mux.HandleFunc("GET /v1/markets", s.handleMarkets)
	s.mux.HandleFunc("GET /v1/markets/sources", s.handleMarketSources)

	// Per-pool listing — every (source, base, quote) tuple in the
	// recency window. Backs the /dexes table on the explorer.
	s.mux.HandleFunc("GET /v1/pools", s.handlePools)

	// Per-pool-contract reserves + constant-product depth (ADR-0039 lake read). The literal path
	// wins over any future /v1/pools/{...} wildcard.
	s.mux.HandleFunc("GET /v1/pools/reserves", s.handlePoolReserves)

	// Native (CAP-38) liquidity-pool reserves + depth from the `liquidity_pool` LedgerEntry in the
	// lake (ADR-0039); ?pool= selects one pool.
	s.mux.HandleFunc("GET /v1/liquidity-pools", s.handleLiquidityPools)

	// Single-pair activity summary.
	s.mux.HandleFunc("GET /v1/pairs", s.handlePairs)

	// Latest oracle readings per source for an asset.
	s.mux.HandleFunc("GET /v1/oracle/latest", s.handleOracleLatest)

	// One row per (source, asset, quote) with its latest observation in the trailing 7d window.
	s.mux.HandleFunc("GET /v1/oracle/streams", s.handleOracleStreams)

	// SEP-40 passthrough: /v1/price data reshaped to the single-quote contract on-chain oracle
	// readers expect. Quote is fixed at fiat:USD on /lastprice; /x_last_price takes base + quote.
	s.mux.HandleFunc("GET /v1/oracle/lastprice", s.handleOracleLastPrice)
	s.mux.HandleFunc("GET /v1/oracle/prices", s.handleOraclePrices)
	s.mux.HandleFunc("GET /v1/oracle/x_last_price", s.handleOracleXLastPrice)

	// Lending — Blend pools observed in the auction stream.
	s.mux.HandleFunc("GET /v1/lending/pools", s.handleLendingPools)
	// Real per-reserve current-state TVL/util/APY from the lake (ADR-0039).
	s.mux.HandleFunc("GET /v1/lending/pools/{pool}/reserves", s.handleLendingPoolReserves)

	// MEV — auto-flagged MEV-event feed (arbitrage cycles today).
	s.mux.HandleFunc("GET /v1/mev", s.handleMEVEvents)

	// Freeze timeline and cross-reference divergence board (ADR-0019).
	s.mux.HandleFunc("GET /v1/anomalies", s.handleAnomalies)
	s.mux.HandleFunc("GET /v1/divergence", s.handleDivergence)
	s.mux.HandleFunc("GET /v1/divergence/series", s.handleDivergenceSeries)

	// Source catalogue with class + IncludeInVWAP metadata.
	s.mux.HandleFunc("GET /v1/sources", s.handleSources)
	// Per-source health row, same shape as the `sources` section of /v1/diagnostics/ingestion, so
	// one source can be polled cheaply.
	s.mux.HandleFunc("GET /v1/sources/{name}/health", s.handleSourceHealth)

	// Router / aggregator-vault registry + routed-via 24h rollup
	// (migration 0025 Phase B).
	s.mux.HandleFunc("GET /v1/aggregators", s.handleAggregators)

	// Machine-readable summary of the active aggregation policy, mirroring the explorer's
	// /methodology page.
	s.mux.HandleFunc("GET /v1/methodology", s.handleMethodology)

	// SAC wrapper resolution: operator-config map of SAC C-strkey to "CODE-ISSUER", so the
	// explorer labels Soroban DEX pools with symbols instead of raw C-strkeys.
	s.mux.HandleFunc("GET /v1/sac-wrappers", s.handleSACWrappers)

	// Tokenized real-world assets meeting the internal/rwa definition. Identity is always
	// (code, issuer); anyone can issue a token with any code.
	s.mux.HandleFunc("GET /v1/rwa/assets", s.handleRWAAssets)
	s.mux.HandleFunc("GET /v1/rwa/history", s.handleRWAHistory)
	// The hand-vetted stablecoin set (catalogue class=stablecoin with a
	// Stellar issuer) with supply and USD value; the total is a lower bound.
	s.mux.HandleFunc("GET /v1/stablecoins", s.handleStablecoins)
	// Premium/discount to NAV, daily. A ratio of two sampled observations, so neither leg may be
	// carried across a day it was not observed on. See rwa_premium_history.go.
	s.mux.HandleFunc("GET /v1/rwa/premium", s.handleRWAPremiumHistory)

	// Account self-service: all three return 401 for anonymous callers; POST /keys also needs
	// the AccountStore wired (typically only when Redis is reachable).
	s.mux.HandleFunc("GET /v1/account/me", s.handleAccountMe)
	s.mux.HandleFunc("GET /v1/account/usage", s.handleAccountUsage)
	s.mux.HandleFunc("GET /v1/account/keys", s.handleAccountKeysList)
	s.mux.Handle("POST /v1/account/keys", middleware.Idempotency(
		middleware.NewIdempotencyStore(0), accountKeyIdempotencySubject,
	)(http.HandlerFunc(s.handleAccountKeysCreate)))
	s.mux.HandleFunc("DELETE /v1/account/keys/{keyID}", s.handleAccountKeysRevoke)
	// Operator surface (handleAdmin gates tier + X-Reason at the mount):
	// mint a key for ANOTHER identifier; audit-logged via Options.Audit.
	s.handleAdmin("POST /v1/admin/keys", s.handleAdminKeysCreate)
	// Operator surface: revoke ANOTHER identifier's key, the leaked-key kill switch (self-service
	// revoke needs the customer's own credential). Requires X-Reason and audit-logs "key.revoke".
	s.handleAdmin("DELETE /v1/admin/keys/{keyID}", s.handleAdminKeysRevoke)
	// Operator surface: per-account tier, status and limit overrides. PATCH needs X-Reason and
	// audit-logs "account.override.set"; it takes effect on the account's next key Lookup, which
	// enforces a suspended/closed `status` as the account-level kill switch.
	s.handleAdmin("GET /v1/admin/accounts/{id}", s.handleAdminAccountGet)
	s.handleAdmin("PATCH /v1/admin/accounts/{id}", s.handleAdminAccountOverrides)
	// Operator surface: customer-facing status banners; create + resolve require X-Reason and
	// audit-log.
	s.handleAdmin("GET /v1/admin/status-notices", s.handleAdminStatusNoticesList)
	s.handleAdmin("POST /v1/admin/status-notices", s.handleAdminStatusNoticeCreate)
	s.handleAdmin("POST /v1/admin/status-notices/{id}/resolve", s.handleAdminStatusNoticeResolve)
	// Retired: it returns the same 410 for every method and body, so the
	// route cannot distinguish a known email from a new one.
	s.handlePublic("POST /v1/signup", s.handleSignupRetired)
	// Open registration: one unauthenticated POST creates a free-tier account + first API key.
	// Shares /v1/signup's per-IP throttle.
	s.handlePublic("POST /v1/register", s.handleRegister)
	s.handlePublic("GET /v1/signup/verify", s.handleSignupRetired)
	s.handlePublic("POST /v1/signup/verify", s.handleSignupRetired)

	// Dashboard magic-link auth (login, callback, logout). Mounted only when DashboardAuth is
	// wired; otherwise the routes do not exist and ServeMux returns its standard 404.
	if s.DashboardAuth != nil {
		s.DashboardAuth.Mount(s.mux, s.publicRoutes)
	}

	// Dashboard key, webhook and price-alert routes: gated on the session cookie, mounted only
	// when the Postgres platform stores are wired.
	if s.DashboardKeys != nil {
		s.DashboardKeys.Mount(s.mux, s.publicRoutes)
	}
	if s.DashboardWebhooks != nil {
		s.DashboardWebhooks.Mount(s.mux, s.publicRoutes)
	}
	if s.DashboardPriceAlerts != nil {
		s.DashboardPriceAlerts.Mount(s.mux, s.publicRoutes)
	}

	// SEP-10 Web Auth. Both endpoints are unauthenticated by design: they bootstrap auth. Wired
	// only when the server-signing seed and JWT secret are configured.
	s.handlePublic("GET /v1/auth/sep10/challenge", s.handleSEP10Challenge)
	s.handlePublic("POST /v1/auth/sep10/token", s.handleSEP10Token)

	// Bare-root welcome. The `{$}` anchor matches ONLY "/", keeping ServeMux's 405 detection for
	// known paths; unknown paths fall through to Envelope404.
	s.mux.HandleFunc("GET /{$}", s.handleRoot)

	// /robots.txt disallows crawling the JSON API. Without this handler Cloudflare's managed
	// robots.txt serves GET but the origin returns 404 on HEAD.
	s.mux.HandleFunc("GET /robots.txt", s.handleRobotsTxt)

	// /.well-known/security.txt: RFC 9116 disclosure metadata. Its Canonical: directive points at
	// the explorer's copy so the two cannot drift.
	s.handlePublic("GET /.well-known/security.txt", s.handleSecurityTxt)

	// /errors/{slug}: dereferenceable RFC 9457 problem `type` URIs, served as a self-describing
	// page without hand-written docs per slug.
	s.mux.HandleFunc("GET /errors/{slug}", s.handleErrorDoc)
	s.mux.HandleFunc("GET /errors/{$}", s.handleErrorIndex)
}

// ─── Handlers ─────────────────────────────────────────────────────

// healthResponse is the shape for /healthz + /readyz.
type healthResponse struct {
	Status string `json:"status"` // ok | degraded
	// Uptime is a human-readable duration. Precise-to-the-second is
	// fine for monitoring.
	Uptime string `json:"uptime"`
	// Checks is populated on /readyz with per-dependency results.
	// Absent on /healthz.
	Checks []checkResult `json:"checks,omitempty"`
	// StatusRoot points probe consumers at /v1/status, the rollup covering ingest lag, supply,
	// oracle freshness and per-pair SLA latency.
	StatusRoot string `json:"status_root,omitempty"`
}

type checkResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error is populated only when OK is false; freeform string.
	Error string `json:"error,omitempty"`
}

// handleHealthz is the shallow liveness probe: no database or Redis. Probes cover the serving plane only so they do not flap when ingest
// stalls (see /v1/status).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, healthResponse{
		Status:     "ok",
		Uptime:     s.Uptime().Truncate(time.Second).String(),
		StatusRoot: "/v1/status",
	}, Flags{})
}

// handleReadyz pings every ReadyChecker in parallel under a short shared timeout: 200 only if all pass, else 503. Parallel keeps latency
// at the slowest check.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Single-flight + 1s result cache: /v1/readyz is unauthenticated and unlimited (LB probes
	// must never be throttled) and each round holds DB pool slots up to 2s, so uncached spam
	// could exhaust the shared pool. Callers share ONE round per second.
	//
	// readyzMu is never held across the round, or queued callers would block up to the 2s budget.
	// The round runs detached in fillReadyz; waiters can abandon via their own context.
	s.readyzMu.Lock()
	if time.Since(s.readyzAt) < time.Second && s.readyzBody != nil {
		code, body := s.readyzCode, s.readyzBody
		s.readyzMu.Unlock()
		writeReadyz(w, code, body)
		return
	}
	if ch := s.readyzFlight; ch != nil {
		s.readyzMu.Unlock()
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
	} else {
		done := make(chan struct{})
		s.readyzFlight = done
		s.readyzMu.Unlock()
		go s.fillReadyz(done) //nolint:contextcheck // deliberately detached: the round is SHARED by every queued caller (single-flight), so one caller's cancellation must not abort it — see fillReadyz's doc.
		select {
		case <-done:
		case <-r.Context().Done():
			return
		}
	}
	s.readyzMu.Lock()
	code, body := s.readyzCode, s.readyzBody
	s.readyzMu.Unlock()
	writeReadyz(w, code, body)
}

// writeReadyz emits an already-rendered readyz result.
func writeReadyz(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// fillReadyz runs one readyz round and releases the single-flight gate on every exit including a panic, else later /v1/readyz calls wedge
// on a channel nobody closes.
func (s *Server) fillReadyz(done chan struct{}) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(s.logger, "api-readyz-fill", rec)
		}
		s.readyzMu.Lock()
		s.readyzFlight = nil
		s.readyzMu.Unlock()
	}()
	code, body := s.computeReadyz()
	s.readyzMu.Lock()
	s.readyzCode, s.readyzBody, s.readyzAt = code, body, time.Now()
	s.readyzMu.Unlock()
}

// ReadinessProbeCadence is how often [Server.StartReadinessProbe] runs a readiness round: one Prometheus scrape interval.
const ReadinessProbeCadence = 15 * time.Second

// StartReadinessProbe runs a readiness round every `every` until ctx ends, so stellarindex_dependency_up stays current when nothing calls /v1/readyz.
func (s *Server) StartReadinessProbe(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.probeReadyz() //nolint:contextcheck // the round is shared with queued /v1/readyz callers and carries its own 2s budget; ctx bounds only the loop.
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// probeReadyz runs one round through the /v1/readyz single-flight and cache,
// skipping the tick when a request-driven round is already in flight.
func (s *Server) probeReadyz() {
	s.readyzMu.Lock()
	if s.readyzFlight != nil {
		s.readyzMu.Unlock()
		return
	}
	done := make(chan struct{})
	s.readyzFlight = done
	s.readyzMu.Unlock()
	s.fillReadyz(done)
}

// computeReadyz runs one full check round and renders the response. It is detached from any
// caller's request context: one impatient caller's disconnect must not cancel the shared round.
func (s *Server) computeReadyz() (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results := make([]checkResult, len(s.ReadyChecks))
	criticalFlags := make([]bool, len(s.ReadyChecks))
	var wg sync.WaitGroup
	for i, c := range s.ReadyChecks {
		wg.Add(1)
		criticalFlags[i] = c.Critical()
		go func(i int, c ReadyChecker) {
			defer wg.Done()
			// Detached from the handler goroutine, so an unrecovered panic in a Ping would take
			// the API down. Recovering leaves results[i] zero (OK false), so readyz reports NOT
			// READY and the load balancer sheds this instance: fail-closed is right for readiness.
			defer worker.Recover(s.logger, "api-readyz-check")
			r := s.runReadyCheck(ctx, c)
			results[i] = r // distinct indices — no mutex needed

			// Publish the outcome as an alertable gauge: JSON on /v1/readyz alone cannot page on a
			// dependency going away, and ClickHouse (the raw lake) has no exporter of its own.
			up := 0.0
			if r.OK {
				up = 1
			}
			obs.DependencyUp.WithLabelValues(c.Name()).Set(up)
		}(i, c)
	}
	wg.Wait()

	// Critical failures 503; non-critical ones return 200 status="degraded", so a Redis outage
	// cannot make HAProxy drain every healthy backend while the Timescale fallback still serves.
	criticalFailed := false
	anyFailed := false
	for i, r := range results {
		if r.OK {
			continue
		}
		anyFailed = true
		if criticalFlags[i] {
			criticalFailed = true
		}
	}

	resp := healthResponse{
		Status:     "ok",
		Uptime:     s.Uptime().Truncate(time.Second).String(),
		Checks:     results,
		StatusRoot: "/v1/status",
	}
	render := func(status int, flags Flags) (int, []byte) {
		env := Envelope{Data: resp, AsOf: WireTime(time.Now().UTC()), Flags: flags}
		b, err := json.Marshal(env)
		if err != nil {
			return http.StatusInternalServerError, []byte(`{"error":"readyz render"}`)
		}
		return status, b
	}
	switch {
	case criticalFailed:
		resp.Status = "unready"
		return render(http.StatusServiceUnavailable, Flags{Stale: true})
	case anyFailed:
		// Non-critical dependency degraded: the API still serves (Timescale fallback for Redis
		// misses, ADR-0007), so 200 keeps the backend in HAProxy's pool; the body's per-check
		// breakdown tells operators what is down.
		resp.Status = "degraded"
		return render(http.StatusOK, Flags{Stale: true})
	}
	return render(http.StatusOK, Flags{})
}

// readyCheckFailedDetail is the fixed checks[].error for a failed ping. Never the driver error: readyz is unauthenticated and a driver
// error names its endpoint.
const readyCheckFailedDetail = "ping failed — see the API server log for the underlying error"

// readyzPublicError is a Ping failure whose text is safe for the public readyz body; every other error is served as
// [readyCheckFailedDetail].
type readyzPublicError struct{ msg string }

func (e readyzPublicError) Error() string { return e.msg }

// runReadyCheck pings one dependency, logging the raw error server-side
// and returning only a publishable detail.
func (s *Server) runReadyCheck(ctx context.Context, c ReadyChecker) checkResult {
	err := c.Ping(ctx)
	r := checkResult{Name: c.Name(), OK: err == nil}
	if err != nil {
		s.logger.Warn("readyz: dependency ping failed", "check", c.Name(), "critical", c.Critical(), "err", err)
		r.Error = readyCheckFailedDetail
		var pub readyzPublicError
		if errors.As(err, &pub) {
			r.Error = pub.msg
		}
	}
	return r
}

// livezLakeTTL is how long one lake-ping round is reused: at most one ClickHouse query per second under an anonymous probe burst.
const livezLakeTTL = time.Second

// livezLakeUnreadyDetail is the fixed hint for a failed lake ping, never the driver error (it carries the ClickHouse endpoint; the route
// is unauthenticated).
const livezLakeUnreadyDetail = "clickhouse ping failed — see the API server log for the underlying error"

// lakeHealth is the /v1/livez/lake payload (data member of the envelope).
type lakeHealth struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// handleLivezLake is the LAKE-critical probe: 200 iff the ClickHouse checker pings; 503 on failure or when no lake is wired (fails
// closed). /v1/readyz treats ClickHouse as non-critical.
// Single-flight plus 1s cache, since the route is unauthenticated and rate-limit exempt. livezLakeMu is never held across the ping; the
// round runs detached in fillLivezLake.
func (s *Server) handleLivezLake(w http.ResponseWriter, r *http.Request) {
	s.livezLakeMu.Lock()
	if time.Since(s.livezLakeAt) < livezLakeTTL && s.livezLakeBody != nil {
		code, body := s.livezLakeCode, s.livezLakeBody
		s.livezLakeMu.Unlock()
		writeLivezLake(w, code, body)
		return
	}
	if ch := s.livezLakeFlight; ch != nil {
		s.livezLakeMu.Unlock()
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
	} else {
		done := make(chan struct{})
		s.livezLakeFlight = done
		s.livezLakeMu.Unlock()
		go s.fillLivezLake(done) //nolint:contextcheck // deliberately detached: the round is SHARED by every queued caller (single-flight), so one caller's cancellation must not abort it — see fillLivezLake's doc.
		select {
		case <-done:
		case <-r.Context().Done():
			return
		}
	}
	s.livezLakeMu.Lock()
	code, body := s.livezLakeCode, s.livezLakeBody
	s.livezLakeMu.Unlock()
	writeLivezLake(w, code, body)
}

// fillLivezLake runs one lake-ping round and publishes the result, releasing the single-flight
// gate on every exit including a panic, as fillReadyz does.
func (s *Server) fillLivezLake(done chan struct{}) {
	defer close(done)
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(s.logger, "api-livezlake-fill", rec)
		}
		s.livezLakeMu.Lock()
		s.livezLakeFlight = nil
		s.livezLakeMu.Unlock()
	}()
	code, body := s.computeLivezLake()
	s.livezLakeMu.Lock()
	s.livezLakeCode, s.livezLakeBody, s.livezLakeAt = code, body, time.Now()
	s.livezLakeMu.Unlock()
}

// writeLivezLake emits an already-rendered probe result. no-store stops intermediaries caching
// a liveness verdict; the 1s reuse window is ours to control, not a CDN's.
func writeLivezLake(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// computeLivezLake runs one lake-ping round and renders the response, detached from any
// caller's request context for the same reason as computeReadyz.
func (s *Server) computeLivezLake() (int, []byte) {
	render := func(status int, body lakeHealth) (int, []byte) {
		env := Envelope{Data: body, AsOf: WireTime(time.Now().UTC())}
		b, err := json.Marshal(env)
		if err != nil {
			return http.StatusInternalServerError, []byte(`{"error":"livez render"}`)
		}
		return status, b
	}
	for _, c := range s.ReadyChecks {
		if c.Name() != "clickhouse" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			s.logger.Warn("livez/lake: clickhouse ping failed — serving 503 lake-unready", "err", err)
			return render(http.StatusServiceUnavailable, lakeHealth{
				Status: "lake-unready",
				Detail: livezLakeUnreadyDetail,
			})
		}
		return render(http.StatusOK, lakeHealth{Status: "ok"})
	}
	return render(http.StatusServiceUnavailable, lakeHealth{
		Status: "lake-absent",
		Detail: "no clickhouse checker registered — this deployment serves no lake routes and must not receive lake traffic",
	})
}

// handleVersion reports binary version, build date and VCS info; `dirty` flags uncommitted changes in the build tree (false in production
// builds).
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		"version":    version.Version,
		"build_date": version.BuildDate,
		"commit":     version.Commit,
		"dirty":      version.Dirty,
		"go_version": version.GoVersion,
	}, Flags{})
}

// handleSecurityTxt serves /.well-known/security.txt per RFC 9116, pointing at the explorer copy so origins cannot drift; Expires is one
// year out at request time.
func (s *Server) handleSecurityTxt(w http.ResponseWriter, _ *http.Request) {
	expires := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
	body := "# Stellar Index — security.txt (api origin)\n" +
		"# RFC-9116. Mirrors stellarindex.io/.well-known/security.txt;\n" +
		"# the Canonical: URL is the authoritative copy.\n" +
		"\n" +
		"Contact: mailto:security@stellarindex.io\n" +
		"Expires: " + expires + "\n" +
		"Preferred-Languages: en\n" +
		"Canonical: https://stellarindex.io/.well-known/security.txt\n" +
		"Policy: https://github.com/Stellar-Index/StellarIndex/blob/main/SECURITY.md\n" +
		"Acknowledgments: https://github.com/Stellar-Index/StellarIndex/security/advisories\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(body))
}

// handleRoot welcomes accidental visitors at GET / with the binary version and a docs pointer;
// not part of the public API surface (no OpenAPI entry).
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		"name":    "stellar-index",
		"version": version.Version,
		"docs":    "https://docs.stellarindex.io",
		"openapi": "https://docs.stellarindex.io/openapi.yaml",
	}, Flags{})
}

// handleErrorDoc serves a dereferenceable page for one RFC 9457 problem `type` slug: JSON for API clients, minimal HTML for a browser.
func (s *Server) handleErrorDoc(w http.ResponseWriter, r *http.Request) {
	// The slug is URL-path input; constrain it to the kebab-case charset real error types use.
	// Anything else is not ours and is not reflected, which also keeps HTML metacharacters out
	// of every renderer.
	slug := r.PathValue("slug")
	if !errorSlugRE.MatchString(slug) {
		slug = "unknown"
	}
	// A slug this API never emits dereferences to nothing real: 404 it.
	if _, ok := knownErrorSlugs[slug]; !ok {
		http.NotFound(w, r)
		return
	}
	human := humaniseErrorSlug(slug)
	typeURI := "https://api.stellarindex.io/errors/" + slug
	// Content-negotiated on Accept: a cache keyed on URL alone would serve one representation to
	// a client wanting the other for the full max-age=3600.
	w.Header().Add("Vary", "Accept")
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		// html/template auto-escapes every interpolation — no hand-rolled
		// escaper, and gosec's taint analysis trusts it.
		_ = errorDocTmpl.Execute(w, map[string]string{"human": human, "slug": slug})
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, map[string]string{
		"type":    typeURI,
		"slug":    slug,
		"title":   human,
		"detail":  "A Stellar Index API error type. The response body that referenced this URI carries the specifics (status, instance, request_id).",
		"docs":    "https://docs.stellarindex.io",
		"openapi": "https://docs.stellarindex.io/openapi.yaml",
	}, Flags{})
}

// handleErrorIndex serves GET /errors/ (no slug) — a pointer at the docs.
func (s *Server) handleErrorIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, map[string]string{
		"name":   "stellar-index error types",
		"detail": "Problem `type` URIs resolve at /errors/{slug}. See the full API reference for the endpoints that emit each.",
		"docs":   "https://docs.stellarindex.io",
	}, Flags{})
}

// humaniseErrorSlug turns "account-not-found" into "Account not found".
func humaniseErrorSlug(slug string) string {
	if slug == "" {
		return "Error"
	}
	words := strings.Split(slug, "-")
	for i, w := range words {
		if i == 0 && w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// errorSlugRE constrains an error-type slug to the closed kebab-case
// charset every real problem type uses.
var errorSlugRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// knownErrorSlugs is the closed set of problem `type` slugs this API emits; a retired or mistyped slug must not be reflected as a live
// 200 page.
var knownErrorSlugs = map[string]struct{}{
	"account-activity-timeout":        {},
	"account-closed":                  {},
	"account-cohort-timeout":          {},
	"account-cohort-warming":          {},
	"account-create-failed":           {},
	"account-creators-timeout":        {},
	"account-creators-warming":        {},
	"account-get-failed":              {},
	"account-graph-history-timeout":   {},
	"account-graph-history-warming":   {},
	"account-graph-timeout":           {},
	"account-graph-warming":           {},
	"account-list-failed":             {},
	"account-movements-timeout":       {},
	"account-not-found":               {},
	"account-operations-timeout":      {},
	"account-positions-timeout":       {},
	"account-revoke-failed":           {},
	"account-sponsors-timeout":        {},
	"account-sponsors-warming":        {},
	"account-state-timeout":           {},
	"account-store-unavailable":       {},
	"account-trades-timeout":          {},
	"account-transactions-timeout":    {},
	"account-update-failed":           {},
	"accounts-stats-timeout":          {},
	"accounts-stats-warming":          {},
	"aggregators-error":               {},
	"aggregators-unavailable":         {},
	"all-filtered":                    {},
	"already-authenticated":           {},
	"already-signed-up":               {},
	"archive-report-missing":          {},
	"archive-report-unavailable":      {},
	"asset-holders-timeout":           {},
	"asset-is-external":               {},
	"asset-is-stellar":                {},
	"asset-not-found":                 {},
	"auth":                            {},
	"auth-not-configured":             {},
	"auth-status-unavailable":         {},
	"backups-unavailable":             {},
	"body-too-large":                  {},
	"cache-unavailable":               {},
	"cannot-revoke-self":              {},
	"capacity":                        {},
	"change-summary-error":            {},
	"change-summary-not-found":        {},
	"change-summary-unavailable":      {},
	"chart-timeout":                   {},
	"conflicting-filters":             {},
	"contract-code-history-timeout":   {},
	"contract-detail-timeout":         {},
	"contract-interactions-timeout":   {},
	"contract-is-sac":                 {},
	"contract-wasm-not-found":         {},
	"contract-wasm-timeout":           {},
	"contracts-timeout":               {},
	"coverage-unavailable":            {},
	"cross-site-request-blocked":      {},
	"cursor-too-deep":                 {},
	"cursors-error":                   {},
	"cursors-timeout":                 {},
	"cursors-transient":               {},
	"cursors-unavailable":             {},
	"dashboard":                       {},
	"dex-tvl-unavailable":             {},
	"directory-timeout":               {},
	"empty-patch":                     {},
	"endpoint-retired":                {},
	"explorer-unavailable":            {},
	"forbidden":                       {},
	"history-timeout":                 {},
	"history-unavailable":             {},
	"idempotency-key-in-flight":       {},
	"idempotency-key-too-long":        {},
	"identity-pair":                   {},
	"identity-price":                  {},
	"internal":                        {},
	"invalid-account":                 {},
	"invalid-account-id":              {},
	"invalid-address":                 {},
	"invalid-addresses":               {},
	"invalid-aggregate":               {},
	"invalid-asset":                   {},
	"invalid-asset-class":             {},
	"invalid-asset-id":                {},
	"invalid-body":                    {},
	"invalid-class":                   {},
	"invalid-code":                    {},
	"invalid-contract-id":             {},
	"invalid-cursor":                  {},
	"invalid-email":                   {},
	"invalid-entity-id":               {},
	"invalid-entity-type":             {},
	"invalid-g-strkey":                {},
	"invalid-granularity":             {},
	"invalid-group-by":                {},
	"invalid-interval":                {},
	"invalid-issuer":                  {},
	"invalid-kind":                    {},
	"invalid-ledger":                  {},
	"invalid-limit":                   {},
	"invalid-market-cap-quote":        {},
	"invalid-max-age":                 {},
	"invalid-monthly-quota":           {},
	"invalid-notice-body":             {},
	"invalid-notice-id":               {},
	"invalid-observations-param":      {},
	"invalid-order":                   {},
	"invalid-orderbook-request":       {},
	"invalid-pair":                    {},
	"invalid-parameter":               {},
	"invalid-pool":                    {},
	"invalid-price-type":              {},
	"invalid-query":                   {},
	"invalid-quote":                   {},
	"invalid-rate-limit":              {},
	"invalid-records":                 {},
	"invalid-relation":                {},
	"invalid-scope":                   {},
	"invalid-severity":                {},
	"invalid-sigma":                   {},
	"invalid-status":                  {},
	"invalid-stream-param":            {},
	"invalid-suspended-reason":        {},
	"invalid-tier":                    {},
	"invalid-time":                    {},
	"invalid-timeframe":               {},
	"invalid-tip-param":               {},
	"invalid-title":                   {},
	"invalid-ts":                      {},
	"invalid-tx-hash":                 {},
	"invalid-type":                    {},
	"invalid-window":                  {},
	"issuer-error":                    {},
	"issuer-not-found":                {},
	"issuer-timeout":                  {},
	"issuers-error":                   {},
	"issuers-timeout":                 {},
	"issuers-transient":               {},
	"issuers-unavailable":             {},
	"key-mint-not-available":          {},
	"key-not-found":                   {},
	"key-quota-exceeded":              {},
	"label-too-long":                  {},
	"ledger-detail-timeout":           {},
	"ledger-not-found":                {},
	"ledger-tip-unavailable":          {},
	"ledger-transactions-timeout":     {},
	"ledgers-timeout":                 {},
	"lending-timeout":                 {},
	"limit-too-large":                 {},
	"malformed-credential":            {},
	"market-cap-unavailable":          {},
	"market-sources-error":            {},
	"market-sources-timeout":          {},
	"markets-timeout":                 {},
	"method-not-allowed":              {},
	"mev-timeout":                     {},
	"missing-account":                 {},
	"missing-asset":                   {},
	"missing-asset-ids":               {},
	"missing-base":                    {},
	"missing-body":                    {},
	"missing-email":                   {},
	"missing-identifier":              {},
	"missing-key-id":                  {},
	"missing-label":                   {},
	"missing-params":                  {},
	"missing-quote":                   {},
	"missing-reason":                  {},
	"missing-token":                   {},
	"missing-transaction":             {},
	"missing-ts":                      {},
	"monthly-quota-exceeded":          {},
	"monthly-quota-unavailable":       {},
	"name-too-long":                   {},
	"network-stats-error":             {},
	"network-stats-unavailable":       {},
	"network-throughput-timeout":      {},
	"no-trades":                       {},
	"not-found":                       {},
	"observations-timeout":            {},
	"observations-unavailable":        {},
	"ohlc-timeout":                    {},
	"ohlc-unavailable":                {},
	"operations-timeout":              {},
	"operator-required":               {},
	"oracle-lastprice-timeout":        {},
	"oracle-latest-timeout":           {},
	"oracle-prices-timeout":           {},
	"oracle-streams-timeout":          {},
	"oracle-unavailable":              {},
	"oracle-xlastprice-timeout":       {},
	"orderbook-loading":               {},
	"orderbook-unavailable":           {},
	"pool-reserves-timeout":           {},
	"pools-timeout":                   {},
	"price-not-found":                 {},
	"price-unavailable":               {},
	"price-withheld":                  {},
	"protocol-detail-timeout":         {},
	"protocol-not-found":              {},
	"protocol-tvl-not-derived":        {},
	"rate-limited":                    {},
	"request-timeout":                 {},
	"scope-exceeds-caller":            {},
	"sep10-challenge-expired":         {},
	"sep10-malformed-transaction":     {},
	"sep10-unavailable":               {},
	"sep10-verification-failed":       {},
	"sep41-transfers-error":           {},
	"sep41-transfers-timeout":         {},
	"sep41-transfers-transient":       {},
	"sep41-transfers-unavailable":     {},
	"signup-rate-limited":             {},
	"signup-verify-required":          {},
	"source-not-found":                {},
	"status-notice-create-failed":     {},
	"status-notice-list-failed":       {},
	"status-notice-not-found":         {},
	"status-notice-resolve-failed":    {},
	"status-notice-store-unavailable": {},
	"stream-unavailable":              {},
	"supply-error":                    {},
	"supply-incomplete":               {},
	"supply-not-mapped":               {},
	"supply-unavailable":              {},
	"throttle-unavailable":            {},
	"token-expired":                   {},
	"too-many-assets":                 {},
	"too-many-failed-auth":            {},
	"twap-unavailable":                {},
	"tx-detail-timeout":               {},
	"tx-not-found":                    {},
	"unauthorized":                    {},
	"unknown-pool":                    {},
	"unknown-source":                  {},
	"unsupported-media-type":          {},
	"unsupported-reserves-source":     {},
	"verified-currencies-unavailable": {},
	"vwap-unavailable":                {},
	"warming-up":                      {},
}

// errorDocTmpl is the browser page for an error type; html/template auto-escapes the reflected path value.
var errorDocTmpl = htmltemplate.Must(htmltemplate.New("errdoc").Parse(
	`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>{{.human}} · Stellar Index API errors</title>` +
		`<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}` +
		`code{background:#f4f4f5;padding:.1em .3em;border-radius:3px}a{color:#2563eb}</style></head><body>` +
		`<h1>{{.human}}</h1>` +
		`<p>This is a Stellar Index API error type — <code>{{.slug}}</code>. The error response that pointed you ` +
		`here carries the specifics (HTTP status, the request path, and a <code>request_id</code>).</p>` +
		`<p>For the full API reference, see <a href="https://docs.stellarindex.io">docs.stellarindex.io</a>.</p>` +
		`</body></html>`))

// handleRobotsTxt: the JSON API origin is not for indexing, so crawlers are pointed at docs and explorer; `Sitemap:` covers a crawler
// that ignores Disallow.
func (s *Server) handleRobotsTxt(w http.ResponseWriter, _ *http.Request) {
	const body = `# api.stellarindex.io — JSON API, not for human reading.
# Indexable content lives on the companion subdomains:
#   - https://stellarindex.io          — explorer + market UI
#   - https://docs.stellarindex.io     — API reference
#   - https://status.stellarindex.io   — status + incident postmortems

User-agent: *
Disallow: /

Sitemap: https://stellarindex.io/sitemap.xml
`
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(body))
}

// backupMetricsFor resolves the /v1/diagnostics/backups seam: Options.BackupMetrics wins, else the status backend when it answers raw
// PromQL; otherwise 503.
func backupMetricsFor(opts Options) backupMetricsSource {
	if opts.BackupMetrics != nil {
		return opts.BackupMetrics
	}
	if src, ok := opts.StatusBackend.(backupMetricsSource); ok && src != nil {
		return src
	}
	return nil
}
