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

// ReadyChecker is what /readyz polls to decide whether serving-plane dependencies respond.
//
// Ping MUST respect ctx: checkers run in parallel under a shared 2 s deadline, and one that
// ignores ctx turns readyz into a cascade-failure vector for the liveness probe.
//
// Critical() separates "cannot serve without this" (Postgres) from "degrades but serves"
// (Redis; a cache miss falls back to Timescale). /readyz returns 503 ONLY for a failing
// critical check; treating all as critical would let a Redis outage drain every healthy backend.
type ReadyChecker interface {
	Ping(ctx context.Context) error
	Name() string
	Critical() bool
}

// ExpectedSchemaVersion is the migrations/ head this binary was built against. The applied
// schema must be at least this and not dirty, or [NewSchemaVersionChecker] fails readiness
// (critical, 503) so a stale schema never hides behind a green healthz.
//
// The comparison is `applied >= expected`: a NEWER schema is safe during a rolling deploy and
// after a rollback. A dirty row alone is not a mismatch, see [nonAtomicMigrationVersions].
// TestExpectedSchemaVersionMatchesMigrationsHead fails CI if a migration is added unbumped.
const ExpectedSchemaVersion uint = 214

// nonAtomicMigrationVersions lists migrations whose up.sql commits mid-file, breaking
// golang-migrate's one-transaction-per-file guarantee. A dirty flag at any version NOT in
// this set means the attempt rolled back cleanly and the schema is intact at version-1; only
// these versions leave a genuinely partial schema when dirty.
var nonAtomicMigrationVersions = map[uint]bool{30: true}

// SchemaVersionReader reports the applied golang-migrate schema state (schema_migrations.version
// + dirty). cmd/stellarindex-api adapts *timescale.Store, keeping the raw SQL in the binary layer.
type SchemaVersionReader interface {
	// SchemaMigrationVersion returns the highest applied migration number
	// and whether the last migration left the bookkeeping row dirty
	// (partially applied). version==0 means no migrations are applied.
	SchemaMigrationVersion(ctx context.Context) (version uint, dirty bool, err error)
}

// schemaVersionChecker is the critical ReadyChecker that asserts the
// applied schema is at least [ExpectedSchemaVersion] and not dirty.
type schemaVersionChecker struct {
	reader   SchemaVersionReader
	expected uint
}

// NewSchemaVersionChecker builds the schema-head readiness check. It is critical: a
// binary/schema mismatch must drain the backend, not serve partial data behind a green probe.
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
	// Every other migration runs in one transaction, so dirty means the attempt rolled back and
	// the schema is really at version-1. The dirty row still needs an operator `force`, which
	// schemaDirtyChecker reports non-critically so it does not drain the backend by itself.
	effective := version
	if dirty && version > 0 {
		effective = version - 1
	}
	if effective < c.expected {
		return readyzPublicError{fmt.Sprintf("schema/binary mismatch: binary expects migrations head >= %d but only %d is applied (dirty=%v) — a deploy skipped migrations or the binary was swapped ahead of them", c.expected, effective, dirty)}
	}
	return nil
}

// schemaDirtyChecker is the non-critical sibling of schemaVersionChecker: it flags a dirty
// schema_migrations row as readyz "degraded" without draining the backend, because for atomic
// migrations dirty means a clean rollback (see [nonAtomicMigrationVersions]).
type schemaDirtyChecker struct {
	reader SchemaVersionReader
}

// NewSchemaDirtyChecker builds the non-critical dirty-row visibility check.
// Register it alongside [NewSchemaVersionChecker] against the same reader.
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

// OpenBucketCAGGReader lists the continuous aggregates that serve their in-progress bucket
// outside the real-time allowlist; *timescale.Store satisfies it.
type OpenBucketCAGGReader interface {
	OpenBucketCAGGs(ctx context.Context) ([]string, error)
}

// closedBucketChecker is the critical ReadyChecker for ADR-0015 at runtime. Most CAGG readers
// have no closed-bucket predicate and rely on materialized_only; an out-of-band ALTER would make
// them serve the open bucket beside a guarded /v1/price.
type closedBucketChecker struct {
	reader OpenBucketCAGGReader
}

// NewClosedBucketChecker builds the materialized_only readiness check. It is critical: two
// money surfaces disagreeing on one pair must drain the backend.
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

// Server is the HTTP handler for the Stellar Index v1 API. [New] returns it with routes mounted;
// [Server.Handler] yields the http.Handler. Thread-safe.
type Server struct {
	// Options is the construction-time wiring as supplied to [New]; the
	// fields below it are state derived from it.
	Options
	logger         *slog.Logger
	rwaCuratedSnap rwaCuratedCache
	// assetListings memoises one read of the listing directory for the /v1/assets listing-priced
	// valuation arm (asset_listing_valuation.go). Not shared with rwaListings, which serves
	// contract rows only and is rebuilt per RWA request.
	assetListings assetListingCache
	// holds is the operator under-review list (SetHolds).
	holds atomic.Pointer[[]holds.Hold]
	// Cached lake watermark, see lakeWatermark(). lakeWMMu guards the entry only and is never
	// held across the lake read, which would queue callers on a non-context-aware mutex past their
	// deadlines. lakeWMFlight coalesces refreshes; lakeWMNextTry is the negative cache, so a
	// wedged lake is retried once per lakeWatermarkRetryGap, not per request.
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
	// Cached top-N native (CAP-38) liquidity-pool listing: a whole-prefix lake scan (~40k pools)
	// ranked in Go. nativeLPMu is NEVER held across the scan, or the first caller after each TTL
	// lapse would pay it inline while the rest queue. nativeLPFillMu collapses concurrent COLD
	// fills onto one scan; nativeLPRefreshing admits one detached rescan at a time.
	nativeLPMu         sync.Mutex
	nativeLPCached     []LiquidityPoolReservesRow
	nativeLPAll        []clickhouse.NativeLiquidityPoolState
	nativeLPFetched    time.Time
	nativeLPFillMu     sync.Mutex
	nativeLPRefreshing atomic.Bool
	// protocolFast{Mu,Settled,OK} cache the daily-pre-aggregation probe only once it gave a
	// definitive answer (see fastActivity). Not a sync.Once: that would latch a transient
	// first-probe error as "unavailable" for the process lifetime.
	protocolFastMu      sync.Mutex
	protocolFastSettled bool
	protocolFastOK      bool
	protocolPoolTokens  ProtocolPoolTokensReader
	// Per-server TTL + single-flight cache for the expensive
	// /v1/protocols/{name} detail (lazy-init'd — see cachedProtocolDetail).
	protoDetailMu     sync.Mutex
	protoDetailCache  map[string]protoDetailEntry
	protoDetailFlight map[string]chan struct{}
	// Last-good cache for the bespoke analytics block inside that detail view. The block is built
	// last and would otherwise inherit the rebuild's leftover budget and vanish when it runs out.
	// Zero value ready; see protocol_bespoke_cache.go.
	protocolBespokeCache bespokeCache
	// Prewarmed SWR cache for the per-source contract_count on GET /v1/protocols. The roster is a
	// served-tier `SELECT DISTINCT ... LIMIT 5000` scan on an unauthenticated route, so scanning
	// per protocol on every origin miss would be a DoS surface. Zero value ready.
	protocolRosterCache rosterCache
	// TTL + single-flight cache for the classic circulating-supply map (one ~0.5s ClickHouse
	// GROUP BY over the trustline slice, see cachedClassicSupply). Backs market-cap fill on the
	// long tail of /v1/assets.
	classicSupplyMu     sync.Mutex
	classicSupplyCache  map[string]string
	classicSupplyAt     time.Time
	classicSupplyFlight chan struct{}
	// classicSupplyAttemptAt advances on every refresh attempt, unlike classicSupplyAt (success
	// only). Without it a failing refresh leaves the cache stale and every request retries the
	// heavy query and pays the full timeout; it gates retries to classicSupplyRetryGap.
	classicSupplyAttemptAt time.Time
	// Per-asset TTL cache for the lake-flows classic supply, which includes the claimable-balance,
	// LP-reserve and SAC-held supply the trustline sum above cannot see. Filled a bounded batch
	// at a time by a detached refresh; see classic_lake_supply.go.
	lakeSupplyMu        sync.Mutex
	lakeSupply          map[string]lakeSupplyEntry
	lakeSupplyFlight    chan struct{}
	lakeSupplyAttemptAt time.Time
	// Stale-while-revalidate cache for the SEP-1 logo map ([buildSep1ImageMap] key to safe image
	// URL), built from every verified issuer's cached sep1_payload in one scan; see
	// cachedSep1Images. sep1ImagesAt (last rebuilt) drives freshness; sep1ImagesAttemptAt (last
	// started) rate-limits attempts so a failing scan is not retried by every request.
	sep1ImagesMu        sync.Mutex
	sep1ImagesCache     map[string]string
	sep1ImagesAt        time.Time
	sep1ImagesAttemptAt time.Time
	sep1ImagesFlight    chan struct{}

	// RWA membership set: the (code, issuer) pairs meeting the internal/rwa definition, rebuilt
	// off the request path behind a TTL + single-flight. It holds no valuation; membership is
	// decided before any number is attached.
	//
	// rwaAt is the last rebuilt time; rwaAttemptAt the last started time, which rate-limits a
	// failing rebuild (see [Server.readRWAMembership]). rwaFailedAt is cleared by the next
	// success; it distinguishes a lapsed set a rebuild will replace from one nothing is replacing.
	rwaMu        sync.Mutex
	rwaCache     *rwaMembership
	rwaAt        time.Time
	rwaAttemptAt time.Time
	rwaFailedAt  time.Time
	rwaFlight    chan struct{}
	// Oracle reference snapshot: the independent per-instrument valuations the premium/discount
	// is measured against. Kept apart from the membership cache and much shorter-lived because it
	// is a price: a stale NAV against a live market price reports a premium neither supports.
	rwaRefMu     sync.Mutex
	rwaRefCache  *rwaReferences
	rwaRefAt     time.Time
	rwaRefFlight chan struct{}
	// The RWA value-over-time assembly (/v1/rwa/history). Cached on the membership cadence, not
	// the reference one: it is a daily series whose newest bucket cannot move faster than the
	// oracle publishes, and both legs are scans (the lake flow log and every oracle day bucket).
	rwaHistMu     sync.Mutex
	rwaHistCache  *rwaValueHistory
	rwaHistAt     time.Time
	rwaHistFlight chan struct{}
	// The RWA premium-over-time assembly (/v1/rwa/premium). Separate from the value history: they
	// share membership and reference legs but not the market leg, and one failing must not blank
	// the other.
	rwaPremMu     sync.Mutex
	rwaPremCache  *rwaPremiumHistory
	rwaPremAt     time.Time
	rwaPremFlight chan struct{}

	// contractIndex caches contract → protocol for the cohort view's
	// contract labels; see contract_protocol_index.go.
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
	// streamDrain is the shutdown broadcast every SSE writer watches alongside its request
	// context. Created here, not in Options, since every streaming Server needs one; the
	// http.Server owner hands it to RegisterOnShutdown, see [Server.BeginStreamDrain].
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
	// verifiedSACs is the reverse map knownSACs cannot serve: computed SAC contract id to the
	// native/verified-catalogue asset it wraps. It excludes the operator's sac_wrappers (which
	// may name unverified assets) because it backs an identity trust decision, the oracle ticker
	// gate in oracle.go.
	verifiedSACsOnce sync.Once
	verifiedSACs     map[string]canonical.Asset
	// assetDetailCache is the short-TTL response-level cache for /v1/assets/{id} (rendered JSON
	// + Flags per asset_id). A hit skips the handler chain, including the uncached DB calls in
	// applyF2Fields, and cannot drift because the entry is what the handler produced. Nil or
	// ttl=0 always misses.
	assetDetailCache *assetDetailResponseCache
	// assetListCache is the response-level cache for the default
	// /v1/assets listing; see [assetListResponseCache].
	assetListCache *assetListResponseCache
	// pegDeclaredAt is when this deployment adopted the operator's peg declarations
	// ([Options.PegDeclaredAt], else construction time). It is the observed_at
	// [Server.declaredPegSnapshot] stamps on the flat $1 answer: a declaration is a constant, not
	// an observation, so adoption time is the only honest event time.
	pegDeclaredAt time.Time
	// ingestionSnapshot caches IngestionDiagnostics, rebuilt every ~15s by
	// [Server.StartIngestionSnapshotRefresh]. Nil before the first refresh, when the handler
	// builds inline at 200-500ms.
	ingestionSnapshot atomic.Pointer[ingestionSnapshotEntry]
	mux               *http.ServeMux
	// publicRoutes records every route mounted via handlePublic; Handler
	// wires its Mark middleware outside Auth.
	publicRoutes *middleware.PublicRoutes
	started      time.Time
	// requestTimeout bounds every non-streaming request's context via the RequestTimeout
	// middleware. Defaults to [defaultRequestTimeout]; zero only under
	// Options.DisableRequestTimeout. It is the chokepoint that gives every handler a deadline.
	requestTimeout time.Duration
}

// defaultRequestTimeout is the fallback per-request deadline when Options.RequestTimeout is
// unset. 15s outlasts the per-read 8s ceilings (so their more specific errors surface first)
// and stays under the http.Server WriteTimeout (30s) so the app-layer deadline bounds a hang.
const defaultRequestTimeout = 15 * time.Second

// maxHandlerBudget is the ceiling for a per-handler context.WithTimeout budget. A budget at or
// above the global request deadline never fires: the middleware deadline reaches the reader
// first and the handler's specific 503 branch is unreachable.
//
// The 3s headroom is response-writing room, not a boot-enforced minimum;
// [config.APIConfig.validate] requires only api.request_timeout > [config.APIMaxHandlerBudget].
// TestHandlerBudgets_StayInsideTheRequestTimeout and TestMaxHandlerBudgetMatchesConfigBound
// enforce the bound and the config mirror.
const maxHandlerBudget = defaultRequestTimeout - 3*time.Second

// DashboardAuthMounter is satisfied by main.go's dashboardauth.Handlers. It is defined here so
// this package does not import dashboardauth; the dependency runs the other way and main.go
// wires the result into v1.Options.
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
	// Assets, when non-nil, backs /v1/assets and /v1/assets/{id}.
	// Leave nil during early bring-up; handlers return an empty
	// list + degrade single-asset lookups to pure canonical echo.
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

	// Markets, when non-nil, backs /v1/markets. Leave nil and the
	// handler serves an empty list (mirrors /v1/assets' pattern so
	// clients can integrate before the data is available).
	Markets MarketsReader

	// Oracle, when non-nil, backs /v1/oracle/latest. Leave nil to
	// return 503 on that path.
	Oracle OracleReader
	// OracleHistory, when non-nil, backs the price leg of /v1/rwa/history. Nil publishes no series
	// and says so in `basis`: a value series missing its price leg is no series, and an empty
	// chart would read as a finding about the sector.
	OracleHistory RWAOracleHistoryReader
	// MarketHistory, when non-nil, backs the market leg of /v1/rwa/premium: the observed daily
	// dollar VWAP an instrument's published value is measured against. Nil publishes no series
	// and says so in `basis`, for [OracleHistory]'s reason: a premium with one leg missing is
	// not a shorter series, it is no series.
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

	// AccountKeyQuota caps un-revoked API keys one caller identifier may hold via self-service
	// POST /v1/account/keys. Zero selects [defaultAccountKeyQuota]; negative disables it. Without
	// it one caller could mint keys until Redis filled. Flat, not tiered: the route is disabled
	// under `auth_backend=postgres`, so callers come through the Redis validator, whose records
	// have no account row to read a tier from.
	AccountKeyQuota int

	// PlatformAccounts, when non-nil, backs the operator tier-override endpoints
	// (GET/PATCH /v1/admin/accounts/{id}). Production wires the same store the Postgres API-key
	// validator reads overrides from at Lookup, so a staff override applies on the next key
	// lookup. Nil returns 503.
	PlatformAccounts PlatformAccountStore

	// PlatformUsers, when non-nil, lets PATCH /v1/admin/accounts/{id} revoke every dashboard
	// session of an account it closes; nil leaves that to the dashboard middleware's status gate.
	PlatformUsers AccountSessionRevoker

	// RegisterAccounts, when non-nil, backs POST /v1/register, the open onboarding path that
	// creates a free-tier account and mints its first Postgres-backed key (store:
	// APIKeyBudgets.Platform; both must be wired or the endpoint 503s). Production wires the
	// SAME account store as PlatformAccounts, narrowed to create-only. SignupIPThrottle gates it.
	RegisterAccounts RegisterAccountCreator

	// StatusNotices, when non-nil, backs the operator status-banner endpoints
	// (/v1/admin/status-notices) and the public GET /v1/status/notices. Nil makes the public list
	// return `[]` and the admin endpoints 503.
	StatusNotices StatusNoticeStore

	// Audit, when non-nil, receives persisted audit rows for admin actions (key mint, account
	// overrides, status-notice mutations). Nil degrades to log-only audit; the mutation always logs.
	Audit AuditSink

	// SignupIPThrottle, when non-nil, applies a per-IP cap to /v1/register separate from the
	// global limiter. The anonymous global bucket (60/min) would let an attacker bulk-mint
	// about 3,600 keys per hour per IP; typical wiring is a 5/hour-per-IP Redis bucket. Nil
	// leaves signup to the global rate limit alone.
	SignupIPThrottle SignupIPThrottle

	// APIKeyBudgets are the credential stores a tier change must clamp: the enforced per-minute
	// budget lives on the key record, so a tier-only write to accounts.tier would leave a demoted
	// account's keys at their old limit. Zero value disables the clamp; the endpoint then reports
	// keys_clamped=0, so a deployment without a key store degrades visibly.
	APIKeyBudgets APIKeyBudgetStores

	// Divergence, when non-nil, is consulted by /v1/price after a successful LatestPrice lookup;
	// a fired warning sets flags.divergence_warning=true. Nil means no signal and the flag stays
	// false. Wire when both the divergence worker and Redis run.
	Divergence DivergenceLooker

	// Freeze, when non-nil, is consulted by /v1/price (and batch) after a successful
	// LatestPrice lookup. A frozen pair gets flags.frozen=true and flags.single_source=true
	// (anomaly.ActionFreeze). Nil means no freeze signal: flags.frozen stays false and
	// single_source is derived from the observation count. Wire when the aggregator's
	// freeze-marker writer and Redis both run.
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
	// fdv_usd, supply_basis) on /v1/assets/{id}. Nil leaves them null while the body still
	// serves. A non-nil reader still depends on another process populating asset_supply_history.
	Supply SupplyLooker

	// TokenSupply, when non-nil, backs GET /v1/assets/{asset_id}/supply from the decode-at-ingest
	// supply_flows lake (ADR-0034): the raw mint-burn-clawback total for every token, unlike
	// Supply's ADR-0011 circulating/max policy over asset_supply_history. Nil returns 503.
	TokenSupply TokenSupplyReader

	// ContractStorageSupply, when non-nil, gives /v1/assets/{asset_id}/supply a second source for
	// tokens the event log cannot see: the sum of per-holder balances in contract storage.
	// Consulted only when the event-derived reading found no flows, never to adjust an
	// event-based figure: both measure the same tokens and would double-count. Nil leaves a
	// flowless token reporting zero.
	ContractStorageSupply ContractStorageSupplyReader

	// TokenDecimals, when non-nil, overlays on-chain `decimals()` onto /v1/assets/{id} for Soroban
	// tokens, from the lake's captured contract-instance metadata. Classic and native assets are
	// always 7 by protocol and never consult it. Nil keeps the 7 default.
	TokenDecimals TokenDecimalsReader

	// TokenSymbol, when non-nil, resolves a token contract's on-chain SEP-41 symbol from the
	// contract-instance METADATA TokenDecimals reads. Consulted ONLY by the RWA contract arm,
	// after the curated directory has named that exact contract: the value is contract-authored,
	// so it may say WHICH instrument an address holds, never WHETHER it is vouched for. Nil
	// admits only what the curated set binds.
	TokenSymbol TokenSymbolReader

	// RWAContracts, when non-nil, backs the contract arm of GET /v1/rwa/assets: the
	// curated-directory scan supplying its candidates, and the recognised-but-tokenless entity
	// census reported as coverage. Nil serves the classic arm alone and the funnel states the
	// contract population was NOT MEASURED, rather than reporting it as empty.
	RWAContracts RWADirectoryContractReader

	// RWAListings, when non-nil, backs the second arm on GET /v1/rwa/assets: the cached
	// independent listing directory (fed by `listing-sync`) that corroborates a curated contract
	// binding. Nil is a configuration statement: the arm reports NOT MEASURED and curated bindings
	// are refused under independent_listing_unavailable, never as a measured finding that no
	// independent party names the addresses.
	RWAListings RWAListingDirectoryReader
	// RWACurated, when non-nil, backs the curated arm on GET /v1/rwa/assets: a named third
	// party's asset list and its own prices, served apart from the verified set.
	RWACurated RWACuratedDirectoryReader

	// Listings, when non-nil, backs the listing-priced valuation arm on GET /v1/assets and
	// /v1/assets/{asset_id}: the directory RWAListings reads, through a reader serving both
	// address forms. Nil is a configuration statement: assets with a withheld market cap carry
	// `listing_valuation.status = "listing_unavailable"`, meaning nobody looked, never that nobody
	// lists the address.
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

	// PriceAt, when non-nil, backs GET /v1/price/at — point-in-time
	// closed-bucket VWAP for cost-basis/PnL tooling.
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

	// Issuers, when non-nil, backs GET /v1/issuers/{g_strkey}.
	// Production wiring is timescale.Store directly. Nil makes
	// the endpoint return 503.
	Issuers IssuersReader

	// SEP41Transfers, when non-nil, backs GET /v1/contracts/{contract_id}/transfers, which in
	// turn backs per-account net-position queries. Nil returns 503.
	SEP41Transfers SEP41TransfersReader

	// Cursors, when non-nil, backs GET /v1/diagnostics/cursors, the operator diagnostic behind
	// the explorer /diagnostics page. Nil returns 503.
	Cursors CursorsReader

	// CoverageReader, when non-nil, backs the shadow data-derived density on
	// /v1/diagnostics/ingestion from source_coverage_snapshots rows the gap detector upserts
	// every cycle. Nil leaves DensityPctV2 / GapFreePct zero in every row; cursor-derived density
	// remains the authoritative signal.
	CoverageReader SourceCoverageReader

	// CompletenessReader, when non-nil, backs the ADR-0033 Phase 6
	// completeness_* fields on /v1/diagnostics/ingestion. Nil leaves
	// them absent (UI falls back to the gap_free coverage signal).
	CompletenessReader CompletenessReader

	// AuditedSources is completeness.AuditedSources(cfg): the sources the
	// audit is expected to publish a verdict for. /v1/coverage counts an
	// applicable one with no verdict row in total_sources, as
	// unverified_sources. Nil disables that join.
	AuditedSources []string

	// ProtocolContracts, when non-nil, backs the contract registry (instance lists and counts) on
	// /v1/protocols*. Nil keeps the directory serving with empty lists and zero counts.
	ProtocolContracts ProtocolContractsReader

	// ProtocolStats, when non-nil, backs the per-protocol events_24h
	// column on /v1/protocols*. Production wiring is timescale.Store
	// directly (CountRecentEventsBySource). Nil serves zeros.
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

	// MarketSources backs GET /v1/markets/sources (per-source 24h
	// volume breakdown for a pair or asset). timescale.Store satisfies
	// it directly; nil makes the endpoint return an empty list.
	MarketSources MarketSourceReader

	// SourcesStats, when non-nil, populates trade_count_24h on /v1/sources?include=stats. Without
	// it the include flag is silently ignored and the response is the static registry projection.
	SourcesStats SourcesStatsReader

	// Lending, when non-nil, backs /v1/lending/pools (per-Blend-pool summaries). Nil serves an
	// empty array.
	Lending LendingReader

	// MEV, when non-nil, backs /v1/mev (the auto-flagged MEV-event
	// feed). Leave nil and the handler serves an empty array.
	MEV MEVReader

	// Anomalies, when non-nil, backs /v1/anomalies (the durable
	// freeze-event timeline, ADR-0019). Nil → empty payload.
	Anomalies AnomalyReader

	// Divergences, when non-nil, backs /v1/divergence (the
	// per-reference divergence board) and /v1/divergence/series
	// (one pair×reference Δ% history). Nil → empty payload.
	Divergences DivergenceReader

	// DivergenceThresholdPct is the operator's divergence alert threshold, surfaced on
	// /v1/divergence/series so charts shade the band from the same number the worker fires on.
	// Zero omits the field; serving a made-up default would fabricate policy.
	DivergenceThresholdPct float64

	// MinMarketCapVolumeUSD is the valuation-integrity floor. market_cap_usd / fdv_usd is
	// suppressed (null, market_cap_low_liquidity=true) when its price came from a single venue AND
	// trailing-24h USD volume is below the floor, so one dust trade cannot present an obscure
	// asset as worth billions. The AND is load-bearing, see [dustLiquiditySuppressed]. Zero
	// disables the guard.
	//floatmoney:ok operator config constant, not a stored/served amount — see capExceedsObservedTurnover/dustLiquiditySuppressed (assets.go), which compare it via big.NewFloat against ADR-0003 decimal-string money
	MinMarketCapVolumeUSD float64

	// MaxMarketCapVolumeRatio is the valuation-integrity CEILING (config
	// aggregate.max_market_cap_volume_ratio): a market cap or FDV is suppressed when it exceeds
	// this multiple of the asset's trailing-24h USD volume. Not a substitute for
	// MinMarketCapVolumeUSD: one asks whether trading is small, the other whether the claim is
	// large against it. See [capExceedsObservedTurnover].
	//floatmoney:ok dimensionless turnover-ratio ceiling (days-to-turn-over multiple), same non-money class as MinMarketCapVolumeUSD above despite the "cap" substring
	MaxMarketCapVolumeRatio float64

	// Currencies, when non-nil, supplies the fiat-rate snapshot that /v1/assets fiat rows and the
	// /v1/chart fiat:fiat fallback both consume. Nil falls back to an empty currencies state.
	Currencies CurrenciesReader

	// FXCrossMaxAgeHours bounds the age of the forex rate that [Server.tryFiatCrossRate] /
	// [Server.tryUSDAnchoredFiatCross] will serve (config pricing_guard.fx_cross_max_age_hours);
	// older is refused rather than served with a fresh-looking observed_at. 0 falls back to
	// [defaultFXCrossMaxAge] (76h), matching aggregate.composite_reference.fx_max_age_hours.
	FXCrossMaxAgeHours int

	// DisableFiatBasis serves a single-venue fiat book as read instead of
	// the ADR-0053 USD-anchored derivation (pricing_guard.disable_fiat_basis).
	DisableFiatBasis bool

	// FXFixings binds the closed surfaces' FX legs to the vendor's time
	// series (fx_fixings). Nil: closed fiat crosses are not served.
	FXFixings FXFixingReader

	// Explorer, when non-nil, backs the network-explorer endpoints
	// (ADR-0038): /v1/ledgers, /v1/tx, /v1/operations, /v1/contracts,
	// /v1/search — reading the certified ClickHouse lake directly.
	// *clickhouse.ExplorerReader satisfies it. Nil → those routes 503.
	Explorer ExplorerReader

	// IssuerAuthFlags, when non-nil, is the point lookup /v1/issuers/{g}
	// reads the issuer's live AccountEntry through, instead of Explorer's
	// AccountStateCached fan-out to trustlines and offers. Production wiring
	// is *clickhouse.ExplorerReader.
	IssuerAuthFlags IssuerAuthFlagsReader

	// StaticHomeDomain, when non-nil, is the operator-static
	// [metadata.issuer_home_domains] map (gated on no AccountEntry
	// observation). The asset-detail backfill consults it AFTER the live
	// on-chain read and before the curated knownIssuers map.
	StaticHomeDomain func(ctx context.Context, issuer string) (string, bool)

	// SEP41Movements, when non-nil, backs the Postgres recent-tail half of the
	// /v1/accounts/{g_strkey}/movements merge. Separate from SEP41Transfers (contract-scoped);
	// this is address-scoped across every contract. Nil degrades to the ClickHouse pre-P23 archive
	// alone with a coverage_note rather than 503ing; Explorer is the hard dependency.
	SEP41Movements explorerpkg.SEP41MovementsReader

	// Positions, when non-nil, backs GET /v1/accounts/{g}/positions: six per-protocol Postgres
	// folds (blend money-market, blend backstop, phoenix stake, defindex vault shares,
	// sorocredit, aquarius gauge). Nil 503s the endpoint. Venue labels reuse ProtocolPoolTokens.
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

	// FXHistory, when non-nil, lets /v1/chart serve fiat:fiat pairs
	// from the fx_quotes hypertable for ranges beyond 7d. Leave nil
	// to keep /v1/chart fiat:fiat in 7d-only mode.
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

	// KeyPolicy, when non-nil, runs after Auth and before RateLimit, enforcing the per-key policy
	// fields (IP allowlist, Referer allowlist, per-endpoint permissions). Without it they are
	// accepted at key creation but never enforced. Anonymous subjects pass through; policy data
	// only ships on Subjects from the Postgres validator.
	KeyPolicy middleware.Middleware

	// RateLimit, when non-nil, is appended to the middleware stack inside Logger, Auth,
	// KeyPolicy, RequireEmailVerified, UsageTracker and MonthlyQuota, so remote_ip + Subject
	// are already on the context (full order: middleware/doc.go). Production uses
	// middleware.RateLimitBySubject so the anon and keyed per-tier limits both take effect;
	// the single-bucket middleware.RateLimit serves tests.
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

	// UsageReader, when non-nil, backs /v1/account/usage with
	// real per-day counts. Without it the endpoint stays on its
	// "empty list with locked wire shape" default.
	UsageReader UsageReader

	// UsageRollupReader, when non-nil, backs /v1/account/usage with per-day x per-endpoint rows
	// from the `usage_daily` rollups. It takes precedence over UsageReader; the handler falls
	// back to the per-day Redis totals when the rollup read errors or has no rows yet.
	UsageRollupReader UsageRollupReader

	// Hub, when non-nil, backs the closed-bucket SSE endpoint (`/v1/price/stream`). Producers call
	// Hub.Publish(); subscribers attach via [streaming.Stream]. Nil makes it return 503 while the
	// rest of v1 serves. /v1/price/tip/stream also uses it when set; internal/api/streaming/doc.go
	// says which streams replay from it.
	Hub *streaming.Hub

	// Confidence, when non-nil, populates the confidence + factors fields on `/v1/price`
	// (ADR-0019), read from the Redis cache the aggregator writes. Nil, or a cache miss at lookup,
	// leaves the field unset while the rest of the envelope serves.
	Confidence ConfidenceLooker

	// Triangulated, when non-nil, is the fallback /v1/price consults after a Timescale miss: the
	// aggregator's triangulated implied VWAPs plus the provenance marker that gates
	// `flags.triangulated`. Nil leaves triangulated-only pairs 404ing.
	Triangulated TriangulatedPriceLooker

	// CDNEnabled controls whether cacheable routes emit `s-maxage` (CDN tier) beside `max-age`.
	// Default true; set false for deployments without a CDN so a CDN they do not run cannot cache
	// anything later made auth-tied. See [middleware.CacheControlWithCDN].
	CDNEnabled bool

	// StatusBackend, when non-nil, backs /v1/status with Prometheus-derived heartbeats, latency
	// percentiles, freshness and Alertmanager incident counts. Nil serves an in-process surface
	// (uptime and region only) for deployments without a local Prometheus.
	StatusBackend StatusBackend

	// ArchiveReportPath, when non-empty, backs GET /v1/diagnostics/archive: the path of the
	// latest JSON report the ADR-0017 archive-completeness daemon writes. Empty returns 503; a
	// configured path whose file does not exist yet returns 404 (fresh host).
	ArchiveReportPath string

	// BackupMetrics, when non-nil, backs GET /v1/diagnostics/backups. Nil derives it from
	// StatusBackend when that is a *PrometheusStatusBackend; a deployment with no Prometheus
	// gets 503.
	BackupMetrics BackupMetricsSource

	// RegionName + RegionDeployment label /v1/status responses.
	// Default to "unknown" / "production" when unset.
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

	// DashboardKeys, when non-nil, mounts the key-management surface (GET/POST/DELETE
	// /v1/dashboard/keys), gated on the session cookie DashboardAuth sets. main.go builds it only
	// when the Postgres platform stores are reachable.
	DashboardKeys DashboardAuthMounter

	// DashboardWebhooks, when non-nil, mounts the customer-webhook CRUD surface
	// (/v1/dashboard/webhooks and its deliveries listing). The delivery worker that drains the
	// queue (internal/customerwebhook) is independent of these handlers.
	DashboardWebhooks DashboardAuthMounter

	// DashboardPriceAlerts, when non-nil, mounts the price-alert CRUD surface
	// (/v1/dashboard/price-alerts). The evaluator that enqueues `price.alert` webhook deliveries
	// runs in the aggregator (internal/pricealerts) and is independent of these handlers.
	DashboardPriceAlerts DashboardAuthMounter

	// SACWrappers is the operator-config map of SAC C-strkey to classic asset key ("CODE:ISSUER",
	// matching [config.SupplyConfig.SACWrappers]). Backs /v1/sac-wrappers, which the explorer
	// joins client-side to label Soroban DEX pools that use SACs as base/quote. Empty returns an
	// empty map and the explorer shows the raw C-strkey.
	SACWrappers map[string]string

	// NetworkPassphrase is the Stellar network passphrase. It derives deterministic SAC contract
	// ids so the WASM endpoint can answer "SAC, no WASM" for contracts whose instance predates the
	// lake's capture window. Empty disables the computed half.
	NetworkPassphrase string

	// USDPeggedClassics is the operator's allow-list of classic assets trusted as 1:1 USD
	// stablecoins; wire it from the same TradesConfig field as trades.usd_pegged_classic_assets.
	// /v1/chart falls back from a literal X/fiat:USD lookup (no prices_1m rows; the proxy is
	// computed at query time) to X/<peg> when that returns 0 points. Empty disables the fallback.
	USDPeggedClassics []canonical.Asset

	// PegDeclaredAt is when the USDPeggedClassics declaration was adopted: the observed_at
	// /v1/price stamps on the flat $1 for a declared peg no market prices. The declaration carries
	// no timestamp, so when zero the server uses its construction time and the stamp resets on
	// every restart. Set it to pin the wire value.
	PegDeclaredAt time.Time

	// FiatPeggedClassics maps a classic asset_id ("CODE-ISSUER") to the fiat asset the operator
	// declares it 1:1-pegged to. Listed or detail rows whose price_usd is nil after the substance
	// gate get it filled from the current fiat-to-USD rate (the fiatUSDPriceFor chain), stamped
	// price_basis="declared_peg". Empty disables the fill.
	FiatPeggedClassics map[string]canonical.Asset

	// SessionAuth, when non-nil, wraps every handler so a dashboard session cookie populates a
	// SessionContext; anonymous and bearer-token requests pass through. Required for
	// /v1/dashboard/* routes, whose handlers 401 without a session.
	SessionAuth middleware.Middleware

	// VerifiedCurrencies, when non-nil, enables the verified-currency overlay on
	// /v1/assets/{id}: an `unverified_warning` body + flags.unverified_ticker_collision when
	// the asset's code matches a verified currency's ticker but the issuer does not (code
	// alone is never identity). Nil leaves responses unchanged.
	//
	// When set, it also enables slug dispatch: /v1/assets/{slug} matching a verified-currency
	// slug routes to the global view instead of the per-Stellar-asset surface.
	VerifiedCurrencies *currency.Catalogue

	// BackfillCoverage, when non-nil, is the background-refreshed cache behind
	// the coverage section of `/v1/diagnostics/ingestion`; nil omits it.
	BackfillCoverage *CoverageCache

	// UsdVolumePricing feeds /v1/coverage's usd_volume_pricing; nil renders null.
	UsdVolumePricing *UsdVolumePricingCache

	// SDEXOrderBook, when non-nil, is the in-process classic offer book behind GET
	// /v1/sdex/orderbook, loaded once from the lake then advanced every
	// [SDEXOrderBookAdvanceInterval]. Nil or not yet loaded serves an honest 503.
	SDEXOrderBook *SDEXOrderBookCache

	// DEXTVL, when non-nil, is the process-local per-protocol DEX TVL snapshot (pool reserves in
	// USD), refreshed every [DEXTVLRefreshInterval]. It powers the `tvl` field on /v1/protocols
	// because per-request aggregation over the reserve tables is never acceptable on the read
	// path. Nil or cold leaves the field absent.
	DEXTVL *DEXTVLCache

	// NonstandardDecimals, when non-nil, backs read-time normalization: every price-shaped
	// surface (price, vwap, twap, history, ohlc, chart, tip + SSE, SEP-40 passthroughs and
	// last_price fields) scales its served ratio by 10^(dec_base-dec_quote) for any pair with
	// a leg the cache has confirmed as non-7-decimal (aggregate.AdjustPrice). Nil serves the
	// raw ratio. See [NonstandardDecimalsCache] and docs/operations/runbooks/dex.md.
	NonstandardDecimals *NonstandardDecimalsCache

	// GlobalPrice, when non-nil, powers the price block on `/v1/assets/{slug}` global views via
	// the three-tier chain (vwap_native, aggregator_avg, triangulated). Nil leaves the block
	// empty while identity and networks still surface.
	GlobalPrice aggregate.GlobalPriceReader

	// GlobalPriceOpts tunes the three-tier policy; zero uses [aggregate.DefaultGlobalPriceOptions]
	// except the aggregator source list, which is wired explicitly because the defaults cannot
	// know which sources are aggregator class without importing the registry.
	GlobalPriceOpts aggregate.GlobalPriceOptions

	// RequestTimeout is the per-request context deadline for non-streaming requests. Zero or
	// negative falls back to [defaultRequestTimeout]. SSE endpoints are exempt and end on
	// client-disconnect ctx cancellation.
	RequestTimeout time.Duration
	// DisableRequestTimeout omits the RequestTimeout middleware, overriding RequestTimeout. A
	// separate flag because a zero RequestTimeout means "unset", while an explicit
	// api.request_timeout = 0 disables it.
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

// statusServicesOr normalises Options.StatusServices: nil/empty falls back to the pubnet pair.
// The result is a copy and never nil, so a deployment with NO background service still reports
// its own "api" entry.
//
// Names are lower-cased as well as trimmed, the SAME transform config validation applies: the
// heartbeat map is keyed by lower-case Prometheus `job` labels, so a trim-only transform would
// boot clean on ["Indexer"] then pin `overall` at degraded.
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

// BeginStreamDrain tells every SSE connection the process is shutting down, so each ends
// promptly instead of holding http.Server.Shutdown open. Register it with
// http.Server.RegisterOnShutdown. Idempotent and safe from any goroutine.
//
// Deliberately NOT http.Server.BaseContext: that would also abort every ordinary in-flight
// request on SIGTERM; this signal is visible only to stream writers. See [streaming.Drain].
func (s *Server) BeginStreamDrain() { s.streamDrain.Begin() }

// streamOptions is the [streaming.StreamOptions] every stream handler passes. Centralised so
// a new stream gets the shutdown drain and per-heartbeat credential re-check by construction;
// forgetting either fails silently (a deploy stall, or a revoked key that keeps streaming).
func (s *Server) streamOptions() streaming.StreamOptions {
	return streaming.StreamOptions{Drain: s.streamDrain, Revalidate: s.streamRevalidate}
}

// streamRevalidationGates are the middlewareStack entries an open stream re-runs at each
// heartbeat: those that decide whether this caller may be served at all. RateLimit is absent on
// purpose: re-running it would spend a token per heartbeat, and it is admission control, not
// revocation.
var streamRevalidationGates = map[string]bool{
	"PublicRoutes":         true,
	"Auth":                 true,
	"KeyPolicy":            true,
	"RequireEmailVerified": true,
	"MonthlyQuota":         true,
}

// buildStreamRevalidator returns the [streaming.StreamOptions.Revalidate] hook: the request is
// replayed through the gate middlewares in [Server.middlewareStack] order with a terminal
// handler that records it was reached. Any gate that answers instead (401, 403, 429) fails
// the check. Nil when no gate is configured.
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

// discardResponseWriter swallows a revalidation gate's rejection body;
// only whether the gate called through matters.
type discardResponseWriter struct{ header http.Header }

func (d discardResponseWriter) Header() http.Header         { return d.header }
func (d discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardResponseWriter) WriteHeader(int)             {}

// Handler returns the mux wrapped in [Server.middlewareStack], outermost
// first.
func (s *Server) Handler() http.Handler {
	entries := s.middlewareStack()
	stack := make([]middleware.Middleware, len(entries))
	for i, e := range entries {
		stack[i] = e.mw
	}
	return middleware.Chain(s.mux, stack...)
}

// stackEntry is one middleware in [Server.middlewareStack], named as the
// order in internal/api/v1/middleware/doc.go names it.
type stackEntry struct {
	name string
	mw   middleware.Middleware
}

// middlewareStack is the request-path middleware order, outermost first. middleware/doc.go
// documents it and TestMiddlewareStackMatchesPackageDoc fails when the two disagree.
//
// HTTPMetrics sits outside Logger+Recoverer so metrics count requests whose handler panicked.
// SecurityHeaders runs INSIDE Recoverer so a panic's synthesised 500 still carries nosniff.
// CORS runs outside RateLimit so preflight OPTIONS requests do not consume rate-limit budget.
// RateLimit runs inside Logger, Auth, KeyPolicy, RequireEmailVerified, UsageTracker and
// MonthlyQuota so the remote IP and Subject are populated when it keys.
func (s *Server) middlewareStack() []stackEntry {
	stack := []stackEntry{
		{"RequestID", middleware.RequestID},
		{"HTTPMetrics", obs.HTTPMetrics},
		{"Logger", middleware.Logger(s.logger)},
		{"Recoverer", middleware.Recoverer(s.logger)},
		// Inside Recoverer so a panic's 500 response carries nosniff; idempotent with edge proxies.
		{"SecurityHeaders", middleware.SecurityHeaders},
		// Cache-Control per route, set BEFORE handlers run so writeJSON / writeProblem inherit
		// it; handlers may override. CDN-tier `s-maxage` is gated on s.CDNEnabled so a
		// deployment without a CDN never emits a directive one could later honour.
		{"CacheControl", middleware.CacheControlWithCDN(s.CDNEnabled)},
		// Convert the mux's text/plain 404 / 405 into problem+json so they match the rest of the
		// error surface. Sits AFTER CacheControl so the override gets the same Cache-Control
		// directive a handler-side response would.
		{"Envelope404", middleware.Envelope404},
	}
	if s.CORS != nil {
		stack = append(stack, stackEntry{"CORS", s.CORS})
	}
	// 308-redirect trailing-slash paths to the no-slash form (every v1 route is registered
	// without one); 308 preserves method and body. MUST sit INSIDE CORS: outside it the
	// redirect carries no Access-Control-Allow-Origin and a browser fetch dies there.
	stack = append(stack, stackEntry{"TrailingSlashRedirect", middleware.TrailingSlashRedirect(s.mux)})
	// ResolveRoute pre-matches the route pattern via a read-only mux lookup BEFORE the gates
	// can reject the request. They run outside obs.CaptureRoute (innermost), so without this
	// a gate-rejected request would be bucketed "unmatched" by UsageTracker.
	stack = append(stack, stackEntry{"ResolveRoute", middleware.ResolveRoute(s.mux)})
	// RequestTimeout gives every handler a deadline even if it forgets to wrap its own DB read.
	// It sits OUTSIDE the credential/quota/limit block, whose Redis and Postgres round-trips would
	// otherwise run outside the deadline, and INSIDE CORS and TrailingSlashRedirect so a preflight
	// short-circuits without a timer.
	//
	// The pre-handler seams (MonthlyQuota read, RateLimit take, Auth's failed-auth throttle)
	// detach from request cancellation via context.WithoutCancel so a client abort cannot arm
	// their dwell clocks; middleware.throttleContext re-applies this deadline. SSE is exempt;
	// off when requestTimeout <= 0.
	if s.requestTimeout > 0 {
		stack = append(stack, stackEntry{"RequestTimeout", middleware.RequestTimeout(s.requestTimeout)})
	}
	// Auth runs INSIDE CORS (preflight OPTIONS short-circuits before any credential check) but
	// OUTSIDE RateLimit (per-tier limits need the authenticated Subject). publicRoutes.Mark sits
	// directly outside it so handlePublic routes are credential-optional under every auth_mode.
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
	// Usage tracker runs OUTSIDE both quota and rate-limit so it records both kinds of 429 under
	// the `throttled` class; inside MonthlyQuota a quota denial would be counted nowhere. The
	// billable per-day total (the MonthlyQuota input) excludes 429s and most 5xx, see
	// middleware.billableClass, so a counted 429 cannot feed back into the quota that denied it.
	// Best-effort; failures log at debug and never block.
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
	// CaptureRoute MUST be innermost, directly above the mux, so r.Pattern is populated
	// before it reads. It writes the route into the *routeCapture HTTPMetrics planted in the
	// context, since Logger's r.WithContext shadows the original request. See obs.HTTPMetrics.
	return append(stack, stackEntry{"CaptureRoute", obs.CaptureRoute})
}

// Uptime returns how long this server has been running. Exposed
// for debugging / testing.
func (s *Server) Uptime() time.Duration { return time.Since(s.started) }

// proxyForwardHeaders are the headers a reverse proxy adds for a remote client. Their PRESENCE
// is the signal, not their value; a direct local scrape sets none.
var proxyForwardHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Real-Ip",
	"Forwarded",
}

// loopbackOnly wraps `next` so it returns 404 unless the request is a local, un-proxied
// scrape. Used for `/metrics`.
//
// Both required: (1) RemoteAddr is loopback; (2) NO proxy-forwarding header. (2) makes the
// guard real: Caddy runs on the same host, so a misconfigured Caddy presents a LOOPBACK
// RemoteAddr for public traffic, while mainstream proxies set X-Forwarded-For by default.
//
// Defence in depth: the real control is the Caddyfile 404ing /metrics from public hosts
// (configs/caddy/Caddyfile.api); a proxy that STRIPS forwarding headers would still pass.
// 404 (not 403) deliberately: 403 confirms the route exists.
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

// handlePublic mounts a route that is unauthenticated by design (a credential bootstrap or public
// status surface), answering without an API key or SEP-10 JWT under every auth_mode. Its OpenAPI
// operation must declare `security: []`; TestPublicRoutes_MatchOpenAPISecurityNone holds the two
// in lockstep.
func (s *Server) handlePublic(pattern string, h http.HandlerFunc) {
	s.publicRoutes.Handle(s.mux, pattern, h)
}

// handleAdmin mounts an operator-only route. The operator tier and, for writes, the X-Reason
// header are enforced here so a new /v1/admin/ handler cannot ship without them; handlers keep
// their own checks as defence in depth.
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
	// Health / meta endpoints. Deliberately NOT behind rate-limit
	// middleware — infra (k8s probes, load balancers) hits these.
	s.mux.HandleFunc("GET /v1/issuers", s.handleIssuersList)
	s.mux.HandleFunc("GET /v1/issuers/{g_strkey}", s.handleIssuer)

	// Per-contract SEP-41 transfer audit trail (transfer, approve, set_admin, set_authorized)
	// with optional ?from= / ?to= filters; enables per-account net-position queries.
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", s.handleSEP41Transfers)

	s.mux.HandleFunc("GET /v1/changes/{entity_type}/{id}", s.handleChangeSummary)
	s.mux.HandleFunc("GET /v1/diagnostics/cursors", s.handleCursors)
	s.mux.HandleFunc("GET /v1/diagnostics/ingestion", s.handleDiagnosticsIngestion)
	// Latest archive-completeness report (ADR-0017) — read-through of
	// the JSON file the daily verify timer writes. Backs the explorer
	// /diagnostics archive panel. 503 when unconfigured, 404 pre-first-run.
	s.mux.HandleFunc("GET /v1/diagnostics/archive", s.handleDiagnosticsArchive)
	// Backup + DR-evidence freshness vs. SLO, read from Prometheus; backs the status page's
	// Backups panel. 503 when no metrics backend is wired.
	s.mux.HandleFunc("GET /v1/diagnostics/backups", s.handleDiagnosticsBackups)
	s.mux.HandleFunc("GET /v1/coverage", s.handleCoverageVerdicts)

	// Protocols pillar (explorer-data-inventory.md §7.8): directory + per-protocol
	// detail. Static registry always serves; dynamic joins degrade.
	s.mux.HandleFunc("GET /v1/protocols", s.handleProtocolsList)
	s.mux.HandleFunc("GET /v1/protocols/{name}", s.handleProtocolDetail)
	// Per-pool DEX TVL drill-down: the pools + legs behind the
	// `tvl` block, served from the same in-process snapshot.
	s.mux.HandleFunc("GET /v1/protocols/{name}/tvl", s.handleProtocolTVL)

	// SDEX order-book depth — live classic offers from the in-process
	// book (503 problem until the initial lake load completes).
	s.mux.HandleFunc("GET /v1/sdex/orderbook", s.handleSDEXOrderbook)

	// Live-ingest frontier (latest ingested ledger and lag). /tip is a 2s-cached poll; /stream is
	// the SSE counterpart pushing one ledger_update per new ledger.
	s.mux.HandleFunc("GET /v1/ledger/tip", s.handleLedgerTip)
	s.mux.HandleFunc("GET /v1/ledger/stream", s.handleLedgerStream)

	// Network explorer (ADR-0038) — read the certified ClickHouse lake.
	// Handler implementations live in internal/api/v1/explorer; this is
	// the sole place they're mounted.
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
	// Account-creator league table. A literal segment, so it wins
	// over the {g_strkey} wildcard below exactly as /stats does.
	s.mux.HandleFunc("GET /v1/accounts/creators", s.explorerHandler.AccountCreators)
	s.mux.HandleFunc("GET /v1/accounts/sponsors", s.explorerHandler.AccountSponsors)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/transactions", s.explorerHandler.AccountTransactions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/operations", s.explorerHandler.AccountOperations)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/movements", s.explorerHandler.AccountMovements)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/positions", s.explorerHandler.AccountPositions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/trades", s.explorerHandler.AccountTrades)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/activity", s.explorerHandler.AccountActivity)
	// The sponsorship and account-creation graph for one account; the only surface answering
	// "where did this account come from".
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/graph", s.explorerHandler.AccountGraph)
	// The same graph on a time axis (monthly activity). A sub-resource, not a parameter, so the
	// snapshot payload does not grow a series nobody asked for.
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

	// Prometheus scrape endpoint. Deliberately unversioned: operator-facing, not public API.
	//
	// Also gated at the Go layer on loopback AND un-proxied (see loopbackOnly): Caddy should
	// already 404 it, and this catches a stale Caddyfile. A public hit fingerprints the
	// deployment, which justifies two layers.
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

	// Current price — last-trade fallback today; VWAP path when
	// the aggregator ships.
	s.mux.HandleFunc("GET /v1/price", s.handlePrice)

	// Point-in-time closed bucket at-or-before ts +
	// multi-horizon change strip (1h/24h/7d/30d) — both back onto the
	// same finest-CAGG point-in-time reader.
	s.mux.HandleFunc("GET /v1/price/at", s.handlePriceAt)
	s.mux.HandleFunc("GET /v1/price/changes", s.handlePriceChanges)

	// Rolling-window tip surface (ADR-0018) — VWAP over the last
	// few seconds, falling back to last-good-price when the window
	// is empty. NOT cross-region consistent; use /v1/price for that.
	s.mux.HandleFunc("GET /v1/price/tip", s.handlePriceTip)

	// SSE counterpart of /v1/price/tip — same compute logic, pushed
	// on a per-connection tick. See ADR-0018 §"SSE wires onto the
	// tip surface".
	s.mux.HandleFunc("GET /v1/price/tip/stream", s.handlePriceTipStream)

	// Raw per-source observations (ADR-0018 Surface 3) — array of
	// most-recent trade per source for the pair. No aggregation; the
	// rawest of the three consistency surfaces.
	s.mux.HandleFunc("GET /v1/observations", s.handleObservations)

	// SSE counterpart of /v1/observations — same compute, pushed on
	// a per-connection tick. interval_seconds tunes cadence.
	s.mux.HandleFunc("GET /v1/observations/stream", s.handleObservationsStream)

	// Closed-bucket SSE — fed by the aggregator publishing into the
	// shared Hub on each window close. Carries the strict ADR-0015
	// closed-bucket consistency contract that /v1/price serves.
	s.mux.HandleFunc("GET /v1/price/stream", s.handlePriceStream)

	// Batch price lookup, up to 100 assets per request.
	s.mux.HandleFunc("GET /v1/price/batch", s.handlePriceBatch)

	// Batch price lookup via JSON body — same shape, raises the
	// per-request ceiling to 1000.
	s.mux.HandleFunc("POST /v1/price/batch", s.handlePriceBatchPost)

	// Trade history within a time window.
	s.mux.HandleFunc("GET /v1/history", s.handleHistory)

	// Aggregated history at a granularity over the asset's full
	// indexed range. CAGG-served (prices_<granularity>); per
	// ADR-0015 only closed buckets returned.
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

	// Current per-pool-contract reserves + constant-product depth
	// (ADR-0039 lake read; Soroswap only today). Literal path wins
	// over any future /v1/pools/{...} wildcard in Go's mux.
	s.mux.HandleFunc("GET /v1/pools/reserves", s.handlePoolReserves)

	// Native (CAP-38) liquidity-pool two-sided reserves + depth,
	// read from the `liquidity_pool` LedgerEntry in the lake
	// (ADR-0039). Listing ranked by LP count; ?pool= for one pool.
	s.mux.HandleFunc("GET /v1/liquidity-pools", s.handleLiquidityPools)

	// Single-pair activity summary.
	s.mux.HandleFunc("GET /v1/pairs", s.handlePairs)

	// Latest oracle readings per source for an asset.
	s.mux.HandleFunc("GET /v1/oracle/latest", s.handleOracleLatest)

	// Every active oracle stream — one row per (source, asset, quote)
	// triple, latest observation in the trailing 7d window. Backs
	// the explorer's /oracles "price streams" table.
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

	// Anomalies + divergence — the freeze timeline + cross-reference
	// divergence board (ADR-0019).
	s.mux.HandleFunc("GET /v1/anomalies", s.handleAnomalies)
	s.mux.HandleFunc("GET /v1/divergence", s.handleDivergence)
	s.mux.HandleFunc("GET /v1/divergence/series", s.handleDivergenceSeries)

	// Source catalogue — every venue the aggregator knows about,
	// with class + IncludeInVWAP metadata.
	s.mux.HandleFunc("GET /v1/sources", s.handleSources)
	// Per-source live health row — the same shape as the `sources`
	// section on /v1/diagnostics/ingestion, addressable per venue so
	// the explorer /sources/{name} page polls one source cheaply.
	s.mux.HandleFunc("GET /v1/sources/{name}/health", s.handleSourceHealth)

	// Router / aggregator-vault registry + routed-via 24h rollup
	// (migration 0025 Phase B).
	s.mux.HandleFunc("GET /v1/aggregators", s.handleAggregators)

	// Methodology: machine-readable summary of the active aggregation policy (VWAP method, outlier
	// filters, stablecoin proxy, source classes, ADR refs), mirroring the explorer's /methodology
	// page.
	s.mux.HandleFunc("GET /v1/methodology", s.handleMethodology)

	// SAC wrapper resolution: operator-config map of SAC C-strkey to "CODE-ISSUER", so the
	// explorer labels Soroban DEX pools with symbols instead of raw C-strkeys.
	s.mux.HandleFunc("GET /v1/sac-wrappers", s.handleSACWrappers)

	// Tokenized real-world assets: the (code, issuer) set meeting the internal/rwa definition,
	// with aggregates and the rule that produced it. Identity is always (code, issuer); a code
	// alone is not an asset because anyone can issue a token called anything.
	s.mux.HandleFunc("GET /v1/rwa/assets", s.handleRWAAssets)
	s.mux.HandleFunc("GET /v1/rwa/history", s.handleRWAHistory)
	// The hand-vetted stablecoin set (catalogue class=stablecoin with a
	// Stellar issuer) with supply and USD value; the total is a lower bound.
	s.mux.HandleFunc("GET /v1/stablecoins", s.handleStablecoins)
	// Premium/discount to net asset value as a daily series. A ratio of two sampled observations,
	// so neither leg may be carried across a day it was not observed on. See
	// rwa_premium_history.go.
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
	// Operator surface: per-account tier, status and rate-limit / monthly-quota overrides. Same
	// TierOperator gate as /v1/admin/keys; PATCH also requires an X-Reason header and audit-logs
	// "account.override.set". It takes effect on the account's next API-key validator Lookup,
	// which already enforces a suspended/closed `status` as the account-level kill switch.
	s.handleAdmin("GET /v1/admin/accounts/{id}", s.handleAdminAccountGet)
	s.handleAdmin("PATCH /v1/admin/accounts/{id}", s.handleAdminAccountOverrides)
	// Operator surface: customer-facing status banners (incident
	// tooling, admin Phase 1.5). Create/list/resolve gated on
	// TierOperator; create + resolve require X-Reason and audit-log.
	s.handleAdmin("GET /v1/admin/status-notices", s.handleAdminStatusNoticesList)
	s.handleAdmin("POST /v1/admin/status-notices", s.handleAdminStatusNoticeCreate)
	s.handleAdmin("POST /v1/admin/status-notices/{id}/resolve", s.handleAdminStatusNoticeResolve)
	// Retired: it returns the same 410 for every method and body, so the
	// route cannot distinguish a known email from a new one.
	s.handlePublic("POST /v1/signup", s.handleSignupRetired)
	// Open registration — the curl-first agent onboarding path:
	// creates a free-tier platform account + first API key in one
	// unauthenticated POST. Shares /v1/signup's per-IP throttle.
	s.handlePublic("POST /v1/register", s.handleRegister)
	s.handlePublic("GET /v1/signup/verify", s.handleSignupRetired)
	s.handlePublic("POST /v1/signup/verify", s.handleSignupRetired)

	// Dashboard magic-link auth (login, callback, logout). Mounted only when DashboardAuth is
	// wired; otherwise the routes do not exist and ServeMux returns its standard 404.
	if s.DashboardAuth != nil {
		s.DashboardAuth.Mount(s.mux, s.publicRoutes)
	}

	// Dashboard key-management routes — gated internally on the
	// session cookie planted by DashboardAuth's middleware. Mount
	// only when main.go wired Postgres for the platform stores.
	if s.DashboardKeys != nil {
		s.DashboardKeys.Mount(s.mux, s.publicRoutes)
	}
	// Dashboard webhook-management routes. Same
	// session-cookie + Postgres-wiring gate as dashboardKeys above.
	if s.DashboardWebhooks != nil {
		s.DashboardWebhooks.Mount(s.mux, s.publicRoutes)
	}
	// Dashboard price-alert-management routes. Same
	// session-cookie + Postgres-wiring gate as dashboardKeys above.
	if s.DashboardPriceAlerts != nil {
		s.DashboardPriceAlerts.Mount(s.mux, s.publicRoutes)
	}

	// SEP-10 Web Auth. Both endpoints are unauthenticated by design: challenge bootstraps auth
	// from a public G-strkey and the JWT from /token authenticates later requests. The validator
	// is wired only when the server-signing seed and JWT secret are configured.
	s.handlePublic("GET /v1/auth/sep10/challenge", s.handleSEP10Challenge)
	s.handlePublic("POST /v1/auth/sep10/token", s.handleSEP10Token)

	// Bare-root welcome pointing at the docs. The `{$}` anchor matches ONLY the literal "/",
	// so ServeMux's 405 method-mismatch detection for known paths stays intact; unknown paths
	// fall through to Envelope404.
	s.mux.HandleFunc("GET /{$}", s.handleRoot)

	// /robots.txt disallows crawler indexing of the JSON API; indexable content lives on the
	// explorer and docs sites. Without this handler Cloudflare's managed robots.txt is served
	// on GET but the API origin returns 404 on HEAD.
	s.mux.HandleFunc("GET /robots.txt", s.handleRobotsTxt)

	// /.well-known/security.txt: RFC 9116 disclosure metadata. Its Canonical: directive points at
	// the explorer's copy so the two cannot drift.
	s.handlePublic("GET /.well-known/security.txt", s.handleSecurityTxt)

	// /errors/{slug}: dereferenceable RFC 9457 problem `type` URIs, which SHOULD resolve to
	// documentation. Serves a self-describing page (humanised slug plus a pointer at the full
	// reference) without hand-written docs per slug.
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

// handleHealthz is the shallow liveness probe: 200 while the process runs and the mux serves.
// It does NOT touch the database or Redis; that is the readiness probe's job.
//
// /healthz and /readyz cover the serving plane only (process, postgres, redis); ingest lag,
// supply and oracle freshness live at `/v1/status`, since probes must not flap when a backfill
// stalls or one source goes silent.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, healthResponse{
		Status:     "ok",
		Uptime:     s.Uptime().Truncate(time.Second).String(),
		StatusRoot: "/v1/status",
	}, Flags{})
}

// handleReadyz is the deep readiness probe: it pings every ReadyChecker in parallel under a
// short shared timeout, 200 only if all pass, else 503. Parallel keeps latency at the slowest
// single check, so the 2s budget is not eaten serially and a typical 1s probe timeout does not
// flap the pod.
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

// fillReadyz runs one readyz round and publishes the result, releasing the single-flight gate
// on every exit including a panic; a flight left set would wedge every later /v1/readyz behind
// a channel nobody closes.
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

// ReadinessProbeCadence is how often [Server.StartReadinessProbe] runs a
// readiness round: one Prometheus scrape interval.
const ReadinessProbeCadence = 15 * time.Second

// StartReadinessProbe runs a readiness round every `every` until ctx ends, so
// stellarindex_dependency_up stays current when nothing calls /v1/readyz; otherwise the gauge
// freezes and an outage after the last probe never reads 0.
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

// readyCheckFailedDetail is the fixed checks[].error text /v1/readyz serves for a failed ping.
// Never the driver error: readyz is unauthenticated and rate-limit exempt, and a dependency's
// error names its endpoint (pgx adds user and database). The real error goes to the server log.
const readyCheckFailedDetail = "ping failed — see the API server log for the underlying error"

// readyzPublicError is a Ping failure whose text is safe for the public readyz body (never an
// endpoint, credential or driver output). Only this type is echoed; every other error is
// served as [readyCheckFailedDetail].
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

// livezLakeTTL bounds how long one lake-ping round is reused, like readyz's cache: at LB probe
// cadence (~10s) each probe gets a fresh round, while a burst of anonymous probes costs at most
// one ClickHouse query per second.
const livezLakeTTL = time.Second

// livezLakeUnreadyDetail is the fixed hint served when the lake ping fails. Never the driver
// error: the route is unauthenticated and rate-limit exempt, and err.Error() carries the
// ClickHouse endpoint. The real error goes to the server log once per cache round.
const livezLakeUnreadyDetail = "clickhouse ping failed — see the API server log for the underlying error"

// lakeHealth is the /v1/livez/lake payload (data member of the envelope).
type lakeHealth struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// handleLivezLake is the LAKE-critical health probe. /v1/readyz treats ClickHouse as
// NON-critical, so a lake-dead region stays in a load balancer's pool for lake-backed routes.
// This is the complement: 200 iff the ClickHouse checker pings; 503 on failure OR when no lake
// is wired (absent fails closed). Lake-route monitors point here, pricing monitors at /v1/readyz.
//
// Single-flight + 1s result cache: the route shares readyz's auth and rate-limit exemptions,
// which are only safe behind it; otherwise every anonymous request would run a fresh
// `LakeTipLedger` query against a lake that is already struggling.
//
// livezLakeMu is never held across the ping, or queued callers would block up to the 5s budget
// during the outage the route exists to surface. The round runs detached in fillLivezLake;
// waiters can abandon it via their own request context.
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

// handleVersion reports binary version, build date and VCS info for fleet-wide "what's
// running" checks. `dirty` reports uncommitted changes in the build tree (production builds
// should always be false).
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		"version":    version.Version,
		"build_date": version.BuildDate,
		"commit":     version.Commit,
		"dirty":      version.Dirty,
		"go_version": version.GoVersion,
	}, Flags{})
}

// handleSecurityTxt serves /.well-known/security.txt per RFC 9116. Canonical: points at the
// explorer copy so the two origins cannot drift. Expires is computed at request time, one
// year out, so it is always a valid future date.
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

// handleErrorDoc serves a dereferenceable page for one RFC 9457 problem `type` slug. Slugs are
// self-descriptive, so the humanised slug plus a link to the full reference beats a 404.
// Content-negotiated: JSON for API clients, minimal HTML for a browser.
func (s *Server) handleErrorDoc(w http.ResponseWriter, r *http.Request) {
	// The slug is URL-path input; constrain it to the kebab-case charset real error types use.
	// Anything else is not ours and is not reflected, which also keeps HTML metacharacters out
	// of every renderer.
	slug := r.PathValue("slug")
	if !errorSlugRE.MatchString(slug) {
		slug = "unknown"
	}
	// A slug that isn't one of the error types this API actually emits
	// can't be dereferenced into anything real — 404 it rather than
	// rendering a humanised page for a type that never existed.
	if _, ok := knownErrorSlugs[slug]; !ok {
		http.NotFound(w, r)
		return
	}
	human := humaniseErrorSlug(slug)
	typeURI := "https://api.stellarindex.io/errors/" + slug
	// This handler content-negotiates on Accept: a shared cache
	// keying solely on the URL would happily serve one representation to a
	// client that asked for the other for the full max-age=3600 window.
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

// knownErrorSlugs is the closed set of RFC 9457 problem `type` slugs this API emits. A slug
// outside it matches errorSlugRE's charset but names no real error type, so a retired or
// mistyped one must not be reflected as a live 200 page.
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

// errorDocTmpl is the minimal browser page for an error type. html/template auto-escapes
// {{.human}} / {{.slug}}, so the reflected path value is safe by construction (and gosec
// trusts it, unlike a hand-rolled escaper).
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

// handleRobotsTxt serves /robots.txt: the JSON API origin is not for crawler indexing, so
// search engines are pointed at the docs and explorer subdomains, and the `Sitemap:` directive
// covers a crawler that ignores the Disallow.
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

// backupMetricsFor resolves the /v1/diagnostics/backups metrics seam: an explicit
// Options.BackupMetrics wins, else the status backend is reused when it answers raw PromQL.
// Any other StatusBackend (test fakes, nil) leaves the endpoint unconfigured and 503.
func backupMetricsFor(opts Options) backupMetricsSource {
	if opts.BackupMetrics != nil {
		return opts.BackupMetrics
	}
	if src, ok := opts.StatusBackend.(backupMetricsSource); ok && src != nil {
		return src
	}
	return nil
}
