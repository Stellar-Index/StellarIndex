// Binary stellarindex-api is the public REST + SSE API server.
//
// The route surface lives in internal/api/v1/server.go and the OpenAPI spec. Flags: -config PATH
// (required) and -dry-run (load config, open connections, validate, exit). SIGINT/SIGTERM cancel
// the root context and the HTTP server drains for up to 30 s; SSE connections are signalled
// separately in run() because they never go idle and would hold the drain open for its full budget.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/redact"
	workerpkg "github.com/Stellar-Index/StellarIndex/internal/worker"

	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/mev"
	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardkeys"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardpricealerts"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardwebhooks"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/holds"
	"github.com/Stellar-Index/StellarIndex/internal/logincodereaper"
	"github.com/Stellar-Index/StellarIndex/internal/magiclinkreaper"
	"github.com/Stellar-Index/StellarIndex/internal/metadata"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/signupreaper"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/forex"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/redisclient"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
	"github.com/Stellar-Index/StellarIndex/internal/version"
)

// recoverBackgroundWorker logs a panic in a detached worker instead of killing the process; the worker is not restarted (a stale cache
// beats a crash loop). Not for the http.Server goroutine, whose death should end the process.
func recoverBackgroundWorker(logger *slog.Logger, worker string) {
	// recover() only works one frame deep, so this cannot delegate to worker.Recover.
	if r := recover(); r != nil {
		workerpkg.Report(logger, worker, r)
	}
}

func main() {
	var (
		cfgPath     = flag.String("config", "", "Path to TOML config file (required)")
		dryRun      = flag.Bool("dry-run", false, "Load config + open connections + exit without serving")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	if *cfgPath == "" {
		fmt.Fprintln(os.Stderr, "stellarindex-api: -config is required")
		flag.Usage()
		os.Exit(2)
	}

	if err := run(*cfgPath, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "stellarindex-api: %s\n", redact.Credentials(err.Error()))
		os.Exit(1)
	}
}

func run(cfgPath string, dryRun bool) error { //nolint:gocognit,funlen,gocyclo // dispatch-heavy wiring; splitting would reduce linearity
	cfg, err := config.LoadWithEnv(cfgPath)
	if err != nil {
		return err
	}

	// Install the alias registry before any read path runs so classic and SAC forms fold
	// together, SAC last so a thin Soroban pool never outranks classic depth. Fail closed: a
	// malformed wrapper would silently under-count volume.
	aliasRegistry, err := canonical.NewAliasRegistry(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	canonical.InstallAliasRegistry(aliasRegistry)

	// The movements boundary is network-specific, so /movements reads must not use the pubnet const.
	timescale.InstallMovementsFloor(cfg.Stellar.MovementsFloorLedger)

	// SAC-address derivation must use the configured network, or a test-net asset would serve a
	// pubnet contract address that wallets send funds to.
	canonical.InstallNetworkPassphrase(cfg.Stellar.Passphrase())

	logger := mkLogger(cfg.Obs)
	logger.Info(
		"starting",
		"version", version.String(),
		"region", cfg.Region.ID,
		"listen", cfg.API.ListenAddr,
		"external_url", cfg.API.ExternalBaseURL,
		"auth_mode", cfg.API.AuthMode,
		"dry_run", dryRun,
	)

	// Risky-config warnings do not block startup; see docs/operations/pre-launch-hardening.md.
	warnUnsafeBind(logger, cfg.API.ListenAddr, cfg.API.TrustedProxyCIDRs)
	warnOpenCORS(logger, cfg.API.AllowedOrigins, cfg.API.AuthMode)
	warnCollapsedStreamCap(logger, cfg.API.ListenAddr, cfg.API.Streaming.MaxStreamsPerIP, cfg.API.TrustedProxyCIDRs)
	warnCollapsedAnonThrottle(logger, cfg.API.AuthMode, cfg.API.AnonRateLimitPerMin, cfg.API.TrustedProxyCIDRs)

	// The SEP-10 validator is built below, after rdb exists, because its replay guard is Redis-backed.

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// Storage is required. OpenServing sets a session statement_timeout on every serving
	// connection as a SQL-side backstop; it stays longer than api.request_timeout so the
	// app-layer deadline fires first.
	store, err := timescale.OpenServing(rootCtx, cfg.Storage.PostgresDSN, cfg.API.ServingStatementTimeout)
	if err != nil {
		cancel() // nothing else registered yet; release the signal ctx
		return fmt.Errorf("storage: %w", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Warn("storage close", "err", err)
		}
	}()
	logger.Info("storage connected")
	// Pool wait time is the signal that one slow route class is starving the latency-SLO routes.
	obs.Registry.MustRegister(collectors.NewDBStatsCollector(store.DB(), "serving"))

	// Redis is optional here and startup does not block on it; the readiness probe reports its
	// state. Production uses Sentinel mode (ADR-0024). Both client constructors are lazy, so
	// -dry-run pings explicitly or a bad address or password would never surface.
	rdb := redisclient.Build(cfg.Storage)
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
		mode := redisclient.Mode(cfg.Storage)
		if dryRun {
			pingCtx, cancelPing := context.WithTimeout(rootCtx, 5*time.Second)
			err := rdb.Ping(pingCtx).Err()
			cancelPing()
			if err != nil {
				return fmt.Errorf("redis: ping (%s mode): %w", mode, err)
			}
		} else {
			// A rolled-back binary writes key records without indexing them;
			// dropping `ready` makes the first lookup rebuild from the records.
			invCtx, cancelInv := context.WithTimeout(rootCtx, 5*time.Second)
			if err := auth.NewRedisAPIKeyStore(rdb).InvalidateKeyIndex(invCtx); err != nil {
				logger.Warn("api-key index not invalidated at startup; lookups trust the existing index", "err", err)
			}
			cancelInv()
		}
		logger.Info("redis configured", "mode", mode)
	}
	// Without Redis the auth throttles and the passkey replay guard are per-process. That is
	// unsafe across replicas: a replayed finish-login on another instance would mint a session
	// (account takeover). Require the operator to assert single-instance, else refuse to start.
	if err := assertRedisOrSingleInstance(rdb != nil, cfg.API.SingleInstance); err != nil {
		return err
	}
	// Registered after the store and redis defers so LIFO runs cancel first: background workers
	// must unwind before the handles they query are closed.
	defer cancel()

	// SEP-10 is wired regardless of auth_mode and must be replay-protected, so it is built after
	// rdb exists. resolveSEP10Validator holds the fail-closed policy. The explorer reader behind
	// the signer lookup is bound later, once ClickHouse is dialled.
	sep10Accounts := &lakeAccountSigners{}
	var sep10AccountLoader sep10.AccountLoader
	if cfg.Storage.ClickHouseAddr != "" {
		sep10AccountLoader = sep10Accounts
	}
	sep10Validator, err := resolveSEP10Validator(cfg.API.SEP10, cfg.API.AuthMode, cfg.Stellar.Passphrase(), rdb, sep10AccountLoader, logger)
	if err != nil {
		return err
	}

	// Build readiness-check set. Each implements v1.ReadyChecker.
	checks := []v1.ReadyChecker{
		wiring.StoreChecker{S: store},
		// Critical: a schema older than this binary expects drains the backend (503) rather than
		// serving stale data behind a 200.
		v1.NewSchemaVersionChecker(wiring.SchemaChecker{DB: store.DB()}),
		// Non-critical: a dirty row shows as degraded without draining the fleet.
		v1.NewSchemaDirtyChecker(wiring.SchemaChecker{DB: store.DB()}),
		// ADR-0015: unguarded CAGG readers depend on materialized_only;
		// drain if an out-of-band ALTER makes a view serve its open bucket.
		v1.NewClosedBucketChecker(store),
	}
	if rdb != nil {
		checks = append(checks, wiring.RedisChecker{RDB: rdb})
	}

	// SEP-1 payloads come from the stored issuers.sep1_payload, never a live fetch per request.

	// An empty trusted-proxy list means X-Forwarded-For is ignored entirely. Config validation
	// already parsed the CIDRs, so an error here is a programmer bug and fails startup.
	if err := middleware.SetTrustedProxyCIDRs(cfg.API.TrustedProxyCIDRs); err != nil {
		return fmt.Errorf("middleware.SetTrustedProxyCIDRs: %w", err)
	}
	if len(cfg.API.TrustedProxyCIDRs) > 0 {
		logger.Info("trusted-proxy CIDRs wired",
			"count", len(cfg.API.TrustedProxyCIDRs),
			"cidrs", cfg.API.TrustedProxyCIDRs)
	}

	// CORS is wired only when origins are configured; empty means same-origin only.
	var cors middleware.Middleware
	if len(cfg.API.AllowedOrigins) > 0 {
		cors = middleware.CORS(middleware.CORSOptions{
			AllowedOrigins:      cfg.API.AllowedOrigins,
			AllowCredentials:    cfg.API.AllowCredentials,
			CredentialedOrigins: cfg.API.CredentialedOrigins,
			// The default omits DELETE and PATCH, which the v1 mux serves; without them the
			// browser blocks those cross-origin requests at preflight.
			AllowedMethods: []string{"GET", "HEAD", "OPTIONS", "POST", "PATCH", "DELETE"},
		})
	}

	// Separate buckets per tier: anon is keyed by remote IP, auth by API key or subject. Without
	// Redis, ratelimit.New falls back to an in-process limiter so tiers stay enforced on a single
	// instance. A limit of 0 is accepted but leaves the tier unbounded, so warn loudly at boot.
	if cfg.API.AnonRateLimitPerMin == 0 {
		logger.Warn("anonymous rate limit is DISABLED (api.anon_rate_limit_per_min=0) — anonymous requests are UNBOUNDED",
			"auth_mode", cfg.API.AuthMode)
	}
	if cfg.API.KeyRateLimitPerMin == 0 {
		logger.Warn("per-key rate limit is DISABLED (api.key_rate_limit_per_min=0) — authenticated requests are UNBOUNDED",
			"auth_mode", cfg.API.AuthMode)
	}
	var rateLimit middleware.Middleware
	if cfg.API.AnonRateLimitPerMin > 0 || cfg.API.KeyRateLimitPerMin > 0 {
		var anonBucket, authBucket *ratelimit.Bucket
		if cfg.API.AnonRateLimitPerMin > 0 {
			anonBucket = ratelimit.New(rdb, cfg.API.AnonRateLimitPerMin, time.Minute,
				ratelimit.WithDwellTime(cfg.API.RateLimitDwell))
		}
		if cfg.API.KeyRateLimitPerMin > 0 {
			authBucket = ratelimit.New(rdb, cfg.API.KeyRateLimitPerMin, time.Minute,
				ratelimit.WithDwellTime(cfg.API.RateLimitDwell))
		}
		rateLimit = middleware.RateLimitBySubject(
			anonBucket,
			authBucket,
			middleware.SkipHealthAndMetrics,
			logger.With("component", "ratelimit"),
		)
		backend := "redis"
		// In-process buckets are per-process, so several instances would multiply every limit;
		// that is why the fallback logs at Warn.
		logFn := logger.Info
		if rdb == nil {
			backend = "in-process (single-instance fallback — no Redis)"
			logFn = logger.Warn
		}
		logFn(
			"rate-limit tiers wired",
			"anon_per_min", cfg.API.AnonRateLimitPerMin,
			"key_per_min", cfg.API.KeyRateLimitPerMin,
			"anon_enabled", anonBucket != nil,
			"key_enabled", authBucket != nil,
			"backend", backend,
		)
	}

	// Auth runs before the rate limiter so tiers key off the authenticated subject, which leaves
	// a request with a bad credential unthrottled. This bucket throttles only credential
	// failures, keyed on client IP and key prefix, inside the Auth middleware.
	var failedAuthLimiter *ratelimit.Bucket
	if cfg.API.FailedAuthRateLimitPerMin > 0 {
		failedAuthLimiter = ratelimit.New(rdb, cfg.API.FailedAuthRateLimitPerMin, time.Minute,
			ratelimit.WithDwellTime(cfg.API.RateLimitDwell))
		if rdb == nil {
			logger.Warn("failed-auth throttle is in-process (single-instance fallback — no Redis); "+
				"a multi-instance deployment multiplies the credential-stuffing budget by instance count",
				"failed_auth_per_min", cfg.API.FailedAuthRateLimitPerMin)
		}
	}

	// Per-account usage counter, built only with Redis; a nil counter disables it. Month-to-date
	// reconciles against usage_daily so an evicted Redis day key cannot read as a quiet day.
	var usageCounter *usage.Counter
	if rdb != nil {
		usageCounter = usage.New(rdb, usage.WithDurableDays(store))
	}

	// Built after the dashboard bundle so the Postgres backend can share its platform stores.
	var authMW middleware.Middleware

	// The /v1/account/keys store needs Redis; without it the route returns 503. Under
	// auth_backend=postgres it stays nil, because the validator reads platform.api_keys: a Redis
	// DELETE would leave the live Postgres key authenticating, a revocation that silently no-ops.
	// /v1/dashboard/keys is the single source of truth there.
	var accountStore v1.AccountStore
	switch {
	case rdb == nil:
		// no store
	case cfg.API.AuthBackend == "postgres":
		logger.Warn("auth_backend=postgres: /v1/account/keys self-service surface DISABLED to avoid a split-brain with the Postgres validator (revocation would silently no-op); use the Postgres-backed /v1/dashboard/keys instead",
			"reason", "AccountStore writer (redis) != APIKeyValidator reader (postgres)")
	default:
		accountStore = auth.NewRedisAPIKeyStore(rdb)
	}

	// Per-IP signup throttle, separate from the global limiter. The cap is a compiled default
	// (auth.SignupIPThrottleOptions), not a config key.
	var signupIPThrottle v1.SignupIPThrottle
	if rdb != nil {
		signupIPThrottle = auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{})
	} else {
		// A nil throttle would bound /v1/register only by the global anonymous limit, which allows
		// thousands of accounts per hour from one IP.
		signupIPThrottle = newInProcessSignupIPThrottle()
		logger.Warn("signup IP throttle is in-process (single-instance fallback — no Redis); " +
			"the per-IP signup cap is NOT shared across instances")
	}

	// The aggregator's divergence results live in Redis. The API only calls LookupCached and
	// builds no References; the aggregator is the sole RefreshPair caller.
	var divergenceLooker v1.DivergenceLooker
	if rdb != nil {
		divSvc, err := divergence.NewService(divergence.ServiceOptions{
			Cache:                rdb,
			Threshold:            cfg.Divergence.Threshold,
			MinSourcesForWarning: cfg.Divergence.MinSourcesForWarning,
		})
		if err != nil {
			return fmt.Errorf("divergence service: %w", err)
		}
		divergenceLooker = wiring.NewDivergenceAdapter(divSvc)
		logger.Info("divergence cache reader wired",
			"threshold_pct", cfg.Divergence.Threshold,
			"min_sources_for_warning", cfg.Divergence.MinSourcesForWarning)
	}

	// Home-domain lookups per ADR-0021; newHomeDomainLookups explains the split chain.
	homeDomainLookup := newHomeDomainLookups(
		metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: store}),
		cfg.Metadata.HomeDomainFor,
		func(msg string, kv ...any) {
			logger.With("component", "metadata-lcm").Warn(msg, kv...)
		},
	)

	// Reads the freeze markers the aggregator publishes (ADR-0019). Without Redis the looker is
	// nil and flags.frozen is always false.
	var freezeLooker v1.FrozenLooker
	if rdb != nil {
		fl, err := freeze.NewLooker(rdb)
		if err != nil {
			return fmt.Errorf("freeze looker: %w", err)
		}
		freezeLooker = fl
		logger.Info("freeze looker wired")
	}

	// Reads the MEV worker's run heartbeat; without Redis /v1/mev always flags detectors_partial.
	var mevPulse v1.MEVPulseLooker
	if rdb != nil {
		hb, err := mev.NewRedisHeartbeat(mev.HeartbeatOptions{Cache: rdb, Logger: logger})
		if err != nil {
			return fmt.Errorf("mev heartbeat: %w", err)
		}
		mevPulse = hb
	}

	// The hub is built unconditionally so /v1/price/stream never 503s; the per-pair publisher
	// starts only when [api.streaming].pairs is set, and the Redis subscriber binds below.
	hub := streaming.NewHub(streaming.DefaultBufferSize)
	hub.SetMaxTopics(cfg.API.Streaming.MaxTopics)

	// A global SSE cap alone lets one client hold the whole budget. The per-IP cap uses the
	// trusted-proxy-aware resolver so it tracks the real client behind the proxy; the CIDRs
	// were installed above.
	streaming.SetMaxStreamsPerIP(cfg.API.Streaming.MaxStreamsPerIP)
	streaming.SetStreamClientIPResolver(middleware.RemoteIP)
	streaming.SetMaxConcurrentStreams(cfg.API.Streaming.MaxConcurrentStreams)

	// One thin-market substance gate is shared by every raw prices_1m serving path, so all of
	// them agree on which pairs are too thin to price. The Redis-VWAP fallback in
	// internal/api/v1/price.go is the one surface not routed through it.
	substanceGate := buildSubstanceGate(cfg.PricingGuard, store, logger)
	// The scam gate withholds the price when either leg's issuer is flagged scam-class. It sits
	// at the reader seam; handlers that compute their own price (vwap, twap, chart, price/tip)
	// consult it themselves, see scam.go. Nil when the directory reader is absent.
	scamGate := pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{Logger: logger})
	priceReader := wiring.StorePriceReader{S: store, Logger: logger, Substance: substanceGate, Scam: scamGate}

	// The latest-oracle query is expensive; a Redis read-through cache inside the oracle push
	// interval absorbs polling fan-out. Without Redis it reads the store directly.
	var oracleReader v1.OracleReader = wiring.StoreOracleReader{S: store}
	if rdb != nil {
		oracleReader = wiring.CachedOracleReader{
			Inner: oracleReader,
			RDB:   rdb,
			Log:   logger.With("component", "oracle-cache"),
		}
		logger.Info("oracle reader wrapped with Redis cache",
			"ttl", cachekeys.OracleLatestTTL.String())
	}
	// The Redis layer has no single-flight, so concurrent misses stampede the DB, as does a Redis
	// outage. The in-process layer adds single-flight and survives Redis being down.
	const oracleInProcessTTL = 3 * time.Second
	oracleReader = v1.NewCachedOracleReader(oracleReader, oracleInProcessTTL)
	logger.Info("oracle reader wrapped with in-process cache",
		"ttl", oracleInProcessTTL.String())

	// The asset and market lists aggregate 14 days of trades, so a 60 s Redis cache absorbs
	// polling without delaying new listings by more than a minute.
	var assetReader v1.AssetReader = wiring.StoreAssetReader{S: store, ListingHomeDomains: homeDomainLookup.listing, DetailHomeDomainLookup: homeDomainLookup.detail}
	var marketsReader v1.MarketsReader = wiring.StoreMarketsReader{S: store}
	if rdb != nil {
		assetReader = wiring.CachedAssetReader{
			Inner: assetReader,
			RDB:   rdb,
			Log:   logger.With("component", "assets-cache"),
		}
		marketsReader = wiring.CachedMarketsReader{
			Inner: marketsReader,
			RDB:   rdb,
			Log:   logger.With("component", "markets-cache"),
		}
		logger.Info("catalogue readers wrapped with Redis cache",
			"ttl", cachekeys.CatalogueListTTL.String())
	}

	// Who this deployment says it is on /v1/status; see statusIdentity.
	statusRegion, statusDeployment, statusServices := statusIdentity(cfg)

	// An empty Prometheus URL leaves /v1/status with only the region label and uptime.
	var statusBackend v1.StatusBackend
	if cfg.API.PrometheusURL != "" {
		statusBackend = &v1.PrometheusStatusBackend{
			URL:    cfg.API.PrometheusURL,
			Client: &http.Client{Timeout: 2 * time.Second},
		}
		logger.Info("status backend wired", "prometheus_url", cfg.API.PrometheusURL)
	}

	// An empty dashboard BaseURL leaves the dashboard auth routes unmounted.
	dashboardBundle, err := buildDashboardBundle(cfg.API.Dashboard, store.DB(), rdb, logger)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}

	// The forex worker backs /v1/currencies and survives upstream failures, keeping the prior
	// snapshot. With an empty [external.massive] api_key every fetch gets 401 and the endpoint
	// serves "warming up".
	forexCache := forex.NewCache()
	// Hourly: boards update at most hourly, and a faster poll only spends metered quota.
	forexWorker := forex.NewWorker(
		forex.NewClient(cfg.External.Massive.APIKey),
		forexCache,
		logger.With("component", "forex"),
		time.Hour,
	)
	// Persisting to fx_quotes lets /v1/currencies chart history beyond the in-memory window.
	forexWorker = forexWorker.WithWriter(&wiring.ForexQuoteWriter{Store: store}).
		WithReader(&wiring.ForexQuoteWriter{Store: store}).
		WithFixingWriter(&wiring.ForexQuoteWriter{Store: store})
	// Massive is a paid feed and the only primary, so a lapse would break every fiat-quoted pair
	// once the lookback expired. ECB is a free standby with narrower coverage, used only when the
	// primary fails; the source label follows the feed that served.
	forexWorker = forexWorker.WithFallbacks(forex.ECBProvider{})
	// Held outside the serving chain: the worker stores it and never
	// fetches it, so enabling it spends no quota and changes no served rate.
	if oxr := cfg.External.OpenExchangeRates; oxr.Enabled {
		forexWorker = forexWorker.WithCorroborator(forex.OpenExchangeRatesProvider{AppID: oxr.AppID, Endpoint: oxr.Endpoint})
	}

	// Dry-run must exit before the first goroutine starts or any heavy first-pass query runs.
	if dryRun {
		logger.Info("dry-run complete — exiting")
		return nil
	}

	// bgWG lets shutdown wait for detached workers; otherwise the webhook sender could exit
	// between a customer accepting the POST and MarkDelivered, repeating the delivery on the next
	// boot. The wait shares the listener-drain deadline, so a wedged worker delays exit but cannot
	// block it. The cache warmers and the subscriber hold no durable write and do not join.
	var bgWG sync.WaitGroup

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "trade-walk-warm")
		if err := store.WarmTradeWalkPool(rootCtx); err != nil {
			logger.Warn("trade walk pool warm failed", "err", err)
		}
	}()

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "scam-sac-index-warm")
		scamGate.WarmSACIndex(rootCtx)
	}()

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "forex-worker")
		if err := forexWorker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("forex worker exited", "err", err)
		}
	}()

	// auth_mode=none yields a nil middleware and downstream treats a missing Subject as anonymous.
	// The Postgres backend needs the dashboard bundle, which owns the platform stores.
	authOpts := authValidatorOptions{
		Backend:           cfg.API.AuthBackend,
		Rdb:               rdb,
		PostgresValidator: dashboardBundle.pgValidator,
		SEP10:             sep10Validator,
		FailedAuthLimiter: failedAuthLimiter,
	}
	if dashboardBundle.accounts != nil {
		// The suspend kill switch must apply on the redis backend too, or a suspended account's
		// API keys keep serving.
		authOpts.AccountStatus = dashboardBundle.accounts
	}
	authMW = buildAuthMiddleware(cfg.API.AuthMode, authOpts, logger)

	// The caches are hoisted so the prewarm goroutine can warm them and keep cold misses off the
	// request path. Each TTL is at least twice the prewarm cadence, so a delayed refresh still
	// serves a warm entry.
	cachedSourcesStats := v1.NewCachedSourcesStatsReader(store, 10*time.Minute)
	cachedMarketsReader := v1.NewCachedMarketsReader(marketsReader, 2*time.Minute)
	cachedAssetsReader := v1.NewCachedAssetsReader(store, 2*time.Minute)
	// The issuers listing is a full aggregate no index can help, and the verified-issuer
	// catalogue changes slowly, so a long TTL is safe.
	cachedIssuersReader := v1.NewCachedIssuersReader(store, issuersCacheTTL)
	// The lending pool list aggregates both Blend tables in full on every call; auction counts and
	// 30-day flows barely move in a minute.
	cachedLendingReader := v1.NewCachedLendingReader(store, time.Minute)
	// Network stats is the slowest route; stale-while-revalidate with a 30 s TTL keeps it off the
	// request path, and trailing-24h figures barely move in 30 s.
	cachedNetworkStats := v1.NewCachedNetworkStatsReader(store, 30*time.Second)

	usdPegs := cfg.Trades.USDPeggedClassics(logger)
	fiatPegs := parseFiatPeggedClassics(cfg.PricingGuard.FiatPeggedClassicAssets, logger)

	// The catalogue is embedded, so a load failure is a broken build and fatal. It must load
	// before the prewarm goroutine, which uses it to prewarm canonical asset ids.
	verifiedCurrencies, err := currency.LoadEmbeddedForNetwork(cfg.Stellar.Network)
	if err != nil {
		return fmt.Errorf("load verified-currency catalogue: %w", err)
	}
	logger.Info("verified-currency catalogue loaded", "entries", len(verifiedCurrencies.All()))

	// Stellar canonical asset ids from the catalogue feed extra prewarm calls so canonical-form
	// lookups hit a warm cache. Native is prewarmed separately and empty ids have no issuance.
	var verifiedAssetIDs []string
	for _, vc := range verifiedCurrencies.All() {
		for _, ne := range vc.Issuance {
			if ne.Network != "stellar" {
				continue
			}
			if ne.AssetID == "" || ne.AssetID == "native" {
				continue
			}
			verifiedAssetIDs = append(verifiedAssetIDs, ne.AssetID)
		}
	}
	logger.Info("prewarm: verified canonical asset_ids extracted",
		"count", len(verifiedAssetIDs))

	// Seed the assets listing before the listener and prewarm start. A cold fill is slow and
	// the first request arrives almost immediately, so only an existing entry can answer it. It
	// also lets the prewarm take the stale-serve branch instead of blocking on cold aggregates.
	// Bounded and optional: an unreachable Redis seeds nothing.
	listingSnapshots := newAssetsListingSnapshots(rdb, logger.With("component", "assets-listing-snapshot"))
	seedCtx, seedCancel := context.WithTimeout(rootCtx, assetsListingSnapshotBudget)
	seeded, requested := seedAssetListingsFromSnapshots(seedCtx, logger, cachedAssetsReader, listingSnapshots)
	seedCancel()
	logger.Info("assets listing boot seed",
		"seeded", seeded, "requested", requested,
		"max_age", assetsListingSeedMaxAge.String())

	// StellarIssued(), not All(): the unified listing's catalogue phase serves only Stellar-issued
	// entries, and the classic remaining fill is computed against that count.
	go prewarmCaches(rootCtx, logger.With("component", "prewarm"), cachedSourcesStats, cachedMarketsReader, cachedAssetsReader, cachedIssuersReader, verifiedAssetIDs, listingSnapshots, cachedNetworkStats, len(verifiedCurrencies.StellarIssued()))

	// The TLS expiry probe catches a silent auto-renewal failure; its alert is in
	// deploy/monitoring/rules/api.yml.
	if len(cfg.API.TLSCertProbeHosts) > 0 {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "tls-cert-probe")
			if err := v1.RunTLSCertProbe(rootCtx, cfg.API.TLSCertProbeHosts, logger.With("component", "tls-cert-probe")); err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("tls cert probe exited", "err", err)
			}
		}()
		logger.Info("tls cert probe wired",
			"hosts", cfg.API.TLSCertProbeHosts,
			"cadence", v1.TLSCertProbeInterval.String())
	}

	// The coverage query is too slow for a request path, so it refreshes in the background and
	// the endpoint reports an empty section until the first refresh completes.
	backfillCoverageCache := v1.NewCoverageCache(store, logger.With("component", "backfill-coverage"))
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "backfill-coverage-cache")
		// The query takes tens of seconds on a large hypertable, and a shorter timeout left the
		// snapshot permanently pending. The timeout only bounds a refresh attempt and stays below
		// the refresh interval so refreshes never stack.
		const coverageRefreshTimeout = 2 * time.Minute

		// Populate before the first tick so the first status poll sees data.
		refreshWithTimeout(rootCtx, backfillCoverageCache.Refresh, coverageRefreshTimeout, logger,
			"backfill coverage initial refresh")
		runRefreshLoop(rootCtx, backfillCoverageCache.Refresh, v1.CoverageRefreshInterval, coverageRefreshTimeout, logger,
			"backfill coverage periodic refresh")
	}()

	// USD volume pricing axis on /v1/coverage: one ts-bounded 24h count per
	// refresh, never per request.
	usdVolumePricingCache := v1.NewUsdVolumePricingCache(store, logger.With("component", "usd-volume-pricing"))
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "usd-volume-pricing-cache")
		const usdVolumePricingRefreshTimeout = 5 * time.Minute
		refreshWithTimeout(rootCtx, usdVolumePricingCache.Refresh, usdVolumePricingRefreshTimeout, logger,
			"usd volume pricing initial refresh")
		runRefreshLoop(rootCtx, usdVolumePricingCache.Refresh, v1.UsdVolumePricingRefreshInterval,
			usdVolumePricingRefreshTimeout, logger, "usd volume pricing periodic refresh")
	}()

	// Mirrors nonstandard_decimals_assets in-process (docs/operations/runbooks/dex.md). Readiness
	// stays 503 until one load succeeds, since a cold cache serves the offender's raw price; failing
	// hard at boot would restart-loop systemd on a Postgres blip.
	nonstandardDecimalsCache := v1.NewNonstandardDecimalsCache(store, logger.With("component", "nonstandard-decimals-cache"))
	checks = append(checks, wiring.PrimeNonstandardDecimalsCache(rootCtx, nonstandardDecimalsCache, logger))
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "nonstandard-decimals-cache")
		runRefreshLoop(rootCtx, nonstandardDecimalsCache.Refresh, v1.NonstandardDecimalsRefreshInterval,
			wiring.NonstandardDecimalsRefreshTimeout, logger, "nonstandard-decimals cache periodic refresh")
	}()

	// Token supply reads the lake (ADR-0034) and is optional: without ClickHouse, or after a
	// failed dial, the reader stays nil and the endpoint 503s while the rest serves.
	// dialLakeReadersAtBoot dials both lake readers concurrently.
	var (
		lakeSupply      *clickhouse.SupplyReader
		lakeSupplyErr   error
		lakeExplorer    *clickhouse.ExplorerReader
		lakeExplorerErr error
	)
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		user, pass := cfg.Storage.ClickHouseServingUser, cfg.Storage.ClickHouseServingPassword
		lakeSupply, lakeSupplyErr, lakeExplorer, lakeExplorerErr = dialLakeReadersAtBoot(rootCtx, logger, addr, time.Now().Add(clickhouseBootDialBudget),
			func(ctx context.Context) (*clickhouse.SupplyReader, error) {
				return clickhouse.NewSupplyReaderAuth(ctx, addr, user, pass)
			},
			func(ctx context.Context) (*clickhouse.ExplorerReader, error) {
				return clickhouse.NewExplorerReaderAuth(ctx, addr, user, pass)
			})
	}
	var tokenSupplyReader v1.TokenSupplyReader
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		sr, err := lakeSupply, lakeSupplyErr
		if err != nil {
			logger.Warn("token supply reader unavailable; /v1/assets/{id}/supply will 503", "addr", addr, "err", err)
		} else {
			defer func() { _ = sr.Close() }()
			tokenSupplyReader = v1.NewTokenSupplyCache(sr)
			logger.Info("token supply reader wired (ClickHouse supply_flows)", "addr", addr)
		}
	}

	// The explorer reader (ADR-0038) serves the ledger, tx, operation, contract and search routes
	// from the certified lake and is optional. The vars below are the same concrete lake reader
	// through narrower seams; each stays nil without the lake and its feature then degrades
	// (503 or absent), never a zero.
	var explorerReader v1.ExplorerReader
	var protocolActivityReader v1.ProtocolActivityReader
	// The watermark reader stamps responses with as_of_ledger and flags.stale (ADR-0041).
	var lakeWatermarkReader v1.LakeWatermarkReader
	var tokenDecimalsReader v1.TokenDecimalsReader
	var issuerAuthFlagsReader v1.IssuerAuthFlagsReader
	// Sums supply from contract storage for tokens the SEP-41 event log cannot see.
	var storageSupplyReader v1.ContractStorageSupplyReader
	// Read only by the RWA contract arm, and only after the curated directory names the contract.
	var tokenSymbolReader v1.TokenSymbolReader
	var soroswapTVLReserves v1.SoroswapTVLReserveReader
	var phoenixTVLReserves v1.PhoenixTVLReserveReader
	var cometTVLReserves v1.CometTVLReserveReader
	var sdexOfferBook v1.SDEXOfferBookReader
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		er, err := lakeExplorer, lakeExplorerErr
		// The ready check registers for a configured ClickHouse even when this dial failed.
		checks = append(checks, wiring.ClickhouseReadyChecks(addr, er, err, clickhouseBootDialBudget)...)
		if err != nil {
			logger.Warn("explorer reader unavailable; /v1/ledgers etc. will 503", "addr", addr, "err", err)
		} else {
			defer func() { _ = er.Close() }()
			// A failing refresh keeps /v1/accounts on its 503 warming state, so log it.
			er.SetWealthRefreshErrorHandler(func(err error) {
				logger.Warn("accounts wealth refresh failed; /v1/accounts stays on warming state", "err", err)
			})
			explorerReader = er
			sep10Accounts.bind(er)
			protocolActivityReader = er
			lakeWatermarkReader = er
			tokenDecimalsReader = er
			issuerAuthFlagsReader = er
			storageSupplyReader = er
			tokenSymbolReader = er
			soroswapTVLReserves = er
			phoenixTVLReserves = er
			cometTVLReserves = er
			sdexOfferBook = er
			logger.Info("explorer reader wired (ClickHouse lake, ADR-0038)", "addr", addr)
		}
	}

	// The DEX TVL snapshot refreshes in the background and legs are valued through the same VWAP
	// USD tiers as trades.usd_volume, so TVL and volume share one methodology. Handlers read an
	// O(1) snapshot and omit the field until the first refresh, never 503.
	dexTVLSources := v1.DEXTVLSources{
		SoroswapPairs:    store,
		SoroswapReserves: soroswapTVLReserves,
		AquariusReserves: store,
		// Curated pool sets only (ADR-0035). Phoenix stake contracts are excluded because they
		// hold LP shares and would double-count the underlying.
		PhoenixPools:    phoenix.MainnetTVLPools(),
		PhoenixReserves: phoenixTVLReserves,
		CometPools:      comet.MainnetGatedSet(),
		CometReserves:   cometTVLReserves,
		// The same trust gates as every price surface; a withheld leg counts its pool unpriced so
		// the total stays an honest lower bound.
		Gate: buildDEXTVLValueGate(substanceGate, scamGate, usdPegs),
		// Pools are permissionless, so only vetted-catalogue assets, native and declared pegs are
		// valued; other legs count unpriced.
		Verified: verifiedCurrencies,
		Logger:   logger.With("component", "dex-tvl"),
	}
	if fx, err := timescale.NewVWAPUSDFXResolver(store, timescale.VWAPUSDFXResolverOptions{
		USDPegs: cfg.Trades.USDPeggedClassicAssets,
	}); err != nil {
		logger.Warn("dex tvl usd resolver unavailable; tvl legs will be unpriced", "err", err)
	} else {
		dexTVLSources.Pricer = fx
	}
	if len(cfg.Trades.USDPeggedClassicAssets) > 0 {
		if spec, err := timescale.NewUSDVolumeQuoteSpec(cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers); err != nil {
			logger.Warn("dex tvl usd-peg spec unavailable; pegged legs price via VWAP instead", "err", err)
		} else {
			dexTVLSources.PegInfo = spec
		}
	}
	dexTVLCache := v1.NewDEXTVLCache(dexTVLSources)
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "dex-tvl-cache")
		// Stays well below the refresh interval so refreshes never stack.
		const dexTVLRefreshTimeout = 3 * time.Minute
		refreshWithTimeout(rootCtx, dexTVLCache.Refresh, dexTVLRefreshTimeout, logger, "dex tvl initial refresh")
		runRefreshLoop(rootCtx, dexTVLCache.Refresh, v1.DEXTVLRefreshInterval, dexTVLRefreshTimeout, logger,
			"dex tvl periodic refresh")
	}()

	// The order book loads the lake's live offers once per start, then applies incremental reads
	// bounded by the contiguous tip so the cursor never crosses a dropped ledger, and rebuilds
	// daily to self-heal. The endpoint returns 503 until the initial load completes.
	var sdexOrderBook *v1.SDEXOrderBookCache
	if sdexOfferBook != nil {
		sdexOrderBook = v1.NewSDEXOrderBookCache(sdexOfferBook, logger.With("component", "sdex-orderbook"))
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "sdex-orderbook-cache")
			// MaintainTick owns the whole policy (load, retry, advance, quarantine drain, reload) so
			// the package tests cover it.
			sdexOrderBook.MaintainTick(rootCtx)
			tick := time.NewTicker(v1.SDEXOrderBookAdvanceInterval)
			defer tick.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-tick.C:
					sdexOrderBook.MaintainTick(rootCtx)
				}
			}
		}()
	}

	// Durable audit rows for admin key mints; nil degrades to log-only audit.
	var adminAudit v1.AuditSink
	if pgDB := store.DB(); pgDB != nil {
		adminAudit = postgresstore.NewAuditStore(postgresstore.New(pgDB))
	}

	// The account store backs the tier-override endpoints and is the same store the Postgres
	// key validator reads, so an override applies on the next lookup. The notice store backs the
	// status banner. Both need Postgres; nil gives admin 503s and an empty public notices list.
	var (
		platformAccountStore v1.PlatformAccountStore
		platformUserStore    v1.AccountSessionRevoker
		registerAccountStore v1.RegisterAccountCreator
		statusNoticeStore    v1.StatusNoticeStore
	)
	if pgDB := store.DB(); pgDB != nil {
		// One store serves both seams: Get/Update for overrides, Create for register.
		acctStore := postgresstore.NewAccountStore(postgresstore.New(pgDB))
		platformAccountStore = acctStore
		platformUserStore = postgresstore.NewUserStore(postgresstore.New(pgDB))
		registerAccountStore = acctStore
		statusNoticeStore = postgresstore.NewStatusNoticeStore(postgresstore.New(pgDB))
	}

	// Lowering an account's tier must clamp both the Postgres dashboard keys and the Redis
	// self-service keys; a missing store skips that half. The key-cache invalidator is chosen by
	// auth_backend inside NewAPIKeyBudgetStores, not by whether Redis exists: under the redis
	// backend apikey:<hash> is the credential itself and invalidating it would delete live keys.
	var platformKeys platform.APIKeyStore
	if pgDB := store.DB(); pgDB != nil {
		platformKeys = postgresstore.NewAPIKeyStore(postgresstore.New(pgDB))
	}
	var budgetsRedis redis.Cmdable
	if rdb != nil {
		budgetsRedis = rdb
	}
	apiKeyBudgets := v1.NewAPIKeyBudgetStores(platformKeys, budgetsRedis, cfg.API.AuthBackend)

	apiSrv := v1.New(v1.Options{
		Network:     cfg.Stellar.Network,
		Logger:      logger.With("component", "api"),
		ReadyChecks: checks,
		Assets:      assetReader,
		Prices:      priceReader,
		// SWR cache on LatestTradePerSource only, an unbounded scan that 503s uncached. The cold
		// fill is detached so it outlives the handler deadline.
		History: v1.NewCachedHistoryReader(wiring.StoreHistoryReader{S: store}, 2*time.Minute),
		// Consulted only after an empty read and asks when a pair starts, so it is not a
		// HistoryReader method.
		CoverageFloor: wiring.StoreCoverageFloorReader{S: store},
		// Markets and pools scan 24h of trades per hit, so a 30 s cache fronts them.
		Markets: cachedMarketsReader,
		Oracle:  oracleReader,
		// Uncached on purpose: the RWA history handler caches the assembled series, and a second
		// cache would add a second staleness window.
		OracleHistory: store,
		// Uncached for the same reason as OracleHistory.
		MarketHistory:        store,
		RWAPremiumSubstance:  substanceGate.Policy(),
		Sep1Cache:            store,
		Accounts:             accountStore,
		PlatformAccounts:     platformAccountStore,
		PlatformUsers:        platformUserStore,
		RegisterAccounts:     registerAccountStore,
		APIKeyBudgets:        apiKeyBudgets,
		StatusNotices:        statusNoticeStore,
		Audit:                adminAudit,
		SignupIPThrottle:     signupIPThrottle,
		RequireEmailVerified: requireEmailVerifiedOrNil(cfg.API.SignupRequireEmailVerification),
		Divergence:           divergenceLooker,
		Substance:            substanceGate,
		// One-hop pricing for Soroban-native assets, gated on both legs by the same substanceGate.
		TransitivePricer:      store,
		Scam:                  scamGate,
		Confidence:            redisConfidenceLooker{rdb: rdb},
		Triangulated:          wiring.RedisTriangulatedLooker{RDB: rdb},
		Freeze:                freezeLooker,
		Supply:                wiring.StoreSupplyLooker{S: store},
		TokenSupply:           tokenSupplyReader,
		ContractStorageSupply: storageSupplyReader,
		TokenDecimals:         tokenDecimalsReader,
		TokenSymbol:           tokenSymbolReader,
		// The contract arm of the RWA assets list reads the same curated directory as the classic arm.
		RWAContracts: store,
		// The cached independent listing directory that listing-sync fills. Staleness bounds are
		// enforced in the reader's SQL, so a stale snapshot closes the arm.
		RWAListings:        store,
		RWACurated:         store,
		Listings:           store,
		ContractCatalogue:  store,
		LakeWatermark:      lakeWatermarkReader,
		Explorer:           explorerReader,
		IssuerAuthFlags:    issuerAuthFlagsReader,
		StaticHomeDomain:   homeDomainLookup.static,
		Volume:             wiring.StoreVolumeReader{S: store},
		Change24h:          newMinuteChange24hReader(storeChange24hReader{s: store, pegs: usdPegs, decimals: nonstandardDecimalsCache, logger: logger}),
		PriceAt:            storePriceAtReader{s: store, substance: substanceGate, scam: scamGate, logger: logger.With("component", "price-at-guard")},
		ChangeSummary:      store,
		AssetsReader:       cachedAssetsReader,
		Issuers:            cachedIssuersReader,
		SEP41Transfers:     store,
		SEP41Movements:     store,
		Positions:          store,
		AccountTrades:      store,
		AccountActivity:    store,
		Directory:          store,
		VolumeCharacter:    store,
		Cursors:            store,
		CoverageReader:     store,
		CompletenessReader: store,
		AuditedSources:     completeness.AuditedSources(cfg),
		// Protocols pillar; the directory degrades to empties when these are absent.
		ProtocolContracts:  store,
		ProtocolStats:      store,
		ProtocolActivity:   protocolActivityReader,
		ProtocolBespoke:    store,
		SoroswapPairs:      store,
		ProtocolPoolTokens: store,
		DEXTVL:             dexTVLCache,
		SDEXOrderBook:      sdexOrderBook,
		NetworkStats:       cachedNetworkStats,
		// Direct store read: the scan uses a partial index and the edge cache absorbs fan-out.
		Aggregators: store,
		// A single-pair GROUP BY is cheap and needs no markets cache layer.
		MarketSources: v1.NewCachedMarketSourceReader(store, time.Minute),
		// The 24h per-source aggregation takes seconds, so a 60 s cache fronts it.
		SourcesStats: cachedSourcesStats,
		Lending:      cachedLendingReader,
		MEV:          store,
		MEVPulse:     mevPulse,
		Anomalies:    store,
		Divergences:  store,
		// The same threshold the divergence worker fires on, so charts shade the real alert band.
		DivergenceThresholdPct: cfg.Divergence.Threshold,
		// A market cap priced from one venue below the volume floor is served null with
		// market_cap_low_liquidity=true rather than asserting billions off a dust trade.
		MinMarketCapVolumeUSD:   cfg.Aggregate.MinMarketCapVolumeUSD,
		MaxMarketCapVolumeRatio: cfg.Aggregate.MaxMarketCapVolumeRatio,
		Currencies:              wiring.NewForexAdapter(forexCache),
		// The in-memory forex cache never expires on its own, so the cross-rate fallbacks need a
		// staleness budget.
		FXCrossMaxAgeHours: cfg.PricingGuard.FXCrossMaxAgeHours,
		DisableFiatBasis:   cfg.PricingGuard.DisableFiatBasis,
		FXFixings:          store,
		FXHistory:          &wiring.FXHistoryReader{Store: store},
		SEP10:              sep10Validator,
		Hub:                hub,
		CORS:               cors,
		Auth:               authMW,
		KeyPolicy:          middleware.KeyPolicy(),
		// Reads the counters the UsageTracker writes. Both validators map MonthlyQuota onto the
		// Subject; a validator that leaves it 0 makes the middleware skip the request.
		MonthlyQuota: middleware.MonthlyQuota(usageCounter, logger.With("component", "monthly-quota"),
			middleware.WithMonthlyQuotaDwellTime(cfg.API.MonthlyQuotaDwell)),
		RateLimit:    rateLimit,
		UsageTracker: middleware.UsageTracker(usageCounter, logger.With("component", "usage")),
		// Updates last_used columns asynchronously, debounced per key via Redis SETNX; needs both
		// Postgres and Redis.
		TouchUsage: touchUsageMiddlewareOrNil(dashboardBundle.keysStore, rdb, logger),
		// Nil unless the counter is real, so the handler returns an empty list.
		UsageReader: wiring.UsageReaderOrNil(usageCounter),
		// Reads the usage_daily rollup; the handler prefers it and falls back per request when the
		// read errors or the subject has no rows yet.
		UsageRollupReader: wiring.UsageRollupReaderOrNil(store),
		CDNEnabled:        cfg.API.CDNEnabled,
		// Per-request deadline for non-streaming requests; the serving-pool statement_timeout is
		// the SQL-side backstop.
		RequestTimeout: cfg.API.RequestTimeout,
		// A request_timeout of 0 disables the middleware.
		DisableRequestTimeout: cfg.API.RequestTimeout == 0,
		StatusBackend:         statusBackend,
		ArchiveReportPath:     cfg.API.ArchiveReportPath,
		RegionName:            statusRegion,
		RegionDeployment:      statusDeployment,
		StatusServices:        statusServices,
		DashboardAuth:         nilOrMounter(dashboardBundle.auth),
		DashboardKeys:         nilOrMounter(dashboardBundle.keys),
		DashboardWebhooks:     nilOrMounter(dashboardBundle.webhooks),
		DashboardPriceAlerts:  nilOrMounter(dashboardBundle.priceAlerts),
		SessionAuth:           dashboardBundle.middleware,
		SessionPeeker:         sessionPeekerAdapter{},
		SACWrappers:           cfg.Supply.SACWrappers,
		NetworkPassphrase:     cfg.Stellar.Passphrase(),
		USDPeggedClassics:     usdPegs,
		FiatPeggedClassics:    fiatPegs,
		VerifiedCurrencies:    verifiedCurrencies,
		BackfillCoverage:      backfillCoverageCache,
		UsdVolumePricing:      usdVolumePricingCache,
		NonstandardDecimals:   nonstandardDecimalsCache,
		GlobalPrice: wiring.GlobalPriceReader{
			S:   store,
			Tri: wiring.RedisTriangulatedLooker{RDB: rdb},
			PKPairFor: func(base, quote canonical.Asset) (canonical.Pair, error) {
				return canonical.NewPair(base, quote)
			},
			Logger:    logger,
			Substance: substanceGate,
			Scam:      scamGate,
		},
		GlobalPriceOpts: aggregate.GlobalPriceOptions{
			AggregatorSources: external.AggregatorSources(),
		},
	})

	if cfg.API.HoldsFile != "" {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "holds-watch")
			holds.Watch(rootCtx, cfg.API.HoldsFile, holdsReloadInterval, apiSrv.SetHolds, logger.With("component", "holds"))
		}()
	}

	// Ceilings on shared tip-stream producers; config defaults equal the built-in ones.
	apiSrv.SetMaxTipProducers(cfg.API.Streaming.MaxTipProducers)
	apiSrv.SetMaxTipProducersPerCaller(cfg.API.Streaming.MaxTipProducersPerCaller)

	// Slow builds are prewarmed out of band, since a cold fill on the request path would outlive
	// the request timeout and cost the first visitor after a deploy a degraded page. The
	// 5-minute cadence against 10-minute TTLs keeps entries warm, and a warm call is a cheap
	// no-op. Every prewarm in this loop would otherwise build inline on a request deadline.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-supply-wealth")
		const cadence = 5 * time.Minute
		apiSrv.PrewarmClassicSupply(rootCtx)
		apiSrv.PrewarmAccountsWealth(rootCtx)
		apiSrv.PrewarmContractsDirectory(rootCtx)
		apiSrv.PrewarmContractStats(rootCtx)
		apiSrv.PrewarmContractProtocolIndex(rootCtx)
		apiSrv.PrewarmOpTypeStats(rootCtx)
		apiSrv.PrewarmNetworkThroughput(rootCtx)
		apiSrv.PrewarmNativeLiquidityPools(rootCtx)
		apiSrv.PrewarmSep1Images(rootCtx)
		t := time.NewTicker(cadence)
		defer t.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				apiSrv.PrewarmClassicSupply(rootCtx)
				apiSrv.PrewarmAccountsWealth(rootCtx)
				apiSrv.PrewarmContractsDirectory(rootCtx)
				apiSrv.PrewarmContractStats(rootCtx)
				apiSrv.PrewarmContractProtocolIndex(rootCtx)
				apiSrv.PrewarmOpTypeStats(rootCtx)
				apiSrv.PrewarmNetworkThroughput(rootCtx)
				apiSrv.PrewarmNativeLiquidityPools(rootCtx)
				apiSrv.PrewarmSep1Images(rootCtx)
			}
		}
	}()

	// Own goroutine: a cold pass takes minutes and must not delay the cheap prewarms above.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-classic-lake-supply")
		prewarmClassicLakeSupply(rootCtx, apiSrv)
	}()

	// Own goroutine: the membership rebuild scans every issuer SEP-1 payload and takes seconds. The
	// 5-minute cadence against 10-minute TTLs only guarantees the cache is never cold; request-
	// kicked detached refreshes keep it fresh.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-rwa")
		const cadence = 5 * time.Minute
		apiSrv.PrewarmRWA(rootCtx)
		t := time.NewTicker(cadence)
		defer t.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
				apiSrv.PrewarmRWA(rootCtx)
			}
		}
	}()

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-asset-list")
		t := time.NewTicker(v1.AssetListPrewarmCadence)
		defer t.Stop()
		for {
			apiSrv.PrewarmAssetList(rootCtx)
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
			}
		}
	}()

	// Own goroutine: a full sweep is minutes of serial work. On-demand builds die at the request
	// deadline, so every protocol and window is warmed ahead. The sweep restarts after the
	// previous one ends (sleep, not a ticker, so sweeps never overlap) and stays inside the
	// protocolDetailTTL stale horizon.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-protocol-details")
		const betweenSweeps = 10 * time.Minute
		for {
			// Cheap roster counts first, so the directory is warm before the heavy sweep.
			apiSrv.PrewarmProtocolRosters(rootCtx)
			apiSrv.PrewarmProtocolDetails(rootCtx)
			select {
			case <-rootCtx.Done():
				return
			case <-time.After(betweenSweeps):
			}
		}
	}()

	// The producer runs only when pairs are configured; an empty list is valid and subscribers get
	// heartbeats only. A bad pair string fails startup rather than being dropped.
	streamPairs, err := parseStreamingPairs(cfg.API.Streaming.Pairs)
	if err != nil {
		return fmt.Errorf("api.streaming.pairs: %w", err)
	}
	if len(streamPairs) > 0 {
		pub := streampublish.New(hub, priceReader, cfg.API.Streaming.PollInterval, logger.With("component", "stream-publisher"),
			streampublish.Options{Decimals: nonstandardDecimalsCache, Frozen: freezeLooker})
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "stream-publisher")
			if err := pub.Run(rootCtx, streamPairs); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("stream publisher exited", "err", err)
			}
		}()
		logger.Info("stream publisher running", "pairs", len(streamPairs), "interval", cfg.API.Streaming.PollInterval)
	} else {
		logger.Info("stream publisher disabled (no pairs configured); /v1/price/stream serves heartbeats only")
	}

	// Refreshes the ingestion diagnostics snapshot the handler serves. If it stops, the handler
	// falls back to its inline build, slower but not an outage. The literal wrapper puts the
	// recover guard on this goroutine's stack.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "ingestion-snapshot-refresh")
		apiSrv.StartIngestionSnapshotRefresh(rootCtx)
	}()

	// Keeps stellarindex_dependency_up fresh without /v1/readyz traffic, so
	// a dependency outage alerts even when no probe is polling.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "readiness-probe")
		apiSrv.StartReadinessProbe(rootCtx, v1.ReadinessProbeCadence)
	}()

	httpSrv := &http.Server{
		Addr:              cfg.API.ListenAddr,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Shutdown waits for connections to go idle and an SSE stream never does, so one open stream
	// would pin the whole 30 s drain and starve the worker wait sharing its deadline. The hook
	// fires the instant Shutdown starts. Do not use BaseContext instead: it would also cancel
	// every ordinary in-flight request on SIGTERM.
	httpSrv.RegisterOnShutdown(apiSrv.BeginStreamDrain)

	// The closed-bucket subscriber is bound to rootCtx, and a Run error leaves the rest of the API
	// serving. Gate on rdb, not hub: the Hub is always non-nil, and NewSubscriber(nil, ...) would
	// abort startup. Without Redis the stream serves heartbeats only.
	if rdb != nil && hub != nil {
		sub, err := redispub.NewSubscriber(rdb, redispub.DefaultChannel, hub, logger.With("component", "stream-sub"))
		if err != nil {
			return fmt.Errorf("redispub subscriber: %w", err)
		}
		go runSubscriberSupervised(rootCtx, sub, logger.With("component", "stream-sub"))
	}

	// Customer-webhook delivery worker, wired only when the dashboard webhook store is up.
	if dashboardBundle.webhookStore != nil {
		worker := customerwebhook.New(dashboardBundle.webhookStore, customerwebhook.Options{
			Logger: logger.With("component", "customer-webhook"),
		})
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "customer-webhook")
			if err := worker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("customer-webhook worker exited",
					"err", err)
			}
		}()
		logger.Info("customer-webhook delivery worker started")
	}

	// Folds the Redis per-endpoint counters into usage_daily so usage analytics outlive Redis's
	// TTL. Needs both backends.
	if rollup := usage.NewRollup(usageCounter, store, usage.DefaultRollupInterval,
		logger.With("component", "usage-rollup")); rollup != nil {
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "usage-rollup")
			if err := rollup.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("usage-rollup worker exited", "err", err)
			}
		}()
		logger.Info("usage-rollup worker started", "interval", usage.DefaultRollupInterval)
	}

	// Deletes orphan accounts left by a lost signup race. Registration is wired off
	// platformAccountStore, not the dashboard bundle (nil when base_url is unset), so the reaper
	// must bind to the same store or a Postgres-without-dashboard deployment would never reap.
	if cfg.SignupReaper.Enabled {
		if orphans, ok := platformAccountStore.(signupreaper.OrphanStore); ok && orphans != nil {
			reaper := signupreaper.New(orphans, signupreaper.Options{
				Logger: logger.With("component", "signup-reaper"),
			})
			bgWG.Add(1)
			go func() {
				defer bgWG.Done()
				defer recoverBackgroundWorker(logger, "signup-reaper")
				if err := reaper.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("signup-reaper worker exited", "err", err)
				}
			}()
			logger.Info("signup-reaper worker started",
				"interval", signupreaper.DefaultInterval, "min_age", signupreaper.DefaultMinAge)
		} else {
			logger.Info("signup-reaper enabled but Postgres account store not wired — skipping")
		}
	}

	// Login-code lockouts are keyed by an attacker-chosen email from an unauthenticated endpoint,
	// and only this sweep bounds the table. It is a DoS control, so it is deliberately not behind
	// any toggle and runs whenever the Postgres token store is wired.
	if lockouts, ok := dashboardBundle.tokens.(logincodereaper.LockoutStore); ok && lockouts != nil {
		lockoutReaper := logincodereaper.New(lockouts, logincodereaper.Options{
			Logger: logger.With("component", "login-code-lockout-reaper"),
		})
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "login-code-lockout-reaper")
			if err := lockoutReaper.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("login-code-lockout-reaper worker exited", "err", err)
			}
		}()
		logger.Info("login-code-lockout reaper started",
			"interval", logincodereaper.DefaultInterval,
			"retention", logincodereaper.DefaultRetention)
	}

	// Magic-link tokens hold PII (email, requested_ip) for attacker-chosen addresses from an
	// unauthenticated endpoint, and only this sweep bounds the table. Like the lockout reaper it is
	// a DoS/PII control and is deliberately not behind any toggle.
	if links, ok := dashboardBundle.tokens.(magiclinkreaper.MagicLinkStore); ok && links != nil {
		linkReaper := magiclinkreaper.New(links, magiclinkreaper.Options{
			Logger: logger.With("component", "magic-link-token-reaper"),
		})
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "magic-link-token-reaper")
			if err := linkReaper.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("magic-link-token-reaper worker exited", "err", err)
			}
		}()
		logger.Info("magic-link-token reaper started",
			"interval", magiclinkreaper.DefaultInterval,
			"retention", magiclinkreaper.DefaultRetention)
	}

	// Ended sessions and finished webhook deliveries; ungated for the same reason as the reapers
	// above.
	startRetentionReapers(rootCtx, &bgWG, logger, retentionReaperTargets(dashboardBundle, rdb, cfg.API.AuthBackend, logger))

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// Self-calls /v1/assets/<id> for native and every verified currency, and the explorer's listing reads, each 60 s so every cache
	// the handler touches stays warm, not just the prewarmCaches slots. It goes through the real
	// handler, so cache keys byte-match a user request and cannot drift from the prewarm.
	if selfPrewarmAdmitted(cfg.API.AuthMode) {
		go selfPrewarmAssetEndpoints(rootCtx, logger.With("component", "self-prewarm"), cfg.API.ListenAddr, verifiedAssetIDs)
	} else {
		logger.Warn("self-prewarm disabled: auth_mode requires a credential the self-call does not carry",
			"auth_mode", cfg.API.AuthMode,
			"effect", "/v1/assets/{id} caches outside prewarmCaches' SWR slots warm on first user request")
	}

	select {
	case <-rootCtx.Done():
		logger.Info("shutdown signal received — draining for up to 30s")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}

	shutdownCtx, stopDrain := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopDrain()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown", "err", err)
	} else {
		logger.Info("clean shutdown")
	}

	// Wait for the workers, which are already unwinding on the cancelled rootCtx. This runs after
	// Shutdown so serving stops first. It shares shutdownCtx with the listener drain: 30 s is the
	// whole deploy downtime budget, so a listener that spends it all leaves the workers nothing.
	// The stream drain above keeps the listener's share small.
	workersDone := make(chan struct{})
	go func() {
		// A Wait panic means an Add/Done imbalance; workersDone stays open on purpose so the
		// select below reports "did not drain" instead of claiming a drain that never happened.
		defer recoverBackgroundWorker(logger, "background-worker-drain")
		bgWG.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
		logger.Info("background workers drained")
	case <-shutdownCtx.Done():
		logger.Warn("background workers did not drain within the shutdown budget — exiting anyway")
	}
	return nil
}

// authValidatorOptions is the wiring buildAuthMiddleware needs beyond the mode string.
type authValidatorOptions struct {
	Backend           string // "redis" (default) or "postgres"
	Rdb               redis.UniversalClient
	PostgresValidator *auth.PostgresAPIKeyValidator // non-nil when dashboard wired
	SEP10             auth.SEP10Validator
	// FailedAuthLimiter throttles invalid-credential attempts per IP and per key prefix; nil
	// disables it.
	FailedAuthLimiter *ratelimit.Bucket
	// AccountStatus wires the account kill switch into the redis validator. A deployment with
	// Postgres must pass it, or a suspended account keeps authenticating.
	AccountStatus auth.AccountStatusReader
}

// buildAuthMiddleware turns auth_mode into a middleware, or nil for mode=none. A postgres
// backend without the platform stores falls back to Noop, so every request 503s.
func buildAuthMiddleware(mode string, opts authValidatorOptions, logger *slog.Logger) middleware.Middleware {
	switch mode {
	case "", "none":
		return nil
	case "sep10":
		return middleware.Auth(middleware.AuthOptions{
			Mode:              middleware.AuthModeSEP10,
			SEP10:             opts.SEP10,
			FailedAuthLimiter: opts.FailedAuthLimiter,
		})
	case "apikey":
		return middleware.Auth(middleware.AuthOptions{
			Mode:              middleware.AuthModeAPIKey,
			APIKey:            buildAPIKeyValidator(opts, logger, "apikey"),
			FailedAuthLimiter: opts.FailedAuthLimiter,
		})
	case "apikey_optional":
		return middleware.Auth(middleware.AuthOptions{
			Mode:              middleware.AuthModeAPIKeyOptional,
			APIKey:            buildAPIKeyValidator(opts, logger, "apikey_optional"),
			FailedAuthLimiter: opts.FailedAuthLimiter,
		})
	}
	logger.Error("unknown auth_mode — server falling through to no-auth", "mode", mode)
	return nil
}

// buildAPIKeyValidator picks the Redis or Postgres backend; a missing dependency yields Noop so
// requests fail loudly instead of silently demoting.
func buildAPIKeyValidator(opts authValidatorOptions, logger *slog.Logger, modeName string) auth.APIKeyValidator {
	switch opts.Backend {
	case "postgres":
		if opts.PostgresValidator == nil {
			logger.Error("auth_backend=postgres but the dashboard bundle is not wired — every request will 503",
				"mode", modeName,
				"reason", "set api.dashboard.base_url to enable the Postgres backend (the bundle owns the platform store handles the validator borrows)")
			return auth.NoopAPIKeyValidator{}
		}
		logger.Info("auth: apikey validator wired",
			"mode", modeName, "backend", "postgres",
			"cache", opts.Rdb != nil)
		return opts.PostgresValidator
	default:
		// "redis" or unset — default to the legacy backend.
		if opts.Rdb == nil {
			logger.Error("auth_backend=redis but Redis is not configured — every request will 503",
				"mode", modeName,
				"reason", "RedisAPIKeyValidator requires a Redis client")
			return auth.NoopAPIKeyValidator{}
		}
		var ropts []auth.RedisOption
		if opts.AccountStatus != nil {
			ropts = append(ropts, auth.WithAccountStatus(opts.AccountStatus))
		}
		logger.Info("auth: apikey validator wired",
			"mode", modeName, "backend", "redis",
			"account_status_gate", opts.AccountStatus != nil)
		return auth.NewRedisAPIKeyValidator(opts.Rdb, ropts...)
	}
}

// nilOrMounter returns a nil interface for a nil *Handlers. A bare assignment would wrap a typed
// nil in a non-nil interface, mount the routes, and panic on the first request.
func nilOrMounter[T v1.DashboardAuthMounter](h T) v1.DashboardAuthMounter {
	var zero T
	if any(h) == any(zero) {
		return nil
	}
	return h
}

// dashboardBundle holds the dashboard wiring for v1.Options. The platform stores ride along so
// the Postgres auth validator shares them instead of opening a second pool.
type dashboardBundle struct {
	auth         *dashboardauth.Handlers
	keys         *dashboardkeys.Handlers
	webhooks     *dashboardwebhooks.Handlers
	priceAlerts  *dashboardpricealerts.Handlers
	webhookStore platform.WebhookStore
	middleware   middleware.Middleware
	keysStore    platform.APIKeyStore
	accounts     platform.AccountStore
	// tokens is the store behind magic-link and lockout rows, exposed for the lockout reaper.
	tokens platform.TokenStore
	// users carries the sessions table the retention reaper sweeps.
	users       platform.UserStore
	pgValidator *auth.PostgresAPIKeyValidator
	// sender and emailFrom let the public signup flow reuse the dashboard's transport.
	sender    notify.Sender
	emailFrom string
}

// Without a Resend env var the sender is notify.UnconfiguredSender, so magic-link login answers
// 503 instead of claiming an email it cannot deliver; the token is never logged.

// buildWebhookHandlers builds the webhook CRUD handlers over a fresh store and returns the store
// for the delivery worker.
func buildWebhookHandlers(db *sql.DB, sealer *platform.WebhookKeySealer, logger *slog.Logger) (*postgresstore.WebhookStore, *dashboardwebhooks.Handlers, error) {
	store := postgresstore.NewSealingWebhookStore(postgresstore.New(db), sealer)
	h, err := dashboardwebhooks.NewHandlers(dashboardwebhooks.Config{
		Webhooks: store,
		Logger:   logger.With("component", "dashboard-webhooks"),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dashboard webhooks handlers: %w", err)
	}
	return store, h, nil
}

// buildSealingWebhookHandlers is [buildWebhookHandlers] with the store
// sealing signing keys when the seal secret is set, after sealing any
// key a previous start stored raw.
func buildSealingWebhookHandlers(cfg config.DashboardConfig, db *sql.DB, logger *slog.Logger) (*postgresstore.WebhookStore, *dashboardwebhooks.Handlers, error) {
	sealer, err := buildWebhookKeySealer(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	store, h, err := buildWebhookHandlers(db, sealer, logger)
	if err != nil {
		return nil, nil, err
	}
	if sealer != nil {
		sealLegacyWebhookKeys(store, logger)
	}
	return store, h, nil
}

// buildWebhookKeySealer reads the webhook seal secret from the env var
// cfg names. Unset returns nil (keys stored raw); a too-short value is a
// startup error rather than a weak key.
func buildWebhookKeySealer(cfg config.DashboardConfig, logger *slog.Logger) (*platform.WebhookKeySealer, error) {
	secret := os.Getenv(cfg.WebhookSealKeyEnv)
	if secret == "" {
		logger.Warn("webhook seal key env unset — new customer-webhook signing keys are stored unsealed, "+
			"and deliveries to webhooks whose key is already sealed wait until it is set",
			"env", cfg.WebhookSealKeyEnv)
		return nil, nil
	}
	sealer, err := platform.NewWebhookKeySealer([]byte(secret))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.WebhookSealKeyEnv, err)
	}
	return sealer, nil
}

// sealLegacyWebhookKeys seals signing keys written before a seal key was
// configured. A failure only leaves those keys raw until the next start.
func sealLegacyWebhookKeys(store *postgresstore.WebhookStore, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := store.SealLegacySigningKeys(ctx)
	if err != nil {
		logger.Error("seal legacy customer-webhook signing keys", "err", err, "sealed", n)
		return
	}
	if n > 0 {
		logger.Info("sealed legacy customer-webhook signing keys", "count", n)
	}
}

// buildPriceAlertHandlers builds the price-alert CRUD handlers; the evaluator runs in the
// aggregator binary.
func buildPriceAlertHandlers(pg *postgresstore.Store, logger *slog.Logger) (*dashboardpricealerts.Handlers, error) {
	h, err := dashboardpricealerts.NewHandlers(dashboardpricealerts.Config{
		Alerts: postgresstore.NewPriceAlertStore(pg),
		Logger: logger.With("component", "dashboard-price-alerts"),
	})
	if err != nil {
		return nil, fmt.Errorf("dashboard price-alert handlers: %w", err)
	}
	return h, nil
}

// assertRedisOrSingleInstance refuses a Redis-less start that has not asserted single-instance.
// Without Redis the auth throttles and passkey replay guard are per-process, which is unsafe
// across several instances, and a process cannot detect its fleet size.
func assertRedisOrSingleInstance(redisEnabled, singleInstance bool) error {
	if redisEnabled || singleInstance {
		return nil
	}
	return fmt.Errorf("refusing to start: Redis is disabled (storage.redis_addr empty, no sentinels) " +
		"and api.single_instance is not set. The auth throttles and the passkey ceremony replay guard " +
		"fall back to per-process state, which is unsafe behind more than one API instance (cross-instance " +
		"ceremony replay can mint a session; per-IP/email throttle caps multiply by the replica count). " +
		"Provide Redis for any multi-instance deployment, or set api.single_instance=true to assert this is " +
		"a single instance and accept per-process auth accounting")
}

// These mirror auth.RedisLoginThrottle's defaults (10/h per IP, 5/h per email) so the Redis-less
// fallback enforces the same policy with single-instance accounting.
const (
	inProcessLoginThrottleWindow      = time.Hour
	inProcessLoginThrottleMaxPerIP    = 10
	inProcessLoginThrottleMaxPerEmail = 5
)

// inProcessLoginThrottle is the Redis-less magic-link send throttle. It reuses the nil-Redis
// ratelimit.Bucket, whose memory is bounded, instead of a second unbounded map.
type inProcessLoginThrottle struct {
	perIP    *ratelimit.Bucket
	perEmail *ratelimit.Bucket
}

func newInProcessLoginThrottle() *inProcessLoginThrottle {
	return &inProcessLoginThrottle{
		perIP:    ratelimit.New(nil, inProcessLoginThrottleMaxPerIP, inProcessLoginThrottleWindow),
		perEmail: ratelimit.New(nil, inProcessLoginThrottleMaxPerEmail, inProcessLoginThrottleWindow),
	}
}

// Allow implements dashboardauth.LoginThrottle; the in-process bucket never errors, so the discarded errors are always nil.
func (t *inProcessLoginThrottle) Allow(ctx context.Context, ip, email string) (bool, error) {
	allowed := true
	if ip != "" {
		// Masked by ThrottleIPKey so an IPv6 /64 cannot mint a fresh per-IP budget per /128.
		res, _ := t.perIP.Take(ctx, "ip:"+ratelimit.ThrottleIPKey(ip))
		allowed = res.Allowed
	}
	// The per-email Take runs only after a passing per-IP Take. Otherwise an exhausted IP could
	// insert a tracked key per target address until the store's key cap folds the rest into one
	// shared overflow bucket, denying magic-link sends to every untracked email.
	if allowed && email != "" {
		// Hash with auth.HashEmail, the same canonicalisation as RedisLoginThrottle, so a
		// re-spelling cannot bypass this fallback.
		res, _ := t.perEmail.Take(ctx, "mail:"+auth.HashEmail(email))
		allowed = res.Allowed
	}
	return allowed, nil
}

// inProcessSignupIPThrottleMaxPerHour mirrors auth.RedisSignupIPThrottle's default of 5 per hour.
const inProcessSignupIPThrottleMaxPerHour = 5

// holdsReloadInterval is how often the API re-reads the holds file.
const holdsReloadInterval = 15 * time.Second

// inProcessSignupIPThrottle is the Redis-less per-IP signup throttle, built like
// inProcessLoginThrottle.
type inProcessSignupIPThrottle struct {
	bucket *ratelimit.Bucket
}

func newInProcessSignupIPThrottle() *inProcessSignupIPThrottle {
	return &inProcessSignupIPThrottle{
		bucket: ratelimit.New(nil, inProcessSignupIPThrottleMaxPerHour, time.Hour),
	}
}

// CheckIP implements v1.SignupIPThrottle; the only error is auth.ErrSignupRateLimited.
func (t *inProcessSignupIPThrottle) CheckIP(ctx context.Context, ip string) error {
	if ip == "" {
		return nil
	}
	// Masked by ThrottleIPKey; a bare IPv6 address is the wrong key.
	res, _ := t.bucket.Take(ctx, ratelimit.ThrottleIPKey(ip))
	if !res.Allowed {
		return auth.ErrSignupRateLimited
	}
	return nil
}

// wireDashboardAuthThrottles sets the email locker, login throttle and passkey replay guard from Redis availability. Without Redis: no
// SETNX locker (Suspend-on-conflict recovery), in-process login buckets (never no limit), in-process passkey guard.
func wireDashboardAuthThrottles(authCfg *dashboardauth.Config, rdb redis.UniversalClient, logger *slog.Logger) {
	if rdb != nil {
		authCfg.EmailLocker = auth.NewRedisSignupEmailLocker(rdb)
		authCfg.LoginThrottle = auth.NewRedisLoginThrottle(rdb, auth.LoginThrottleOptions{})
		authCfg.PasskeyCeremonyGuard = auth.NewRedisPasskeyCeremonyGuard(rdb)
		return
	}
	authCfg.LoginThrottle = newInProcessLoginThrottle()
	logger.Warn("dashboard magic-link throttle is in-process (single-instance fallback — no Redis); " +
		"per-email/per-IP caps are NOT shared across instances")
	logger.Warn("passkey ceremony replay guard is in-process (single-instance fallback — no Redis); " +
		"a spent challenge is NOT visible to other instances")
}

// buildDashboardGenerator wires the token generator with the secret every dashboard-auth MAC key
// derives from; dashboardauth refuses to start with passkeys wired and no secret.
func buildDashboardGenerator(cfg config.DashboardConfig, logger *slog.Logger) *dashboardauth.Generator {
	generator := dashboardauth.NewGenerator()
	if secret := os.Getenv(cfg.CodeSecretEnv); secret != "" {
		generator.Secret = []byte(secret)
	} else {
		logger.Warn("dashboard code secret env unset — using a random per-process secret; "+
			"in-flight sign-in codes and passkey ceremonies will not survive a restart, "+
			"and behind more than one API instance both fail whenever a request lands on another instance",
			"env", cfg.CodeSecretEnv)
	}
	return generator
}

// dashboardKeyMirror is the Redis key store every dashboard-minted key is
// also written to, whatever auth_backend says: a key minted under one
// backend must still die on revoke after a flip to the other. Nil without Redis.
func dashboardKeyMirror(rdb redis.UniversalClient) dashboardkeys.KeyMirror {
	if rdb == nil {
		return nil
	}
	return auth.NewRedisAPIKeyStore(rdb)
}

func buildDashboardBundle(cfg config.DashboardConfig, db *sql.DB, rdb redis.UniversalClient, logger *slog.Logger) (dashboardBundle, error) {
	if cfg.BaseURL == "" {
		logger.Warn("dashboard not wired (api.dashboard.base_url is empty); /v1/auth/* + /v1/dashboard/* will 404")
		return dashboardBundle{}, nil
	}
	if db == nil {
		return dashboardBundle{}, errors.New("dashboard requires a Postgres connection")
	}

	pg := postgresstore.New(db)
	accounts := postgresstore.NewAccountStore(pg)
	users := postgresstore.NewUserStore(pg)
	tokens := postgresstore.NewTokenStore(pg)
	keysStore := postgresstore.NewAPIKeyStore(pg)

	sender, err := buildDashboardSender(cfg, logger)
	if err != nil {
		return dashboardBundle{}, err
	}

	// One audit log for admin surfaces, staff look-ups and credential changes, so one query shows
	// who changed a credential and from where.
	dashboardAudit := postgresstore.NewAuditStore(pg)
	authCfg := dashboardauth.Config{
		Accounts:  accounts,
		Users:     users,
		Tokens:    tokens,
		Sender:    sender,
		Generator: buildDashboardGenerator(cfg, logger),
		// Passkey (WebAuthn) sign-in.
		Passkeys: postgresstore.NewWebAuthnCredentialStore(pg),
		Audit:    dashboardAudit,
		Logger:   logger.With("component", "dashboard-auth"),
		// Set explicitly: the session-resolver middleware calls cfg.Now() on every authenticated
		// request and would nil-deref.
		Now:              func() time.Time { return time.Now().UTC() },
		DashboardBaseURL: cfg.BaseURL,
		EmailFrom:        cfg.EmailFrom,
		MagicLinkTTL:     time.Duration(cfg.MagicLinkTTLMinutes) * time.Minute,
		SessionTTL:       time.Duration(cfg.SessionTTLDays) * 24 * time.Hour,
		CookieSecure:     cfg.CookieSecure,
		// cookie_domain scopes only the presence hint; credential cookies are host-only.
		SessionHintDomain: cfg.CookieDomain,
		// DELETE /v1/dashboard/account and GET .../export.
		AccountEraser:   &accounterasure.Eraser{Store: accounts, Redis: rdb, Logger: logger.With("component", "account-erasure")},
		AccountExporter: &accounterasure.Exporter{Store: accounts, Redis: rdb},
	}
	wireDashboardAuthThrottles(&authCfg, rdb, logger)
	authH, err := dashboardauth.NewHandlers(&authCfg)
	if err != nil {
		return dashboardBundle{}, fmt.Errorf("dashboard auth handlers: %w", err)
	}
	// Built here so Revoke can invalidate its cached key; reused when auth_backend is postgres.
	pgValidator, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     keysStore,
		Accounts: accounts,
		Cache:    rdb,
	})
	if err != nil {
		return dashboardBundle{}, fmt.Errorf("postgres auth validator: %w", err)
	}

	keysH, err := dashboardkeys.NewHandlers(dashboardkeys.Config{
		Keys:             keysStore,
		Mirror:           dashboardKeyMirror(rdb),
		CacheInvalidator: pgValidator,
		Audit:            dashboardAudit,
		Logger:           logger.With("component", "dashboard-keys"),
	})
	if err != nil {
		return dashboardBundle{}, fmt.Errorf("dashboard keys handlers: %w", err)
	}

	// Same Postgres store the delivery worker drains.
	webhookStore, webhooksH, err := buildSealingWebhookHandlers(cfg, db, logger)
	if err != nil {
		return dashboardBundle{}, err
	}

	priceAlertsH, err := buildPriceAlertHandlers(pg, logger)
	if err != nil {
		return dashboardBundle{}, err
	}
	logger.Info("dashboard wired",
		"base_url", cfg.BaseURL,
		"magic_link_ttl_minutes", cfg.MagicLinkTTLMinutes,
		"session_ttl_days", cfg.SessionTTLDays,
		"cookie_secure", cfg.CookieSecure)

	return dashboardBundle{
		auth:         authH,
		keys:         keysH,
		webhooks:     webhooksH,
		priceAlerts:  priceAlertsH,
		webhookStore: webhookStore,
		middleware:   middleware.Middleware(dashboardauth.Middleware(&authCfg)),
		keysStore:    keysStore,
		accounts:     accounts,
		tokens:       tokens,
		users:        users,
		pgValidator:  pgValidator,
		sender:       sender,
		emailFrom:    cfg.EmailFrom,
	}, nil
}

// resolveSEP10Validator applies the auth_mode fallback: unconfigured SEP-10 wires the Noop; configured SEP-10 fails closed without Redis
// (no replay guard) or the account lake (no signer check), so auth_mode=sep10 aborts startup and other modes degrade to the Noop.
func resolveSEP10Validator(
	cfg config.SEP10Config,
	authMode string,
	passphrase string,
	rdb redis.UniversalClient,
	accounts sep10.AccountLoader,
	logger *slog.Logger,
) (auth.SEP10Validator, error) {
	v, err := buildSEP10Validator(cfg, passphrase, rdb, accounts)
	if err != nil {
		// Only auth_mode=sep10 needs the validator to bootstrap; otherwise a Noop keeps the rest
		// of the API up.
		if authMode == "sep10" {
			return nil, fmt.Errorf("sep10 validator: %w (auth_mode=sep10 requires it)", err)
		}
		logger.Warn("sep10 validator not wired; /v1/auth/sep10/* will return 404 sep10-unavailable",
			"err", err)
		return auth.NoopSEP10Validator{}, nil
	}
	logger.Info("sep10 validator wired (replay-guarded, signer-threshold checked)",
		"web_auth_domain", cfg.WebAuthDomain,
		"home_domain", cfg.HomeDomain,
		"challenge_ttl", cfg.ChallengeTTL,
		"jwt_ttl", cfg.JWTTTL)
	return v, nil
}

func buildSEP10Validator(cfg config.SEP10Config, passphrase string, rdb redis.UniversalClient, accounts sep10.AccountLoader) (auth.SEP10Validator, error) {
	if cfg.SeedEnv == "" || cfg.JWTSecretEnv == "" {
		return nil, errors.New("sep10: seed_env / jwt_secret_env not configured")
	}
	seed := os.Getenv(cfg.SeedEnv)
	if seed == "" {
		return nil, fmt.Errorf("sep10: env %s is unset or empty", cfg.SeedEnv)
	}
	jwtSecret := os.Getenv(cfg.JWTSecretEnv)
	if jwtSecret == "" {
		return nil, fmt.Errorf("sep10: env %s is unset or empty", cfg.JWTSecretEnv)
	}

	network := passphrase

	// Fail closed: without the Redis replay guard a stolen signed challenge could mint JWTs for
	// the whole ChallengeTTL, so a configured SEP-10 with no Redis refuses to start.
	if rdb == nil {
		return nil, fmt.Errorf(
			"sep10: %w: SEP-10 auth is configured but Redis is not — the replay guard requires Redis",
			sep10.ErrReplayGuardUnavailable,
		)
	}
	replayGuard := sep10.NewRedisReplayGuard(rdb)

	v, err := sep10.NewValidator(sep10.Options{
		ServerSeed:        seed,
		NetworkPassphrase: network,
		WebAuthDomain:     cfg.WebAuthDomain,
		HomeDomain:        cfg.HomeDomain,
		ChallengeTTL:      cfg.ChallengeTTL,
		JWTTTL:            cfg.JWTTTL,
		JWTSecret:         []byte(jwtSecret),
		ReplayGuard:       replayGuard,
		AccountLoader:     accounts,
	})
	if err != nil {
		return nil, fmt.Errorf("sep10: NewValidator: %w", err)
	}
	return v, nil
}

// lakeAccountSigners serves SEP-10 signer lookup from the lake's current account state; until bound (or if the dial failed) every lookup
// errors, so verification fails closed.
type lakeAccountSigners struct {
	er atomic.Pointer[clickhouse.ExplorerReader]
}

func (l *lakeAccountSigners) bind(er *clickhouse.ExplorerReader) { l.er.Store(er) }

func (l *lakeAccountSigners) LoadAccountSigners(ctx context.Context, accountID string) (sep10.AccountSigners, error) {
	er := l.er.Load()
	if er == nil {
		return sep10.AccountSigners{}, errors.New("sep10: account lake reader not connected")
	}
	st, err := er.AccountSigners(ctx, accountID)
	if err != nil {
		return sep10.AccountSigners{}, err
	}
	out := sep10.AccountSigners{Exists: st.Exists, MasterWeight: st.MasterWeight, MedThreshold: st.ThreshMed}
	for _, s := range st.Signers {
		out.Signers = append(out.Signers, sep10.Signer{Key: s.Key, Weight: s.Weight})
	}
	return out, nil
}

type homeDomainLookups struct {
	listing func(ctx context.Context, issuers []string) map[string]string
	detail  func(ctx context.Context, issuer string) (string, bool)
	static  func(ctx context.Context, issuer string) (string, bool)
}

func newHomeDomainLookups(live *metadata.LCMHomeDomainResolver, static func(issuer string) (string, bool), warnFn func(msg string, kv ...any)) homeDomainLookups {
	return homeDomainLookups{
		listing: metadata.ChainedHomeDomainBatch(live, static, warnFn),
		detail:  metadata.ObservedHomeDomainLookup(live, warnFn),
		static:  metadata.StaticHomeDomainFallback(live, static, warnFn),
	}
}

// redisConfidenceLooker reads the confidence.Score the aggregator writes to Redis. A miss returns
// (zero, false, nil) and the response omits confidence; read errors propagate for logging.
type redisConfidenceLooker struct{ rdb redis.UniversalClient }

func (r redisConfidenceLooker) LookupConfidence(ctx context.Context, asset, quote canonical.Asset, window time.Duration) (v1.PriceSnapshotConfidence, bool, error) {
	if r.rdb == nil {
		return v1.PriceSnapshotConfidence{}, false, nil
	}
	key := cachekeys.Confidence(asset, quote, window)
	raw, err := r.rdb.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return v1.PriceSnapshotConfidence{}, false, nil
	}
	if err != nil {
		return v1.PriceSnapshotConfidence{}, false, fmt.Errorf("confidence cache get %s: %w", key, err)
	}
	var score confidence.Score
	if err := json.Unmarshal(raw, &score); err != nil {
		return v1.PriceSnapshotConfidence{}, false, fmt.Errorf("confidence cache decode %s: %w", key, err)
	}
	return v1.PriceSnapshotConfidence{
		Confidence: score.Confidence,
		Factors: v1.ConfidenceFactors{
			ZScore:               score.Factors.ZScore,
			SourceCount:          score.Factors.SourceCount,
			Diversity:            score.Factors.Diversity,
			Liquidity:            score.Factors.Liquidity,
			CrossOracle:          score.Factors.CrossOracle,
			BaselineQuality:      score.Factors.BaselineQuality,
			CrossOracleChecked:   score.Factors.CrossOracleChecked,
			CrossOracleAgreement: score.Factors.CrossOracleAgreement,
			LiquidityMeasured:    score.Factors.LiquidityMeasured,

			TriangulationAgreement: score.Factors.TriangulationAgreement,
			TriangulationChecked:   score.Factors.TriangulationChecked,
			BaselineAgeDays:        score.Factors.BaselineAgeDays,
			BootstrapCapped:        score.Factors.BootstrapCapped,
		},
	}, true, nil
}

// dexTVLGateSurface labels the TVL refresh on the withheld metrics. It is distinct from
// "price_read" because the background refresh rate has nothing to do with request traffic.
const dexTVLGateSurface = "dex_tvl"

// dexTVLValueGate asks pricingguard.Gate whether a reserve leg's asset may carry a USD value,
// never a gate half directly (TestWithholdingGatesAreSpelledOnlyAtTheChokepoint), so TVL cannot
// drift from /v1/price. The scam verdict is quote-independent and refuses the whole asset.
type dexTVLValueGate struct {
	substance *pricingguard.SubstanceGate
	scam      *pricingguard.ScamGate
	usdPegs   []canonical.Asset
}

// buildDEXTVLValueGate returns a nil interface when neither guard was built. A struct literal
// would never compare equal to nil, so /v1/protocols would claim a substance screen that
// disable_substance_gate has switched off.
func buildDEXTVLValueGate(
	substance *pricingguard.SubstanceGate,
	scam *pricingguard.ScamGate,
	usdPegs []canonical.Asset,
) v1.TVLValueGate {
	if substance == nil && scam == nil {
		return nil
	}
	return dexTVLValueGate{substance: substance, scam: scam, usdPegs: usdPegs}
}

// Screens lists only the screens this deployment wired, scam directory first; a nil sub-guard allows everything, so naming it would claim a screen that never ran.
func (g dexTVLValueGate) Screens() []string {
	screens := make([]string, 0, 2)
	if g.scam != nil {
		screens = append(screens, v1.TVLScreenScamDirectory)
	}
	if g.substance != nil {
		screens = append(screens, v1.TVLScreenSubstanceFloor)
	}
	return screens
}

// ValueWithheld reports whether asset's reserves must contribute no USD value; native XLM is never withheld, and the per-quote fan-out is one question to the Gate so metrics count the asset once per refresh.
func (g dexTVLValueGate) ValueWithheld(ctx context.Context, asset canonical.Asset) bool {
	gate := pricingguard.Gate{Substance: g.substance, Scam: g.scam}
	return gate.AssetValueWithholding(ctx, asset, g.usdPegs, dexTVLGateSurface) != pricingguard.NotWithheld
}

// buildSubstanceGate maps [pricing_guard] onto the substance policy; nil (allow-everything) when
// disabled.
func buildSubstanceGate(cfg config.PricingGuardConfig, store *timescale.Store, logger *slog.Logger) *pricingguard.SubstanceGate {
	if cfg.DisableSubstanceGate {
		logger.Warn("pricing_guard: substance gate DISABLED by config — aggregated prices serve for every pair regardless of trailing market substance")
		return nil
	}
	pol := pricingguard.SubstancePolicyFromValues(
		cfg.SubstanceMinVolumeUSD, cfg.SubstanceMinBuckets,
		cfg.SubstanceMinSpanMinutes, cfg.SubstanceWindowHours)
	return pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{Policy: pol, Logger: logger})
}

// metadataStoreLookup adapts the store to metadata.AccountObservationLookup. Only a missing row
// means not observed; a NULL home_domain or is_removal row observes that there is none.
type metadataStoreLookup struct{ s accountObservationReader }

type accountObservationReader interface {
	LatestAccountObservationAtOrBefore(ctx context.Context, accountID string, asOfLedger uint32) (timescale.AccountObservation, error)
	LatestAccountObservationsAtOrBefore(ctx context.Context, accountIDs []string, asOfLedger uint32) (map[string]timescale.AccountObservation, error)
}

func (a metadataStoreLookup) HomeDomainAtOrBefore(ctx context.Context, issuer string, asOfLedger uint32) (metadata.IssuerHomeDomain, error) {
	row, err := a.s.LatestAccountObservationAtOrBefore(ctx, issuer, asOfLedger)
	if errors.Is(err, timescale.ErrNotFound) {
		return metadata.IssuerHomeDomain{}, nil
	}
	if err != nil {
		return metadata.IssuerHomeDomain{}, err
	}
	return observedHomeDomain(row), nil
}

func (a metadataStoreLookup) HomeDomainsAtOrBefore(ctx context.Context, issuers []string, asOfLedger uint32) (map[string]metadata.IssuerHomeDomain, error) {
	rows, err := a.s.LatestAccountObservationsAtOrBefore(ctx, issuers, asOfLedger)
	if err != nil {
		return nil, err
	}
	out := make(map[string]metadata.IssuerHomeDomain, len(rows))
	for issuer, row := range rows {
		out[issuer] = observedHomeDomain(row)
	}
	return out, nil
}

func observedHomeDomain(row timescale.AccountObservation) metadata.IssuerHomeDomain {
	hd := metadata.IssuerHomeDomain{Observed: true}
	if !row.IsRemoval && row.HomeDomain != nil {
		hd.Domain = *row.HomeDomain
	}
	return hd
}

// usdQuoteAsset is the implicit USD quote for 24h-ago anchors, equal to v1's defaultPriceQuote.
var usdQuoteAsset = func() canonical.Asset {
	a, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		panic("stellarindex-api: USD quote asset must parse: " + err.Error())
	}
	return a
}()

// storeChange24hReader serves the latest closed prices_1m bucket at or before now-24h; sql.ErrNoRows becomes v1.ErrChange24hUnavailable.
// On a literal miss it tries asset/<peg> from the operator-declared USD-pegged set (nothing on-chain quotes fiat:USD); decimals
// normalises the anchor (see normalizeChange24hAnchor).
type storeChange24hReader struct {
	s        *timescale.Store
	pegs     []canonical.Asset
	decimals aggregate.DecimalsLookup
	logger   *slog.Logger // nil → guard logging disabled
}

// change24hAnchorMaxStaleness is unbounded because ClosedVWAP1mAtOrBefore bounds nothing either.
const change24hAnchorMaxStaleness = time.Duration(math.MaxInt64)

// errChange24hAnchorGuarded: the anchor bucket exists but the guard refused it.
var errChange24hAnchorGuarded = errors.New("change_24h anchor refused by the serving-sanity guard")

func (r storeChange24hReader) USDPrice24hAgo(ctx context.Context, asset canonical.Asset) (string, error) {
	at := time.Now().Add(-24 * time.Hour)
	// Each anchor passes the serving-sanity guard; a refused bucket moves on to the next market,
	// so a manipulated minute is never the divisor of a percentage.
	guardedAnchor24h := func(ctx context.Context, pair canonical.Pair, at time.Time) (timescale.Vwap1mRow, error) {
		row, err := r.s.ClosedVWAP1mAtOrBefore(ctx, pair, at)
		if err != nil {
			return timescale.Vwap1mRow{}, err
		}
		served, ok := pricingguard.GuardServedVWAP1mAt(ctx, r.s, r.logger, pair, row, at, change24hAnchorMaxStaleness)
		if !ok {
			return timescale.Vwap1mRow{}, errChange24hAnchorGuarded
		}
		return served, nil
	}
	row, err := guardedAnchor24h(ctx, canonical.Pair{Base: asset, Quote: usdQuoteAsset}, at)
	if err == nil {
		return normalizeChange24hAnchor(r.decimals, row.VWAP, asset, usdQuoteAsset)
	}
	if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, errChange24hAnchorGuarded) {
		return "", err
	}
	// Walk the operator's USD pegs; the first non-error row wins.
	for _, peg := range r.pegs {
		// Fold through the alias registry, since the batch path passes a SAC spelling through
		// as typed and an exact-spelling skip would read a pair whose sides are one asset.
		if canonical.CanonicalAsset(peg).Equal(canonical.CanonicalAsset(asset)) {
			continue
		}
		pegRow, pegErr := guardedAnchor24h(ctx, canonical.Pair{Base: asset, Quote: peg}, at)
		if pegErr == nil {
			// Normalise against the peg the bucket was read from, not fiat:USD.
			return normalizeChange24hAnchor(r.decimals, pegRow.VWAP, asset, peg)
		}
	}
	return "", v1.ErrChange24hUnavailable
}

// change24hAnchorDigits is wide enough that scaling a small ratio down cannot round it to zero.
const change24hAnchorDigits = 20

// normalizeChange24hAnchor puts the raw 24h-ago bucket on the decimals scale of the normalised
// current price, else a flat 9-decimals market serves roughly +9900%. An unparseable flagged
// value reads as no anchor: a null change, never a wrong one.
func normalizeChange24hAnchor(lookup aggregate.DecimalsLookup, vwap string, base, quote canonical.Asset) (string, error) {
	baseDec := aggregate.ResolveDecimals(lookup, base)
	quoteDec := aggregate.ResolveDecimals(lookup, quote)
	if baseDec == quoteDec {
		return vwap, nil
	}
	raw, ok := new(big.Rat).SetString(vwap)
	if !ok || raw.Sign() <= 0 {
		return "", v1.ErrChange24hUnavailable
	}
	return aggregate.AdjustPrice(raw, baseDec, quoteDec).FloatString(change24hAnchorDigits), nil
}

// The restart backoff bounds: a 1 s floor recovers fast from a blip, and a 30 s ceiling stops a
// sustained outage hot-looping against Redis.
const (
	subscriberRestartMinBackoff = time.Second
	subscriberRestartMaxBackoff = 30 * time.Second
)

// subscriberRunner is the part of redispub.Subscriber the supervisor needs, so tests can inject a
// fake.
type subscriberRunner interface {
	Run(ctx context.Context) error
	Channel() string
}

// runSubscriberSupervised restarts sub.Run with exponential backoff so one pubsub hiccup cannot silence the closed-bucket feed. A panic
// unwinds past the loop, so the guard sits here: SSE falls back to heartbeats.
func runSubscriberSupervised(ctx context.Context, sub subscriberRunner, logger *slog.Logger) {
	defer recoverBackgroundWorker(logger, "stream-subscriber")
	runSubscriberSupervisedWithBackoff(ctx, sub, logger, subscriberRestartMinBackoff, subscriberRestartMaxBackoff)
}

// runSubscriberSupervisedWithBackoff takes the bounds as parameters so tests avoid production
// delays. A run that stayed up for maxBackoff resets to the floor, and it returns on cancellation
// or a nil error.
func runSubscriberSupervisedWithBackoff(ctx context.Context, sub subscriberRunner, logger *slog.Logger, minBackoff, maxBackoff time.Duration) {
	backoff := minBackoff
	for {
		started := time.Now()
		err := sub.Run(ctx)
		if err == nil || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return
		}
		if time.Since(started) >= maxBackoff {
			backoff = minBackoff
		}
		logger.Error("stream subscriber exited; restarting",
			"channel", sub.Channel(), "err", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// parseStreamingPairs converts the configured [base, quote] rows to Pairs, failing startup on any
// malformed row rather than dropping a pair the operator expected.
func parseStreamingPairs(rows [][]string) ([]canonical.Pair, error) {
	out := make([]canonical.Pair, 0, len(rows))
	for i, row := range rows {
		if len(row) != 2 {
			return nil, fmt.Errorf("row %d: expected [base, quote], got %d elements", i, len(row))
		}
		base, err := canonical.ParseAsset(row[0])
		if err != nil {
			return nil, fmt.Errorf("row %d base %q: %w", i, row[0], err)
		}
		quote, err := canonical.ParseAsset(row[1])
		if err != nil {
			return nil, fmt.Errorf("row %d quote %q: %w", i, row[1], err)
		}
		out = append(out, canonical.Pair{Base: base, Quote: quote})
	}
	return out, nil
}

func mkLogger(cfg config.ObsConfig) *slog.Logger {
	return obs.NewLogger(cfg, "stellarindex-api")
}

// warnUnsafeBind warns, without blocking startup, when the API listens on a public interface
// with no trusted proxy CIDRs.
func warnUnsafeBind(logger *slog.Logger, listenAddr string, trustedProxyCIDRs []string) {
	// SplitHostPort handles bracketed IPv6, so a public "[::]" bind is not missed.
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Unparseable, so the bind cannot be classified; stay silent.
		return
	}
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if loopback {
		return
	}
	// Empty host, 0.0.0.0 and :: all mean every interface.
	if host == "0.0.0.0" || host == "::" || host == "" {
		if len(trustedProxyCIDRs) == 0 {
			logger.Warn("SECURITY: API is listening on a public interface with no trusted proxy CIDRs configured — direct :PORT requests bypass any TLS/WAF you have in front. Bind to 127.0.0.1 OR populate trusted_proxy_cidrs with your reverse proxy's source range.",
				"listen", listenAddr,
				"docs", "https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/pre-launch-hardening.md")
			return
		}
		logger.Warn("API listening on a public interface; trusting forwarded headers from configured proxies. Confirm your reverse proxy strips client-supplied X-Forwarded-* headers before forwarding.",
			"listen", listenAddr,
			"trusted_proxy_cidrs", trustedProxyCIDRs)
	}
}

// warnCollapsedStreamCap warns when the per-IP SSE stream cap cannot tell clients apart. RemoteIP
// trusts X-Forwarded-For only from TrustedProxyCIDRs, so behind an unconfigured proxy every
// client shares one budget. Loopback binds are exempt.
func warnCollapsedStreamCap(logger *slog.Logger, listenAddr string, maxStreamsPerIP int, trustedProxyCIDRs []string) {
	if maxStreamsPerIP <= 0 || len(trustedProxyCIDRs) > 0 {
		return
	}
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return
	}
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return
	}
	logger.Warn("SECURITY: api.streaming.max_streams_per_ip is set but trusted_proxy_cidrs is empty — "+
		"if the API runs behind a reverse proxy, every client resolves to the proxy's single address and the "+
		"per-IP SSE stream cap collapses into ONE shared global budget instead of a per-client cap. Populate "+
		"trusted_proxy_cidrs with your reverse proxy's source range.",
		"listen", listenAddr,
		"max_streams_per_ip", maxStreamsPerIP,
		"docs", "https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/pre-launch-hardening.md")
}

// warnCollapsedAnonThrottle warns when the anonymous throttle cannot tell clients apart behind a proxy (empty trusted_proxy_cidrs shares
// one bucket, an availability self-DoS); only trusted_proxy_cidrs silences it.
func warnCollapsedAnonThrottle(logger *slog.Logger, authMode string, anonRateLimitPerMin int, trustedProxyCIDRs []string) {
	if anonRateLimitPerMin <= 0 || len(trustedProxyCIDRs) > 0 {
		return
	}
	if !authModeAdmitsAnonymous(authMode) {
		return
	}
	logger.Warn("SECURITY: api.anon_rate_limit_per_min is set but trusted_proxy_cidrs is empty — "+
		"if this API runs behind a reverse proxy, every anonymous client resolves to the proxy's single "+
		"address and the per-IP anonymous request throttle collapses into ONE shared budget for the whole "+
		"anonymous tier (an availability self-DoS). If behind a reverse proxy, set trusted_proxy_cidrs to "+
		"your proxy's source range.",
		"auth_mode", authMode,
		"anon_rate_limit_per_min", anonRateLimitPerMin,
		"docs", "https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/pre-launch-hardening.md")
}

// authModeAdmitsAnonymous reports whether anonymous requests reach the anonymous rate-limit
// bucket. apikey and sep10 reject them with 401 first.
func authModeAdmitsAnonymous(mode string) bool {
	switch mode {
	case "none", "apikey_optional":
		return true
	default:
		return false
	}
}

// parseFiatPeggedClassics resolves fiat_pegged_classic_assets into the asset_id to fiat Asset map; Validate already rejects malformed
// entries, so skipping is a backstop. Keys are re-canonicalised so lookup never depends on operator spelling.
func parseFiatPeggedClassics(raw map[string]string, logger *slog.Logger) map[string]canonical.Asset {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]canonical.Asset, len(raw))
	for rawAsset, ticker := range raw {
		a, err := canonical.ParseAsset(rawAsset)
		if err != nil || a.Type != canonical.AssetClassic {
			logger.Warn("fiat_pegged_classic_assets: skipping non-classic or malformed key",
				"raw", rawAsset, "err", err)
			continue
		}
		fiat, err := canonical.NewFiatAsset(ticker)
		if err != nil {
			logger.Warn("fiat_pegged_classic_assets: skipping unknown fiat ticker",
				"raw", rawAsset, "ticker", ticker, "err", err)
			continue
		}
		out[a.String()] = fiat
	}
	return out
}

// warnOpenCORS warns when CORS allows every origin while auth_mode permits credentials.
func warnOpenCORS(logger *slog.Logger, allowedOrigins []string, authMode string) {
	// Any "*" entry is wide open, because middleware.CORS echoes "*" to every origin regardless
	// of the other entries.
	hasWildcard := false
	for _, o := range allowedOrigins {
		if o == "*" {
			hasWildcard = true
			break
		}
	}
	if !hasWildcard {
		return
	}
	switch authMode {
	case "apikey", "apikey_optional", "sep10":
		logger.Warn("SECURITY: CORS allows every origin (\"*\") and auth_mode permits credentials — narrow [api].allowed_origins to your explorer / explorer hostnames before exposing the API publicly.",
			"auth_mode", authMode,
			"docs", "https://github.com/Stellar-Index/StellarIndex/blob/main/docs/operations/pre-launch-hardening.md")
	}
}

// requireEmailVerifiedOrNil returns the email-verification gate when
// signup_require_email_verification is on, else nil.
func requireEmailVerifiedOrNil(enabled bool) middleware.Middleware {
	if !enabled {
		return nil
	}
	return middleware.RequireEmailVerified()
}

// touchUsageMiddlewareOrNil returns the TouchUsage middleware only when both a keys store (the
// UPDATE) and Redis (the SETNX debounce) are present; otherwise last_used stays unset.
func touchUsageMiddlewareOrNil(keys platform.APIKeyStore, rdb redis.UniversalClient, logger *slog.Logger) middleware.Middleware {
	if keys == nil || rdb == nil {
		return nil
	}
	debouncer := auth.NewRedisTouchDebouncer(rdb, 0)
	return middleware.TouchUsage(keys, debouncer, logger.With("component", "touch-usage"))
}

// statusIdentity returns the region, deployment tier and heartbeat services for /v1/status. They
// come from config because a test net runs no aggregator, which a fixed list would report as
// degraded forever, and a fixed tier would label it production.
func statusIdentity(cfg config.Config) (region, deployment string, services []string) {
	return cfg.Region.ID, cfg.Region.Deployment, cfg.API.StatusServices
}

// sessionPeekerAdapter lets v1 read the session without importing dashboardauth.
type sessionPeekerAdapter struct{}

func (sessionPeekerAdapter) SessionFromContext(ctx context.Context) (v1.SessionInfo, bool) {
	sc, ok := dashboardauth.SessionFromContext(ctx)
	if !ok {
		return v1.SessionInfo{}, false
	}
	return v1.SessionInfo{
		UserID:          sc.User.ID.String(),
		Email:           sc.User.Email,
		DisplayName:     sc.User.DisplayName,
		Role:            string(sc.User.Role),
		IsStaff:         sc.User.IsStaff,
		EmailVerifiedAt: sc.User.EmailVerifiedAt,
		LastLoginAt:     sc.User.LastLoginAt,
		AccountID:       sc.Account.ID.String(),
		AccountName:     sc.Account.Name,
		AccountSlug:     sc.Account.Slug,
		AccountTier:     string(sc.Account.Tier),
		AccountStatus:   string(sc.Account.Status),
		// What auth enforces on a default-minted key, not the tier ceiling.
		AccountRateLimitPerMin:     sc.Account.EffectiveRateLimitPerMin(),
		AccountMonthlyRequestQuota: sc.Account.EffectiveMonthlyQuota(),
	}, true
}

// prewarmCaches keeps the heaviest read caches hot (cold fills take 5-10 s) on two cadences, since
// one combined cycle held a Postgres backend at high CPU: heavy every 5 min, light every 60 s. The
// panic guard sits inside this named function, which the panic-guard test resolves.
const (
	lightCadence    = 60 * time.Second
	issuersCacheTTL = 5 * time.Minute
)

func prewarmCaches(
	ctx context.Context,
	logger *slog.Logger,
	stats *v1.CachedSourcesStatsReader,
	markets *v1.CachedMarketsReader,
	assetsReader *v1.CachedAssetsReader,
	issuers *v1.CachedIssuersReader,
	verifiedAssetIDs []string,
	snaps *assetsListingSnapshots,
	networkStats *v1.CachedNetworkStatsReader,
	catalogueLen int,
) {
	defer recoverBackgroundWorker(logger, "prewarm-caches")
	heavyCadence := 5 * time.Minute

	// Both passes fire at startup concurrently: the slow heavy pass would otherwise delay the
	// listing keys users hit first, leaving them cold for tens of seconds after every restart.
	// They touch disjoint readers, so nothing races.
	var warm sync.WaitGroup
	warm.Add(2)
	go func() {
		defer warm.Done()
		defer recoverBackgroundWorker(logger, "prewarm-light-initial")
		prewarmLight(ctx, logger, markets, assetsReader, issuers, verifiedAssetIDs, snaps, networkStats, catalogueLen)
	}()
	go func() {
		defer warm.Done()
		defer recoverBackgroundWorker(logger, "prewarm-heavy-initial")
		prewarmHeavy(ctx, logger, stats)
	}()
	warm.Wait()

	heavyTick := time.NewTicker(heavyCadence)
	defer heavyTick.Stop()
	lightTick := time.NewTicker(lightCadence)
	defer lightTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heavyTick.C:
			prewarmHeavy(ctx, logger, stats)
		case <-lightTick.C:
			prewarmLight(ctx, logger, markets, assetsReader, issuers, verifiedAssetIDs, snaps, networkStats, catalogueLen)
		}
	}
}

func prewarmHeavy(
	ctx context.Context,
	logger *slog.Logger,
	stats *v1.CachedSourcesStatsReader,
) {
	// Per-call deadlines stop a slow query from stalling the whole
	// cycle. A missed cycle is fine — the cache entry survives at
	// its TTL and the next cycle retries.
	statsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Every /dexes/<source> page requests sparkline7d; unwarmed, that read
	// costs ~8.5s on the request path against ~1.1s without it.
	if err := stats.Refresh(statsCtx); err != nil {
		logger.Debug("prewarm sources stats failed", "err", err)
	}
}

// assetsPrewarmBatchTimeout bounds each of the two independent assetsReader batches in
// prewarmLight. It is a var so a test can shorten it.
var assetsPrewarmBatchTimeout = 20 * time.Second

func prewarmLight(
	ctx context.Context,
	logger *slog.Logger,
	markets *v1.CachedMarketsReader,
	assetsReader *v1.CachedAssetsReader,
	issuers *v1.CachedIssuersReader,
	verifiedAssetIDs []string,
	snaps *assetsListingSnapshots,
	networkStats *v1.CachedNetworkStatsReader,
	catalogueLen int,
) {
	// A 5-minute ceiling on the whole cycle: a shared 60 s budget across ~25 sequential calls
	// aborted the tail when the first few were slow, leaving the cache cold for hours. Cycles
	// cannot overlap, so a slow one only drops later ticks.
	mkCtx, mkCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer mkCancel()

	// ORDER MATTERS: the /v1/assets listing keys are warmed first, because they back the
	// explorer landing page and would otherwise queue behind the slow markets and pools reads.

	// /v1/coins backs the unified currencies listing. Its prependNative path subtracts one from
	// limit when no filter is set, so a limit=200 request looks up the Limit: 199 key; warming
	// 200 would fill a different one.
	assetsReaderCtx, assetsReaderCancel := context.WithTimeout(ctx, assetsPrewarmBatchTimeout)
	defer assetsReaderCancel()
	if _, err := assetsReader.ListAssetsExt(assetsReaderCtx, timescale.ListAssetsOptions{Limit: 199}); err != nil {
		logger.Debug("prewarm asset-catalogue listing failed", "err", err)
	}

	// The separate /v1/assets listing does the opposite arithmetic to /v1/coins; see
	// prewarmAssetListings.
	prewarmAssetListings(assetsReaderCtx, logger, assetsReader, snaps, catalogueLen)

	// /v1/assets/native is the most-trafficked single-asset page, and its heavy cold read is the
	// exact method the handler calls. It runs right after the other assetsReaderCtx calls, since
	// the loops below use the separate mkCtx and could spend that 20 s budget. It gets its own
	// deadline so a slow listing warm cannot make this batch silently no-op at Debug.
	assetDetailCtx, assetDetailCancel := context.WithTimeout(ctx, assetsPrewarmBatchTimeout)
	defer assetDetailCancel()
	if _, err := assetsReader.GetNativeAssetRow(assetDetailCtx); err != nil {
		logger.Debug("prewarm native asset-catalogue row failed", "err", err)
	}

	// Verified assets get the same warm-cache treatment as native, because canonical asset_id
	// requests otherwise cold-fill the heavy listing query. A miss is harmless since the user
	// request still fronts the cache.
	for _, assetID := range verifiedAssetIDs {
		prewarmAssetDetail(assetDetailCtx, logger, assetsReader, assetID)
	}
	// GetNativeAssetRow warmed one slot; this covers the other readers /v1/assets/native fans
	// out to.
	prewarmAssetDetail(assetDetailCtx, logger, assetsReader, "native")

	// The prewarm args must byte-match the key the handler looks up, or the slot is phantom and
	// every cold request still runs a 10-30 s scan. Both routes default to
	// MarketsOrderVolume24hDesc; /v1/markets "pair" is explicit-only but also warmed. The
	// /v1/pools half lives in prewarmPools.
	// One call per order fills the head slot every first page up to 200 is sliced from (the home
	// panel's 3 through the catalogue's 200). Alphabetical order is the stable-keyset order a
	// full-catalogue walker passes explicitly; the explorer passes order_by=volume_24h_usd_desc.
	for _, order := range []timescale.MarketsOrder{timescale.MarketsOrderPair, timescale.MarketsOrderVolume24hDesc} {
		if _, _, err := markets.DistinctPairsExt(mkCtx, "", 200, order); err != nil {
			logger.Debug("prewarm markets failed", "order", order, "err", err)
		}
	}
	prewarmPools(mkCtx, logger, markets)

	// Each /dexes/{source} page requests /v1/pools?source=<dex>&limit=100, a distinct cache key
	// per source; cold, it runs a 10-30 s trades scan. Warm the limit=100 default order.
	for _, src := range []string{"soroswap", "phoenix", "aquarius", "sdex", "comet"} {
		filter := timescale.PoolsFilter{Sources: []string{src}}
		if _, _, err := markets.AllPools(mkCtx, filter, "", 100, timescale.MarketsOrderVolume24hDesc); err != nil {
			logger.Debug("prewarm per-source pools failed", "source", src, "err", err)
		}
	}

	// Per-CEX markets prewarm: the explorer's /exchanges/{name} pairs table
	// fires `/v1/markets?source=<src>&limit=200` (volume-desc default), a
	// SourceMarkets slot distinct from the unfiltered DistinctPairsExt above.
	for _, src := range v1.CexSourceNames() {
		if _, _, err := markets.SourceMarkets(mkCtx, src, "", 200, timescale.MarketsOrderVolume24hDesc); err != nil {
			logger.Debug("prewarm per-source markets failed", "source", src, "err", err)
		}
	}

	// The issuers TTL is 5 minutes, and at low traffic most requests arrive after it lapses, so
	// an unwarmed slot made the cold fill the common case. The query cannot be indexed away.
	// The 60 s cadence is 5x inside the TTL, so one dropped cycle cannot expose a cold slot.
	prewarmIssuers(mkCtx, logger, issuers)

	// Without this every restart left the network strip paying the full aggregate inline on its
	// first request.
	prewarmNetworkStats(mkCtx, logger, networkStats)
}

// prewarmPools warms the unfiltered /v1/pools head slot. It is split out so a test can assert the
// key without the other readers. The arguments must byte-match handlePools: the registry DEX
// source set (not nil), an empty cursor and MarketsOrderVolume24hDesc.
func prewarmPools(ctx context.Context, logger *slog.Logger, markets *v1.CachedMarketsReader) {
	if markets == nil {
		return
	}
	filter := timescale.PoolsFilter{Sources: v1.DexSourceNames()}
	if _, _, err := markets.AllPools(ctx, filter, "", 200, timescale.MarketsOrderVolume24hDesc); err != nil {
		logger.Debug("prewarm pools failed", "err", err)
	}
}

// assetListingPrewarmLimits are the ?limit= values /v1/assets callers actually send, taken from
// observed traffic rather than explorer source, which missed the costly ?limit=50. include=sparkline
// is not a dimension, since it reads its own warm slots and leaves this cache key unchanged.
var assetListingPrewarmLimits = []int{1, 5, 10, 50, 100, 500}

// prewarmAssetListings warms the /v1/assets listing keys. The handler overfetches by one, so ?limit=50 looks up key 51; this takes
// user-facing limits and adds the +1 once. Variants persist to Redis (snaps, nil-safe) via ListAssetsExtAt.
func prewarmAssetListings(
	ctx context.Context, logger *slog.Logger, assetsReader *v1.CachedAssetsReader,
	snaps *assetsListingSnapshots, catalogueLen int,
) {
	if assetsReader == nil {
		return
	}
	for _, opts := range assetListingPrewarmOptions() {
		warmAssetListingOptions(ctx, logger, assetsReader, snaps, opts)
	}
	// The unified landing page fills its shortfall from the classic phase with a different limit
	// (userLimit-catalogueLen plus the overfetch), so mirror that or its first load misses.
	for _, opts := range catalogueFillPrewarmOptions(catalogueLen) {
		warmAssetListingOptions(ctx, logger, assetsReader, snaps, opts)
	}
}

// warmAssetListingOptions runs and persists one prewarm variant for both key sets.
func warmAssetListingOptions(
	ctx context.Context, logger *slog.Logger, assetsReader *v1.CachedAssetsReader,
	snaps *assetsListingSnapshots, opts timescale.ListAssetsOptions,
) {
	rows, observedAt, _, err := assetsReader.ListAssetsExtAt(ctx, opts)
	if err != nil {
		logger.Debug("prewarm assets listing failed",
			"limit", opts.Limit, "order", opts.Order, "err", err)
		return
	}
	snaps.save(ctx, opts, rows, observedAt)
}

// catalogueFillPrewarmOptions is the classic-phase key serveCatalogueUnifiedPage requests for each
// userLimit given catalogueLen rows, skipping limits the catalogue alone satisfies. It stays apart
// from assetListingPrewarmOptions because the two mirror different handlers' arithmetic.
func catalogueFillPrewarmOptions(catalogueLen int) []timescale.ListAssetsOptions {
	out := make([]timescale.ListAssetsOptions, 0, len(assetListingPrewarmLimits))
	for _, userLimit := range assetListingPrewarmLimits {
		remaining := userLimit - catalogueLen
		if remaining <= 0 {
			continue
		}
		out = append(out, timescale.ListAssetsOptions{
			Limit: remaining + v1.AssetsListOverfetchBy,  // mirror fetchClassicUnifiedRows
			Order: timescale.AssetsOrderVolume24hUSDDesc, // fetchClassicUnifiedRows never varies this
		})
	}
	return out
}

// assetListingPrewarmOptions is the exact set of cache keys to warm, separate from the IO so the
// +1 and the limit set can be tested without a database.
func assetListingPrewarmOptions() []timescale.ListAssetsOptions {
	orders := []timescale.AssetsOrder{
		timescale.AssetsOrderObservationCountDesc, // `order_by` absent
		timescale.AssetsOrderVolume24hUSDDesc,     // the explorer's home table
	}
	out := make([]timescale.ListAssetsOptions, 0, len(orders)*len(assetListingPrewarmLimits))
	for _, order := range orders {
		for _, userLimit := range assetListingPrewarmLimits {
			out = append(out, timescale.ListAssetsOptions{
				Limit: userLimit + v1.AssetsListOverfetchBy, // mirror handleAssetListFromAssets
				Order: order,
			})
		}
	}
	return out
}

// classicLakeSupplySweepGap is the pause between lake-supply sweeps, sized against the 30-minute cache TTL; it bounds how long a caller
// can still see the trustline-only supply figure.
const classicLakeSupplySweepGap = time.Minute

// prewarmClassicLakeSupply keeps the per-asset lake supply cache warm; cold, routes fall back to a trustline-only sum that misses
// claimable, LP and SAC supply. It takes listing options so the warmed set matches prewarmAssetListings, and sweeps then sleeps to avoid
// overlapping ClickHouse load.
func prewarmClassicLakeSupply(ctx context.Context, srv *v1.Server) {
	if srv == nil {
		return
	}
	for {
		srv.PrewarmClassicLakeSupply(ctx, assetListingPrewarmOptions())
		select {
		case <-ctx.Done():
			return
		case <-time.After(classicLakeSupplySweepGap):
		}
	}
}

// prewarmIssuerLimits are the limits real /v1/issuers callers send: 1 and 100 (explorer), 5
// (r1-smoke) and 100 (sla-probe, OpenAPI default, and the handler's fallback when limit is
// omitted). The reader serves every limit from one entry, so the list exists for the guard test.
var prewarmIssuerLimits = []int{1, 5, 100}

// prewarmIssuers keeps the /v1/issuers slots warm, split out so the limit set can be asserted.
func prewarmIssuers(ctx context.Context, logger *slog.Logger, issuers *v1.CachedIssuersReader) {
	if issuers == nil {
		return
	}
	for _, lim := range prewarmIssuerLimits {
		if _, err := issuers.ListIssuers(ctx, lim); err != nil {
			logger.Debug("prewarm issuers failed", "limit", lim, "err", err)
		}
	}
}

// prewarmNetworkStats keeps the /v1/network/stats SWR slot warm; split out like prewarmIssuers.
func prewarmNetworkStats(ctx context.Context, logger *slog.Logger, networkStats *v1.CachedNetworkStatsReader) {
	if networkStats == nil {
		return
	}
	if _, err := networkStats.GetNetworkStats(ctx); err != nil {
		logger.Debug("prewarm network stats failed", "err", err)
	}
}

// prewarmAssetDetail warms every SWR key /v1/assets/{id} fans out to with the handler's exact arguments (limit 5 for GetAssetTopMarkets
// must track its literal); misses cold-fill on the request path, so errors log at Debug.
func prewarmAssetDetail(ctx context.Context, logger *slog.Logger, assetsReader *v1.CachedAssetsReader, assetID string) {
	// No bail on the first failure: each reader has its own slot.
	prewarmAssetCall(ctx, logger, "GetAssetByAssetID", assetID, func() (any, error) {
		return assetsReader.GetAssetByAssetID(ctx, assetID)
	})
	prewarmAssetCall(ctx, logger, "GetAssetTopMarkets", assetID, func() (any, error) {
		return assetsReader.GetAssetTopMarkets(ctx, assetID, 5)
	})
	prewarmAssetCall(ctx, logger, "GetAssetPriceHistory24h", assetID, func() (any, error) {
		return assetsReader.GetAssetPriceHistory24h(ctx, assetID)
	})
	prewarmAssetCall(ctx, logger, "GetAssetPriceHistory7d", assetID, func() (any, error) {
		return assetsReader.GetAssetPriceHistory7d(ctx, assetID)
	})
	prewarmAssetCall(ctx, logger, "GetAssetMarketsCount", assetID, func() (any, error) {
		return assetsReader.GetAssetMarketsCount(ctx, assetID)
	})
	prewarmAssetCall(ctx, logger, "GetAssetTradeCount24h", assetID, func() (any, error) {
		return assetsReader.GetAssetTradeCount24h(ctx, assetID)
	})
	prewarmAssetCall(ctx, logger, "GetAssetATH", assetID, func() (any, error) {
		return assetsReader.GetAssetATH(ctx, assetID)
	})
}

// prewarmAssetCall logs a failed reader warm; any lets it cover the differing reader signatures.
func prewarmAssetCall(ctx context.Context, logger *slog.Logger, name, assetID string, fn func() (any, error)) {
	if _, err := fn(); err != nil {
		logger.Debug("prewarm asset call failed", "reader", name, "asset_id", assetID, "err", err)
	}
	_ = ctx // each fn already captures the context; arg kept for symmetry / future timeout pattern.
}

// storePriceAtReader serves /v1/price/at and /v1/price/changes via ClosedVWAPAtOrBefore (ErrNoRows to the sentinel). It carries the same
// withholding gates as wiring.StorePriceReader or these routes would republish prices /v1/price refuses; thin-market is asked at ts, not
// now.
type storePriceAtReader struct {
	s         *timescale.Store
	substance *pricingguard.SubstanceGate // nil → no thin-market gate
	scam      *pricingguard.ScamGate      // nil → no scam-issuer gate
	logger    *slog.Logger                // nil → guard logging disabled
}

func (r storePriceAtReader) PriceAt(
	ctx context.Context, pair canonical.Pair, ts time.Time, maxStaleness time.Duration,
) (string, time.Time, int, error) {
	if withheld := wiring.PriceWithheld(ctx, r.substance, r.scam, pair.Base, pair.Quote, "price_at", wiring.AsOfInstant(ts)); withheld != pricingguard.NotWithheld {
		return "", time.Time{}, 0, v1.PriceWithheldError(withheld)
	}
	row, err := r.s.ClosedVWAPAtOrBefore(ctx, pair, ts, maxStaleness)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
		}
		return "", time.Time{}, 0, err
	}
	// The finest rung is the raw prices_1m bucket with no outlier filter, so apply the same
	// trailing-baseline guard as /v1/price, on the 1m rung only (a 1-minute baseline cannot judge
	// coarser bars). A refused bucket is ErrPriceAtGuarded, not unavailable, so the handler says
	// withheld rather than no data.
	if row.Resolution == timescale.Granularity1m {
		served, ok := pricingguard.GuardServedVWAP1mAt(ctx, r.s, r.logger, pair,
			timescale.Vwap1mRow{Bucket: row.Bucket, VWAP: row.VWAP}, ts, maxStaleness)
		if !ok {
			return "", time.Time{}, 0, v1.ErrPriceAtGuarded
		}
		row.VWAP, row.Bucket = served.VWAP, served.Bucket
	}
	// observed_at is the bucket close, when its VWAP became final (ADR-0015).
	return row.VWAP, row.Bucket.Add(row.Resolution.BucketDuration()), row.Resolution.Seconds(), nil
}
