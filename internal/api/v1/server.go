package v1

import (
	"context"
	"encoding/json"
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

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/incidents"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// ReadyChecker is the interface /readyz polls to decide whether
// the serving-plane dependencies are responsive. Implementations
// in cmd/stellarindex-api/main.go:
//
//   - storeChecker (wraps *timescale.Store.DB().PingContext) — critical
//   - redisChecker (wraps *redis.Client.Ping) — non-critical
//
// Ping MUST respect ctx and return promptly on cancellation — the
// handler runs every checker in parallel under a shared 2 s
// deadline; a misbehaving checker that ignores ctx can turn readyz
// into a cascade-failure vector for the liveness probe.
//
// Critical() distinguishes "API can't serve requests without
// this" (Postgres — no fallback for trade/aggregate reads) from
// "API can degrade-but-serve without this" (Redis — cache miss
// falls back to Timescale per ADR-0007). The /readyz handler
// uses this to return 503 ONLY when a critical check fails;
// a failing non-critical check produces a 200 response with
// `status="degraded"` so edge load balancers (HAProxy, k8s
// readiness probes) keep the backend in service while operators
// see the per-check breakdown in the response body.
//
// F-1275 (codex audit-2026-05-13): pre-wave-110 every check was
// effectively critical — a Redis outage would 503 readyz and
// HAProxy would drain every healthy API backend even though
// Timescale fallback kept the actual customer-facing surface
// serving correctly.
type ReadyChecker interface {
	Ping(ctx context.Context) error
	Name() string
	Critical() bool
}

// ExpectedSchemaVersion is the migrations/ head this binary was built
// against — the highest-numbered migration under migrations/. A
// running binary REQUIRES the applied schema (schema_migrations.version)
// to be at least this value and not dirty; otherwise its code is
// serving against a schema older or more partial than it assumes.
//
// REC-06 (audit-2026-08-14): the ansible deploy runs `stellarindex-migrate
// up` before swapping binaries, so schema>=binary on the normal path —
// but nothing asserted that at RUNTIME. A deploy passing migrations_skip,
// a hot-swapped binary, or a golang-migrate dirty leftover left the binary
// serving against a stale/partial schema while /v1/healthz and /v1/readyz
// stayed 200 — the exact "partial-outage while healthz 200" shape the
// deploy-time guard aimed to prevent, but only at deploy time.
// [NewSchemaVersionChecker] closes the runtime gap as a CRITICAL readiness
// check: a schema/binary mismatch drains the backend from the load
// balancer (503) instead of silently under-serving behind a green probe.
//
// The comparison is `applied >= expected` (not `==`) on purpose: a schema
// NEWER than the binary is the safe/expected state during a rolling deploy
// (migrations run ahead of the swap) and after a rollback (old binary,
// newer schema). Only an applied head BELOW the binary's expectation — or
// a dirty row — is a mismatch.
//
// This constant MUST equal the head under migrations/; the parity test
// TestExpectedSchemaVersionMatchesMigrationsHead fails CI if a migration
// is added without bumping it.
const ExpectedSchemaVersion uint = 150

// SchemaVersionReader reports the applied golang-migrate schema state
// (schema_migrations.version + dirty). cmd/stellarindex-api adapts
// *timescale.Store over its *sql.DB — keeping the raw SQL in the binary
// layer, mirroring the storeChecker/redisChecker adapter pattern.
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

// NewSchemaVersionChecker builds the REC-06 schema-head readiness check.
// Critical()==true: a binary/schema mismatch must drain the backend, not
// keep serving stale/partial data behind a green probe.
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
	if dirty {
		return fmt.Errorf("schema_migrations is dirty at version %d: a migration is partially applied — the binary must not serve against a half-migrated schema", version)
	}
	if version < c.expected {
		return fmt.Errorf("schema/binary mismatch: binary expects migrations head >= %d but only %d is applied — a deploy skipped migrations or the binary was swapped ahead of them", c.expected, version)
	}
	return nil
}

// Server is the HTTP handler for the Stellar Index v1 API.
//
// Construction: [New] returns a Server with routes mounted.
// Call [Server.Handler] to get an http.Handler for an
// [http.Server].
//
// Thread-safe.
type Server struct {
	logger              *slog.Logger
	checks              []ReadyChecker
	assets              AssetReader
	prices              PriceReader
	history             HistoryReader
	markets             MarketsReader
	oracle              OracleReader
	sep1Cache           Sep1CachedReader
	accounts            AccountStore
	accountKeyQuota     int
	platformAccounts    PlatformAccountStore
	registerAccounts    RegisterAccountCreator
	apiKeyBudgets       APIKeyBudgetStores
	statusNotices       StatusNoticeStore
	audit               AuditSink
	signups             SignupTracker
	signupIPThrottle    SignupIPThrottle
	signupVerifier      SignupVerifier
	signupVerifyEmailer SignupVerifyEmailer
	apiKeyEmailVerifier APIKeyEmailVerifier
	divergence          DivergenceLooker
	freeze              FrozenLooker
	substance           PriceSubstanceGate
	transitive          TransitivePricer
	scam                PriceScamGate
	supply              SupplyLooker
	tokenSupply         TokenSupplyReader
	tokenDecimals       TokenDecimalsReader
	lakeWatermarkReader LakeWatermarkReader
	// Cached lake watermark (ADR-0041 D4) — see lakeWatermark() in
	// lake_watermark.go. Refreshed at most every lakeWatermarkTTL.
	lakeWMMu       sync.Mutex
	lakeWMLedger   uint32
	lakeWMClosedAt time.Time
	lakeWMFetched  time.Time
	// Cached top-N native (CAP-38) liquidity-pool listing — a
	// whole-`liquidity_pool`-prefix lake scan (~40k pools) ranked in
	// Go; cached so the listing endpoint doesn't re-scan per request
	// (see handleLiquidityPools in liquidity_pools.go).
	nativeLPMu              sync.Mutex
	nativeLPCached          []LiquidityPoolReservesRow
	nativeLPFetched         time.Time
	volume                  VolumeReader
	change24h               Change24hReader
	priceAt                 PriceAtReader
	changesum               ChangeSummaryReader
	assetsReader            AssetsReader
	issuers                 IssuersReader
	sep41Transfers          SEP41TransfersReader
	cursors                 CursorsReader
	coverageReader          SourceCoverageReader
	completenessReader      CompletenessReader
	protocolContractsReader ProtocolContractsReader
	protocolStats           ProtocolStatsReader
	protocolActivity        ProtocolActivityReader
	// protocolFast{Mu,Settled,OK} cache the daily-pre-aggregation probe —
	// but only once it produced a DEFINITIVE answer (see fastActivity).
	// Deliberately NOT a sync.Once: Once would latch a transient first-probe
	// error as "unavailable" for the process lifetime (the C1-048 class).
	protocolFastMu      sync.Mutex
	protocolFastSettled bool
	protocolFastOK      bool
	protocolBespoke     ProtocolBespokeReader
	protocolPoolTokens  ProtocolPoolTokensReader
	dexTVL              *DEXTVLCache
	sdexOrderBook       *SDEXOrderBookCache
	// Per-server TTL + single-flight cache for the expensive
	// /v1/protocols/{name} detail (lazy-init'd — see cachedProtocolDetail).
	protoDetailMu     sync.Mutex
	protoDetailCache  map[string]protoDetailEntry
	protoDetailFlight map[string]chan struct{}
	// Last-good cache for the bespoke analytics block INSIDE that detail
	// view. The block is built last, so it used to inherit whatever was
	// left of the rebuild's budget and got dropped from the page when
	// that ran out (§2.6b). Zero value ready — see
	// protocol_bespoke_cache.go.
	protocolBespokeCache bespokeCache
	// Prewarmed SWR cache for the per-source contract_count on
	// GET /v1/protocols. The registry-empty roster is a served-tier
	// `SELECT DISTINCT … LIMIT 5000` scan, and the route is unauthenticated,
	// so scanning per protocol on every origin miss was a DoS surface (W1.3).
	// Zero value ready — see protocol_roster_cache.go.
	protocolRosterCache rosterCache
	// Per-server TTL + single-flight cache for the broad-coverage
	// classic circulating-supply map (one ~0.5s ClickHouse GROUP BY over
	// the trustline slice — see cachedClassicSupply). Backs market-cap
	// fill on the long tail of /v1/assets.
	classicSupplyMu     sync.Mutex
	classicSupplyCache  map[string]string
	classicSupplyAt     time.Time
	classicSupplyFlight chan struct{}
	// classicSupplyAttemptAt advances on EVERY refresh attempt, success or
	// failure — unlike classicSupplyAt, which advances only on success. Without
	// it a failing refresh left the cache permanently stale, so every request
	// retried the heavy query and paid the full request timeout (the 2026-07-21
	// /v1/assets 15s latch). It gates retries to classicSupplyRetryGap.
	classicSupplyAttemptAt time.Time
	// Per-server TTL + single-flight cache for the SEP-1 logo map
	// (canonical asset_id → safe image URL), built from every verified
	// issuer's cached sep1_payload in one scan. Backs the image fill on
	// the /v1/assets listing so the homepage grid renders real logos
	// instead of fallback avatars — see cachedSep1Images.
	sep1ImagesMu           sync.Mutex
	sep1ImagesCache        map[string]string
	sep1ImagesAt           time.Time
	sep1ImagesFlight       chan struct{}
	soroswapPairs          SoroswapPairsReader
	networkStats           NetworkStatsReader
	aggregators            AggregatorsReader
	marketSources          MarketSourceReader
	sourcesStats           SourcesStatsReader
	lending                LendingReader
	mev                    MEVReader
	anomalies              AnomalyReader
	divergences            DivergenceReader
	divergenceThresholdPct float64
	minMarketCapVolumeUSD  float64
	currencies             CurrenciesReader
	explorer               ExplorerReader
	explorerHandler        *explorerpkg.Handler // network-explorer endpoints (ADR-0038); see explorer.go
	// directory resolves curated third-party issuer labels
	// (account_directory, migration 0136) for the additive
	// issuer_directory_* fields on /v1/assets + /v1/assets/{id}.
	// Same reader the explorer handler + GET /v1/directory use; nil
	// omits the fields. DISPLAY-ONLY — never feeds pricing/verification.
	directory explorerpkg.DirectoryReader
	// volumeCharacter rolls the per-asset trailing-window account-structure
	// signals + derived volume_character on /v1/assets/{id} (design §2).
	// Nil omits the fields. ANALYTICS-only — never feeds pricing/verification
	// and never re-ranks (that is §4).
	volumeCharacter VolumeCharacterReader

	// readyz single-flight cache (inventory #26) — see handleReadyz.
	readyzMu   sync.Mutex
	readyzAt   time.Time
	readyzCode int
	readyzBody []byte

	// livez/lake single-flight cache (#310) — see handleLivezLake.
	livezLakeMu          sync.Mutex
	livezLakeAt          time.Time
	livezLakeCode        int
	livezLakeBody        []byte
	fxHistory            FXHistoryReader
	sessionPeeker        SessionPeeker
	incidents            []incidents.Incident
	sep10                auth.SEP10Validator
	cors                 middleware.Middleware
	auth                 middleware.Middleware
	keyPolicy            middleware.Middleware
	rateLimit            middleware.Middleware
	monthlyQuota         middleware.Middleware
	touchUsage           middleware.Middleware
	requireEmailVerified middleware.Middleware
	usageTracker         middleware.Middleware
	usageReader          UsageReader
	usageRollupReader    UsageRollupReader
	hub                  *streaming.Hub
	// tipProducers is the shared tip-stream producer registry (RT-1):
	// one compute loop per distinct (asset, quote, window) publishing
	// into the hub, refcounted by open /v1/price/tip/stream
	// connections. See price_tip_producers.go.
	tipProducers         tipProducerRegistry
	confidence           ConfidenceLooker
	triangulated         TriangulatedPriceLooker
	cdnEnabled           bool
	statusBackend        StatusBackend
	backupMetrics        backupMetricsSource
	backups              backupsCache
	archiveReportPath    string
	regionName           string
	regionDeployment     string
	statusServices       []string
	dashboardAuth        DashboardAuthMounter
	dashboardKeys        DashboardAuthMounter
	dashboardWebhooks    DashboardAuthMounter
	dashboardPriceAlerts DashboardAuthMounter
	sessionAuth          middleware.Middleware
	// verifiedCurrencies is the loaded *currency.Catalogue — the
	// cross-chain currency seed (USDC, USDT, BTC, ETH, …) plus per-
	// network identities. Powers the `unverified_warning` body +
	// flags.unverified_ticker_collision attachment on /v1/assets/{id}
	// (R-018 Phase 1.1). Nil-safe: applyUnverifiedWarning returns
	// false when the catalogue isn't wired, leaving every response
	// without the warning surface — that's the same behaviour as
	// pre-1.1.
	verifiedCurrencies *currency.Catalogue
	// sacReserveAssets maps a Stellar-Asset-Contract (SAC) contract
	// C-strkey → the canonical classic/native asset it wraps, built
	// lazily from verifiedCurrencies (ADR-0039 lending TVL: a Blend
	// reserve's underlying is the asset's SAC, so we price via this).
	sacReserveAssets map[string]string
	sacReserveOnce   sync.Once
	// backfillCoverage is the per-source min/max-ledger snapshot
	// powering /v1/diagnostics/ingestion's coverage section. Nil
	// leaves that section absent. See [CoverageCache].
	backfillCoverage *CoverageCache
	// nonstandardDecimals backs the read-time dex-nonstandard-decimals
	// forward normalization (docs/operations/runbooks/
	// dex-nonstandard-decimals.md): every price-shaped serving path
	// resolves per-leg decimals through it (aggregate.ResolveDecimals)
	// and scales the finished ratio via aggregate.AdjustPrice. Nil
	// disables normalization entirely — every asset resolves to the
	// 7dp default and all prices serve raw, the pre-guard behaviour.
	// See [NonstandardDecimalsCache].
	nonstandardDecimals *NonstandardDecimalsCache
	// globalPrice + globalPriceOpts power the /v1/assets/{slug}
	// global view's three-tier fallback chain (R-018 Phase 1.3a/1.4a).
	// Nil-safe: handleGlobalAsset returns a view without the price
	// block when not wired — the slug still resolves to a catalogue
	// entry, networks[] still populates, and consumers can drill
	// into the Stellar network's deep_link for per-asset pricing.
	globalPrice     aggregate.GlobalPriceReader
	globalPriceOpts aggregate.GlobalPriceOptions
	// sacWrappers is the operator-config map of Stellar-Asset-Contract
	// C-strkey → "CODE-ISSUER" canonical asset key. Surfaced on
	// /v1/sac-wrappers so the explorer can resolve raw Soroban
	// contract addresses (which Soroswap/Phoenix/Aquarius/Comet
	// emit as base/quote in their swap events) back to readable
	// asset symbols. Nil means "operator hasn't configured the map"
	// — the endpoint serves an empty object.
	sacWrappers map[string]string
	// networkPassphrase is the Stellar network passphrase, used to derive
	// deterministic SAC contract ids for known assets (isKnownSAC). Empty
	// disables the computed-SAC half of the check (sac_wrappers still apply).
	networkPassphrase string
	// knownSACs is the cached union of sac_wrappers + computed SAC ids
	// (native + verified catalogue), built once via knownSACsOnce.
	knownSACsOnce sync.Once
	knownSACs     map[string]struct{}
	// assetDetailCache is the response-level cache for /v1/assets/{id}.
	// Stores the pre-rendered JSON bytes + Flags per asset_id with a
	// short TTL (30s by default). Cache hits skip the entire handler
	// chain — resolveAssetDetail, applySep1Overlay (even on Redis
	// hit), applyF2Fields (4 uncached DB calls: volume / 2× price /
	// supply), applyAssetExtensionFields. Drift-safe by construction:
	// the cached entry IS what the handler produces.
	//
	// Pre-cache benchmark (rc.63 internal localhost on r1): ~700-900ms
	// warm. The 7-reader fan-out caches (CachedAssetsReader SWR) are
	// hot from prewarmCaches + selfPrewarmAssetEndpoints, so the
	// remaining cost is in the F2 chain. Wrapping each F2 reader is
	// 4 new wrapper types; the response-level cache is one type.
	//
	// Nil-safe: a nil cache short-circuits every method to no-op +
	// miss. ttl=0 has the same effect at config layer.
	assetDetailCache *assetDetailResponseCache
	// usdPeggedClassics is the operator's allow-list of classic
	// credit assets they declare as USD-pegged stablecoins.
	// Mirrors trades.usd_pegged_classic_assets from config. Used
	// at chart-fallback time: when /v1/chart is asked for X/fiat:USD
	// and the literal pair has zero points (because we don't store
	// synthetic XLM/USD in prices_1m — the proxy is applied at
	// query time), the chart handler retries against X/<peg> for
	// each entry until one returns data, marking the response
	// `triangulated: true` for transparency.
	usdPeggedClassics []canonical.Asset
	// fiatPeggedClassics maps a classic asset_id to the fiat currency
	// the OPERATOR declares it 1:1-pegged to (pricing_guard.
	// fiat_pegged_classic_assets). Drives the declared-peg price fill
	// on the asset listing + detail surfaces: a configured row whose
	// price_usd is nil AFTER the substance gate ran gets price_usd =
	// current fiat→USD FX rate, stamped price_basis="declared_peg".
	// See [Server.fillDeclaredPegPrice] for the ordering invariant.
	fiatPeggedClassics map[string]canonical.Asset
	// ingestionSnapshot caches a fully-built IngestionDiagnostics
	// computed every ~15s by a background goroutine launched via
	// [Server.StartIngestionSnapshotRefresh]. Powers
	// /v1/diagnostics/ingestion sub-millisecond when populated
	// (#16). Nil before the first refresh fires; handler falls back
	// to inline-build (the legacy 200-500ms path) in that case.
	ingestionSnapshot atomic.Pointer[ingestionSnapshotEntry]
	mux               *http.ServeMux
	started           time.Time
	// requestTimeout bounds every non-streaming request's context via
	// the RequestTimeout middleware (see Handler). Defaulted in New to
	// [defaultRequestTimeout] when Options.RequestTimeout is unset. The
	// durable chokepoint behind C3-1/C3-2/P1 (audit-2026-07-16) — every
	// handler inherits a deadline even when it forgets its own.
	requestTimeout time.Duration
}

// defaultRequestTimeout is the fallback per-request deadline applied by
// the RequestTimeout middleware when Options.RequestTimeout is unset.
// 15s is longer than the per-read 8s ceilings (so a per-read deadline
// surfaces its own, more specific error first) and shorter than the
// http.Server WriteTimeout (30s) so the app-layer deadline is what
// actually bounds a hung request.
const defaultRequestTimeout = 15 * time.Second

// DashboardAuthMounter is the interface main.go's
// dashboardauth.Handlers satisfies — defined here so this package
// doesn't import dashboardauth (the dependency goes the other
// way: dashboardauth uses internal/notify + internal/platform,
// both of which are leaf packages, and main.go wires the result
// into v1.Options).
type DashboardAuthMounter interface {
	Mount(mux *http.ServeMux)
}

// Options configures a [Server] at construction.
type Options struct {
	Logger *slog.Logger
	// ReadyChecks are polled by /readyz. Order matters only for
	// log output (first-failed wins).
	ReadyChecks []ReadyChecker
	// Assets, when non-nil, backs /v1/assets and /v1/assets/{id}.
	// Leave nil during early bring-up; handlers return an empty
	// list + degrade single-asset lookups to pure canonical echo.
	Assets AssetReader
	// Prices, when non-nil, backs /v1/price. Leave nil to return
	// 503 — the handler is mounted either way so clients can
	// integrate against the wire contract before we have a
	// reader wired.
	Prices PriceReader

	// History, when non-nil, backs /v1/history. Leave nil to return
	// 503 on that path.
	History HistoryReader

	// Markets, when non-nil, backs /v1/markets. Leave nil and the
	// handler serves an empty list (mirrors /v1/assets' pattern so
	// clients can integrate before the data is available).
	Markets MarketsReader

	// Oracle, when non-nil, backs /v1/oracle/latest. Leave nil to
	// return 503 on that path.
	Oracle OracleReader
	// Sep1Cache, when non-nil, enables the SEP-1 overlay on
	// /v1/assets/{id}. The handler reads from the `issuers.sep1_payload`
	// JSONB column populated by `stellarindex-ops sep1-refresh`.
	// Pre-2026-05-29 this was a live HTTPS fetch (MetadataResolver);
	// the live path dominated /v1/assets/{id} p95 (~4s long tail) so
	// it's now cron-only.
	Sep1Cache Sep1CachedReader

	// CORS, when non-nil, is inserted above RateLimit in the
	// middleware stack. Preflight OPTIONS requests short-circuit
	// before the rate-limit counter increments. Typically
	// constructed via middleware.CORS(...) with AllowedOrigins
	// drawn from cfg.API.AllowedOrigins.
	CORS middleware.Middleware

	// Accounts, when non-nil, backs POST /v1/account/keys (key
	// issuance). Leave nil to make that endpoint return 503 — the
	// GET endpoints (/me, /usage) only consult the request-context
	// Subject and don't need the store. Wire only when Redis is
	// reachable; the binary's auth.NewRedisAPIKeyStore enforces that.
	Accounts AccountStore

	// AccountKeyQuota caps how many un-revoked API keys ONE caller
	// identifier may hold through the self-service POST
	// /v1/account/keys path. Zero (the default) selects
	// [defaultAccountKeyQuota]; a negative value disables the check.
	//
	// C3-015 (audit-2026-07-23): the self-service mint had no cap at
	// all, so a single authenticated caller could mint keys in a loop
	// until Redis filled — while the parallel dashboard mint path
	// (dashboardkeys.HandleCreate) has enforced a tier-aware ceiling
	// since F-1257. It is a flat cap rather than a tier ladder because
	// this surface's Subjects carry no platform tier: main.go disables
	// the route entirely under `auth_backend=postgres`, so every caller
	// that reaches it authenticated through the Redis validator, whose
	// records have no account row to read a tier from.
	AccountKeyQuota int

	// PlatformAccounts, when non-nil, backs the operator tier-override
	// endpoints (GET/PATCH /v1/admin/accounts/{id}). Production wires
	// postgresstore.NewAccountStore — the SAME store the Postgres
	// API-key validator reads the account's rate-limit / monthly-quota
	// overrides from at Lookup time, so a staff-set override takes
	// effect on the next key lookup. Nil makes those endpoints 503.
	PlatformAccounts PlatformAccountStore

	// RegisterAccounts, when non-nil, backs POST /v1/register — the
	// open, curl-first onboarding path that creates a free-tier
	// platform account and mints its first Postgres-backed API key
	// (the key store is APIKeyBudgets.Platform; both must be wired or
	// the endpoint 503s). Production wires the SAME
	// postgresstore.NewAccountStore instance as PlatformAccounts,
	// narrowed to create-only. The per-IP SignupIPThrottle gates it.
	RegisterAccounts RegisterAccountCreator

	// StatusNotices, when non-nil, backs the operator status-banner
	// endpoints (POST/GET /v1/admin/status-notices, resolve) and the
	// public GET /v1/status/notices. Production wires
	// postgresstore.NewStatusNoticeStore (migration 0082). Nil makes
	// the public list return `[]` and the admin endpoints 503.
	StatusNotices StatusNoticeStore

	// Audit, when non-nil, receives persisted audit rows for admin
	// actions (POST /v1/admin/keys → "key.mint"; account overrides;
	// status-notice mutations). Production wires
	// postgresstore.NewAuditStore. Nil degrades to structured-log-only
	// audit (the mutation still logs unconditionally).
	Audit AuditSink

	// Signups, when non-nil, backs POST /v1/signup's per-email
	// duplicate check. Without it, signup still works but isn't
	// idempotent on the email — a second signup for the same address
	// just mints another key. Production wires a Redis-backed
	// implementation that persists email-hash → key-id; nil makes
	// the duplicate check a no-op (key always mints).
	Signups SignupTracker

	// SignupIPThrottle, when non-nil, applies a per-IP cap to
	// /v1/signup separate from the global-rate-limit middleware.
	// The global IP bucket allows 60/min anonymous; that's plenty
	// for browsing the public surfaces but lets an attacker
	// bulk-mint signup→key_id pairs (one signup per request, so
	// 60 keys per minute per IP, ~3,600/hour). This throttle
	// hardens specifically against the signup-bulk-mint abuse
	// vector — typical wiring is a 5/hour-per-IP Redis bucket.
	// Nil keeps the legacy "trust the global rate limit alone"
	// behaviour. F-1232 (audit-2026-05-12).
	SignupIPThrottle SignupIPThrottle

	// SignupVerifier, when non-nil, backs the email-ownership-
	// proof flow added in F-1218 (codex audit-2026-05-12). The
	// signup handler issues a single-use token via
	// `Reserve(token, keyID, ttl)`; the
	// `GET /v1/signup/verify?token=…` handler consumes it via
	// `Consume(token)` and (in subsequent waves) flips the key
	// to a verified state. Nil disables the verify endpoint —
	// it returns 503 with a clear "verification not configured"
	// message so customers don't get the silent-no-op surprise.
	SignupVerifier SignupVerifier

	// SignupVerifyEmailer, when non-nil + paired with a non-nil
	// `SignupVerifier`, makes the signup handler issue a token,
	// Reserve it, and email the click-through verify URL.
	// F-1218 wave 44 (codex audit-2026-05-12). Nil keeps the
	// signup-handler response shape unchanged (no email sent,
	// `email_verification_sent: false` on the wire); the
	// verifier endpoint stays a no-op until wave 44 is wired
	// end-to-end.
	SignupVerifyEmailer SignupVerifyEmailer

	// APIKeyEmailVerifier, when non-nil, lets the
	// `/v1/signup/verify` handler flip the `EmailVerifiedAt`
	// timestamp on the underlying API key record after Consume.
	// F-1218 wave 45 (codex audit-2026-05-12). Production wiring
	// is `auth.RedisAPIKeyStore.MarkEmailVerified`. Nil disables
	// the marker write — the verify endpoint still returns 200
	// (the customer's click is acknowledged), but the optional
	// `RequireEmailVerified` gate can't reflect it back into
	// subsequent requests.
	APIKeyEmailVerifier APIKeyEmailVerifier

	// APIKeyBudgets are the credential stores a TIER CHANGE has to
	// clamp. Wired so PATCH /v1/admin/accounts/{id} can enforce a
	// tier-lowering on the keys the account can actually
	// authenticate with, instead of only writing accounts.tier
	// (52105fdb residual, audit-2026-07-23: the enforced per-minute
	// budget is read straight off the key record, so an operator
	// demoting Pro→Free left every existing key at 10_000/min).
	//
	// Zero value disables the admin-side clamp: the tier write still
	// lands and the endpoint reports keys_clamped=0, so a deployment
	// without a key store degrades visibly rather than silently.
	APIKeyBudgets APIKeyBudgetStores

	// Divergence, when non-nil, is consulted by /v1/price after a
	// successful LatestPrice lookup. When the lookup says
	// "warning fired" for the asset, the response carries
	// flags.divergence_warning=true. Nil means "no divergence
	// signal available" — the flag stays at its default false.
	// Wire when both the divergence worker and Redis are running.
	Divergence DivergenceLooker

	// Freeze, when non-nil, is consulted by /v1/price (and
	// /v1/price/batch) after a successful LatestPrice lookup. When
	// it reports "frozen" for the pair, the response carries
	// flags.frozen=true and flags.single_source=true (per
	// anomaly.ActionFreeze, ADR-0019). Nil means "no freeze signal
	// available" — flags.frozen stays false and flags.single_source
	// is derived from the observation count instead. Wire when the
	// aggregator's freeze-marker writer + Redis are both running.
	Freeze FrozenLooker

	// Substance, when non-nil, is the serving-side thin-market gate
	// ([PriceSubstanceGate], production impl
	// internal/pricingguard.SubstanceGate). The server consults it on
	// price-computing paths that bypass PriceReader (the tip
	// rolling-window VWAP); reader-backed paths carry the gate inside
	// the wired readers. Nil disables handler-side gating (the readers
	// may still gate independently).
	Substance PriceSubstanceGate

	// Scam, when non-nil, withholds the aggregated price for
	// directory-scam-flagged issuers on the paths the server gates
	// directly (the tip VWAP). Reader-backed paths gate inside the
	// readers. Production impl internal/pricingguard.ScamGate.
	Scam PriceScamGate

	// Supply, when non-nil, populates the F2 fields
	// (total_supply, circulating_supply, max_supply, market_cap_usd,
	// fdv_usd, supply_basis) on /v1/assets/{id} per ADR-0011.
	// Production wiring: a thin adapter around timescale.Store.LatestSupply.
	// Nil means "F2 fields unavailable" — the asset-detail body still
	// serves; F2 fields stay null. A non-nil reader still depends on
	// some other process populating asset_supply_history; this repo
	// snapshot only wires the read path.
	Supply SupplyLooker

	// TokenSupply, when non-nil, backs GET /v1/assets/{asset_id}/supply with
	// the live decode-at-ingest supply_flows lake (ADR-0034) — the raw
	// Σmint−Σburn−Σclawback total for EVERY token (vs Supply's ADR-0011
	// circulating/max policy over the 9-asset asset_supply_history). Production
	// wiring is *clickhouse.SupplyReader. Nil → the endpoint 503s.
	TokenSupply TokenSupplyReader

	// TokenDecimals, when non-nil, overlays real on-chain `decimals()` onto
	// /v1/assets/{id} for Soroban tokens, read from the lake's captured
	// contract-instance METADATA (token-sdk convention). Classic + native
	// assets are ALWAYS 7 by protocol and never consult it. Production
	// wiring is *clickhouse.ExplorerReader. Nil → Soroban details keep the
	// 7 default.
	TokenDecimals TokenDecimalsReader

	// LakeWatermark, when non-nil, stamps lake-backed responses
	// (/v1/assets/{id}/supply, /v1/accounts/{g}, /v1/assets/{id}/holders)
	// with `as_of_ledger` and flips `flags.stale` when the lake's captured
	// tip trails now beyond lakeStaleThreshold (ADR-0041 Decision 4).
	// Production wiring is *clickhouse.ExplorerReader. Nil → those fields
	// are omitted and the flag never fires from the watermark.
	LakeWatermark LakeWatermarkReader

	// Volume, when non-nil, populates the `volume_24h_usd` field on
	// /v1/assets/{id} (trailing-24h USD-denominated trade volume
	// across every pair the asset participates in). Per Freighter V2
	// scope. Production wiring: a thin adapter around
	// timescale.Store.Volume24hUSDForAsset. Nil leaves the field
	// null — independent of Supply, so the volume can serve even
	// when supply isn't yet wired (and vice versa).
	Volume VolumeReader

	// Change24h, when non-nil, populates the `change_24h_pct` field
	// on /v1/assets/{id} (signed percentage change vs the asset's
	// USD price ~24h ago). Production wiring: a thin adapter around
	// timescale.Store.ClosedVWAP1mAtOrBefore at t=now-24h. Nil
	// leaves the field null. Independent of Supply / Volume — any
	// combination of (Supply, Volume, Change24h) is legal.
	Change24h Change24hReader

	// PriceAt, when non-nil, backs GET /v1/price/at — point-in-time
	// closed-bucket VWAP for cost-basis/PnL tooling (board #46).
	PriceAt PriceAtReader

	// ChangeSummary, when non-nil, backs GET /v1/changes/{entity_type}/{id}.
	// Production wiring: a thin adapter around
	// timescale.Store.GetChangeSummary, which reads the
	// change_summary_5m hypertable populated by the changesummary
	// worker (Phase 3). Powers every multi-window delta strip on
	// the explorer. Nil makes the endpoint return 503.
	ChangeSummary ChangeSummaryReader

	// AssetsReader, when non-nil, supplies the asset-catalogue overlay
	// the /v1/assets handlers fan out across (price / volume /
	// market_cap / sparkline / ATH / top_markets). The standalone
	// /v1/coins HTTP route was removed in rc.48; this seam stays
	// because every /v1/assets row sources the same data through
	// it. Production wiring is timescale.Store directly (implements
	// ListAssetsExt). Nil makes the affected /v1/assets fields 503.
	AssetsReader AssetsReader

	// TransitivePricer, when non-nil, supplies a one-hop USD price for
	// assets the catalogue's two hard-coded shapes (direct_usd,
	// asset_vs_xlm) cannot reach — notably Soroban-native contract
	// assets, which cannot appear in `classic_assets` at all. Nil leaves
	// pricing exactly as it is. Production wiring is timescale.Store.
	TransitivePricer TransitivePricer

	// Issuers, when non-nil, backs GET /v1/issuers/{g_strkey}.
	// Production wiring is timescale.Store directly. Nil makes
	// the endpoint return 503.
	Issuers IssuersReader

	// SEP41Transfers, when non-nil, backs GET
	// /v1/contracts/{contract_id}/transfers. Production wiring is
	// timescale.Store directly (it implements ListSEP41Transfers).
	// Nil makes the endpoint return 503. F-0021 closure
	// (audit-2026-05-26): per-account net-position queries — the
	// Stellar moat feature CG/CMC structurally cannot offer.
	SEP41Transfers SEP41TransfersReader

	// Cursors, when non-nil, backs GET /v1/diagnostics/cursors.
	// Production wiring is timescale.Store directly (it implements
	// ListCursors). Nil makes the endpoint return 503. Operator-
	// facing diagnostic; powers the explorer /diagnostics page.
	Cursors CursorsReader

	// CoverageReader, when non-nil, backs the ADR-0031 shadow
	// data-derived density on /v1/diagnostics/ingestion. Reads
	// source_coverage_snapshots rows that the gap detector
	// (in the aggregator binary) upserts every cycle. Production
	// wiring is timescale.Store directly (ListSourceCoverage). Nil
	// leaves DensityPctV2 / GapFreePct as zero in every response
	// row; v1 cursor-derived density remains the authoritative
	// signal during the Phase 1 shadow window.
	CoverageReader SourceCoverageReader

	// CompletenessReader, when non-nil, backs the ADR-0033 Phase 6
	// completeness_* fields on /v1/diagnostics/ingestion. Nil leaves
	// them absent (UI falls back to the gap_free coverage signal).
	CompletenessReader CompletenessReader

	// ProtocolContracts, when non-nil, backs the contract registry
	// (instance lists + counts) on /v1/protocols*. Production wiring
	// is timescale.Store directly (ListProtocolContracts). Nil keeps
	// the directory serving with empty contract lists / zero counts.
	ProtocolContracts ProtocolContractsReader

	// ProtocolStats, when non-nil, backs the per-protocol events_24h
	// column on /v1/protocols*. Production wiring is timescale.Store
	// directly (CountRecentEventsBySource). Nil serves zeros.
	ProtocolStats ProtocolStatsReader

	// ProtocolActivity, when non-nil, backs the per-protocol lake
	// analytics on /v1/protocols/{name} (event-type breakdown, daily
	// activity series, per-contract event counts). Production wiring is
	// the *clickhouse.ExplorerReader (same lake reader as Explorer). Nil
	// serves the detail view without the analytics fields.
	ProtocolActivity ProtocolActivityReader

	// ProtocolBespoke, when non-nil, backs the per-category bespoke
	// analytics block on /v1/protocols/{name} (TVL/volume/AUM/flows/feeds)
	// from the served-tier projected tables. Production wiring is
	// timescale.Store. Nil serves the detail view without the bespoke block.
	ProtocolBespoke ProtocolBespokeReader

	// SoroswapPairs, when non-nil, supplies soroswap's contract list
	// on /v1/protocols* from the soroswap_pairs registry (its pair
	// set carries token identities and predates protocol_contracts).
	// Production wiring is timescale.Store directly
	// (LoadSoroswapPairRegistry). Nil serves soroswap with an empty
	// contract list / zero count.
	SoroswapPairs SoroswapPairsReader

	// ProtocolPoolTokens, when non-nil, maps each pool-based protocol's
	// contracts to the token contract C-strkeys it holds, so the
	// /v1/protocols/{name} roster renders a human pair ("XLM/USDC") in place
	// of raw C-strkeys. Production wiring is timescale.Store (PoolTokens).
	// Nil serves the roster without the pair label (soroswap still labels
	// its rows from its own token0/token1).
	ProtocolPoolTokens ProtocolPoolTokensReader

	// NetworkStats, when non-nil, backs GET /v1/network/stats —
	// the consolidated home-page aggregate (24h volume, markets,
	// assets indexed, latest ledger). Production wiring is
	// timescale.Store directly. Nil makes the endpoint 503.
	NetworkStats NetworkStatsReader

	// Aggregators, when non-nil, backs GET /v1/aggregators — the
	// routers-registry listing with per-router routed-via 24h
	// rollups (migration 0025 Phase B). Production wiring is
	// timescale.Store directly (AggregatorRollup). Nil makes the
	// endpoint 503.
	Aggregators AggregatorsReader

	// MarketSources backs GET /v1/markets/sources (per-source 24h
	// volume breakdown for a pair or asset). timescale.Store satisfies
	// it directly; nil makes the endpoint return an empty list.
	MarketSources MarketSourceReader

	// SourcesStats, when non-nil, populates the per-source
	// trade_count_24h field on /v1/sources?include=stats. Without
	// it, the include flag is silently ignored and the response
	// stays the all-static-registry projection.
	SourcesStats SourcesStatsReader

	// Lending, when non-nil, backs /v1/lending/pools (the per-Blend-
	// pool summary listing). Leave nil and the handler serves an
	// empty array — same degradation pattern as Markets.
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

	// DivergenceThresholdPct is the operator's divergence alert
	// threshold (config divergence.threshold_pct), surfaced on
	// /v1/divergence/series so charts can shade the alert band
	// from the SAME number the worker fires on. Zero → the field
	// is omitted from the wire (no band drawn); serving a made-up
	// default here would fabricate policy.
	DivergenceThresholdPct float64

	// MinMarketCapVolumeUSD is the valuation-integrity floor (config
	// aggregate.min_market_cap_volume_usd, USD). A market_cap_usd / fdv_usd
	// is SUPPRESSED (served null with market_cap_low_liquidity=true) when its
	// backing price came from a single venue AND the asset's trailing-24h USD
	// volume is below this floor — so one dust trade can't present an obscure
	// asset as worth billions. The AND is load-bearing (see
	// [dustLiquiditySuppressed]). Zero (the default here when a caller doesn't
	// set it) disables the guard; the production binary wires the config value
	// whose own default is 1000.
	MinMarketCapVolumeUSD float64

	// Currencies, when non-nil, supplies the world fiat-currency
	// rates snapshot used by /v1/assets fiat rows + chart fiat:fiat
	// fallback. The standalone /v1/currencies route was removed in
	// rc.48; this seam stays because /v1/assets and /v1/chart both
	// consume the same snapshot. Leave nil to fall back to empty
	// currencies state.
	Currencies CurrenciesReader

	// Explorer, when non-nil, backs the network-explorer endpoints
	// (ADR-0038): /v1/ledgers, /v1/tx, /v1/operations, /v1/contracts,
	// /v1/search — reading the certified ClickHouse lake directly.
	// *clickhouse.ExplorerReader satisfies it. Nil → those routes 503.
	Explorer ExplorerReader

	// SEP41Movements, when non-nil, backs the Postgres "recent tail"
	// half of GET /v1/accounts/{g_strkey}/movements' merge (ADR-0048
	// D5) — timescale.Store satisfies it via
	// ListSEP41TransfersByAddress. A SEPARATE seam from SEP41Transfers
	// above (that one is contract-scoped, for
	// /v1/contracts/{id}/transfers; this one is address-scoped, across
	// every contract). Nil degrades the movements endpoint to serving
	// the ClickHouse pre-P23 archive alone, with an honest
	// coverage_note, rather than 503ing outright — Explorer is the
	// hard dependency for that route, this is a soft one.
	SEP41Movements explorerpkg.SEP41MovementsReader

	// Positions, when non-nil, backs GET /v1/accounts/{g}/positions
	// (the "DeFi positions" view) — six per-protocol Postgres folds
	// (blend money-market, blend backstop, phoenix stake, defindex
	// vault shares, sorocredit, aquarius gauge). timescale.Store
	// satisfies it. Nil 503s the endpoint. Venue human labels reuse
	// ProtocolPoolTokens (below) — the same reader the #91 protocol-
	// roster pair-label work already wired.
	Positions explorerpkg.PositionsReader

	// AccountTrades, when non-nil, backs GET /v1/accounts/{g}/trades —
	// the per-address historic-trades listing over the Postgres
	// `trades` hypertable (taker/maker attribution — see
	// timescale/account_trades.go for what the table can and cannot
	// attribute). timescale.Store satisfies it. Nil 503s the endpoint.
	AccountTrades explorerpkg.TradesReader

	// AccountActivity, when non-nil, backs the Postgres segments of
	// GET /v1/accounts/{g}/activity (trades_total / defi_actions /
	// bridge_transfers); ops_by_type reads the ClickHouse lake via
	// Explorer. timescale.Store satisfies it. Nil degrades those
	// segments with an honest coverage_note; the endpoint 503s only
	// when Explorer is ALSO nil.
	AccountActivity explorerpkg.ActivityReader

	// Directory, when non-nil, resolves curated third-party address
	// labels (account_directory, migration 0136 — synced from the
	// MIT-licensed stellar-expert/public-directory) for the account +
	// contract detail views and GET /v1/directory. timescale.Store
	// satisfies it. Nil omits the `directory` field and 503s the
	// batch endpoint.
	Directory explorerpkg.DirectoryReader

	// VolumeCharacter, when non-nil, populates volume_character + its
	// account-structure signals on /v1/assets/{id} (wash-and-scam-signals
	// design §2). Production wiring: timescale.Store (the maker/taker
	// trades live in Timescale, not the ClickHouse lake). Nil omits the
	// fields. ANALYTICS-only — never feeds pricing/verification, never
	// re-ranks (that is §4).
	VolumeCharacter VolumeCharacterReader

	// FXHistory, when non-nil, lets /v1/chart serve fiat:fiat pairs
	// from the fx_quotes hypertable for ranges beyond 7d. Leave nil
	// to keep /v1/chart fiat:fiat in 7d-only mode.
	FXHistory FXHistoryReader

	// SessionPeeker, when non-nil, lets handlers read the
	// magic-link session bound to the request context. Used by
	// /v1/account/me to surface user/account info for cookie-auth
	// callers (the API-key path uses Subject; both can coexist on a
	// request, in which case session takes precedence).
	SessionPeeker SessionPeeker

	// SEP10, when non-nil, backs GET /v1/auth/sep10/challenge and
	// POST /v1/auth/sep10/token. Production wiring: an
	// auth/sep10.Validator constructed from the binary's signing
	// seed + JWT secret config. Nil makes both endpoints return 503
	// (the binary didn't wire one — typically because the seed/
	// secret config is absent in this deployment).
	SEP10 auth.SEP10Validator

	// Auth, when non-nil, is inserted between CORS and RateLimit.
	// Sets a Subject in the request context that downstream
	// middleware (rate-limit, request logger) and handlers can
	// read via [auth.SubjectFrom]. Typically constructed via
	// middleware.Auth(middleware.AuthOptions{Mode: cfg.API.AuthMode, …}).
	// Leave nil for legacy "no auth, anonymous-only" behaviour;
	// the rate-limit middleware then keys on RemoteIP only.
	Auth middleware.Middleware

	// KeyPolicy, when non-nil, runs AFTER Auth and BEFORE RateLimit.
	// Enforces the per-key policy fields the dashboard surfaces
	// (IP allowlist, Referer allowlist, per-endpoint permissions)
	// against the authenticated Subject. F-1226 (codex
	// audit-2026-05-12): pre-fix these were accepted at key
	// creation but no middleware enforced them at request time.
	// Anonymous subjects pass through unchanged; the policy data
	// only ships on Subjects produced by the Postgres validator.
	// Typically constructed via middleware.KeyPolicy().
	KeyPolicy middleware.Middleware

	// RateLimit, when non-nil, is appended to the middleware stack
	// as the innermost wrapper — so the Logger + Auth middlewares
	// have already populated remote_ip + Subject into the request
	// context. Typically constructed via
	// middleware.RateLimitBySubject(anonBucket, authBucket, ...)
	// so the per-tier limits (api.anon_rate_limit_per_min vs
	// api.key_rate_limit_per_min) actually take effect; the older
	// single-bucket middleware.RateLimit shape is kept for tests
	// but production wiring uses the by-subject form. See
	// cmd/stellarindex-api/main.go for the canonical wire-up.
	RateLimit middleware.Middleware

	// UsageTracker, when non-nil, is inserted at the end of the
	// middleware chain; fires per-request to record per-day
	// counters that feed /v1/account/usage. Best-effort — never
	// blocks a request. Pair with UsageReader to expose the data.
	UsageTracker middleware.Middleware

	// MonthlyQuota, when non-nil, is inserted BEFORE rate-limit so
	// a request that exceeds the per-key monthly cap returns 429
	// without spending a rate-limit token. F-1226 (codex audit-
	// 2026-05-12). Wire-up: middleware.MonthlyQuota(usageCounter,
	// …). Skipped when nil — the cap is opt-in per validator (only
	// Postgres-backed keys carry `Subject.MonthlyQuota`).
	MonthlyQuota middleware.Middleware

	// TouchUsage, when non-nil, is inserted INSIDE rate-limit so
	// a denied (429) request doesn't update the dashboard's "last
	// seen" column for the rejected attempt. The middleware
	// itself fires post-handler with a Redis-SETNX debounce, so
	// per-request cost is one Redis SETNX even on cache hit. F-1226
	// (codex audit-2026-05-12) wave 39. Skipped when nil — opt-in
	// per deployment (requires both Postgres keys store + Redis).
	TouchUsage middleware.Middleware

	// RequireEmailVerified, when non-nil, is inserted AFTER auth
	// and BEFORE rate-limit. It rejects API-key callers whose
	// `EmailVerifiedAt` is zero AND whose identifier indicates a
	// `/v1/signup` origin. F-1218 wave 45 (codex audit-2026-05-12).
	// Opt-in per deployment — production wiring gates this on
	// `cfg.API.SignupRequireEmailVerification` so existing keys
	// keep working through the rollout window.
	RequireEmailVerified middleware.Middleware

	// UsageReader, when non-nil, backs /v1/account/usage with
	// real per-day counts. Without it the endpoint stays on its
	// "empty list with locked wire shape" default.
	UsageReader UsageReader

	// UsageRollupReader, when non-nil, backs /v1/account/usage with
	// per-day × per-endpoint rows (requests / errors / throttled)
	// read from the `usage_daily` Timescale rollups the usage-rollup
	// worker maintains. Takes precedence over UsageReader; the
	// handler falls back to the per-day Redis totals when the
	// rollup read errors or has no rows yet (fresh deployment,
	// worker not yet swept).
	UsageRollupReader UsageRollupReader

	// Hub, when non-nil, backs the closed-bucket SSE endpoint
	// (`/v1/price/stream`). Producers (typically the aggregator's
	// per-window-close pass) call Hub.Publish(); subscribers attach
	// via [streaming.Stream] inside the handler.
	//
	// Leave nil to make `/v1/price/stream` return 503 — the rest
	// of the v1 API serves cleanly. The tip + observations stream
	// endpoints do NOT use this Hub; they are per-connection-tick.
	Hub *streaming.Hub

	// Confidence, when non-nil, populates the confidence + factors
	// fields on `/v1/price` responses (ADR-0019 §"Multi-factor
	// confidence score"). Production wiring: a Redis adapter that
	// reads `confidence:<base>:<quote>:<window>` from the cache
	// the aggregator's confidence-compute path writes.
	//
	// Leave nil to keep the score off the wire — the rest of the
	// `/v1/price` envelope serves cleanly without it. Cache misses
	// at lookup time also leave the field unset.
	Confidence ConfidenceLooker

	// Triangulated, when non-nil, is the fallback /v1/price
	// consults after a Timescale miss. Returns triangulated
	// implied VWAPs (per the aggregator's triangulation worker)
	// + the provenance marker that gates `flags.triangulated`.
	// Production wiring: a Redis adapter reading
	// `vwap:<base>:<quote>:<window>` + the `:provenance` sibling.
	// Nil leaves /v1/price 404'ing for triangulated-only pairs
	// (the historical behaviour).
	Triangulated TriangulatedPriceLooker

	// CDNEnabled controls whether cacheable routes emit `s-maxage`
	// (CDN-tier) Cache-Control directives in addition to `max-age`
	// (client tier). Default: true — operators with a CDN in front
	// of the API leave it on. Set false (via cfg.API.CDNEnabled) for
	// deployments without a CDN, so a CDN they don't run can't cache
	// anything that downstream changes might have made auth-tied.
	// See [middleware.CacheControlWithCDN] for the policy detail.
	CDNEnabled bool

	// StatusBackend, when non-nil, backs /v1/status with
	// Prometheus-derived service heartbeats, latency percentiles,
	// freshness signals, and Alertmanager incident counts. Nil
	// keeps /v1/status serving an in-process surface (uptime +
	// region label only) — useful for deployments without a local
	// Prometheus.
	StatusBackend StatusBackend

	// ArchiveReportPath, when non-empty, backs GET
	// /v1/diagnostics/archive: the filesystem path of the latest
	// JSON report the ADR-0017 archive-completeness daemon writes
	// (`stellarindex-ops archive-completeness verify -output-file`).
	// Empty → the endpoint returns 503; a configured path whose file
	// doesn't exist yet → 404 (fresh host, daemon hasn't run).
	ArchiveReportPath string

	// BackupMetrics, when non-nil, backs GET /v1/diagnostics/backups
	// (the public status page's Backups panel). Nil derives it from
	// StatusBackend when that is a *PrometheusStatusBackend — the
	// production case needs no extra wiring; a deployment with no
	// Prometheus gets a 503 from the endpoint. Tests supply a fake.
	BackupMetrics BackupMetricsSource

	// RegionName + RegionDeployment label /v1/status responses.
	// Default to "unknown" / "production" when unset.
	RegionName       string
	RegionDeployment string

	// StatusServices names the BACKGROUND services this deployment
	// runs, and therefore the only ones /v1/status reports a heartbeat
	// for and rolls `overall` up from. Empty defaults to
	// {"indexer","aggregator"} — the pubnet shape, unchanged. The lean
	// test nets run no aggregator, and before #328 its permanent
	// "unknown" pinned overall at "degraded" forever.
	StatusServices []string

	// DashboardAuth, when non-nil, mounts the customer-dashboard
	// magic-link auth flow (POST /v1/auth/login + GET /v1/auth/callback
	// + POST /v1/auth/logout). Production wiring is a
	// dashboardauth.Handlers built from the Postgres platform stores
	// + a Resend (or Noop) sender; main.go gates construction on
	// cfg.API.Dashboard.BaseURL being non-empty.
	DashboardAuth DashboardAuthMounter

	// DashboardKeys, when non-nil, mounts the dashboard's
	// key-management surface (GET / POST / DELETE /v1/dashboard/keys
	// — the dashboard SPA's source of truth for listing + minting
	// + revoking customer keys, gated on the session cookie that
	// DashboardAuth sets). Same DashboardAuthMounter shape; main.go
	// gates construction on the Postgres platform stores being
	// reachable.
	DashboardKeys DashboardAuthMounter

	// DashboardWebhooks, when non-nil, mounts the dashboard's
	// customer-webhook CRUD surface (GET / POST / PATCH / DELETE
	// /v1/dashboard/webhooks + GET /v1/dashboard/webhooks/{id}/deliveries).
	// Backed by `internal/platform/postgresstore.WebhookStore`; the
	// delivery worker that drains the queue runs in
	// `internal/customerwebhook` and is orthogonal to these
	// handlers. F-1270 (audit-2026-05-12).
	DashboardWebhooks DashboardAuthMounter

	// DashboardPriceAlerts, when non-nil, mounts the dashboard's
	// price-alert CRUD surface (GET / POST /v1/dashboard/price-alerts +
	// PATCH / DELETE /v1/dashboard/price-alerts/{id}). Backed by
	// `internal/platform/postgresstore.PriceAlertStore` (migration
	// 0080); the evaluator that checks the alerts and enqueues
	// `price.alert` webhook deliveries runs in the aggregator
	// (`internal/pricealerts`) and is orthogonal to these handlers.
	// BACKLOG #60.
	DashboardPriceAlerts DashboardAuthMounter

	// SACWrappers is the operator-config map of SAC C-strkey →
	// "CODE-ISSUER" classic asset key. Backs /v1/sac-wrappers,
	// the read-only resolution endpoint the explorer's AssetLabel
	// joins client-side to render readable symbols for Soroban DEX
	// pools (which use SAC contracts as base/quote at the wire). Nil
	// or empty makes the endpoint return an empty map — the explorer
	// degrades to showing the raw C-strkey.
	SACWrappers map[string]string

	// NetworkPassphrase is the Stellar network passphrase (pubnet). Used to
	// derive deterministic SAC contract ids for known assets so the WASM
	// endpoint can answer "SAC, no WASM" for asset contracts whose instance
	// predates the lake's capture window. Empty disables the computed half.
	NetworkPassphrase string

	// USDPeggedClassics is the operator's allow-list of classic
	// credit assets they trust as 1:1 USD stablecoins. Same list
	// fed to trades.usd_pegged_classic_assets — wire it through
	// from the same TradesConfig field. Used by /v1/chart to
	// fall back from a literal X/fiat:USD lookup (which has no
	// rows in prices_1m — the proxy is computed at query time)
	// to X/<peg> when the literal pair returns 0 points. Empty
	// disables the fallback; the chart endpoint still serves the
	// literal pair when one exists.
	USDPeggedClassics []canonical.Asset

	// FiatPeggedClassics maps a classic asset_id (canonical
	// "CODE-ISSUER" wire form) to the fiat asset the operator
	// declares it 1:1-pegged to. Wired from
	// pricing_guard.fiat_pegged_classic_assets. Configured rows on
	// the asset listing + detail surfaces whose price_usd is nil
	// after the substance gate ran get price_usd filled from the
	// current fiat→USD FX rate (FXHistory / Prices — the same
	// fiatUSDPriceFor chain the fiat catalogue rows use), stamped
	// price_basis="declared_peg". Empty disables the fill.
	FiatPeggedClassics map[string]canonical.Asset

	// SessionAuth, when non-nil, wraps every handler so a present
	// dashboard session cookie populates a SessionContext on the
	// request context. Anonymous + bearer-token requests pass
	// through untouched. Required for the /v1/dashboard/* routes
	// to read the session — DashboardKeys handlers 401 on missing
	// session context.
	SessionAuth middleware.Middleware

	// VerifiedCurrencies, when non-nil, enables the verified-
	// currency overlay on /v1/assets/{id}: an `unverified_warning`
	// body + flags.unverified_ticker_collision when the requested
	// asset's code matches a verified currency's Stellar ticker
	// but the issuer doesn't. Production wiring loads
	// currency.LoadEmbedded() in cmd/stellarindex-api/main.go. Nil
	// keeps the warning surface off — every response serves
	// unchanged.
	//
	// When set, also enables the slug dispatch on
	// `/v1/assets/{slug}`: a path that matches a verified-currency
	// slug routes to the global view (Phase 1.4a) instead of the
	// per-Stellar-asset surface.
	VerifiedCurrencies *currency.Catalogue

	// BackfillCoverage, when non-nil, is the process-local cache of
	// per-source min/max ledger + trade count, refreshed on a 5-min
	// background goroutine. Powers the per-source coverage section
	// on `/v1/diagnostics/ingestion`. The underlying SQL is 2–3s on
	// a populated trades hypertable so we never run it synchronously
	// from a request. Nil leaves that section absent from the wire.
	BackfillCoverage *CoverageCache

	// SDEXOrderBook, when non-nil, is the in-process live classic
	// offer book behind GET /v1/sdex/orderbook — loaded once from the
	// lake at process start, then advanced incrementally every
	// [SDEXOrderBookAdvanceInterval] by a background goroutine in
	// cmd/stellarindex-api/main.go. Nil (no lake on this deployment)
	// or not-yet-loaded serves an honest 503 problem.
	SDEXOrderBook *SDEXOrderBookCache

	// DEXTVL, when non-nil, is the process-local snapshot of
	// per-protocol DEX TVL (current pool reserves valued in USD),
	// refreshed on a background goroutine every
	// [DEXTVLRefreshInterval]. Powers the additive `tvl` field on
	// `/v1/protocols` rows and `/v1/protocols/{name}` — per-request
	// aggregation over the reserve tables is never acceptable on the
	// explorer read path. Nil (or a cold, not-yet-refreshed cache)
	// leaves the field absent from the wire.
	DEXTVL *DEXTVLCache

	// NonstandardDecimals, when non-nil, backs the read-time
	// dex-nonstandard-decimals forward normalization: every
	// price-shaped surface (/v1/price incl. batch/windowed, /v1/vwap,
	// /v1/twap, /v1/history, /v1/ohlc single-bar + series, /v1/chart,
	// /v1/price/tip + SSE, the SEP-40 oracle passthroughs, and the
	// markets/pools/pairs last_price fields) scales its served ratio
	// by 10^(dec_base-dec_quote) for any pair with a leg the cache
	// has confirmed as non-7-decimal (aggregate.AdjustPrice). Nil
	// disables normalization — every request serves the raw ratio,
	// the pre-guard behaviour. See [NonstandardDecimalsCache] and
	// docs/operations/runbooks/dex-nonstandard-decimals.md.
	NonstandardDecimals *NonstandardDecimalsCache

	// GlobalPrice, when non-nil, powers the price block on
	// `/v1/assets/{slug}` global views via the three-tier fallback
	// chain (vwap_native → aggregator_avg → triangulated). Nil
	// leaves the price block empty — the slug still resolves, the
	// catalogue identity + networks list still surface, but
	// consumers fall back to the Stellar-network deep_link for a
	// headline price.
	GlobalPrice aggregate.GlobalPriceReader

	// GlobalPriceOpts tunes the three-tier policy. Leave zero-value
	// to use [aggregate.DefaultGlobalPriceOptions] except for the
	// aggregator source list, which is wired explicitly (the
	// defaults can't safely guess which sources are aggregator
	// class without importing the registry).
	GlobalPriceOpts aggregate.GlobalPriceOptions

	// RequestTimeout is the per-request context deadline applied to
	// every non-streaming request by the RequestTimeout middleware
	// (see Handler). Zero (or negative) falls back to
	// [defaultRequestTimeout]. Wire from cfg.API.RequestTimeout.
	// Streaming (SSE) endpoints are exempt — they own their lifecycle
	// through client-disconnect ctx cancellation.
	RequestTimeout time.Duration
}

// New constructs a Server and mounts all v1 routes.
func New(opts Options) *Server { //nolint:funlen // pure field-mapping constructor — one line per Options field; splitting the wiring into helpers would scatter it and gains nothing.
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		logger:                 logger,
		checks:                 opts.ReadyChecks,
		assets:                 opts.Assets,
		prices:                 opts.Prices,
		history:                opts.History,
		markets:                opts.Markets,
		oracle:                 opts.Oracle,
		sep1Cache:              opts.Sep1Cache,
		accounts:               opts.Accounts,
		accountKeyQuota:        opts.AccountKeyQuota,
		platformAccounts:       opts.PlatformAccounts,
		registerAccounts:       opts.RegisterAccounts,
		apiKeyBudgets:          opts.APIKeyBudgets,
		statusNotices:          opts.StatusNotices,
		signups:                opts.Signups,
		signupIPThrottle:       opts.SignupIPThrottle,
		signupVerifier:         opts.SignupVerifier,
		signupVerifyEmailer:    opts.SignupVerifyEmailer,
		apiKeyEmailVerifier:    opts.APIKeyEmailVerifier,
		divergence:             opts.Divergence,
		freeze:                 opts.Freeze,
		substance:              opts.Substance,
		transitive:             opts.TransitivePricer,
		scam:                   opts.Scam,
		supply:                 opts.Supply,
		tokenSupply:            opts.TokenSupply,
		tokenDecimals:          opts.TokenDecimals,
		lakeWatermarkReader:    opts.LakeWatermark,
		volume:                 opts.Volume,
		change24h:              opts.Change24h,
		priceAt:                opts.PriceAt,
		changesum:              opts.ChangeSummary,
		assetsReader:           opts.AssetsReader,
		issuers:                opts.Issuers,
		sep41Transfers:         opts.SEP41Transfers,
		cursors:                opts.Cursors,
		coverageReader:         opts.CoverageReader,
		networkStats:           opts.NetworkStats,
		aggregators:            opts.Aggregators,
		marketSources:          opts.MarketSources,
		sourcesStats:           opts.SourcesStats,
		lending:                opts.Lending,
		mev:                    opts.MEV,
		anomalies:              opts.Anomalies,
		divergences:            opts.Divergences,
		divergenceThresholdPct: opts.DivergenceThresholdPct,
		minMarketCapVolumeUSD:  opts.MinMarketCapVolumeUSD,
		currencies:             opts.Currencies,
		explorer:               opts.Explorer,
		directory:              opts.Directory,
		volumeCharacter:        opts.VolumeCharacter,
		fxHistory:              opts.FXHistory,
		sessionPeeker:          opts.SessionPeeker,
		audit:                  opts.Audit,
		sep10:                  opts.SEP10,
		cors:                   opts.CORS,
		auth:                   opts.Auth,
		keyPolicy:              opts.KeyPolicy,
		rateLimit:              opts.RateLimit,
		monthlyQuota:           opts.MonthlyQuota,
		touchUsage:             opts.TouchUsage,
		requireEmailVerified:   opts.RequireEmailVerified,
		usageTracker:           opts.UsageTracker,
		usageReader:            opts.UsageReader,
		usageRollupReader:      opts.UsageRollupReader,
		hub:                    opts.Hub,
		confidence:             opts.Confidence,
		triangulated:           opts.Triangulated,
		cdnEnabled:             opts.CDNEnabled,
		statusBackend:          opts.StatusBackend,
		backupMetrics:          backupMetricsFor(opts),
		archiveReportPath:      opts.ArchiveReportPath,
		regionName:             valueOr(opts.RegionName, "unknown"),
		regionDeployment:       valueOr(opts.RegionDeployment, "production"),
		statusServices:         statusServicesOr(opts.StatusServices),
		dashboardAuth:          opts.DashboardAuth,
		dashboardKeys:          opts.DashboardKeys,
		dashboardWebhooks:      opts.DashboardWebhooks,
		dashboardPriceAlerts:   opts.DashboardPriceAlerts,
		sessionAuth:            opts.SessionAuth,
		verifiedCurrencies:     opts.VerifiedCurrencies,
		backfillCoverage:       opts.BackfillCoverage,
		nonstandardDecimals:    opts.NonstandardDecimals,
		globalPrice:            opts.GlobalPrice,
		globalPriceOpts:        globalPriceOptsWithDefaults(opts.GlobalPriceOpts),
		sacWrappers:            opts.SACWrappers,
		networkPassphrase:      opts.NetworkPassphrase,
		usdPeggedClassics:      opts.USDPeggedClassics,
		fiatPeggedClassics:     opts.FiatPeggedClassics,
		// 120s TTL on /v1/assets/{id} responses. MUST exceed the
		// selfPrewarmAssetEndpoints cadence (60s) with margin — at the
		// old 30s TTL the cache expired for 30 of every 60 seconds
		// between prewarm passes, so every probe landing in that window
		// (the status page polls /v1/assets/native every 30s) paid the
		// full cold-rebuild cost and inflated API p95/p99 (#52 / rc.67).
		// 120s = one full prewarm interval of headroom; matches the
		// sibling F2-path caches (1–2 min TTL, same 60s prewarm).
		// Underlying data updates per-minute at fastest; 120s staleness
		// still fits the ADR-0015 closed-bucket-only contract. Drift-safe
		// by construction — the cached entry IS what the handler
		// produces (see assetDetailResponseCache doc comment).
		assetDetailCache: newAssetDetailResponseCache(120 * time.Second),
		mux:              http.NewServeMux(),
		started:          time.Now().UTC(),
		requestTimeout:   durationOr(opts.RequestTimeout, defaultRequestTimeout),
	}
	applyProtocolOptions(s, opts)
	s.explorerHandler = explorerHandlerFor(s, opts, logger)
	loadIncidents(s, logger)
	s.mountRoutes()
	return s
}

// applyProtocolOptions copies the Protocols-pillar reader options
// (/v1/protocols*, /v1/coverage joins) onto the server. Split from New
// for funlen — same rationale as loadIncidents; keep the group
// together so the pillar's wiring stays a single auditable block.
func applyProtocolOptions(s *Server, opts Options) {
	s.completenessReader = opts.CompletenessReader
	s.protocolContractsReader = opts.ProtocolContracts
	s.protocolStats = opts.ProtocolStats
	s.protocolActivity = opts.ProtocolActivity
	s.protocolBespoke = opts.ProtocolBespoke
	s.protocolPoolTokens = opts.ProtocolPoolTokens
	s.dexTVL = opts.DEXTVL
	s.sdexOrderBook = opts.SDEXOrderBook
	s.soroswapPairs = opts.SoroswapPairs
}

// loadIncidents loads + caches the embedded incident corpus once at
// startup; the data is small (a few markdown files) and ships with
// the binary, so re-parsing per-request is wasted work. New incident
// posts ship with a redeploy. Split from New for funlen (the Options
// → Server field copy is the bulk of New and must stay a single
// auditable literal).
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

// defaultStatusServices is the pubnet shape of [Options.StatusServices]:
// the background services a full deployment runs. /v1/status reports a
// heartbeat for exactly these (plus the in-process "api" entry) and
// rolls `overall` up from them.
var defaultStatusServices = []string{"indexer", "aggregator"}

// statusServicesOr normalises Options.StatusServices: nil/empty falls
// back to the pubnet pair, so every existing caller (and every test
// that constructs a bare Options) keeps today's behaviour. The slice is
// copied so a caller's backing array can't be mutated through the
// Server, and the result is never nil — a deployment that runs NO
// background service still reports (and rolls up) its own "api" entry.
// The names are lower-cased as well as trimmed, matching the transform
// config validation already applies when it checks them against
// {indexer, aggregator}.
//
// Both halves must apply the SAME transform or the config value passes
// validation and then matches nothing. That is what happened until
// 2026-08-31 (wave-D RD-05): validation lower-cased before its
// allow-list check while this function only trimmed, and the heartbeat
// map is keyed by Prometheus `job` labels stripped of the
// `stellarindex-` prefix — always lower-case. So
// `status_services = ["Indexer"]` booted clean, then reported
// `"status": "unknown"` on every /v1/status request forever, pinning
// `overall` at degraded and the explorer's status page at amber. The
// operator debugging it finds a value that passed validation and
// matches the documented vocabulary — the exact symptom #328 added
// this list to remove.
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

// durationOr returns d when it is positive, else fallback. Used to back
// an unset (zero-value) Options duration with a safe default.
func durationOr(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// globalPriceOptsWithDefaults backs `Options.GlobalPriceOpts` with
// [aggregate.DefaultGlobalPriceOptions] for any zero field so
// callers can supply just the aggregator source list and get
// sensible defaults for everything else.
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
	return o
}

// Handler returns the mux wrapped in the standard middleware stack
// (outermost-first): RequestID → HTTPMetrics → Logger → Recoverer
// → SecurityHeaders → [optional CORS] → [optional RateLimit].
//
// HTTPMetrics sits inside RequestID so future trace-exemplar links
// work, and outside Logger+Recoverer so metrics count every
// request including those where the handler panicked.
//
// SecurityHeaders runs INSIDE Recoverer so a panic's 500
// problem+json response still carries the nosniff header — the
// recoverer synthesises a response header, and SecurityHeaders
// hasn't written yet at that point because the inner handler is
// what panics, not the middleware around it.
//
// CORS runs outside RateLimit so preflight OPTIONS requests don't
// consume rate-limit budget. RateLimit runs innermost — AFTER
// Logger populates remote_ip into the context, so
// middleware.RemoteIPFrom returns a meaningful key.
func (s *Server) Handler() http.Handler {
	stack := []middleware.Middleware{
		middleware.RequestID,
		obs.HTTPMetrics,
		middleware.Logger(s.logger),
		middleware.Recoverer(s.logger),
		// Security headers live inside Recoverer so even a panic's
		// 500 problem+json response carries nosniff. Cheap, always
		// safe, idempotent with any edge-proxy that also sets it.
		middleware.SecurityHeaders,
		// Cache-Control directives per route — set BEFORE handlers
		// run so writeJSON / writeProblem responses inherit the
		// directive. Handlers may override (Etag flows, immutable
		// historical buckets) by setting Cache-Control themselves.
		// CDN-tier `s-maxage` is gated on s.cdnEnabled so deployments
		// without a CDN don't emit a directive a CDN they don't run
		// could later honour.
		middleware.CacheControlWithCDN(s.cdnEnabled),
		// Convert Go's default text/plain 404 / 405 from the mux into
		// problem+json so unknown paths and method mismatches use the
		// same wire shape as the rest of our error surface. Sits AFTER
		// CacheControl so the override gets the same Cache-Control
		// directive a regular handler-side response would.
		middleware.Envelope404,
	}
	if s.cors != nil {
		stack = append(stack, s.cors)
	}
	// 308-redirect trailing-slash paths to their no-slash form
	// (e.g. /v1/assets/native/ → /v1/assets/native). Every v1
	// route is registered without a trailing slash; without this
	// middleware, clients that auto-append (axios with `/v1/`
	// baseURL, OpenAPI codegens, mistyped curl) hit a dead 404.
	// 308 preserves method+body so POST/DELETE don't degrade.
	// MUST sit INSIDE CORS (site-audit S-009): when it ran outside,
	// the 308 carried no Access-Control-Allow-Origin, so a browser
	// fetch of a trailing-slash URL died at the redirect — exactly
	// as dead as the 404 this middleware exists to prevent.
	stack = append(stack, middleware.TrailingSlashRedirect)
	// RequestTimeout bounds every non-streaming request's context so
	// EVERY handler inherits a deadline even when it forgets to wrap its
	// own DB/ClickHouse read (C3-1/C3-2/P1, audit-2026-07-16).
	//
	// It sits OUTSIDE the whole credential/quota/limit block rather than
	// just above CaptureRoute (C3-102, audit-2026-07-23). In the inner
	// position, Auth / KeyPolicy / RequireEmailVerified / MonthlyQuota /
	// RateLimit / UsageTracker / SessionAuth all ran OUTSIDE the deadline
	// — their Redis and Postgres round-trips were bounded only by
	// go-redis's 3 s default and the http.Server WriteTimeout, which does
	// not cancel anything in flight. A slow store therefore held a request
	// goroutine well past the timeout this middleware exists to enforce,
	// and the comment claimed "EVERY handler inherits a deadline" while
	// the pre-handler stack did not.
	//
	// It stays INSIDE CORS/TrailingSlashRedirect (both allocation-free and
	// I/O-free) so a preflight still short-circuits without a timer. The
	// tighter per-handler 8s WithTimeout wrappers layer under it and fire
	// first. Post-RESPONSE bookkeeping in UsageTracker/TouchUsage
	// deliberately detaches from this deadline (context.WithoutCancel with
	// its own bound) so moving the timeout out cannot drop a usage row.
	// SSE endpoints are exempt inside the middleware. Skipped entirely
	// when requestTimeout <= 0 (the middleware also self-guards on that).
	if s.requestTimeout > 0 {
		stack = append(stack, middleware.RequestTimeout(s.requestTimeout))
	}
	// Auth runs INSIDE CORS (so preflight OPTIONS short-circuits
	// before any credential check) but OUTSIDE RateLimit (so
	// per-tier limits see the authenticated Subject in context).
	if s.auth != nil {
		stack = append(stack, s.auth)
	}
	// KeyPolicy runs after Auth (so the Subject is on context) but
	// before RateLimit (so a policy-denied 403 never spends a
	// rate-limit token). F-1226 (codex audit-2026-05-12).
	if s.keyPolicy != nil {
		stack = append(stack, s.keyPolicy)
	}
	// RequireEmailVerified runs after KeyPolicy (same "Subject
	// already resolved" precondition) and BEFORE rate-limit (so
	// an unverified-key 403 doesn't spend a per-minute token).
	// F-1218 wave 45 (codex audit-2026-05-12); opt-in per
	// deployment via the api binary's
	// cfg.API.SignupRequireEmailVerification flag.
	if s.requireEmailVerified != nil {
		stack = append(stack, s.requireEmailVerified)
	}
	// Usage tracker runs OUTSIDE both quota and rate-limit so it
	// observes BOTH kinds of 429 rejection and records them under the
	// per-endpoint `throttled` class. It used to sit INSIDE
	// MonthlyQuota, so a quota denial — which returns without calling
	// next — was counted nowhere at all, and a capped customer's usage
	// report showed zero traffic instead of a wall of throttling (cold
	// audit 2026-08-03; the comments here and in internal/usage
	// claimed both 429 classes stayed visible). The LEGACY per-day
	// total (the MonthlyQuota input) still excludes BOTH 429s and 5xx
	// — the middleware skips it by response status, see
	// middleware.billableClass (COR-05) — so a counted quota-429
	// cannot feed back into the quota it was denied by, and neither a
	// throttled request nor an outage on our side eats billing quota.
	// Best-effort; failures log at debug and never block.
	if s.usageTracker != nil {
		stack = append(stack, s.usageTracker)
	}
	// MonthlyQuota runs AFTER auth/key-policy (so the Subject is
	// on context) but BEFORE rate-limit (so a quota-rejected
	// request doesn't also spend a per-minute token). F-1226
	// (codex audit-2026-05-12).
	if s.monthlyQuota != nil {
		stack = append(stack, s.monthlyQuota)
	}
	if s.rateLimit != nil {
		stack = append(stack, s.rateLimit)
	}
	// TouchUsage runs INSIDE rate-limit (and after the usage
	// tracker for ordering symmetry) so a denied (429) request
	// doesn't bump the dashboard's "last seen" column for the
	// rejected attempt. Wraps next.ServeHTTP — the actual touch
	// fires post-handler with a SETNX debounce so per-request
	// cost is bounded. F-1226 (codex audit-2026-05-12) wave 39.
	if s.touchUsage != nil {
		stack = append(stack, s.touchUsage)
	}
	// Session resolver runs INSIDE rate-limit so the per-account
	// rate limit could observe the dashboard subject in the future
	// (today only key-tier limits look at Subject; once the cutover
	// makes Postgres canonical, dashboard sessions can carry tier
	// info too). Either way the cookie is parsed once per request
	// and the result stays attached for the rest of the chain.
	if s.sessionAuth != nil {
		stack = append(stack, s.sessionAuth)
	}
	// CaptureRoute MUST be innermost — directly above the mux — so
	// r.Pattern is populated before it reads. It writes the matched
	// route into the *routeCapture HTTPMetrics planted in the
	// context, so the outermost metrics middleware can label by
	// route even though Logger's r.WithContext between them shadows
	// the original request struct. See obs.HTTPMetrics docstring
	// for the why.
	stack = append(stack, obs.CaptureRoute)
	return middleware.Chain(s.mux, stack...)
}

// Uptime returns how long this server has been running. Exposed
// for debugging / testing.
func (s *Server) Uptime() time.Duration { return time.Since(s.started) }

// proxyForwardHeaders are the headers a reverse proxy adds when it
// forwards a request on behalf of a remote client. Their PRESENCE is the
// signal, not their value — a direct local scrape sets none of them.
var proxyForwardHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Real-Ip",
	"Forwarded",
}

// loopbackOnly wraps `next` so it returns 404 unless the request looks
// like a genuinely local, un-proxied scrape. Used for `/metrics` so the
// binary refuses to answer anything but the local Prometheus.
//
// Two conditions, both required:
//
//  1. RemoteAddr is a loopback IP (127.0.0.0/8 or ::1).
//  2. The request carries NO proxy-forwarding header.
//
// (2) is what makes this guard actually defend the case its own name
// implies (C3-029/C3-106, audit-2026-07-23). The documented topology is
// Caddy running ON THE SAME HOST proxying to 127.0.0.1:3000, so a
// misconfigured Caddy that forwards public traffic presents a LOOPBACK
// RemoteAddr and sails through a RemoteAddr-only check — the guard was
// inert against exactly the failure it was written for. Caddy's
// reverse_proxy sets X-Forwarded-For (and Host/Proto) by default, and so
// does every other mainstream proxy, so their presence distinguishes
// "someone's request relayed to us" from "the local scraper called us".
//
// This is defence in depth, not the control: the real control is the
// Caddyfile 404ing /metrics from public hosts
// (configs/caddy/Caddyfile.api). A proxy configured to STRIP forwarding
// headers would still pass — that is a deliberate limit, not an
// oversight, and it is why the Caddyfile rule is not optional.
//
// Returns 404 (not 403) deliberately — 403 would confirm the
// route exists; 404 mirrors what a properly-configured Caddy
// would emit and gives no signal to a scanner.
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

func (s *Server) mountRoutes() { //nolint:funlen // route registration is intentionally one block for grep-ability; splitting into sub-functions makes "where is /v1/X served?" harder to answer.
	// Health / meta endpoints. Deliberately NOT behind rate-limit
	// middleware — infra (k8s probes, load balancers) hits these.
	s.mux.HandleFunc("GET /v1/issuers", s.handleIssuersList)
	s.mux.HandleFunc("GET /v1/issuers/{g_strkey}", s.handleIssuer)

	// Per-contract SEP-41 transfer audit-trail. F-0021 closure
	// (audit-2026-05-26): every transfer / approve / set_admin /
	// set_authorized event for a watched SEP-41 contract, with
	// optional ?from= / ?to= address filters. Unlocks per-account
	// net-position queries — the Stellar moat feature CG/CMC
	// structurally cannot offer.
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/transfers", s.handleSEP41Transfers)

	s.mux.HandleFunc("GET /v1/changes/{entity_type}/{id}", s.handleChangeSummary)
	s.mux.HandleFunc("GET /v1/diagnostics/cursors", s.handleCursors)
	s.mux.HandleFunc("GET /v1/diagnostics/ingestion", s.handleDiagnosticsIngestion)
	// Latest archive-completeness report (ADR-0017) — read-through of
	// the JSON file the daily verify timer writes. Backs the explorer
	// /diagnostics archive panel. 503 when unconfigured, 404 pre-first-run.
	s.mux.HandleFunc("GET /v1/diagnostics/archive", s.handleDiagnosticsArchive)
	// Backup + DR-evidence freshness vs. SLO (pgBackRest full/diff/WAL,
	// offsite repo, restore drill, ClickHouse schema snapshot), read
	// from Prometheus. Backs the public status page's Backups panel.
	// 503 when no metrics backend is wired.
	s.mux.HandleFunc("GET /v1/diagnostics/backups", s.handleDiagnosticsBackups)
	s.mux.HandleFunc("GET /v1/coverage", s.handleCoverageVerdicts)

	// Protocols pillar (explorer-ux-plan §5): directory + per-protocol
	// detail. Static registry always serves; dynamic joins degrade.
	s.mux.HandleFunc("GET /v1/protocols", s.handleProtocolsList)
	s.mux.HandleFunc("GET /v1/protocols/{name}", s.handleProtocolDetail)

	// SDEX order-book depth — live classic offers from the in-process
	// book (503 problem until the initial lake load completes).
	s.mux.HandleFunc("GET /v1/sdex/orderbook", s.handleSDEXOrderbook)

	// Live-ingest frontier — a lightweight slice of the ingestion
	// snapshot (latest ingested ledger + lag). /tip is a 2s-cached
	// poll; /stream is the SSE counterpart that pushes one
	// ledger_update per new ledger so a status page renders blocks
	// arriving in real time.
	s.mux.HandleFunc("GET /v1/ledger/tip", s.handleLedgerTip)
	s.mux.HandleFunc("GET /v1/ledger/stream", s.handleLedgerStream)

	// Network explorer (ADR-0038) — read the certified ClickHouse lake.
	// Handler implementations live in internal/api/v1/explorer (D1 M1-7
	// extraction); this is still the sole place they're mounted.
	s.mux.HandleFunc("GET /v1/ledgers", s.explorerHandler.LedgersList)
	s.mux.HandleFunc("GET /v1/ledgers/{seq}", s.explorerHandler.LedgerDetail)
	s.mux.HandleFunc("GET /v1/ledgers/{seq}/transactions", s.explorerHandler.LedgerTransactions)
	s.mux.HandleFunc("GET /v1/operations", s.explorerHandler.Operations)
	s.mux.HandleFunc("GET /v1/tx/{hash}", s.explorerHandler.TxDetail)
	s.mux.HandleFunc("GET /v1/search", s.explorerHandler.Search)
	s.mux.HandleFunc("GET /v1/contracts", s.explorerHandler.ContractsList)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}", s.explorerHandler.ContractDetail)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/wasm", s.explorerHandler.ContractWasm)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/interactions", s.explorerHandler.ContractInteractions)
	s.mux.HandleFunc("GET /v1/contracts/{contract_id}/code-history", s.explorerHandler.ContractCodeHistory)
	s.mux.HandleFunc("GET /v1/accounts", s.explorerHandler.AccountsList)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}", s.explorerHandler.AccountState)
	s.mux.HandleFunc("GET /v1/directory", s.explorerHandler.DirectoryLookup)
	s.mux.HandleFunc("GET /v1/accounts/stats", s.explorerHandler.AccountsStats)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/transactions", s.explorerHandler.AccountTransactions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/operations", s.explorerHandler.AccountOperations)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/movements", s.explorerHandler.AccountMovements)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/positions", s.explorerHandler.AccountPositions)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/trades", s.explorerHandler.AccountTrades)
	s.mux.HandleFunc("GET /v1/accounts/{g_strkey}/activity", s.explorerHandler.AccountActivity)

	s.mux.HandleFunc("GET /v1/incidents", s.handleIncidents)
	s.mux.HandleFunc("GET /v1/incidents.atom", s.handleIncidentsAtom)
	s.mux.HandleFunc("GET /v1/network/stats", s.handleNetworkStats)
	s.mux.HandleFunc("GET /v1/network/throughput", s.explorerHandler.NetworkThroughput)
	s.mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /v1/readyz", s.handleReadyz)
	s.mux.HandleFunc("GET /v1/livez/lake", s.handleLivezLake)
	s.mux.HandleFunc("GET /v1/version", s.handleVersion)
	s.mux.HandleFunc("GET /v1/status", s.handleStatus)
	// Public list of ACTIVE operator-posted status banners (incident
	// tooling, admin Phase 1.5). Anonymous-friendly; the status page
	// renders these alongside the Alertmanager-derived /v1/status
	// incidents block. Empty (`{"notices":[]}`) when unwired.
	s.mux.HandleFunc("GET /v1/status/notices", s.handleStatusNotices)

	// Prometheus scrape endpoint. Deliberately unversioned — it's
	// operator-facing, not part of the public API contract.
	//
	// Defense-in-depth: also gate at the Go layer on the request
	// being loopback AND un-proxied (see loopbackOnly — a
	// same-host proxy presents a loopback RemoteAddr, so the
	// forwarding-header check is the half that actually defends).
	// The intended posture is that Caddy 404s `/metrics` from
	// public hosts (configs/caddy/Caddyfile.api) and only the local
	// Prometheus scraper hits the binary directly via
	// 127.0.0.1:3000. This guard catches the case where the
	// Caddyfile config is stale OR the binary is exposed behind
	// a different proxy that hasn't been audited. /metrics on a
	// public host fingerprints the deployment (Go runtime stats,
	// per-source counters, build info) — the cost of a missed
	// public hit is non-trivial enough to justify two layers of
	// blocking.
	s.mux.Handle("GET /metrics", loopbackOnly(obs.Handler()))

	// Asset catalogue.
	s.mux.HandleFunc("GET /v1/assets", s.handleAssetList)
	// /v1/external/assets — non-Stellar assets (fiat + reference-only coins)
	// split off /v1/assets (LC-001). /v1/assets is Stellar-only.
	s.mux.HandleFunc("GET /v1/external/assets", s.handleExternalAssetList)
	s.mux.HandleFunc("GET /v1/external/assets/{slug}", s.handleExternalAssetGet)
	// /v1/assets/verified must register before /v1/assets/{asset_id}
	// — Go 1.22+ ServeMux picks the more-specific pattern, but
	// listing the static path first keeps the precedence obvious
	// to anyone reading the mount order.
	s.mux.HandleFunc("GET /v1/assets/verified", s.handleAssetsVerified)
	s.mux.HandleFunc("GET /v1/assets/{asset_id}", s.handleAssetGet)
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/metadata", s.handleAssetMetadata)
	// Live per-token supply from the decode-at-ingest supply_flows lake
	// (ADR-0034).
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/supply", s.handleAssetSupply)
	s.mux.HandleFunc("GET /v1/assets/{asset_id}/holders", s.explorerHandler.AssetHolders)

	// Current price — last-trade fallback today; VWAP path when
	// the aggregator ships.
	s.mux.HandleFunc("GET /v1/price", s.handlePrice)

	// Point-in-time closed bucket at-or-before ts (board #46) +
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

	// SEP-40 passthrough surface — same data as /v1/price, reshaped
	// to the single-quote SEP-40 contract that on-chain oracle
	// readers expect. Quote fixed at fiat:USD on /lastprice;
	// /x_last_price takes explicit base + quote.
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

	// Methodology — machine-readable summary of the active
	// aggregation policy (VWAP method, outlier filters,
	// stablecoin proxy, source classes, ADR refs). Mirrors what
	// the explorer's /methodology HTML page documents, in a form
	// transparency consumers can parse. R-023.
	s.mux.HandleFunc("GET /v1/methodology", s.handleMethodology)

	// SAC wrapper resolution — operator-config map of
	// Stellar-Asset-Contract C-strkey → "CODE-ISSUER" classic asset.
	// Used by the explorer to render Soroban DEX pools (Soroswap /
	// Phoenix / Aquarius / Comet) with readable asset symbols
	// instead of raw C-strkeys.
	s.mux.HandleFunc("GET /v1/sac-wrappers", s.handleSACWrappers)

	// Account self-service. /me and /usage require an authenticated
	// Subject; /keys (POST) additionally requires the AccountStore
	// to be wired (typically only when Redis is reachable). All
	// three return 401 for anonymous callers.
	s.mux.HandleFunc("GET /v1/account/me", s.handleAccountMe)
	s.mux.HandleFunc("GET /v1/account/usage", s.handleAccountUsage)
	s.mux.HandleFunc("GET /v1/account/keys", s.handleAccountKeysList)
	s.mux.HandleFunc("POST /v1/account/keys", s.handleAccountKeysCreate)
	s.mux.HandleFunc("DELETE /v1/account/keys/{keyID}", s.handleAccountKeysRevoke)
	// Operator surface: mint a key for ANOTHER identifier. Gated on
	// TierOperator inside the handler; audit-logged via Options.Audit.
	s.mux.HandleFunc("POST /v1/admin/keys", s.handleAdminKeysCreate)
	// Operator surface: revoke ANOTHER identifier's key — the leaked-key
	// kill switch (C3-010, audit-2026-07-23). Self-service revoke needs
	// the customer's own credential, so before this there was no
	// operator path to stop a compromised key. X-Reason + audit-logged
	// "key.revoke".
	s.mux.HandleFunc("DELETE /v1/admin/keys/{keyID}", s.handleAdminKeysRevoke)
	// Operator surface: per-account tier + status + rate-limit /
	// monthly-quota overrides (admin Phase 1.5). Same TierOperator gate as
	// /v1/admin/keys; PATCH additionally requires an X-Reason header and
	// audit-logs "account.override.set". The override takes effect on
	// the next Postgres API-key validator Lookup for that account; a
	// `status` change to suspended/closed is the account-level kill
	// switch that same Lookup already enforces.
	s.mux.HandleFunc("GET /v1/admin/accounts/{id}", s.handleAdminAccountGet)
	s.mux.HandleFunc("PATCH /v1/admin/accounts/{id}", s.handleAdminAccountOverrides)
	// Operator surface: customer-facing status banners (incident
	// tooling, admin Phase 1.5). Create/list/resolve gated on
	// TierOperator; create + resolve require X-Reason and audit-log.
	s.mux.HandleFunc("GET /v1/admin/status-notices", s.handleAdminStatusNoticesList)
	s.mux.HandleFunc("POST /v1/admin/status-notices", s.handleAdminStatusNoticeCreate)
	s.mux.HandleFunc("POST /v1/admin/status-notices/{id}/resolve", s.handleAdminStatusNoticeResolve)
	s.mux.HandleFunc("POST /v1/signup", s.handleSignup)
	// Open registration — the curl-first agent onboarding path:
	// creates a free-tier platform account + first API key in one
	// unauthenticated POST. Shares /v1/signup's per-IP throttle.
	s.mux.HandleFunc("POST /v1/register", s.handleRegister)
	// F-1218 (codex audit-2026-05-12): email-ownership-proof
	// flow. The signup handler issues a token (subsequent
	// wave) and emails it; this endpoint consumes the token
	// from the click-through link.
	s.mux.HandleFunc("GET /v1/signup/verify", s.handleSignupVerify)

	// Customer-dashboard magic-link auth — POST /v1/auth/login +
	// GET /v1/auth/callback + POST /v1/auth/logout. Mounted only
	// when main.go wired a non-nil DashboardAuth (gated on Postgres
	// reachable + cfg.API.Dashboard.BaseURL non-empty); otherwise
	// the routes don't exist and ServeMux returns the standard 404.
	if s.dashboardAuth != nil {
		s.dashboardAuth.Mount(s.mux)
	}

	// Dashboard key-management routes — gated internally on the
	// session cookie planted by DashboardAuth's middleware. Mount
	// only when main.go wired Postgres for the platform stores.
	if s.dashboardKeys != nil {
		s.dashboardKeys.Mount(s.mux)
	}
	// Dashboard webhook-management routes (F-1270). Same
	// session-cookie + Postgres-wiring gate as dashboardKeys above.
	if s.dashboardWebhooks != nil {
		s.dashboardWebhooks.Mount(s.mux)
	}
	// Dashboard price-alert-management routes (BACKLOG #60). Same
	// session-cookie + Postgres-wiring gate as dashboardKeys above.
	if s.dashboardPriceAlerts != nil {
		s.dashboardPriceAlerts.Mount(s.mux)
	}

	// SEP-10 Web Auth. Both endpoints are unauthenticated by design
	// — challenge bootstraps auth from a public Stellar G-strkey;
	// the JWT issued by /token is what authenticates subsequent
	// requests. The validator is wired only when the binary has
	// the server-signing seed + JWT secret configured.
	s.mux.HandleFunc("GET /v1/auth/sep10/challenge", s.handleSEP10Challenge)
	s.mux.HandleFunc("POST /v1/auth/sep10/token", s.handleSEP10Token)

	// Bare-root welcome. GET / lands accidental visitors on a
	// friendly envelope pointing at the docs. The `{$}` anchor means
	// this pattern matches ONLY the literal "/" — it does not catch
	// `/anything-else`, so ServeMux's 405 method-mismatch detection
	// for known paths stays intact. Unknown paths fall through to
	// envelope404Middleware (see Handler()) which converts Go's
	// default text/plain 404 / 405 responses into RFC 9457
	// problem+json.
	s.mux.HandleFunc("GET /{$}", s.handleRoot)

	// /robots.txt — disallow crawler indexing of the API hostname.
	// The endpoints are JSON, not user-facing HTML; crawlers
	// hitting them waste their budget on payloads that won't rank
	// for any meaningful search query. The companion explorer site
	// (stellarindex.io) and docs site (docs.stellarindex.io) are
	// where indexable content lives, with their own robots.txt
	// directives. Without this handler Cloudflare's auto-managed
	// robots.txt is served on GET but the API origin returns 404
	// on HEAD — flagging the inconsistency is what surfaced this
	// gap in the 2026-05-09 audit.
	s.mux.HandleFunc("GET /robots.txt", s.handleRobotsTxt)

	// /.well-known/security.txt — RFC 9116 disclosure metadata.
	// Researchers scanning the API origin for vulnerabilities find
	// the disclosure email here without having to traverse to the
	// explorer subdomain. The Canonical: directive points at the
	// explorer's copy so the two stay aligned without drift.
	s.mux.HandleFunc("GET /.well-known/security.txt", s.handleSecurityTxt)

	// /errors/{slug} — dereferenceable RFC 9457 (7807) problem `type`
	// URIs. Every problem+json response we emit carries
	// type="https://api.stellarindex.io/errors/<slug>", and RFC 9457 says
	// that URI SHOULD resolve to documentation of the error. Site-audit S6:
	// all ~179 of them 404'd — i.e. we published dead documentation links at
	// exactly the moment an integrator is debugging a failure. This serves a
	// self-describing page (the slug, humanised, plus a pointer at the full
	// reference) so the type is dereferenceable without maintaining 179
	// hand-written docs.
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
	// StatusRoot points consumers at /v1/status for the rich
	// rollup that covers ingest lag, supply, oracle freshness,
	// and per-pair SLA latency — F-1210 (codex audit-2026-05-12).
	// Static "/v1/status" today; surfaced here so a probe
	// consumer following only /healthz / /readyz can still find
	// the SLA-truth endpoint without out-of-band knowledge.
	StatusRoot string `json:"status_root,omitempty"`
}

type checkResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error is populated only when OK is false; freeform string.
	Error string `json:"error,omitempty"`
}

// handleHealthz is the shallow liveness probe. Returns 200 as long
// as the process is running + mux is serving. Does NOT touch the
// database or Redis — those are the readiness probe's job.
//
// F-1210 (codex audit-2026-05-12): /healthz and /readyz are
// deliberately scoped to the serving-plane (process, postgres,
// redis). They do NOT report ingest lag, supply state, oracle
// freshness, or per-pair SLA latency. The rich rollup lives at
// `/v1/status`, which aggregates Prometheus-backed signals. The
// scoping is intentional: liveness probes (k8s, systemd) must
// not flap when a backfill stalls or when one source goes silent;
// those are SLO concerns surfaced separately. The healthz response
// links to /v1/status so operators using either endpoint find the
// authoritative view.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, healthResponse{
		Status:     "ok",
		Uptime:     s.Uptime().Truncate(time.Second).String(),
		StatusRoot: "/v1/status",
	}, Flags{})
}

// handleReadyz is the deep readiness probe. Pings every registered
// ReadyChecker in parallel with a short shared timeout. 200 only if
// all pass; 503 otherwise.
//
// Parallelism matters: with 3 checks at 500ms each, serial execution
// uses 1.5s of the 2s budget; parallel uses the max of any single
// check. The k8s liveness-probe timeout is typically 1s — blowing
// past it flaps the pod.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Single-flight + 1s result cache (inventory #26, audit 2026-08-03:
	// /v1/readyz is unauthenticated and unlimited — LB probes must never
	// be throttled — but every call fanned Pings across all checkers,
	// each holding DB pool slots up to 2s, so unauthenticated spam could
	// exhaust the shared pool, r1-confirmed). Concurrent callers now
	// share ONE check round per second: the first computes while holding
	// the lock, the rest queue briefly and serve the fresh cache. A
	// readiness answer up to 1s old is at least as truthful as a
	// point-in-time probe.
	s.readyzMu.Lock()
	if time.Since(s.readyzAt) < time.Second && s.readyzBody != nil {
		code, body := s.readyzCode, s.readyzBody
		s.readyzMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
		return
	}
	code, body := s.computeReadyz() //nolint:contextcheck // deliberately detached: the round is SHARED by every queued caller (single-flight), so one caller's cancellation must not abort it — see computeReadyz's doc.
	s.readyzCode, s.readyzBody, s.readyzAt = code, body, time.Now()
	s.readyzMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// computeReadyz runs one full check round and renders the response.
// Detached from any caller's request context — one impatient caller's
// disconnect must not cancel the round every queued caller shares.
func (s *Server) computeReadyz() (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	results := make([]checkResult, len(s.checks))
	criticalFlags := make([]bool, len(s.checks))
	var wg sync.WaitGroup
	for i, c := range s.checks {
		wg.Add(1)
		criticalFlags[i] = c.Critical()
		go func(i int, c ReadyChecker) {
			defer wg.Done()
			err := c.Ping(ctx)
			r := checkResult{Name: c.Name(), OK: err == nil}
			if err != nil {
				r.Error = err.Error()
			}
			results[i] = r // distinct indices — no mutex needed
		}(i, c)
	}
	wg.Wait()

	// F-1275 (codex audit-2026-05-13): split fail-cases into
	// critical (503) vs non-critical (200 with status="degraded").
	// Pre-wave-110 a Redis outage would 503 readyz and HAProxy
	// would drain every healthy API backend even though Timescale
	// fallback kept the customer-facing surface serving correctly.
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
		env := Envelope{Data: resp, AsOf: time.Now().UTC(), Flags: flags}
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
		// Non-critical dependency degraded — API still serves
		// (Timescale fallback for Redis cache misses per
		// ADR-0007); 200 keeps the backend in HAProxy's pool;
		// the response body's status="degraded" + per-check
		// breakdown tells operators what's down.
		resp.Status = "degraded"
		return render(http.StatusOK, Flags{Stale: true})
	}
	return render(http.StatusOK, Flags{})
}

// livezLakeTTL bounds how long one lake-ping round is reused. Same
// budget as readyz's single-flight cache: at LB probe cadence (~10s)
// every probe still gets a fresh round, while a burst of anonymous
// probes costs at most one ClickHouse query per second.
const livezLakeTTL = time.Second

// livezLakeUnreadyDetail is the FIXED hint served when the lake ping
// fails. Never the driver error: /v1/livez/lake is unauthenticated and
// exempt from the anonymous rate limiter, so whatever it echoes is
// public — and err.Error() carries the ClickHouse endpoint. The real
// error goes to the server log (once per cache round).
const livezLakeUnreadyDetail = "clickhouse ping failed — see the API server log for the underlying error"

// lakeHealth is the /v1/livez/lake payload (data member of the envelope).
type lakeHealth struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// handleLivezLake is the LAKE-critical health probe (multi-region plan
// §7.3 / ADR-0050). /v1/readyz deliberately treats ClickHouse as
// NON-critical so a lake outage degrades rather than un-readies the
// pricing surface — but that same 200 keeps a lake-dead region in a load
// balancer's pool for the ~21 lake-backed routes it can no longer serve
// (the 2026-08-20 multi-region audit's worst explorer-failover gap).
// This endpoint is the complement: 200 iff the registered ClickHouse
// checker pings; 503 when it fails OR when no lake is wired at all — a
// lake-less deployment must never receive lake-route traffic, so absent
// fails closed. Point lake-route LB monitors here; leave pricing
// monitors on /v1/readyz.
//
// Single-flight + 1s result cache (#310, audit 2026-08-29). #266 gave
// this route readyz's infra exemptions — no auth, no anonymous rate
// limit, correct for LB probes — but readyz's safety under those
// exemptions comes from its single-flight cache, which this route
// lacked: EVERY anonymous request ran a fresh `LakeTipLedger` query
// against ClickHouse under a 5s timeout, so unmetered concurrent probes
// were an amplifier pointed at the lake, worst exactly when the lake was
// already struggling. Concurrent callers now share ONE ping round per
// second, exactly like handleReadyz; a liveness answer up to 1s old is
// at least as truthful as a point-in-time probe.
func (s *Server) handleLivezLake(w http.ResponseWriter, _ *http.Request) {
	s.livezLakeMu.Lock()
	if time.Since(s.livezLakeAt) < livezLakeTTL && s.livezLakeBody != nil {
		code, body := s.livezLakeCode, s.livezLakeBody
		s.livezLakeMu.Unlock()
		writeLivezLake(w, code, body)
		return
	}
	code, body := s.computeLivezLake() //nolint:contextcheck // deliberately detached: the ping round is SHARED by every queued caller (single-flight), so one caller's cancellation must not abort it — see computeLivezLake's doc.
	s.livezLakeCode, s.livezLakeBody, s.livezLakeAt = code, body, time.Now()
	s.livezLakeMu.Unlock()
	writeLivezLake(w, code, body)
}

// writeLivezLake emits an already-rendered probe result. no-store keeps
// intermediaries from serving a cached liveness verdict — the 1s reuse
// window is ours to control, not a CDN's.
func writeLivezLake(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// computeLivezLake runs one lake-ping round and renders the response.
// Detached from any caller's request context — one impatient prober's
// disconnect must not cancel the round every queued caller shares (same
// rationale as computeReadyz).
func (s *Server) computeLivezLake() (int, []byte) {
	render := func(status int, body lakeHealth) (int, []byte) {
		env := Envelope{Data: body, AsOf: time.Now().UTC()}
		b, err := json.Marshal(env)
		if err != nil {
			return http.StatusInternalServerError, []byte(`{"error":"livez render"}`)
		}
		return status, b
	}
	for _, c := range s.checks {
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

// handleVersion reports binary version + build date + VCS info.
//
// Operators use this for quick fleet-wide "what's running" checks
// over the API rather than ssh-ing into every host. `version` is
// the human-readable git-describe; `commit` is the full VCS SHA;
// `dirty` reports whether the build tree had uncommitted changes
// (production builds should always be `dirty=false`); `go_version`
// is the runtime Go version.
func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		"version":    version.Version,
		"build_date": version.BuildDate,
		"commit":     version.Commit,
		"dirty":      version.Dirty,
		"go_version": version.GoVersion,
	}, Flags{})
}

// handleSecurityTxt serves /.well-known/security.txt per RFC 9116.
//
// The Canonical: URL points at the explorer copy
// (stellarindex.io/.well-known/security.txt) so the two origins
// don't drift; both the explorer and API surfaces deliberately
// share the same disclosure email + policy URL. Expires is one
// year out — handler runs at request time so it always returns a
// valid future date as long as the binary is up.
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

// handleRoot welcomes accidental visitors at GET /. Returns a small
// envelope with the binary version + a pointer at the docs; not part
// of the public API surface (no OpenAPI entry), strictly a "you've
// reached the API hostname" affordance.
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{
		"name":    "stellar-index",
		"version": version.Version,
		"docs":    "https://docs.stellarindex.io",
		"openapi": "https://docs.stellarindex.io/openapi.yaml",
	}, Flags{})
}

// handleErrorDoc serves a dereferenceable page for one RFC 9457 problem
// `type` slug (site-audit S6). The slugs are self-descriptive
// (account-not-found, rate-limited, invalid-max-age), so a page that echoes
// the slug humanised plus a link to the full reference is genuinely useful
// to an integrator who followed the `type` URI from an error body — far
// better than the 404 they previously hit. Content-negotiated: JSON for API
// clients, a minimal HTML page for a browser.
func (s *Server) handleErrorDoc(w http.ResponseWriter, r *http.Request) {
	// The slug is URL-path input; constrain it to the closed kebab-case
	// charset every real error type uses. Anything else can't be one of
	// ours, so we don't reflect it — this also makes the value provably
	// free of HTML metacharacters before it reaches any renderer.
	slug := r.PathValue("slug")
	if !errorSlugRE.MatchString(slug) {
		slug = "unknown"
	}
	human := humaniseErrorSlug(slug)
	typeURI := "https://api.stellarindex.io/errors/" + slug
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

// errorDocTmpl is the minimal browser page for an error type. html/template
// auto-escapes {{.human}} / {{.slug}}, so the reflected path value is safe
// by construction (and gosec trusts it, unlike a hand-rolled escaper).
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

// handleRobotsTxt serves /robots.txt. The API origin holds JSON
// endpoints not meant for crawler indexing — point search engines
// at the companion docs + explorer subdomains instead. The
// `Sitemap:` directive lets a crawler that ignored the Disallow
// (or has a per-bot exception) at least crawl what's worth
// indexing.
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

// backupMetricsFor resolves the /v1/diagnostics/backups metrics seam:
// an explicit Options.BackupMetrics wins; otherwise the status
// backend is reused when it can answer raw PromQL (the production
// *PrometheusStatusBackend). Any other StatusBackend (test fakes,
// nil) leaves the endpoint unconfigured → 503.
func backupMetricsFor(opts Options) backupMetricsSource {
	if opts.BackupMetrics != nil {
		return opts.BackupMetrics
	}
	if src, ok := opts.StatusBackend.(backupMetricsSource); ok && src != nil {
		return src
	}
	return nil
}
