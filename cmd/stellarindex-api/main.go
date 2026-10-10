// Binary stellarindex-api is the public REST + SSE API server.
//
// Surface (registered in `internal/api/v1/server.go`'s
// `RegisterRoutes`):
//
//   - Pricing: /v1/price, /v1/price/batch (GET + POST),
//     /v1/price/tip, /v1/vwap, /v1/twap, /v1/observations.
//   - Historical: /v1/history, /v1/history/since-inception,
//     /v1/ohlc, /v1/chart.
//   - Catalogue: /v1/assets, /v1/assets/{id}, /v1/assets/{id}/metadata,
//     /v1/markets, /v1/pairs, /v1/sources.
//   - Oracle (SEP-40 passthrough): /v1/oracle/latest,
//     /v1/oracle/lastprice, /v1/oracle/prices,
//     /v1/oracle/x_last_price.
//   - Account self-service: /v1/account/me, /v1/account/usage,
//     /v1/account/keys (POST).
//   - SEP-10 web auth: /v1/auth/sep10/challenge,
//     /v1/auth/sep10/token.
//   - SSE streams: /v1/price/stream, /v1/price/tip/stream,
//     /v1/observations/stream.
//   - Operator-facing: /v1/healthz, /v1/readyz, /v1/version,
//     /metrics.
//
// The canonical list is the `s.mux.HandleFunc(...)` block in
// `internal/api/v1/server.go` and the OpenAPI spec at
// `openapi/stellar-index.v1.yaml`. CI (`lint-docs.sh §2`) keeps
// the two in lock-step.
//
// Flags:
//
//	-config PATH    TOML config file (required)
//	-dry-run        Load config, open connections, validate, exit.
//
// Environment overrides for secrets apply on top of the file. See
// internal/config/load.go LoadWithEnv.
//
// Graceful shutdown: SIGINT / SIGTERM cancel the root context; the
// HTTP server drains for up to 30 s before hard-exiting. Open SSE
// connections are signalled separately (see the RegisterOnShutdown call
// in run()) because they never go idle and would otherwise hold that
// drain open for its full budget.
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

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/confidence"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
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

// recoverBackgroundWorker turns a panic in a DETACHED background worker into a
// logged error instead of a process exit.
//
// An unrecovered panic in any goroutine terminates the whole Go process — it is
// not confined to that goroutine. So a panic in any of the nine
// background workers below (forex poller, TLS-cert probe, two cache refreshers,
// the supply/wealth prewarm, stream publisher, customer-webhook sender, usage
// rollup, signup reaper) would take the entire API down, including every healthy
// request in flight. None of those workers is on the serving path; none of them
// is worth an outage.
//
// The trade-off is stated rather than hidden: the panicking worker STOPS (its
// goroutine unwinds and is not restarted), so a crash-looping refresher becomes
// a silently stale cache instead of a crash-looping process. That is the better
// failure for a read API, but it is a real degradation — hence Error level and
// the full stack, so it cannot pass unnoticed.
//
// Deliberately NOT applied to the http.Server goroutine. If the listener dies
// the process has no reason to live, and recovering there would leave a running
// process serving nothing — strictly worse than crashing. Same reasoning as
// the SSE producers, which recover per-connection for exactly this reason.
//
// It reports through [worker.Report] rather than logging directly, which
// increments stellarindex_worker_panics_total{worker} BEFORE logging, so a
// dead worker is visible to alerting even if the log write fails.
func recoverBackgroundWorker(logger *slog.Logger, worker string) {
	// Note: recover() only works one frame deep, so this cannot simply
	// call worker.Recover — the deferred function IS this one.
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

	// Publish the process-wide asset alias registry BEFORE any read
	// path runs (W2). This is what makes classic↔SAC pairs declared in
	// [supply].sac_wrappers alias-complete on the served money paths:
	// a classic-keyed read (e.g. USDC) now also folds in its SAC-form
	// volume, with the SAC form ordered LAST so a thin Soroban pool
	// never outranks classic depth. Fail-closed — a malformed wrapper is
	// silently under-counted volume, so surface it at boot.
	aliasRegistry, err := canonical.NewAliasRegistry(cfg.Stellar.Passphrase(), cfg.Supply.SACWrappers)
	if err != nil {
		return fmt.Errorf("alias registry: %w", err)
	}
	canonical.InstallAliasRegistry(aliasRegistry)

	// Publish the network-derived P23/CAP-67 movements boundary process-wide
	// so the leaf /movements read paths (timescale.MovementsFloor) resolve to
	// the configured network's value — pubnet's default, or 1 (genesis) on a
	// reset test net — instead of the hardcoded pubnet const. Same start-up
	// install idiom as InstallAliasRegistry above.
	timescale.InstallMovementsFloor(cfg.Stellar.MovementsFloorLedger)

	// Publish the network passphrase so canonical SAC-address derivation
	// (Asset.SacContractID → /v1/assets contract_address, /supply, wasm view)
	// resolves to the CONFIGURED network's native/classic SAC instead of the
	// hardcoded pubnet const — otherwise a test-net asset detail would serve
	// the PUBNET contract address (a value wallets resolve holdings against and
	// send to). Same start-up install idiom as InstallAliasRegistry.
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

	// Pre-launch hardening warnings. These don't block startup —
	// the binary still serves — but operators get loud signals at
	// boot for risky default configurations. See
	// docs/operations/pre-launch-hardening.md.
	warnUnsafeBind(logger, cfg.API.ListenAddr, cfg.API.TrustedProxyCIDRs)
	warnOpenCORS(logger, cfg.API.AllowedOrigins, cfg.API.AuthMode)
	warnCollapsedStreamCap(logger, cfg.API.ListenAddr, cfg.API.Streaming.MaxStreamsPerIP, cfg.API.TrustedProxyCIDRs)
	warnCollapsedAnonThrottle(logger, cfg.API.AuthMode, cfg.API.AnonRateLimitPerMin, cfg.API.TrustedProxyCIDRs)

	// NOTE: the SEP-10 validator is constructed further down, AFTER `rdb`
	// exists (search "SEP-10 validator"). It cannot be built here: the
	// replay guard is Redis-backed, and a configured SEP-10
	// deployment MUST be replay-protected — so construction is deferred to
	// the point where the real Redis client is available and can be wired
	// in (or, when absent, cause a fail-CLOSED result). See the block below.

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// Storage — required. API reads from Timescale (+ Redis cache
	// in a follow-up PR). OpenServing applies a session-level
	// statement_timeout to every serving-pool connection — the SQL-side
	// backstop bounding a runaway
	// request-path query even if Go-side ctx cancellation races. Kept
	// longer than api.request_timeout so the app-layer deadline fires
	// first. Indexer/aggregator use the plain Open (their heavy batch
	// scans set their own longer SET LOCAL statement_timeout).
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

	// Redis — optional at the API layer. When reachable, it backs
	// the SEP-1 metadata cache; when not, the resolver falls
	// through to upstream fetches on every request (slow but
	// correct). We don't block startup on Redis — the readiness
	// probe reflects the truth.
	//
	// Production runs Sentinel mode (per ADR-0024); dev / single-
	// node runs single mode. redisclient.Build picks the branch
	// based on cfg.Storage.RedisSentinelAddrs.
	//
	// Exception: under -dry-run we ping explicitly. Without the
	// ping dry-run is a liar — both NewClient and NewFailoverClient
	// are lazy, so a bad addr / wrong password / wrong network
	// never surfaces. The whole point of dry-run is "does this
	// config actually work?" so a misconfig that only reveals
	// itself under real traffic defeats the flag.
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
	// With Redis absent, the auth throttles and
	// the passkey ceremony replay guard fall back to PER-PROCESS state. That is
	// correct on one instance but unsafe across several — a finish-login replayed
	// to a different instance bypasses the spent-ceremony set (session mint →
	// account takeover), and per-IP/email throttle caps multiply by the replica
	// count. A single process cannot detect its own fleet size, so require the
	// operator to ASSERT single-instance rather than silently downgrade a
	// session-minting path. Refuse to start otherwise. No-op when Redis is
	// configured (rdb != nil) — the Redis backends are fleet-safe.
	if err := assertRedisOrSingleInstance(rdb != nil, cfg.API.SingleInstance); err != nil {
		return err
	}
	// Register cancel AFTER the store + redis defers so LIFO
	// runs cancel FIRST on shutdown — the background workers (forex,
	// prewarm, marketcap, coverage refresher, stream sub/pub, webhook
	// delivery) see context cancellation and unwind BEFORE the
	// store/redis handles they query are closed. Registering cancel
	// before those defers would close the pool first, while
	// goroutines are still issuing queries against it. The HTTP server
	// has its own bounded Shutdown() at the end of run(); cancel running
	// last here doesn't change that path.
	defer cancel()

	// SEP-10 validator — wired regardless of auth_mode so the
	// /v1/auth/sep10/{challenge,token} endpoints serve. Built HERE, after
	// `rdb` exists, because the replay guard is Redis-backed and a
	// configured SEP-10 deployment MUST be replay-protected. The policy
	// (fail closed, degrade to Noop when auth_mode permits) lives in
	// resolveSEP10Validator so it is testable end-to-end. The signer
	// lookup reads the lake's current account state; the explorer reader
	// that serves it is dialled further down and bound there.
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
		// Assert the applied schema head is
		// at least what this binary was built against. Critical, so a
		// migrations-skipped/binary-swap mismatch drains the backend
		// (503) instead of serving stale data behind a 200. A dirty row
		// alone is not sufficient here — see the checker's doc.
		v1.NewSchemaVersionChecker(wiring.SchemaChecker{DB: store.DB()}),
		// Non-critical sibling that surfaces a dirty row as a
		// readyz "degraded" flag for operators to `force` without
		// draining the fleet over a migration that rolled back cleanly.
		v1.NewSchemaDirtyChecker(wiring.SchemaChecker{DB: store.DB()}),
		// ADR-0015: unguarded CAGG readers depend on materialized_only;
		// drain if an out-of-band ALTER makes a view serve its open bucket.
		v1.NewClosedBucketChecker(store),
	}
	if rdb != nil {
		checks = append(checks, wiring.RedisChecker{RDB: rdb})
	}

	// SEP-1 payloads are populated by `stellarindex-ops sep1-refresh`
	// (cron) into `issuers.sep1_payload`. The API reads from there
	// instead of fetching live HTTPS per request, which put /v1/assets/{id}
	// p95 above 4s — see ADR-0033.

	// Trusted-proxy CIDRs. The middleware
	// package's `requestCameViaTrustedProxy` consults this allow-list
	// before honouring `X-Forwarded-For`; an empty list means no
	// proxies are trusted and XFF is ignored entirely. Validation
	// already happened at config-load via internal/config/validate.go,
	// so a parse error here would be a programmer bug, not bad
	// operator input — surface as a hard startup failure.
	if err := middleware.SetTrustedProxyCIDRs(cfg.API.TrustedProxyCIDRs); err != nil {
		return fmt.Errorf("middleware.SetTrustedProxyCIDRs: %w", err)
	}
	if len(cfg.API.TrustedProxyCIDRs) > 0 {
		logger.Info("trusted-proxy CIDRs wired",
			"count", len(cfg.API.TrustedProxyCIDRs),
			"cidrs", cfg.API.TrustedProxyCIDRs)
	}

	// CORS — only wired when the operator configured allowed origins.
	// Empty list means same-origin only (no cross-origin clients).
	var cors middleware.Middleware
	if len(cfg.API.AllowedOrigins) > 0 {
		cors = middleware.CORS(middleware.CORSOptions{
			AllowedOrigins:      cfg.API.AllowedOrigins,
			AllowCredentials:    cfg.API.AllowCredentials,
			CredentialedOrigins: cfg.API.CredentialedOrigins,
			// CORSOptions' own default
			// (GET/HEAD/OPTIONS/POST) omits DELETE and PATCH, but the
			// v1 mux registers DELETE /v1/account/keys/{keyID} and
			// PATCH /v1/admin/accounts/{id} — a cross-origin browser
			// preflight for either gets an Access-Control-Allow-Methods
			// response missing the method it asked about, so the
			// browser blocks the actual request client-side even
			// though the origin itself is allowed and the server would
			// have served it.
			AllowedMethods: []string{"GET", "HEAD", "OPTIONS", "POST", "PATCH", "DELETE"},
		})
	}

	// Rate limit — separate buckets per tier.
	// `anon` is keyed by remote IP (with Subject.Identifier when an
	// auth middleware has stamped one); `auth` is keyed per-API-key
	// or per-Subject for authenticated tiers (apikey, SEP-10).
	//
	// The buckets are constructed
	// even when Redis is absent. ratelimit.New(nil, …) returns a Bucket
	// backed by an IN-PROCESS fixed-window limiter, so the anon/key
	// tiers stay ENFORCED (fail-closed, single-instance accounting)
	// rather than the stack running uncapped; an anonymous flood would
	// otherwise have no limiter at all. Single-instance accounting is correct
	// for the R1 single-instance deployment; a future multi-instance
	// deployment provides Redis and gets fleet-wide accounting back.
	// 0 is a valid, Validate()-accepted
	// value for either limit, but it means "this tier is completely
	// UNBOUNDED" (fail-open) — a config typo or a copy-pasted
	// dev-profile value silently disables abuse protection with no
	// signal anywhere. Warn loudly at boot so the choice is visible
	// even though it isn't rejected (an operator may have a
	// legitimate reason — e.g. auth_mode=apikey with a downstream
	// WAF doing the limiting).
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
		// All rate-limit buckets on the
		// in-process fallback use PER-PROCESS state — a multi-instance
		// deployment multiplies every limit by instance count (each
		// process enforces its own independent budget) instead of
		// sharing one fleet-wide budget the way the Redis path does.
		// That's silently wrong the moment R1 becomes multi-instance,
		// so this is a WARN (not Info) to keep it loud rather than
		// buried in normal boot chatter.
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

	// Failed-auth throttle. Auth runs
	// BEFORE the rate-limit middleware — deliberately, so per-tier
	// limits key off the AUTHENTICATED subject — which means a request
	// with an INVALID credential is rejected (401) before it ever
	// reaches the limiter, leaving credential-stuffing / key-guessing
	// unthrottled. This dedicated bucket throttles ONLY credential
	// FAILURES, keyed on the client IP and the presented key prefix, inside the Auth
	// middleware; valid requests are untouched and still limited by
	// subject downstream. Redis-backed when available, in-process
	// fallback otherwise (same in-process fallback as the rate-limit tiers above). Only engaged
	// when an auth mode is active (mode=none never fails auth).
	var failedAuthLimiter *ratelimit.Bucket
	if cfg.API.FailedAuthRateLimitPerMin > 0 {
		failedAuthLimiter = ratelimit.New(rdb, cfg.API.FailedAuthRateLimitPerMin, time.Minute,
			ratelimit.WithDwellTime(cfg.API.RateLimitDwell))
		// Same per-process-state caveat as
		// the rate-limit tiers above — a multi-instance deployment
		// without Redis multiplies the failed-auth (credential-
		// stuffing) budget by instance count instead of sharing it.
		if rdb == nil {
			logger.Warn("failed-auth throttle is in-process (single-instance fallback — no Redis); "+
				"a multi-instance deployment multiplies the credential-stuffing budget by instance count",
				"failed_auth_per_min", cfg.API.FailedAuthRateLimitPerMin)
		}
	}

	// Per-account usage counter — daily INCRs alongside rate-limit
	// for /v1/account/usage. Only constructed when Redis is wired; the
	// middleware treats a nil counter as disabled.
	// The month-to-date meter reconciles each day against usage_daily,
	// so an evicted Redis day key cannot read as a quiet day.
	var usageCounter *usage.Counter
	if rdb != nil {
		usageCounter = usage.New(rdb, usage.WithDurableDays(store))
	}

	// authMW is built later (after the dashboard bundle) so the
	// Postgres backend can borrow the same platform stores. Forward-
	// declared as nil here so the linter sees the read in v1.Options
	// before the assignment below.
	var authMW middleware.Middleware

	// Account store backs the self-service POST/GET/DELETE
	// /v1/account/keys surface. Only wired when Redis is reachable —
	// without Redis there's nowhere to persist the issued record. The
	// handler then returns 503 for that path; /me + /usage still serve
	// from the request-context Subject without the store.
	//
	// This store is Redis-backed, but under
	// auth_backend=postgres the runtime API-key VALIDATOR authenticates
	// from the platform.api_keys (Postgres) table. The two stores are
	// disjoint, so serving /v1/account/keys from Redis under the Postgres
	// backend is a split-brain: a key minted here would never authenticate,
	// and — the security-relevant half — a DELETE here would hard-remove the
	// Redis record while the live Postgres row keeps authenticating, i.e. a
	// revocation that silently no-ops. So we leave the store nil (the route
	// 503s) under the Postgres cutover; the dashboard keys surface
	// (/v1/dashboard/keys, Postgres-backed, invalidates the validator cache
	// on revoke) is the single source of truth there. r1 runs the default
	// redis backend, where writer and validator agree.
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

	// Per-IP signup throttle, separate
	// from the global rate-limit middleware. Default 5/hour/IP —
	// tight enough to block bulk-mint, loose enough that an
	// operator onboarding a small team through a single shared
	// egress completes normally. The cap is a compiled default
	// (auth.SignupIPThrottleOptions), not a config key.
	var signupIPThrottle v1.SignupIPThrottle
	if rdb != nil {
		signupIPThrottle = auth.NewRedisSignupIPThrottle(rdb, auth.SignupIPThrottleOptions{})
	} else {
		// A nil signupIPThrottle on Redis-less deployments would leave
		// /v1/register
		// bounded ONLY by the global anonymous rate limit (60/min), up
		// to 3,600 accounts per hour from one IP. Same
		// in-process single-instance fallback posture as the
		// magic-link throttle just below.
		signupIPThrottle = newInProcessSignupIPThrottle()
		logger.Warn("signup IP throttle is in-process (single-instance fallback — no Redis); " +
			"the per-IP signup cap is NOT shared across instances")
	}

	// Divergence lookup adapter. Only wired when Redis is reachable
	// (the aggregator's cached results live there). The API binary
	// builds NO References — it only ever calls LookupCached, never
	// RefreshPair, so constructing CoinGecko/Chainlink/oracle
	// references here would be dead weight that also mislead the
	// startup log into claiming a capability (running the cross-check)
	// this binary doesn't have. The aggregator is the sole RefreshPair
	// caller (internal/aggregate/orchestrator/divergence_refresh.go);
	// see DivergenceConfig's doc comment.
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

	// Home-domain lookups per ADR-0021; see newHomeDomainLookups for why
	// the detail surfaces get the chain split around their live read.
	homeDomainLookup := newHomeDomainLookups(
		metadata.NewLCMHomeDomainResolver(metadataStoreLookup{s: store}),
		cfg.Metadata.HomeDomainFor,
		func(msg string, kv ...any) {
			logger.With("component", "metadata-lcm").Warn(msg, kv...)
		},
	)

	// Freeze looker — reads the freeze:<asset>:<quote> cache
	// markers the aggregator's freeze.Writer publishes (ADR-0019
	// Phase 1 + 2 anomaly response). Without this wiring the API's
	// `flags.frozen` is permanently false regardless of what the
	// aggregator's anomaly detector decided. Nil rdb leaves the
	// looker nil, matching the rest of the redis-dependent options
	// — a deployment without Redis still serves prices, just
	// without freeze visibility.
	var freezeLooker v1.FrozenLooker
	if rdb != nil {
		fl, err := freeze.NewLooker(rdb)
		if err != nil {
			return fmt.Errorf("freeze looker: %w", err)
		}
		freezeLooker = fl
		logger.Info("freeze looker wired")
	}

	// Streaming Hub — backs /v1/price/stream's closed-bucket SSE
	// surface. Constructed unconditionally so the handler stops
	// returning 503; producer wiring (the per-pair publisher)
	// activates only when [api.streaming].pairs is non-empty.
	// The Redis pub/sub subscriber is wired against this
	// Hub further down (gated on rdb != nil).
	hub := streaming.NewHub(cfg.API.Streaming.BufferSize)
	// Topic retention overrides:
	// api.streaming.topic_idle_ttl / max_topics drive SetTopicIdleTTL/
	// SetMaxTopics, defaulting to the compiled-in values (15m / 4096) so
	// behaviour is unchanged unless an operator opts into something else.
	hub.SetTopicIdleTTL(cfg.API.Streaming.TopicIdleTTL)
	hub.SetMaxTopics(cfg.API.Streaming.MaxTopics)

	// Per-IP concurrent-SSE-connection cap. The
	// global cap alone lets one client hold the whole budget; this bounds
	// each client. Key off the trusted-proxy-aware resolver (the same one
	// the rate limiter uses) so the cap tracks the real client behind
	// Caddy, not the proxy's single address. SetTrustedProxyCIDRs already
	// ran above, so RemoteIP honours the configured proxy allow-list.
	streaming.SetMaxStreamsPerIP(cfg.API.Streaming.MaxStreamsPerIP)
	streaming.SetStreamClientIPResolver(middleware.RemoteIP)
	// Global concurrent-SSE-connection cap: api.streaming.max_concurrent_streams
	// drives SetMaxConcurrentStreams, defaulting to the package-level 8192 so
	// behaviour is unchanged unless an operator opts into a different value.
	streaming.SetMaxConcurrentStreams(cfg.API.Streaming.MaxConcurrentStreams)

	// Serving-side thin-market substance gate ([pricing_guard]). One
	// gate instance is shared by
	// every raw prices_1m serving path in this binary — the /v1/price
	// reader, the SEP-40 passthrough, the GlobalAssetView headline, the
	// v1 server's tip path, /v1/anomalies, /v1/divergence and /v1/changes
	// (via storedMarketGate / changeSummaryWithheld) — plus the
	// aggregator binary's freeze and divergence webhooks (via
	// priceWithholding.withheld) — so all of those surfaces
	// agree on which pairs are too thin to price. This binary's own
	// Redis-VWAP fallback (tryRedisVWAPFallback, internal/api/v1/price.go)
	// is NOT routed through this gate and remains the one ungated
	// surface; unifying pricingguard into one exported
	// Gate type walked by AST across cmd/* is the still-open follow-up.
	substanceGate := buildSubstanceGate(cfg.PricingGuard, store, logger)
	// Scam-pricing gate: withhold the aggregated price for issuers flagged
	// scam-class in the curated account directory, on EITHER leg of the
	// pair. Wired at the reader seam, which covers the reader-backed
	// surfaces (/v1/price, /v1/price/batch, /v1/price/at, the SEP-40
	// oracle price paths, the asset headline) via wiring.PriceWithheld; the
	// surfaces that compute their own price — /v1/vwap, /v1/twap,
	// /v1/chart, /v1/price/tip — consult the same gate from their
	// handlers, because they never touch this reader (see scam.go's
	// "WHERE IT IS CONSUMED" note; claiming one seam covered them all is
	// how they went ungated for a release). Nil when the directory reader
	// is absent.
	scamGate := pricingguard.NewScamGate(store, pricingguard.ScamGateOptions{Logger: logger})
	priceReader := wiring.StorePriceReader{S: store, Logger: logger, Substance: substanceGate, Scam: scamGate}

	// Oracle reader — Redis-cached read-through wrapper around the
	// store reader. /v1/oracle/latest's DISTINCT ON (source) sort
	// is expensive (~580 ms p95 on R1's oracle_updates volume); a
	// 30 s cache stays inside the oracle push interval and absorbs
	// polling fan-out. Falls back to direct-store reads when Redis
	// is missing.
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
	// In-process TTL + single-flight layer on top of the Redis cache.
	// The cache was added because /v1/oracle/latest p95 exceeded the 200 ms
	// SLO. The Redis cache helps the cross-instance read pattern but
	// has no single-flight, so concurrent misses stampede upstream;
	// during a Redis MISCONF cascade every read falls
	// straight through to the DB. The in-process layer is fast, has
	// single-flight, and survives Redis being unavailable.
	const oracleInProcessTTL = 3 * time.Second
	oracleReader = v1.NewCachedOracleReader(oracleReader, oracleInProcessTTL)
	logger.Info("oracle reader wrapped with in-process cache",
		"ttl", oracleInProcessTTL.String())

	// Catalogue readers — same Redis read-through pattern for the
	// /v1/assets and /v1/markets list endpoints. Both derive from
	// 14-day-window aggregations over the trades hypertable
	// (~450-500 ms cold), so a 60 s cache absorbs polling fan-out
	// without delaying new-listing surfacing more than once-a-minute.
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

	// Status backend — points /v1/status at a local Prometheus when
	// configured. Empty URL leaves the endpoint serving an
	// in-process surface (region label + uptime only).
	var statusBackend v1.StatusBackend
	if cfg.API.PrometheusURL != "" {
		statusBackend = &v1.PrometheusStatusBackend{
			URL:    cfg.API.PrometheusURL,
			Client: &http.Client{Timeout: 2 * time.Second},
		}
		logger.Info("status backend wired", "prometheus_url", cfg.API.PrometheusURL)
	}

	// Customer-dashboard email-code/magic-link auth + key-management
	// surface. Empty BaseURL leaves the dashboard auth flow off — the
	// routes simply aren't mounted. This is the expected pre-launch
	// shape: the explorer renders a signed-out surface until the
	// operator configures Resend + the dashboard base_url (the in-site
	// dashboard at stellarindex.io/account).
	dashboardBundle, err := buildDashboardBundle(cfg.API.Dashboard, store.DB(), rdb, logger)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}

	// Forex shim — periodic fetch of fiat rates from massive.com.
	// Cache is in-memory; worker installs a snapshot every
	// [external.massive] refresh_interval (default 1h).
	// Backs /v1/currencies. Worker survives upstream failures
	// (logs at warn) — the cache holds the prior snapshot.
	//
	// API key is [external.massive] api_key, normally set by the
	// MASSIVE_API_KEY env var (systemd EnvironmentFile=/etc/default/stellarindex
	// on r1). When empty, the worker still constructs but every fetch
	// returns 401; a stale cache stays in place and /v1/currencies
	// serves "warming up" until the key is provided.
	forexCache := forex.NewCache()
	forexInterval, clamped := cfg.External.Massive.EffectiveRefreshInterval()
	if clamped {
		logger.Warn("forex: external.massive.refresh_interval below floor — clamped",
			"configured", cfg.External.Massive.RefreshInterval, "using", forexInterval)
	}
	forexWorker := forex.NewWorker(
		forex.NewClient(cfg.External.Massive.APIKey),
		forexCache,
		logger.With("component", "forex"),
		forexInterval,
	)
	// Wire fx_quotes persistence — every refresh tick writes the
	// latest rates + 7d history to the hypertable so /v1/currencies
	// can serve historical charts beyond the in-memory window.
	forexWorker = forexWorker.WithWriter(&wiring.ForexQuoteWriter{Store: store}).
		WithReader(&wiring.ForexQuoteWriter{Store: store}).
		WithFixingWriter(&wiring.ForexQuoteWriter{Store: store})
	// Standby FX source. `massive` is a PAID feed and was the ONLY series
	// in stellarindex_external_fx_last_quote_unix,
	// so a 401/429/subscription lapse silently broke every fiat-quoted
	// pair once the 7-day forex-snap lookback expired. ECB is free,
	// keyless and authoritative — narrower coverage (~30 currencies,
	// working days only) but rates rather than none. Only consulted when
	// the primary fails; the source label follows the feed that served.
	forexWorker = forexWorker.WithFallbacks(forex.ECBProvider{})
	// Held outside the serving chain: the worker stores it and never
	// fetches it, so enabling it spends no quota and changes no served rate.
	if oxr := cfg.External.OpenExchangeRates; oxr.Enabled {
		forexWorker = forexWorker.WithCorroborator(forex.OpenExchangeRatesProvider{AppID: oxr.AppID, Endpoint: oxr.Endpoint})
	}

	// Dry-run exits HERE — before the first `go` statement and
	// before the heavy background SQL (backfill-coverage refresh,
	// self-prewarm). Dry-run's contract is "load config + open
	// connections + validate, then exit"; it must NOT spin up the
	// forex / prewarm / marketcap / coverage / stream goroutines or
	// run their first-pass queries. Everything above
	// this point is pure construction + connection validation, which
	// is exactly what dry-run is meant to exercise.
	if dryRun {
		logger.Info("dry-run complete — exiting")
		return nil
	}

	// bgWG tracks every detached background worker started below so
	// shutdown can wait for them instead of vanishing underneath them:
	// run() would return as soon as httpSrv.Shutdown
	// came back and the process would exit with workers mid-flight — the
	// customer-webhook sender could be between "customer accepted the
	// POST" and "MarkDelivered", which is exactly how a delivery gets
	// repeated on the next boot. The wait shares one deadline with the
	// listener drain — see the wait itself at the end of run() — so a
	// wedged worker delays exit but cannot prevent it.
	//
	// The three warmers started as named functions (prewarmCaches,
	// selfPrewarmAssetEndpoints, runSubscriberSupervised) deliberately
	// do NOT join: they hold no durable write to finish — cache fills
	// and self-issued GETs — so waiting on them would lengthen every
	// deploy's downtime for nothing.
	var bgWG sync.WaitGroup

	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "forex-worker")
		if err := forexWorker.Run(rootCtx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("forex worker exited", "err", err)
		}
	}()

	// Auth — translate the configured auth_mode + auth_backend into
	// the middleware. auth_mode=none yields nil (server stack omits
	// it; downstream code treats absence-of-Subject as anonymous).
	// Postgres backend opt-in requires the dashboard bundle (which
	// owns the platform store handles).
	authOpts := authValidatorOptions{
		Backend:           cfg.API.AuthBackend,
		Rdb:               rdb,
		PostgresValidator: dashboardBundle.pgValidator,
		SEP10:             sep10Validator,
		FailedAuthLimiter: failedAuthLimiter,
	}
	if dashboardBundle.accounts != nil {
		// The account kill switch must bite on the DEFAULT
		// redis backend too, not just the Postgres validator —
		// otherwise suspending an account revokes dashboard access
		// while its API keys keep serving.
		authOpts.AccountStatus = dashboardBundle.accounts
	}
	authMW = buildAuthMiddleware(cfg.API.AuthMode, authOpts, logger)

	// Hoist cache instances out of the Options literal so the
	// prewarm goroutine can hammer them on startup + on per-query-
	// cost cadences — keeps every cold-cache miss off the user's
	// path. The first /exchanges or /dexes pageload after a binary
	// restart now hits a warm cache.
	//
	// TTLs stay inside the
	// prewarm cadence with headroom. sources_stats query takes ~8s
	// on a 3-month dataset and scales linearly with data depth —
	// 10min TTL + 5min prewarm cadence means the next refresh
	// fires well before TTL expiry, and a delayed refresh still
	// serves a warm entry. Markets/pools/assetsReader are sub-second
	// individually but the prewarm loop runs 12+ variants per
	// cycle; 2min TTL + 60s prewarm cadence is the same
	// double-cushion pattern.
	cachedSourcesStats := v1.NewCachedSourcesStatsReader(store, 10*time.Minute)
	cachedMarketsReader := v1.NewCachedMarketsReader(marketsReader, 2*time.Minute)
	cachedAssetsReader := v1.NewCachedAssetsReader(store, 2*time.Minute)
	// `/v1/issuers` p95 was ~404ms (over the
	// 200ms SLO target). EXPLAIN ANALYZE on r1 showed the listing's
	// HashAggregate-over-58k-issuers + top-N heapsort takes ~196ms
	// in PG alone before JSON marshalling. No index helps (full
	// aggregate over both tables is mandatory). 5min TTL is the
	// "verified-issuer catalogue moves on human timescale" knob —
	// same rationale as cachedSourcesStats.
	cachedIssuersReader := v1.NewCachedIssuersReader(store, issuersCacheTTL)
	// /v1/network/stats is the slowest /v1 route (~485ms p95 on r1 — a
	// network-wide 24h aggregate over the served tier) and feeds the
	// explorer's network strip. SWR with a 30s TTL keeps it off the
	// request path; the trailing-24h figures don't move materially in 30s.
	cachedNetworkStats := v1.NewCachedNetworkStatsReader(store, 30*time.Second)

	usdPegs := cfg.Trades.USDPeggedClassics(logger)
	fiatPegs := parseFiatPeggedClassics(cfg.PricingGuard.FiatPeggedClassicAssets, logger)

	// Load the verified-currency catalogue.
	// Failure here is fatal — the seed YAML is embedded; a parse
	// error means a code change broke the build artifact, not an
	// operator misconfiguration. Loaded BEFORE the prewarm goroutine
	// because prewarmCaches now uses it to extend canonical
	// asset_id prewarming (if it loaded after the goroutine started,
	// prewarmLight would only know native and every other canonical-form
	// asset_id lookup would miss cache and pay the ~3s getAssetBySlugSQL
	// cold cost).
	verifiedCurrencies, err := currency.LoadEmbedded()
	if err != nil {
		return fmt.Errorf("load verified-currency catalogue: %w", err)
	}
	logger.Info("verified-currency catalogue loaded", "entries", len(verifiedCurrencies.All()))

	// Extract the Stellar-network canonical asset_ids the verified-
	// currency catalogue points at. Each entry feeds an additional
	// GetAssetByAssetID prewarm call so a programmatic client hitting
	// /v1/assets/USDC-GA5Z…, /v1/assets/EURC-GDH…, etc. lands on a
	// warm cache instead of cold-filling the heavy
	// `listAssetsBaseSelect` whole-asset-universe CTE chain on every
	// canonical-form request. Excludes native (already prewarmed by
	// the GetNativeAssetRow path) and empty AssetIDs (the rare
	// off-Stellar networks where a verified currency exists but has
	// no Stellar issuance yet).
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

	// Boot seed for the /v1/assets listing — BEFORE the listener
	// and before the prewarm. On r1 the HTTP listener is up 12 ms after
	// "starting" and the first browser request lands at +4.0 s, while a
	// cold listing fill takes ~11 s, so nothing that races the listener
	// can help; the only way to answer that request quickly is to have
	// an entry already. Seeding here also means the prewarm's own first
	// pass takes the cache's stale-serve branch (detached refresh) —
	// the same shape it takes at every steady-state TTL lapse — instead
	// of blocking sequentially on twelve cold aggregates.
	//
	// Bounded and entirely optional: a cold, missing or unreachable
	// Redis seeds nothing and leaves the cold-fill behaviour
	// exactly as it was.
	listingSnapshots := newAssetsListingSnapshots(rdb, logger.With("component", "assets-listing-snapshot"))
	seedCtx, seedCancel := context.WithTimeout(rootCtx, assetsListingSnapshotBudget)
	seeded, requested := seedAssetListingsFromSnapshots(seedCtx, logger, cachedAssetsReader, listingSnapshots)
	seedCancel()
	logger.Info("assets listing boot seed",
		"seeded", seeded, "requested", requested,
		"max_age", assetsListingSeedMaxAge.String())

	// StellarIssued(), not All(): the unified listing's catalogue phase
	// (serveCatalogueUnifiedPage) only ever serves Stellar-issued entries,
	// so that is the count its classic "remaining" fill
	// is computed against — see catalogueFillPrewarmOptions.
	go prewarmCaches(rootCtx, logger.With("component", "prewarm"), cachedSourcesStats, cachedMarketsReader, cachedAssetsReader, cachedIssuersReader, verifiedAssetIDs, listingSnapshots, cachedNetworkStats, len(verifiedCurrencies.StellarIssued()))

	// TLS cert expiry self-probe. Public
	// TLS is fronted by Caddy + Let's Encrypt with auto-renewal 30d
	// before expiry, but a silent renewal failure (DNS, rate limit,
	// ACME quota) would otherwise only surface at cert expiry. The
	// probe emits `stellarindex_tls_cert_not_after_unix{host}` on a
	// 6h cadence; the matching alert in
	// deploy/monitoring/rules/api.yml fires at < 14 days remaining.
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

	// Per-source backfill coverage cache. The underlying SQL is 2-3s
	// on a populated trades hypertable so it can't run inline from
	// /v1/diagnostics/ingestion's request path. First refresh runs
	// in the background — endpoint reports an empty coverage section
	// until it completes (within ~5s of process start). Subsequent
	// refreshes happen on the v1.CoverageRefreshInterval cadence.
	backfillCoverageCache := v1.NewCoverageCache(store, logger.With("component", "backfill-coverage"))
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "backfill-coverage-cache")
		// Refresh timeout is 2 min: the per-source coverage query
		// (BackfillCoverageStats) does ~13 sources × (ts-ordered
		// LIMIT 1 earliest/latest + 24h count) plus two shared
		// scalars (approximate_row_count + 24h total). On r1's
		// ~2700-chunk trades hypertable that totals ~40-90s. This is
		// a BACKGROUND goroutine — the timeout never bounds an API
		// request, only how long one refresh attempt may run before
		// the next CoverageRefreshInterval (5 min) tick. 2 min sits
		// comfortably below the 5-min interval so refreshes never
		// stack. 30s was too short for the old
		// `GROUP BY source` query (and even the rewritten per-source
		// form on sdex), leaving the snapshot permanently
		// "pending".
		const coverageRefreshTimeout = 2 * time.Minute

		// Initial population — block-with-timeout so the first
		// status-page poll after restart sees data sooner than the
		// next ticker boundary.
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

	// Read-time dex-nonstandard-decimals serving guard (confirmed
	// production bug — see docs/operations/runbooks/
	// dex.md). Mirrors `nonstandard_decimals_assets`
	// (migration 0093, upserted by the aggregator's decimals-guard sweep)
	// in-process so /v1/price, /v1/vwap, /v1/history, /v1/ohlc can decline
	// a pair with a confirmed-offending leg without a per-request DB
	// round trip. Table is tiny (offenders should be near-zero), so the
	// full read is cheap; refresh cadence controls how quickly a fix
	// (row removed) or a new confirmation propagates.
	//
	// The first load runs inline, before any handler, prewarm or stream
	// publisher can read the cache, and readiness stays 503 until one load
	// has succeeded: a cold cache serves a confirmed offender's raw price.
	// (A hard-fail-on-boot variant of this guard was tried and reverted:
	// it puts systemd into a restart loop on a Postgres blip instead of
	// paging through the readiness signal below.)
	nonstandardDecimalsCache := v1.NewNonstandardDecimalsCache(store, logger.With("component", "nonstandard-decimals-cache"))
	checks = append(checks, wiring.PrimeNonstandardDecimalsCache(rootCtx, nonstandardDecimalsCache, logger))
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "nonstandard-decimals-cache")
		runRefreshLoop(rootCtx, nonstandardDecimalsCache.Refresh, v1.NonstandardDecimalsRefreshInterval,
			wiring.NonstandardDecimalsRefreshTimeout, logger, "nonstandard-decimals cache periodic refresh")
	}()

	// Live per-token supply from the decode-at-ingest supply_flows lake
	// (ADR-0034) backs GET /v1/assets/{id}/supply. Optional: when ClickHouse
	// isn't configured, the reader stays nil and the endpoint 503s. A failed
	// dial is non-fatal — the rest of the API still serves. Both lake
	// readers are dialled concurrently inside one short retry window (see
	// dialLakeReadersAtBoot).
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
			tokenSupplyReader = sr
			logger.Info("token supply reader wired (ClickHouse supply_flows)", "addr", addr)
		}
	}

	// Network-explorer reader (ADR-0038) — serves /v1/ledgers, /v1/tx,
	// /v1/operations, /v1/contracts, /v1/search from the certified lake.
	// Optional + non-fatal, same posture as the supply reader.
	var explorerReader v1.ExplorerReader
	// protocolActivityReader is the SAME concrete lake reader, surfaced through
	// the narrower ProtocolActivityReader seam for /v1/protocols/{name}
	// analytics. Same nil-degrade posture.
	var protocolActivityReader v1.ProtocolActivityReader
	// lakeWatermarkReader stamps lake-backed responses with as_of_ledger +
	// flags.stale (ADR-0041 Decision 4); tokenDecimalsReader overlays real
	// Soroban decimals() on /v1/assets/{id}. Both are the SAME concrete lake
	// reader through narrower seams, same nil-degrade posture.
	var lakeWatermarkReader v1.LakeWatermarkReader
	var tokenDecimalsReader v1.TokenDecimalsReader
	// issuerAuthFlagsReader is the same lake reader through the key_xdr point
	// lookup /v1/issuers/{g} re-reads the issuer's AccountEntry with.
	var issuerAuthFlagsReader v1.IssuerAuthFlagsReader
	// storageSupplyReader is the SAME concrete lake reader through a narrower
	// seam: it sums a token's supply out of its own Soroban contract storage
	// for the tokens the SEP-41 event log cannot see. Same nil-degrade posture
	// — without it those tokens keep reporting the zero they report today.
	var storageSupplyReader v1.ContractStorageSupplyReader
	// tokenSymbolReader resolves a token contract's on-chain SEP-41 symbol
	// from the same captured contract-instance METADATA. Read ONLY by the
	// RWA contract arm, and only after the curated directory has named that
	// exact contract. Same concrete lake reader, same nil-degrade posture.
	var tokenSymbolReader v1.TokenSymbolReader
	// soroswapTVLReserves / phoenixTVLReserves / cometTVLReserves are
	// the SAME concrete lake reader through the narrow current-state
	// seams the DEX TVL snapshot consumes. Same nil-degrade posture
	// (the protocol's TVL is absent without the lake).
	var soroswapTVLReserves v1.SoroswapTVLReserveReader
	var phoenixTVLReserves v1.PhoenixTVLReserveReader
	var cometTVLReserves v1.CometTVLReserveReader
	// sdexOfferBook is the SAME concrete lake reader through the live
	// offer-book seam /v1/sdex/orderbook maintains itself from. Same
	// nil-degrade posture (the endpoint 503s without the lake).
	var sdexOfferBook v1.SDEXOfferBookReader
	if addr := cfg.Storage.ClickHouseAddr; addr != "" {
		er, err := lakeExplorer, lakeExplorerErr
		// The readiness checker is registered for a CONFIGURED ClickHouse
		// whether or not this dial succeeded — see wiring.ClickhouseReadyChecks.
		checks = append(checks, wiring.ClickhouseReadyChecks(addr, er, err, clickhouseBootDialBudget)...)
		if err != nil {
			logger.Warn("explorer reader unavailable; /v1/ledgers etc. will 503", "addr", addr, "err", err)
		} else {
			defer func() { _ = er.Close() }()
			// Surface background wealth-refresh failures:
			// a persistently-failing refresh keeps /v1/accounts on its 503
			// warming state, and the first time round the failure was
			// silent because the query was dying at the CH execution cap.
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

	// DEX TVL snapshot cache — per-protocol pooled-liquidity USD value
	// for /v1/protocols (+ the explorer's /dexes page). Reserve reads
	// are three batched lake lookups (soroswap pair instance storage;
	// phoenix pool persistent storage; comet per-token balance
	// records — the latter two scoped to their ADR-0035 curated pool
	// sets) + one served-tier scan (aquarius_reserves); legs are
	// valued through the SAME VWAP USD tier system that stamps
	// trades.usd_volume (peg → direct VWAP → XLM bridge), so TVL and
	// volume figures on one page share a single pricing methodology.
	// Refreshed in the background — handlers read an O(1) snapshot and
	// omit the field until the first refresh completes (honest empty,
	// never 503).
	dexTVLSources := v1.DEXTVLSources{
		SoroswapPairs:    store,
		SoroswapReserves: soroswapTVLReserves,
		AquariusReserves: store,
		// Curated pool sets only (fail-closed, ADR-0035): stake
		// contracts are excluded from the phoenix list — they hold LP
		// shares, which would double-count the pools' underlying.
		PhoenixPools:    append(append([]string{}, phoenix.MainnetPools...), phoenix.MainnetMapPools...),
		PhoenixReserves: phoenixTVLReserves,
		CometPools:      comet.MainnetGatedSet(),
		CometReserves:   cometTVLReserves,
		// Same serving trust gates every price surface consults, on the
		// same chokepoint. Without this the TVL sum was the one
		// published USD figure with no scam screen and a $0.01 floor:
		// a directory-flagged issuer's token with one self-traded minute
		// on any peg market was valued into the protocol headline at its
		// own VWAP. A withheld leg now counts its pool unpriced, so the
		// number stays an honest lower bound rather than becoming a
		// fabricated one.
		Gate: buildDEXTVLValueGate(substanceGate, scamGate, usdPegs),
		// Identity screen: pools are permissionless, so only the
		// hand-vetted catalogue's assets (plus native and declared pegs)
		// are valued; a self-listed token's legs count unpriced.
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
		// 3 min per refresh sits well below the 10-min interval so
		// refreshes never stack; a refresh is one lake lookup + one
		// served-tier scan + a bounded set of prices_1m point reads.
		const dexTVLRefreshTimeout = 3 * time.Minute
		refreshWithTimeout(rootCtx, dexTVLCache.Refresh, dexTVLRefreshTimeout, logger, "dex tvl initial refresh")
		runRefreshLoop(rootCtx, dexTVLCache.Refresh, v1.DEXTVLRefreshInterval, dexTVLRefreshTimeout, logger,
			"dex tvl periodic refresh")
	}()

	// SDEX live order book (/v1/sdex/orderbook). The initial load
	// streams the lake's whole live-offer slice ONCE per process start
	// (work-shape-bounded FINAL scan — see
	// clickhouse/sdex_offer_book_reader.go for the trade-off note);
	// afterwards a 60s ticker applies partition-pruned incremental
	// change reads, bounded by the lake's contiguous tip so the cursor
	// never crosses a dropped ledger, and rebuilds the book wholesale
	// once a day as the self-heal. The endpoint serves an honest 503
	// problem until the initial load completes.
	var sdexOrderBook *v1.SDEXOrderBookCache
	if sdexOfferBook != nil {
		sdexOrderBook = v1.NewSDEXOrderBookCache(sdexOfferBook, logger.With("component", "sdex-orderbook"))
		bgWG.Add(1)
		go func() {
			defer bgWG.Done()
			defer recoverBackgroundWorker(logger, "sdex-orderbook-cache")
			// MaintainTick owns the whole policy — initial load and its
			// per-tick retry, advance, the version-tie quarantine drain
			// (the crossed-book zombie class), the periodic
			// re-load, and every step's timeout and failure log — so it
			// is exercised by the package's tests rather than only here.
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

	// Admin audit sink — durable "key.mint" rows for POST
	// /v1/admin/keys. Wired whenever Postgres is reachable (platform
	// audit_log, migration 0027); nil degrades the admin handler to
	// structured-log-only audit.
	var adminAudit v1.AuditSink
	if pgDB := store.DB(); pgDB != nil {
		adminAudit = postgresstore.NewAuditStore(postgresstore.New(pgDB))
	}

	// Platform account store — backs the operator tier-override
	// endpoints (PATCH /v1/admin/accounts/{id}) AND is the SAME store
	// the Postgres API-key validator reads overrides from, so a
	// staff-set override is effective on the next key Lookup. Status
	// notice store (migration 0082) backs the operator status-banner
	// endpoints + public /v1/status/notices. Both wired only when
	// Postgres is reachable; nil degrades the admin endpoints to 503
	// and the public notices list to `[]`.
	var (
		platformAccountStore v1.PlatformAccountStore
		platformUserStore    v1.AccountSessionRevoker
		registerAccountStore v1.RegisterAccountCreator
		statusNoticeStore    v1.StatusNoticeStore
	)
	if pgDB := store.DB(); pgDB != nil {
		// One concrete AccountStore serves both narrowed seams:
		// Get/Update for the admin override endpoints, Create for
		// POST /v1/register.
		acctStore := postgresstore.NewAccountStore(postgresstore.New(pgDB))
		platformAccountStore = acctStore
		platformUserStore = postgresstore.NewUserStore(postgresstore.New(pgDB))
		registerAccountStore = acctStore
		statusNoticeStore = postgresstore.NewStatusNoticeStore(postgresstore.New(pgDB))
	}

	// On PATCH /v1/admin/accounts/{id}, the credential stores must clamp
	// when an operator LOWERS an
	// account's tier — Postgres-backed dashboard keys and Redis-backed
	// self-service keys. Each half is nil-safe: a missing store means
	// that half is skipped and the endpoint's audit row records
	// keys_clamped=0.
	//
	// The key-cache invalidator is decided by auth_backend inside
	// v1.NewAPIKeyBudgetStores, NOT by "is Redis configured": under the
	// default redis backend apikey:<hash> is the canonical credential,
	// and an invalidator wired here DELeted every /v1/register key an
	// account held on any override / status / tier PATCH.
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
		// 2m SWR cache on LatestTradePerSource only (the
		// /v1/observations primitive — an unbounded DISTINCT ON scan
		// over the trades hypertable, ~8s → 503 uncached). All other
		// HistoryReader methods pass through. Cold fill is detached
		// so it outlives the handler's 8s ceiling and warms the
		// cache for the status page's 2-min poll.
		History: v1.NewCachedHistoryReader(wiring.StoreHistoryReader{S: store}, 2*time.Minute),
		// Coverage-floor probe behind the empty-window signal. Not part
		// of HistoryReader: it is consulted only when a serving read
		// came back empty, has its own TTL memo in the handler layer,
		// and reads a different question (when does this pair START)
		// than any serving method answers.
		CoverageFloor: wiring.StoreCoverageFloorReader{S: store},
		// Wrap with a 30s TTL cache. /v1/markets and /v1/pools both
		// scan ~24h of the trades hypertable on every hit (5-10s
		// each); the explorer hits them on every page load. 30s
		// freshness is plenty for trade-volume aggregates.
		Markets: cachedMarketsReader,
		Oracle:  oracleReader,
		// The day-bucket CAGG read behind /v1/rwa/history's price leg.
		// Uncached here on purpose: the handler caches the whole
		// ASSEMBLED series behind its own TTL + single flight, so a
		// second cache at the reader would only add a second staleness
		// window to the same figures.
		OracleHistory: store,
		// The hour-bucket CAGG read behind /v1/rwa/premium's market
		// leg. Uncached here for OracleHistory's reason — the handler
		// caches the assembled series, not the read.
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
		// One-hop pricing for assets the catalogue cannot reach at all —
		// Soroban-native contract assets, which cannot appear in
		// classic_assets. Gated on BOTH legs by the same substanceGate
		// above, so it widens coverage without lowering the bar.
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
		// The contract arm of /v1/rwa/assets. Both seams are the SAME
		// Postgres store the classic arm already reads its curated
		// directory through — the contract arm draws a different
		// population from it, not a different trust source.
		RWAContracts: store,
		// C2's SECOND arm: the cached independent listing directory the
		// `listing-sync` ops command fills. Also the same store — the
		// independence that matters is between the two SOURCES whose
		// claims are compared, not between the tables they are cached
		// in, and both staleness bounds are enforced in the reader's
		// own SQL so a stale snapshot closes the arm rather than
		// admitting on it.
		RWAListings:        store,
		RWACurated:         store,
		Listings:           store,
		ContractCatalogue:  store,
		LakeWatermark:      lakeWatermarkReader,
		Explorer:           explorerReader,
		IssuerAuthFlags:    issuerAuthFlagsReader,
		StaticHomeDomain:   homeDomainLookup.static,
		Volume:             wiring.StoreVolumeReader{S: store},
		Change24h:          storeChange24hReader{s: store, pegs: usdPegs, decimals: nonstandardDecimalsCache, logger: logger},
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
		// Protocols pillar (/v1/protocols*): contract registry, 24h
		// event census, soroswap pair registry. All three optional —
		// the directory degrades to zeros/empties when absent.
		ProtocolContracts:  store,
		ProtocolStats:      store,
		ProtocolActivity:   protocolActivityReader,
		ProtocolBespoke:    store,
		SoroswapPairs:      store,
		ProtocolPoolTokens: store,
		DEXTVL:             dexTVLCache,
		SDEXOrderBook:      sdexOrderBook,
		NetworkStats:       cachedNetworkStats,
		// Routers registry + routed-via 24h rollup (/v1/aggregators).
		// Direct store read: the routed-trades scan rides the partial
		// routed_via index and the registry is a handful of rows; the
		// 60s edge cache (cachecontrol) absorbs explorer fan-out.
		Aggregators: store,
		// Per-source 24h volume breakdown (/v1/markets/sources). Reads the
		// raw store directly — a single-pair GROUP BY source is cheap and
		// doesn't need the markets cache layer.
		MarketSources: store,
		// Wrap with a 60s TTL cache. The underlying SQL aggregations
		// (24h trades-hypertable scan grouped by source) take 5-10s;
		// the explorer hits these on every /dexes + /exchanges page
		// load. 60s freshness is plenty for a 24h-trailing aggregate.
		SourcesStats: cachedSourcesStats,
		Lending:      store,
		MEV:          store,
		Anomalies:    store,
		Divergences:  store,
		// Surfaces the SAME threshold the divergence worker fires on,
		// so /v1/divergence/series charts shade the real alert band.
		DivergenceThresholdPct: cfg.Divergence.Threshold,
		// Valuation-integrity floor: a market cap whose backing price came
		// from a single venue with sub-floor 24h volume is served null (with
		// market_cap_low_liquidity=true) rather than asserting billions off a
		// dust trade. See v1.dustLiquiditySuppressed.
		MinMarketCapVolumeUSD:   cfg.Aggregate.MinMarketCapVolumeUSD,
		MaxMarketCapVolumeRatio: cfg.Aggregate.MaxMarketCapVolumeRatio,
		Currencies:              wiring.NewForexAdapter(forexCache),
		// Staleness budget for the fiat-cross-rate / USD-anchored-fiat-cross
		// fallbacks — the in-memory forex cache never expires on
		// its own.
		FXCrossMaxAgeHours: cfg.PricingGuard.FXCrossMaxAgeHours,
		DisableFiatBasis:   cfg.PricingGuard.DisableFiatBasis,
		FXFixings:          store,
		FXHistory:          &wiring.FXHistoryReader{Store: store},
		SEP10:              sep10Validator,
		Hub:                hub,
		CORS:               cors,
		Auth:               authMW,
		KeyPolicy:          middleware.KeyPolicy(),
		// Monthly-quota enforcer.
		// Reads month-to-date counters from the same Redis Counter
		// the UsageTracker writes. Both the Postgres validator and
		// the Redis validator now map MonthlyQuota onto the Subject
		// (apikey_redis.go maps rec.MonthlyQuota; store_mirror.go
		// persists it), so metered keys are enforced on the default
		// redis backend too. A validator that leaves it 0 makes the
		// middleware short-circuit per request.
		MonthlyQuota: middleware.MonthlyQuota(usageCounter, logger.With("component", "monthly-quota"),
			middleware.WithMonthlyQuotaDwellTime(cfg.API.MonthlyQuotaDwell)),
		RateLimit:    rateLimit,
		UsageTracker: middleware.UsageTracker(usageCounter, logger.With("component", "usage")),
		// TouchUsage half:
		// asynchronously update the api_keys row's `last_used_at` /
		// `last_used_ip` / `last_used_user_agent` columns, debounced
		// per (key, 5min) via Redis SETNX so the hot row sees at
		// most one UPDATE per window. Only wired when both Postgres
		// and Redis are present; deployments missing either fall
		// back to the legacy "no last_used updates" posture.
		TouchUsage: touchUsageMiddlewareOrNil(dashboardBundle.keysStore, rdb, logger),
		// Wire the UsageReader adapter only when the counter is real, so the
		// handler's `usageReader == nil` short-circuit returns an empty list.
		UsageReader: wiring.UsageReaderOrNil(usageCounter),
		// Per-endpoint usage rollups: reads the
		// `usage_daily` hypertable the usage-rollup worker below
		// maintains. The handler prefers this over UsageReader and
		// falls back per-request when the read errors or the table
		// has no rows for the subject yet.
		UsageRollupReader: wiring.UsageRollupReaderOrNil(store),
		CDNEnabled:        cfg.API.CDNEnabled,
		// Per-request deadline applied to every non-streaming request
		// Backs the RequestTimeout
		// middleware; the serving-pool statement_timeout below is the
		// SQL-side backstop.
		RequestTimeout: cfg.API.RequestTimeout,
		// api.request_timeout = 0 is documented to disable the middleware.
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

	// Shared tip-stream producer ceiling. The config defaults equal the
	// registry's built-in ones, so behaviour changes only on opt-in.
	apiSrv.SetMaxTipProducers(cfg.API.Streaming.MaxTipProducers)
	apiSrv.SetMaxTipProducersPerCaller(cfg.API.Streaming.MaxTipProducersPerCaller)

	// Prewarm the classic circulating-supply cache OUT OF BAND. It backs
	// market-cap enrichment on /v1/assets, and its backing full-table GROUP BY
	// outlives the request timeout — so a cold fill on the request path costs
	// the first visitor after every deploy a slow, degraded page. Filling it
	// here means no user request ever pays for it. This runs separately from
	// prewarmCaches (started earlier, before this server exists) but on the
	// same 5-minute cadence as prewarmHeavy; the cache's TTL is 10 minutes, so
	// that keeps it permanently warm. Each call reuses the request path's
	// single-flight + retry-gap, so a still-warm cache is a cheap no-op.
	//
	// PrewarmAccountsWealth rides the same loop. Its cache
	// has a 15-minute TTL, so a 5-minute cadence keeps it permanently warm
	// with two cycles of slack. PrewarmContractsDirectory joins it:
	// the /v1/contracts default rung is a
	// multi-day GROUP BY that must never run on a request deadline, and its
	// 5-minute TTL exactly matches this cadence. All calls are cheap
	// no-ops when already warm — each returns as soon as it sees a live
	// entry, and only kicks off its detached refresh on a miss.
	// PrewarmOpTypeStats also rides this loop: the
	// /v1/operations op-type panel had SWR + detached refresh but nothing
	// warmed it at boot, so the first directory hit after every deploy
	// rendered without it. Its 5-minute TTL matches this cadence exactly.
	// PrewarmNetworkThroughput joins them: the
	// /v1/network/throughput series is a FINAL scan over up to a year of
	// ledgers that would run inline on the 8s request budget, so a cold
	// or loaded /network first load lost the panel entirely. Its cache
	// also has a 5-minute TTL — this cadence keeps it permanently fresh.
	// PrewarmNativeLiquidityPools joins them: the
	// /v1/liquidity-pools ranked listing had NO prewarm anywhere, so the
	// first visitor after every boot paid the whole-prefix lake scan
	// inline. Its ENTRY only needs to EXIST for the request path to stop
	// blocking — freshness is maintained by the request-kicked detached
	// refresh at its own 60s TTL — so this cadence is a
	// never-cold/repair guarantee, not the freshness mechanism, and the
	// call is a no-op whenever the entry is already warm.
	// PrewarmSep1Images joins them: the /v1/assets logo map
	// is a scan over every issuer's cached stellar.toml — 448 MB of JSON
	// across 35,829 issuers on r1, and growing — rebuilt INLINE would cost
	// whichever request found it expired 10-13 s. The rebuild is detached,
	// so a cold map costs a request nothing but its logos; this is what
	// stops it being cold in the first place. Its 10-minute TTL gives this cadence two cycles of
	// slack, exactly like PrewarmClassicSupply above.
	// PrewarmContractProtocolIndex joins them: the cohort
	// view's contract → protocol map is seventeen registry reads that
	// would run inline on whichever request found it expired — and a
	// request that had already spent its budget would cache a statics-only
	// map for everyone. Its 10-minute TTL gives this cadence the same two
	// cycles of slack; an incomplete build retries within 30 s.
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

	// Lake-flows supply prewarm: its own goroutine, for the same reason the
	// protocol sweep below has one — a cold pass walks the whole listing
	// population 32 contracts at a time and takes minutes, and it must never
	// delay the cheap prewarms in the loop above.
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-classic-lake-supply")
		prewarmClassicLakeSupply(rootCtx, apiSrv)
	}()

	// RWA prewarm: its own goroutine, NOT the 5-minute loop above, for
	// the same reason the lake-supply pass has one — the membership
	// rebuild is an indexed scan over every issuer-bound SEP-1 payload
	// (1.18M currency entries on r1) plus the curated-directory walk,
	// measured at ~11.5 s, and it must never delay the cheap prewarms.
	//
	// What it is for: before the rebuild was detached, the /rwa page's three routes
	// shared ONE ten-minute membership cache that rebuilt INLINE on
	// whichever request happened to find it expired. The route's
	// latency on r1 was perfectly bimodal — 39 of 43 requests under 1 s,
	// the other 4 over 10 s — so roughly one visitor in ten met a
	// twelve-second page. The rebuild is detached now, so a stale set
	// costs a request nothing; this is what stops it being cold in the
	// first place, and what makes the waiter for the first build after
	// a deploy this goroutine rather than a visitor.
	//
	// Cadence: 5 minutes against the caches' 10-minute TTLs, the same
	// two-cycles-of-slack rule PrewarmClassicSupply and
	// PrewarmSep1Images follow. Freshness does not depend on it — the
	// request-kicked detached refresh maintains that — so this is a
	// never-cold/repair guarantee, and every pass that finds the entry
	// warm is a no-op.
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

	// Protocol-detail prewarm: its own goroutine (NOT the 5-minute loop
	// above — a full sweep is minutes of serial build work and must never
	// delay the cheap prewarms). Sweeps ALL protocols × bespoke windows
	// one build at a time so every /v1/protocols/{name} page + ?days=
	// window is warm before anyone asks (under replay load
	// every on-demand bespoke build died at the request deadline and
	// pages lost their visual suites). The sweep re-runs 10 minutes after
	// the previous sweep ENDS (sleep, not a ticker, so sweeps can never
	// overlap); with a measured sweep cost of ~3–6 min that refreshes
	// every entry every ~13–16 min, comfortably inside the cache's
	// 20-minute stale horizon (protocolDetailTTL).
	bgWG.Add(1)
	go func() {
		defer bgWG.Done()
		defer recoverBackgroundWorker(logger, "prewarm-protocol-details")
		const betweenSweeps = 10 * time.Minute
		for {
			// Warm the directory's roster counts (cheap, ~15 quick reads)
			// before the far heavier detail sweep, so /v1/protocols is warm
			// early and no first request pays for a cold roster scan.
			apiSrv.PrewarmProtocolRosters(rootCtx)
			apiSrv.PrewarmProtocolDetails(rootCtx)
			select {
			case <-rootCtx.Done():
				return
			case <-time.After(betweenSweeps):
			}
		}
	}()

	// Closed-bucket producer — only spawn when the operator
	// configured pairs to broadcast. Empty pair list is a valid
	// deployment (Hub still constructs; subscribers connect and
	// receive heartbeats with no events). Bad pair strings fail
	// loud at startup rather than silently dropping a pair.
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

	// Background refresher for /v1/diagnostics/ingestion. Builds
	// the snapshot every 15s into an atomic.Pointer that the handler
	// serves sub-ms (the inline build was 200-500ms — fine, but the
	// status-page tile polls every 15-30s and this turns it into a
	// near-zero-cost endpoint). Inline-build remains as the
	// not-yet-warm fallback inside the handler.
	// Wrapped in a literal rather than started as `go apiSrv.Start…`
	// because the guard has to live on this goroutine's stack and the
	// callee is a method on internal/api/v1's Server, shared with other
	// entry points. The refresher is a cache filler: if it
	// stops, the handler falls back to its inline build —
	// 200-500ms per request, not an outage.
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

	// Tell the SSE writers when the drain starts. httpSrv.Shutdown waits
	// for every active connection to become IDLE, and an SSE connection
	// never is — it holds an open response for as long as the client
	// reads. The stream handlers watch r.Context(), which cancels when
	// the CLIENT leaves, not when this process does, so without this
	// hook a single attached stream pinned the drain for the whole 30s
	// budget below: Shutdown returned "context deadline exceeded", the
	// process exited on top of the still-open connection (the client saw
	// a truncated response), and the background-worker wait that shares
	// the deadline inherited nothing. Measured on r1, same
	// binary: 30.18s with one browser on /v1/ledger/stream, 0.21s with
	// none — 30s of avoidable downtime on every deploy that happened to
	// have a viewer attached, against a 99.9% availability SLO.
	//
	// Shutdown invokes registered hooks the instant it starts, which is
	// exactly the moment the streams need to hear about it.
	//
	// NOT BaseContext. Deriving every request context from rootCtx would
	// also free the streams, and would cancel every ordinary in-flight
	// request along with them the moment SIGTERM landed — trading a
	// stream problem for an abrupt teardown of the overwhelming
	// majority of traffic that is not a stream. The drain is visible
	// only to the stream writers.
	httpSrv.RegisterOnShutdown(apiSrv.BeginStreamDrain)

	// Run the closed-bucket subscriber alongside the HTTP server.
	// Bound to rootCtx — SIGINT/SIGTERM cancels both the server and
	// the subscriber together. Run errors don't take the API down
	// (the rest of the surface keeps serving); they log + leave the
	// stream endpoint serving 503 implicitly via the Hub falling
	// silent.
	// Gate the subscriber on
	// `rdb != nil`. The Hub is always non-nil because streaming.NewHub
	// is called unconditionally a few hundred lines above; using
	// `hub != nil` here would let a Redis-less deployment pass the
	// gate, after which `redispub.NewSubscriber(nil, ...)` would return
	// "RedisSubscriber is required" and abort startup. Every
	// other Redis-backed feature in this file gates on `rdb != nil`;
	// streaming should too. Without Redis the Hub stays silent —
	// `/v1/price/stream` serves heartbeats but no closed-bucket
	// events, matching the documented "Redis optional at API
	// layer" posture.
	if rdb != nil && hub != nil {
		sub, err := redispub.NewSubscriber(rdb, cfg.Storage.RedisClosedBucketChannel, hub, logger.With("component", "stream-sub"))
		if err != nil {
			return fmt.Errorf("redispub subscriber: %w", err)
		}
		go runSubscriberSupervised(rootCtx, sub, logger.With("component", "stream-sub"))
	}

	// Customer-webhook delivery worker. Drains the
	// queue every 5s, HMAC-signs payloads, POSTs to customer URLs
	// with exponential backoff on 5xx/network errors. Wired only
	// when the dashboard webhook store came up (i.e. Postgres is
	// reachable + the dashboard surface is enabled); leaves
	// non-dashboard deployments unaffected.
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

	// Usage-rollup worker: folds the Redis per-endpoint
	// detail counters (written by middleware.UsageTracker) into the
	// `usage_daily` Timescale hypertable every 5 min so
	// /v1/account/usage can serve per-endpoint request / error /
	// throttle analytics beyond Redis's 35-day TTL. Needs both
	// backends; deployments missing Redis keep the legacy
	// per-day-total posture.
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

	// Speculative-account reaper. Deletes orphan `accounts`
	// rows left by a lost signup race — Suspended with a `signup-race:`
	// reason, no user + no key. POST /v1/register (the source of those
	// races) is wired off platformAccountStore/Postgres alone — NOT off
	// the dashboard bundle, which stays nil when api.dashboard.base_url
	// is unset — so the reaper binds to the same platformAccountStore
	// seam registration uses, not dashboardBundle.accounts, or a
	// Postgres-without-dashboard deployment accepts registrations with
	// no reaper ever running. Runs only when Postgres is reachable (the
	// concrete store implements the narrow OrphanStore seam). Bounded
	// to rootCtx for graceful shutdown, same as the workers above.
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

	// Login-code lockout retention. `login_code_lockouts` is
	// keyed by an ATTACKER-CHOSEN email — POST /v1/auth/verify-code is
	// unauthenticated, and a wrong guess against a synthetic address
	// inserts a row that no successful sign-in will ever clear. This
	// sweep is the only thing that bounds the table.
	//
	// Deliberately NOT gated on cfg.SignupReaper (or any other toggle):
	// this is a DoS control, and switching it off with an unrelated
	// knob is how a bound quietly stops existing. It runs whenever the
	// dashboard's Postgres token store is wired, which is exactly when
	// the endpoint that writes those rows is reachable.
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

	// Magic-link token retention. `magic_link_tokens` is durable
	// plaintext PII (email + requested_ip) keyed on an ATTACKER-CHOSEN
	// email — POST /v1/auth/login is unauthenticated and inserts a
	// permanent row for any address, and a link nobody clicks is never
	// consumed. This sweep is the only thing that bounds the table.
	//
	// Like the login-code lockout reaper above, deliberately NOT gated
	// on any toggle: it is a DoS/PII control, and it runs whenever the
	// dashboard's Postgres token store is wired — exactly when the
	// endpoint that writes those rows is reachable.
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

	// Ended sessions and finished webhook deliveries. Ungated for
	// the same reason as the two reapers above: a PII bound with an off
	// switch is not a bound.
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

	// HTTP self-call prewarm. Hits /v1/assets/<id> for
	// native + every verified currency on a 60s cadence so EVERY
	// cache the handler touches — not just the 7 CachedAssetsReader
	// SWR slots warmed by prewarmCaches — stays hot. Covers the F2
	// path (Volume24hUSDForAsset / LatestSupply / lookupUSDPrice /
	// populateChange24h) and any downstream readers added in future
	// without prewarm wiring needing to track them.
	//
	// Drift-safe by construction: the call hits the same Server.Handler
	// + same mux + same handler functions a user request takes, so
	// every internal lookup happens with byte-identical args. Per
	// `feedback_prewarm_handler_drift` this is the canonical pattern
	// when handler fan-out is wider than the prewarm goroutine knows
	// about.
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

	// Now let the background workers finish. rootCtx is
	// already cancelled — that is the only reason we reached this line —
	// so each worker is unwinding; this waits for the unwind instead of
	// exiting on top of it. Deliberately AFTER httpSrv.Shutdown so
	// serving stops first: a worker that outlives its budget delays the
	// exit, it does not hold requests open.
	//
	// It shares shutdownCtx with the listener drain, so 30s is the
	// budget for BOTH — what the drain does not spend is what the
	// workers get, not a second 30s. That is intentional: this budget is
	// the deploy's downtime, and two independent budgets would let one
	// restart cost 60s. It does mean a listener that spends the lot
	// leaves the workers nothing (an attached SSE stream once held Shutdown
	// for the full
	// 30s and this wait then reported "did not drain" 51µs later, having
	// never actually waited). The registered stream drain above is what
	// keeps the listener's share small; the workers themselves were
	// never the problem — every one of them returns on rootCtx
	// cancellation and its in-flight work is context-bounded.
	workersDone := make(chan struct{})
	go func() {
		// Guarded like every other detached goroutine here. The only way
		// bgWG.Wait() panics is an Add/Done imbalance in this file, and
		// on that path workersDone stays OPEN on purpose: the select
		// below then reports "did not drain" rather than claiming a
		// drain that never happened.
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

// authValidatorOptions carries the wiring buildAuthMiddleware
// needs beyond the mode string — the backend choice + the
// already-built Postgres validator (when backend=postgres).
type authValidatorOptions struct {
	Backend           string // "redis" (default) or "postgres"
	Rdb               redis.UniversalClient
	PostgresValidator *auth.PostgresAPIKeyValidator // non-nil when dashboard wired
	SEP10             auth.SEP10Validator
	// FailedAuthLimiter throttles invalid-credential attempts per IP and
	// per API-key prefix. Nil disables it. Attached to the Auth middleware for every
	// mode that can actually fail auth (i.e. not mode=none).
	FailedAuthLimiter *ratelimit.Bucket
	// AccountStatus wires the account kill switch into the
	// REDIS validator (the backend r1 actually runs). Nil when the
	// dashboard bundle (and thus Postgres) is absent — the gate then
	// simply stays off, but a
	// deployment with Postgres present MUST pass it or a suspended
	// account keeps authenticating on the default backend.
	AccountStatus auth.AccountStatusReader
}

// buildAuthMiddleware translates the configured auth_mode into a
// concrete middleware. Returns nil for mode=none — the server stack
// omits absent middleware entirely so anonymous traffic doesn't
// pay a per-request closure cost.
//
// auth_mode=apikey requires a working API-key validator (Redis or
// Postgres). auth_mode=sep10 wires the same validator as the
// /v1/auth/sep10/* endpoints. The Postgres backend additionally
// requires the platform stores; missing them falls back to Noop
// (every request 503s — the correct fail-loud behaviour for a
// deployment that opted in to the postgres backend without the
// platform tables wired).
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

// buildAPIKeyValidator picks Redis vs Postgres backend based on
// auth_backend config. Postgres falls back to Noop (every request
// 503s) when the dashboard bundle wasn't wired — fail loud rather
// than silently demote.
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

// buildSEP10Validator constructs an [auth.SEP10Validator] from the
// API's SEP10Config. Reads the seed + JWT secret from the
// configured env vars; missing or empty values surface as errors
// the caller can decide to handle (auth_mode=sep10 → fail loud at
// startup; otherwise → wire a Noop and log).
//
// Empty SeedEnv / JWTSecretEnv (config not opted into SEP-10) is
// also an error — the caller treats it as "feature not configured"
// and falls back to the Noop. The behaviour is symmetric across
// "env name unset" and "env value empty": both mean the operator
// hasn't supplied a credential.
// nilOrMounter returns nil-typed nil when the supplied
// concrete *Handlers pointer is nil, otherwise returns it as
// the v1.DashboardAuthMounter interface.
//
// Naked assignment (`opts.DashboardKeys = bundle.keys`) wraps a
// typed-nil pointer in a non-nil interface, so server.go's
// `if s.dashboardKeys != nil` would mount routes whose handlers
// then panic on first request when they dereference cfg. This
// helper sidesteps the Go interface-nil-vs-pointer-nil gotcha.
func nilOrMounter[T v1.DashboardAuthMounter](h T) v1.DashboardAuthMounter {
	// Generic constraint catches both *dashboardauth.Handlers and
	// *dashboardkeys.Handlers without runtime reflection.
	var zero T
	if any(h) == any(zero) {
		return nil
	}
	return h
}

// dashboardBundle bundles the dashboard wirings main.go threads
// into v1.Options — the auth handlers, the keys handlers, and
// the session-resolving middleware that runs in the global
// request stack so dashboardkeys.HandleList et al can read the
// session context.
//
// The platform stores ride along too so the Postgres-backed auth
// validator (cfg.API.AuthBackend == "postgres") can borrow the
// same handles instead of opening a second connection pool.
type dashboardBundle struct {
	auth         *dashboardauth.Handlers
	keys         *dashboardkeys.Handlers
	webhooks     *dashboardwebhooks.Handlers
	priceAlerts  *dashboardpricealerts.Handlers
	webhookStore platform.WebhookStore
	middleware   middleware.Middleware
	keysStore    platform.APIKeyStore
	accounts     platform.AccountStore
	// tokens is the same store the auth handlers write magic-link rows
	// and login-code lockout rows through; exposed so the login-code-lockout
	// reaper can bind to its narrow sweep seam.
	tokens platform.TokenStore
	// users carries the sessions table the session retention reaper sweeps.
	users       platform.UserStore
	pgValidator *auth.PostgresAPIKeyValidator
	// sender + emailFrom are exported so the public-API signup
	// flow can re-use the same Resend / Noop
	// transport the dashboard auth flow uses, without having to
	// build a second sender at the top level.
	sender    notify.Sender
	emailFrom string
}

// buildDashboardBundle wires the customer-dashboard magic-link
// auth flow + the key-management surface. Returns a bundle with
// all-nil fields (and a logged warn) when the operator hasn't
// configured BaseURL — the API still serves; /v1/auth/* and
// /v1/dashboard/* routes simply aren't mounted.
//
// When BaseURL is configured but the Resend env var is unset or
// empty, the sender is a notify.UnconfiguredSender (see
// buildDashboardSender): magic-link login answers 503 and counts a
// failed send instead of claiming an email it cannot deliver.
// The plaintext token is never logged (only its HASH is stored), so an
// emailed sign-in is impossible without a configured sender.

// buildWebhookHandlers constructs the dashboard webhook CRUD handlers
// atop a fresh WebhookStore over the same Postgres the
// delivery worker (a goroutine in main()) drains. Returns the store so
// the bundle can thread it to the worker.
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

// buildPriceAlertHandlers constructs the dashboard price-alert CRUD
// handlers atop the shared platform store. The evaluator
// that checks these alerts + enqueues price.alert deliveries runs in
// the aggregator binary (internal/pricealerts); these handlers are the
// customer-facing registration surface.
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

// assertRedisOrSingleInstance refuses to start a Redis-less deployment that
// has not asserted single-instance. With Redis
// absent the auth throttles + the passkey ceremony replay guard use per-process
// state, which is safe on ONE instance but unsafe across several; a process
// cannot detect its own fleet size, so the operator must either provide Redis
// (fleet-safe) or explicitly assert single-instance. No-op when Redis is present.
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

// inProcessLoginThrottleWindow / *MaxPerIP / *MaxPerEmail mirror
// auth.RedisLoginThrottle's documented defaults (10 sends/hour/IP,
// 5 sends/hour/email) so the Redis-less fallback enforces the SAME
// policy, just with single-instance (not fleet-wide) accounting.
const (
	inProcessLoginThrottleWindow      = time.Hour
	inProcessLoginThrottleMaxPerIP    = 10
	inProcessLoginThrottleMaxPerEmail = 5
)

// inProcessLoginThrottle implements dashboardauth.LoginThrottle
// as the Redis-less fallback for the
// magic-link send throttle — see newInProcessLoginThrottle's call
// site for the threat this closes. Wraps two ratelimit.Bucket
// instances constructed with a nil Redis client, which is the same
// in-process single-instance fixed-window fallback already relied
// on for the anon/key rate-limit tiers a few hundred lines up
// (ratelimit.New(nil, …)) — that fallback has a bounded memory
// footprint, so reusing it here inherits that bound for free
// instead of hand-rolling a second unbounded map.
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

// Allow implements dashboardauth.LoginThrottle. The in-process
// Bucket path never returns an error (no backend to fail — see
// ratelimit's localStore), so the discarded errors here are always
// nil; the signature is kept for interface conformance.
func (t *inProcessLoginThrottle) Allow(ctx context.Context, ip, email string) (bool, error) {
	allowed := true
	if ip != "" {
		// Masked to the throttle-key identity — see [ratelimit.ThrottleIPKey]
		// — so an IPv6 /64 can't rotate a fresh /128 per request and mint
		// an unbounded number of per-IP budgets.
		res, _ := t.perIP.Take(ctx, "ip:"+ratelimit.ThrottleIPKey(ip))
		allowed = res.Allowed
	}
	// The per-email Take is gated behind a passing per-IP Take (checked
	// first, above): an IP whose own bucket is already exhausted must not
	// still be able to spend a NEW email's per-email budget on a call
	// that's denied anyway. Unconditional per-email spending let an
	// attacker with one exhausted IP insert one new tracked key per
	// distinct target address, and localStore's fixed 100k-key cap folds
	// every key beyond that into a single shared overflow bucket sized to
	// ONE per-email limit (5) — so filling the store denies magic-link
	// sends to every not-yet-tracked email for the rest of the window
	// (CA2-A30-correct-4).
	if allowed && email != "" {
		// dashboardauth.LoginThrottle's doc requires implementations to
		// hash the (canonicalised) email before keying on it. Uses
		// auth.HashEmail — the same canonicalisation (RFC-5322 addr-spec,
		// +tag stripping, gmail dot-folding) as RedisLoginThrottle, so
		// this Redis-less fallback can't be bypassed by a re-spelling
		// that the fleet-wide path already closes.
		res, _ := t.perEmail.Take(ctx, "mail:"+auth.HashEmail(email))
		allowed = res.Allowed
	}
	return allowed, nil
}

// inProcessSignupIPThrottleMaxPerHour mirrors
// auth.RedisSignupIPThrottle's documented default (5 signups/hour/IP)
// so the Redis-less fallback enforces the same policy with
// single-instance accounting.
const inProcessSignupIPThrottleMaxPerHour = 5

// holdsReloadInterval is how often the API re-reads the holds file.
const holdsReloadInterval = 15 * time.Second

// inProcessSignupIPThrottle implements v1.SignupIPThrottle
// as the Redis-less fallback for the
// per-IP signup throttle — see its construction site for the threat
// this closes. Same ratelimit.New(nil, …) in-process bucket pattern
// as inProcessLoginThrottle.
type inProcessSignupIPThrottle struct {
	bucket *ratelimit.Bucket
}

func newInProcessSignupIPThrottle() *inProcessSignupIPThrottle {
	return &inProcessSignupIPThrottle{
		bucket: ratelimit.New(nil, inProcessSignupIPThrottleMaxPerHour, time.Hour),
	}
}

// CheckIP implements v1.SignupIPThrottle. The in-process Bucket path
// never errors (no backend to fail), so the only non-nil return is
// auth.ErrSignupRateLimited on quota exhaustion — the handler
// (signupIPThrottleOK) already knows how to translate that into 429.
func (t *inProcessSignupIPThrottle) CheckIP(ctx context.Context, ip string) error {
	if ip == "" {
		return nil
	}
	// Masked to the throttle-key identity — see
	// [ratelimit.ThrottleIPKey] for why a bare IPv6 address is the wrong
	// key.
	res, _ := t.bucket.Take(ctx, ratelimit.ThrottleIPKey(ip))
	if !res.Allowed {
		return auth.ErrSignupRateLimited
	}
	return nil
}

// wireDashboardAuthThrottles sets authCfg's EmailLocker + LoginThrottle
// based on Redis availability. Split out of buildDashboardBundle to
// keep that function under the funlen ceiling.
//
// Per-email signup lock. Redis-
// backed SETNX serialises first-login provisioning so two callback
// callers for the same just-verified email can't both create
// speculative Account rows. Redis-less deployments leave the locker
// nil and fall back to the Suspend-on-conflict recovery path (still
// safe; the orphan row gets reaped).
//
// Magic-link send throttle: per-IP +
// per-target-email caps so /v1/auth/login can't be used to
// email-bomb an inbox or burn the email-send quota. Defaults: 10/h
// per IP, 5/h per email.
//
// Redis-less deployments fall back to an in-process two-bucket
// throttle (same shape + defaults as auth.RedisLoginThrottle). Without it
// only the global anonymous per-IP rate limit (60/min) bounds /v1/auth/login,
// which caps REQUEST volume but not the per-target-EMAIL dimension, so a
// single IP under that ceiling could still bomb one victim's inbox and
// burn the Resend send quota. The cap is downgraded to single-instance
// accounting, never fully disabled — the same posture as the rate-limit
// tiers' in-process fallback in run().
// Passkey ceremony replay guard: each WebAuthn
// challenge is single-use, and the spent-set has to be SHARED or a
// replay routed to another instance is simply not seen. Redis-less
// deployments fall back to dashboardauth's in-process guard (installed
// by its validate()) — same single-instance downgrade the throttle
// takes, never an off switch.
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

// buildDashboardGenerator wires the token generator with the server
// secret every dashboard-auth MAC key is derived from. Without the env,
// dashboardauth's validate() refuses to start while passkeys are wired
// (always, here); the warning names what an unset secret breaks.
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

	// One audit_log (migration 0027) for the admin surfaces, the staff
	// look-up and every dashboard credential change, so one query
	// answers "who added or removed this credential, and from where".
	dashboardAudit := postgresstore.NewAuditStore(pg)
	authCfg := dashboardauth.Config{
		Accounts:  accounts,
		Users:     users,
		Tokens:    tokens,
		Sender:    sender,
		Generator: buildDashboardGenerator(cfg, logger),
		// Passkey (WebAuthn) sign-in — webauthn_credentials, migration 0140.
		Passkeys: postgresstore.NewWebAuthnCredentialStore(pg),
		Audit:    dashboardAudit,
		Logger:   logger.With("component", "dashboard-auth"),
		// Now is consumed by BOTH NewHandlers (validate() defaults it)
		// AND the session-resolver Middleware. NewHandlers now takes
		// &authCfg, so validate()'s defaults land on this same struct
		// and both paths share one clock. Still set explicitly: leaving
		// it nil nil-derefs cfg.Now() in resolveSession on
		// every authenticated request (the magic-link cookie resolved
		// fine, then /v1/account/me 500'd, so login looked broken).
		Now:              func() time.Time { return time.Now().UTC() },
		DashboardBaseURL: cfg.BaseURL,
		EmailFrom:        cfg.EmailFrom,
		MagicLinkTTL:     time.Duration(cfg.MagicLinkTTLMinutes) * time.Minute,
		SessionTTL:       time.Duration(cfg.SessionTTLDays) * 24 * time.Hour,
		CookieSecure:     cfg.CookieSecure,
		// cookie_domain scopes only the presence hint; credential
		// cookies are always host-only __Host- cookies.
		SessionHintDomain: cfg.CookieDomain,
		// DELETE /v1/dashboard/account + GET .../export.
		AccountEraser:   &accounterasure.Eraser{Store: accounts, Redis: rdb, Logger: logger.With("component", "account-erasure")},
		AccountExporter: &accounterasure.Exporter{Store: accounts, Redis: rdb},
	}
	wireDashboardAuthThrottles(&authCfg, rdb, logger)
	authH, err := dashboardauth.NewHandlers(&authCfg)
	if err != nil {
		return dashboardBundle{}, fmt.Errorf("dashboard auth handlers: %w", err)
	}
	// Optional Postgres-backed runtime auth validator. Constructed
	// here so the dashboard's Revoke handler can call its
	// InvalidateCachedKey after a successful soft-delete; main.go
	// also re-uses it when cfg.API.AuthBackend == "postgres".
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

	// Dashboard webhook handlers atop the same Postgres store
	// the delivery worker (a goroutine in main()) drains.
	webhookStore, webhooksH, err := buildSealingWebhookHandlers(cfg, db, logger)
	if err != nil {
		return dashboardBundle{}, err
	}

	// Dashboard price-alert CRUD atop the same Postgres.
	priceAlertsH, err := buildPriceAlertHandlers(pg, logger)
	if err != nil {
		return dashboardBundle{}, err
	}
	logger.Info("dashboard wired",
		"base_url", cfg.BaseURL,
		"magic_link_ttl_minutes", cfg.MagicLinkTTLMinutes,
		"session_ttl_days", cfg.SessionTTLDays,
		"cookie_secure", cfg.CookieSecure)

	// authCfg drives both the handlers AND the resolver
	// middleware — the latter needs the same Accounts / Users /
	// Tokens stores to resolve the cookie on every request.
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

// resolveSEP10Validator builds the SEP-10 validator and applies the
// auth_mode fallback policy in ONE place.
// Callers pass the real Redis client so the guarded (replay-protected)
// validator is what actually gets wired (a two-phase nil-rdb-then-rebuild
// build never wired it and could latently fall open).
//
// Behaviour by configuration:
//   - SEP-10 unconfigured (no seed_env / jwt_secret_env): buildSEP10Validator
//     errors "not configured"; we wire the Noop (404 sep10-unavailable on /v1/auth/sep10/*) so
//     the binary still boots — the common r1 auth_mode=apikey_optional case.
//   - SEP-10 configured + Redis available: the guarded validator, in EVERY
//     auth_mode.
//   - SEP-10 configured + Redis absent: FAIL CLOSED. buildSEP10Validator
//     never returns a guard-free validator (it errors ErrReplayGuardUnavailable);
//     under auth_mode=sep10 that error aborts startup, otherwise it degrades to
//     the Noop. We never serve SEP-10 without the replay guard.
//   - SEP-10 configured + no account lake (accounts nil): the same fail-closed
//     policy via ErrAccountLoaderUnavailable. We never authenticate an
//     on-chain account without checking its signers and medium threshold.
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
		// auth_mode=sep10 makes this a hard failure — we MUST have a
		// validator to bootstrap auth at all. Otherwise log + carry on
		// with a Noop so the handlers return 404 specifically for
		// /v1/auth/sep10/* without taking down the rest of the API.
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

	// Wire the Redis-backed replay
	// guard whenever the API has Redis (i.e. always in production).
	// Without it, a captured signed challenge XDR is reusable for
	// the full ChallengeTTL window — long enough for an attacker
	// who steals one signed XDR (e.g. via XSS exfil) to mint a
	// stream of JWTs after the user closes the tab.
	//
	// Fail LOUD, not open. Reaching this
	// function means SEP-10 auth is configured (seed_env + jwt_secret_env
	// are set, checked above). A nil Redis here would leave replayGuard
	// nil and the validator falls open — issuing JWTs with no replay
	// protection. Refuse to start so a misconfigured deploy is caught at
	// boot, not silently exploited for the ChallengeTTL window at runtime.
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

// lakeAccountSigners serves SEP-10's signer lookup from the lake's
// current account state. It is bound once the explorer reader has
// dialled; until then (or if the dial failed) every lookup errors, so
// verification fails closed.
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

// redisConfidenceLooker adapts the shared Redis client to
// v1.ConfidenceLooker by reading the JSON-encoded confidence.Score
// the aggregator writes at `confidence:<base>:<quote>:<window>`.
//
// Cache miss → (zero, false, nil); the v1 handler then leaves the
// confidence fields off the wire for that response. Read errors
// propagate so the handler can log; the response still ships
// without confidence.
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

// dexTVLGateSurface is this path's low-cardinality label on
// obs.PriceServe{Scam,Substance}WithheldTotal. A distinct constant, not
// a reuse of "price_read": an operator seeing a step-change needs to
// know WHICH surface withheld, and the TVL refresh asks the gates on a
// 10-minute background cadence over the whole pool token set, so its
// rate has nothing to do with request traffic.
const dexTVLGateSurface = "dex_tvl"

// dexTVLValueGate adapts the price-withholding chokepoint to the
// question the DEX TVL snapshot asks per reserve leg: may this
// platform publish a USD valuation of this asset at all?
//
// It answers through pricingguard.Gate — never by consulting either
// gate half itself, which is the MSP-cluster invariant
// (TestWithholdingGatesAreSpelledOnlyAtTheChokepoint) — so the TVL
// figure can never drift out of step with what /v1/price serves for
// the same asset.
//
// The quote fan-out is v1.Server.listingPriceAllowed's, via the shared
// pricingguard.AssetSubstanceVerdict: an asset's value is publishable
// when ANY of its plausible backing pairs clears the floor (vs XLM, the
// dominant on-chain quote; vs fiat:USD, which the alias union extends to
// the CEX-fed crypto:X series; or vs an operator-declared USD peg, the
// resolver's direct_usd route). The scam verdict is quote-independent,
// so a flagged issuer refuses the whole asset.
type dexTVLValueGate struct {
	substance *pricingguard.SubstanceGate
	scam      *pricingguard.ScamGate
	usdPegs   []canonical.Asset
}

// buildDEXTVLValueGate wires the TVL snapshot's trust gate from the
// guards this deployment actually built, and returns a nil INTERFACE
// when it built neither.
//
// The nil arm is why this is a function rather than a struct literal at
// the call site: v1.DEXTVLSources.Gate is an interface, and an
// interface holding a non-pointer struct is never == nil however empty
// the struct is. Assigning `dexTVLValueGate{}` unconditionally makes
// v1's documented "no gate wired"
// degradation unreachable in the API binary, so /v1/protocols would claim a
// substance screen that [pricing_guard] disable_substance_gate = true
// has switched off.
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

// Screens implements [v1.TVLValueGate]: the withholding screens THIS
// deployment wired, in the order the Basis sentence reads them. The
// scam directory comes first because its verdict is quote-independent
// (a flagged issuer withholds on every arm), the substance floor
// second. A gate whose sub-guard is nil is nil-receiver-safe and
// allow-everything, so naming it would claim a screen that never ran.
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

// ValueWithheld reports whether asset's reserves must contribute no USD
// value. Native XLM is never withheld — it is definitionally liquid and
// its identity pairs degenerate under the alias union, the same
// exemption listingPriceAllowed carries.
//
// The per-quote fan-out is one decision, so it is asked of the Gate as
// one question: the withheld metrics count the asset once per refresh.
func (g dexTVLValueGate) ValueWithheld(ctx context.Context, asset canonical.Asset) bool {
	gate := pricingguard.Gate{Substance: g.substance, Scam: g.scam}
	return gate.AssetValueWithholding(ctx, asset, g.usdPegs, dexTVLGateSurface) != pricingguard.NotWithheld
}

// buildSubstanceGate maps the [pricing_guard] config section onto the
// pricingguard substance policy. Returns nil — a valid
// allow-everything gate (nil-receiver safe) — when the operator
// disabled it.
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

// metadataStoreLookup adapts *timescale.Store to
// metadata.AccountObservationLookup. Only the absence of a row means
// "not observed"; a row with NULL home_domain or is_removal=true is an
// observation that the account has no home_domain.
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

// usdQuoteAsset is the implicit USD quote used to anchor 24h-ago
// price lookups in [storeChange24hReader]. Same string value as
// the v1 handler's defaultPriceQuote — keeping them constructed
// independently here avoids reaching into v1's unexported
// `mustParseAsset`.
var usdQuoteAsset = func() canonical.Asset {
	a, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		panic("stellarindex-api: USD quote asset must parse: " + err.Error())
	}
	return a
}()

// storeChange24hReader adapts *timescale.Store to v1.Change24hReader.
// Looks up the latest closed prices_1m bucket whose end is at-or-
// before now-24h for the asset/USD pair. sql.ErrNoRows (asset
// first traded < 24h ago, or retention pruned the bucket) is
// translated to v1.ErrChange24hUnavailable so the handler treats
// it as "feature unavailable for this asset" rather than a real
// failure. Other errors propagate unchanged.
//
// Pegs is the same operator-declared classic USD-pegged set used
// by the v1 handler's tryStablecoinFiatProxy fallback. When the
// literal asset/fiat:USD lookup misses (the steady-state case on
// Stellar mainnet — nothing on-chain quotes in fiat:USD), this
// adapter walks the pegs and re-runs the at-or-before lookup
// against asset/<peg>. First non-error result wins. Without
// this, /v1/assets/{id}.change_24h_pct silently stays null for
// every on-chain asset (the /v1/price handler has the same fallback).
//
// decimals is the confirmed non-7-decimals table. The bucket this
// returns is a RAW prices_1m ratio, and both callers divide it into a
// current price that is ALREADY decimals-normalised (lookupUSDPrice and
// the batch row), so an un-normalised anchor put the two legs of one
// percentage on different scales — see [normalizeChange24hAnchor].
type storeChange24hReader struct {
	s        *timescale.Store
	pegs     []canonical.Asset
	decimals aggregate.DecimalsLookup
	logger   *slog.Logger // nil → guard logging disabled
}

// change24hAnchorMaxStaleness lets the guard stand a last-known-good in
// for a refused anchor at any age: ClosedVWAP1mAtOrBefore itself puts no
// staleness bound on the anchor, so neither does its stand-in.
const change24hAnchorMaxStaleness = time.Duration(math.MaxInt64)

// errChange24hAnchorGuarded: the anchor bucket exists but the guard refused it.
var errChange24hAnchorGuarded = errors.New("change_24h anchor refused by the serving-sanity guard")

func (r storeChange24hReader) USDPrice24hAgo(ctx context.Context, asset canonical.Asset) (string, error) {
	at := time.Now().Add(-24 * time.Hour)
	// Each anchor is a raw prices_1m bucket, so it passes the point-in-time
	// serving-sanity guard like every other one that reaches a response. A
	// refused bucket is errChange24hAnchorGuarded: the next market is tried,
	// as for a pair with no bucket, and a manipulated minute is never the
	// divisor of a percentage.
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
	// Stablecoin-fiat proxy fallback: walk the operator's USD pegs
	// and try asset/<peg>. First non-error row wins.
	for _, peg := range r.pegs {
		// Fold through the alias registry like the other three
		// self-pair guards. An exact-spelling skip is not enough
		// here: of the two callers only handleAssetGet collapses the
		// request through canonical.CanonicalAsset before it reaches
		// this reader, while the batch row path parses the id and
		// passes it through as typed, so a SAC spelling arrived and
		// spent a read on a pair whose two sides are one asset.
		if canonical.CanonicalAsset(peg).Equal(canonical.CanonicalAsset(asset)) {
			continue
		}
		pegRow, pegErr := guardedAnchor24h(ctx, canonical.Pair{Base: asset, Quote: peg}, at)
		if pegErr == nil {
			// Against the peg it was READ from, not the requested
			// fiat:USD: the factor belongs to the pair behind the bucket.
			return normalizeChange24hAnchor(r.decimals, pegRow.VWAP, asset, peg)
		}
	}
	return "", v1.ErrChange24hUnavailable
}

// change24hAnchorDigits is the precision a normalised anchor is rendered
// at. The anchor is only ever a divisor for a two-decimal percentage, so
// this is generous rather than load-bearing — wide enough that scaling a
// small ratio DOWN cannot round it to zero.
const change24hAnchorDigits = 20

// normalizeChange24hAnchor applies the dex-nonstandard-decimals forward
// normalisation (aggregate.AdjustPrice) to the RAW 24h-ago bucket, with
// the legs of the pair it was actually read from.
//
// The current-price leg of change_24h_pct has been normalised since the
// /v1/assets price_usd fix; the anchor was not. So for a confirmed
// 9-decimals token the percentage compared a corrected price with a raw
// one a hundred times smaller, and a FLAT market served roughly +9900%.
// That is worse than both legs being raw, where the factor cancelled.
//
// Returns vwap byte-identical when the legs share a scale (every pair
// with no confirmed row — the common case). For a flagged pair a value
// that cannot be parsed cannot be corrected, and a raw anchor against a
// normalised price is the defect itself, so that reads as "no anchor"
// (v1.ErrChange24hUnavailable → a null change, never a wrong one).
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

// parseStreamingPairs converts the operator-declared
// `[api.streaming].pairs` TOML rows (each a [base, quote]
// two-element string array) into canonical Pairs.
//
// Validation is strict: each row must have exactly two non-empty
// strings, and each string must round-trip through
// canonical.ParseAsset. Any error returns immediately so the
// binary fails loud at startup rather than silently dropping
// pairs the operator expected to be streamed.
// subscriberRestartMinBackoff / subscriberRestartMaxBackoff bound the
// restart delay [runSubscriberSupervised] applies between
// consecutive Run attempts — 1s floor so a transient blip recovers
// fast, 30s ceiling so a sustained outage doesn't hot-loop
// reconnect attempts against Redis.
const (
	subscriberRestartMinBackoff = time.Second
	subscriberRestartMaxBackoff = 30 * time.Second
)

// subscriberRunner is the subset of [*redispub.Subscriber] that
// [runSubscriberSupervised] needs, narrowed to an interface so tests
// can inject a fake that fails on demand without a real/miniredis
// Redis connection. *redispub.Subscriber satisfies this with no
// changes.
type subscriberRunner interface {
	Run(ctx context.Context) error
	Channel() string
}

// runSubscriberSupervised drives sub.Run in a restart loop with
// exponential backoff.
// [redispub.Subscriber.Run]'s own doc says "any unexpected
// stream-end is surfaced as an error so the caller can decide
// whether to retry" — a caller that just logs the error and lets the
// goroutine exit for good leaves
// /v1/price/stream's closed-bucket feed permanently silent
// (heartbeats only, no price_update events) for the rest of the
// process lifetime after a single Redis pubsub hiccup. This
// fulfils the documented retry contract.
//
// The supervisor restarts sub.Run on ERRORS; it cannot restart it after a
// PANIC, because the panic unwinds past the loop. So the guard sits here,
// on the goroutine's own stack, and the trade-off is the usual one: the
// subscriber stops (SSE falls back to heartbeats) instead of the API
// process dying with every in-flight request on it.
func runSubscriberSupervised(ctx context.Context, sub subscriberRunner, logger *slog.Logger) {
	defer recoverBackgroundWorker(logger, "stream-subscriber")
	runSubscriberSupervisedWithBackoff(ctx, sub, logger, subscriberRestartMinBackoff, subscriberRestartMaxBackoff)
}

// runSubscriberSupervisedWithBackoff is [runSubscriberSupervised]
// with the backoff floor/ceiling as parameters so tests can run the
// real restart loop without waiting through production-sized delays.
// Backs off minBackoff..maxBackoff (doubling) between restarts; a
// run that stayed up for at least maxBackoff resets to the floor so
// a later blip doesn't inherit an already-maxed delay. Returns
// (instead of looping) on ctx cancellation or a nil error (clean
// shutdown).
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

// warnUnsafeBind logs a security warning at startup when the API
// is configured to listen on a non-loopback address WITHOUT a
// reverse-proxy CIDR allow-list. The combination is the classic
// "raw HTTP exposed to the public internet" footgun:
//
//   - 0.0.0.0:3000 binds to every interface
//   - empty TrustedProxyCIDRs means we trust X-Forwarded-For from
//     the immediate peer, OR (if empty list) we don't honour it at
//     all and everyone looks like the socket peer
//
// Operators running behind Caddy / Cloudflare / similar should
// either (a) bind to 127.0.0.1 + let the proxy share the host or
// (b) populate TrustedProxyCIDRs with the proxy's source range.
//
// Doesn't block startup — the binary still serves — but the
// warning is loud enough to surface in journalctl + log aggregation.
func warnUnsafeBind(logger *slog.Logger, listenAddr string, trustedProxyCIDRs []string) {
	// net.SplitHostPort correctly handles bracketed IPv6 literals —
	// "[::]:3000" → host "::", "[::1]:3000" → host "::1" — where the
	// naive strings.Cut(":") split "[::]:3000" into host "[" and never
	// matched the all-interfaces check, so a public IPv6 all-interfaces
	// bind would ship with NO warning.
	host, _, err := net.SplitHostPort(listenAddr)
	if err != nil {
		// Not a parseable host:port — can't classify the bind, so stay
		// silent rather than warn on something we don't understand.
		return
	}
	loopback := host == "127.0.0.1" || host == "::1" || host == "localhost"
	if loopback {
		return
	}
	// Empty host, 0.0.0.0, and :: all mean "every interface". After
	// SplitHostPort the brackets are already stripped, so a "[::]" listen
	// addr arrives here as "::".
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

// warnCollapsedStreamCap logs a warning at startup when the per-IP
// SSE concurrent-stream cap can't actually
// discriminate clients. The cap
// keys on middleware.RemoteIP, which only trusts X-Forwarded-For from
// TrustedProxyCIDRs; behind a reverse proxy WITHOUT that configured,
// every request resolves to the proxy's own single socket address, so
// MaxStreamsPerIP silently collapses into one shared global budget
// for every client instead of a per-client cap — the first
// MaxStreamsPerIP clients through the proxy exhaust it for everyone
// else. Loopback binds are exempt (no proxy in front by definition).
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

// warnCollapsedAnonThrottle logs a warning at startup when the
// anonymous HTTP request throttle can't discriminate clients behind a
// reverse proxy. The anonymous
// per-IP bucket keys on the resolved client IP (remoteIPPrefixFor —
// the forge-resistant XFF resolver), which only trusts X-Forwarded-For
// from TrustedProxyCIDRs. With trusted_proxy_cidrs EMPTY behind a
// reverse proxy, every anonymous request resolves to the proxy's own
// single socket address, so anon_rate_limit_per_min collapses into ONE
// shared bucket for the WHOLE anonymous tier — the first N/min anon
// requests through the proxy exhaust it for every other anonymous
// caller: an availability self-DoS on a fresh deploy (the default
// config ships trusted_proxy_cidrs=[]).
//
// Fires only when the anonymous tier is actually reachable AND capped:
// an active anon bucket (anon_rate_limit_per_min > 0) under an auth
// mode that admits anonymous callers (none / apikey_optional). Auth
// modes that require a credential (apikey / sep10) reject anonymous
// requests with 401 before the limiter, so the anon bucket is never
// exercised and no collapse can bite.
//
// Unlike warnCollapsedStreamCap this DOES fire for a loopback bind —
// bind is not consulted at all. The canonical R1 shape is
// 127.0.0.1:3000 behind Caddy, which is EXACTLY where the collapse
// bites; only a non-empty trusted_proxy_cidrs (as R1 correctly sets)
// silences it, never the bind address. We can't cheaply tell a
// direct-bind (no proxy) deploy apart from a behind-a-proxy one, so
// the message is worded conditionally ("if this API is behind a
// reverse proxy") rather than asserting a misconfiguration outright.
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

// authModeAdmitsAnonymous reports whether the configured auth_mode lets
// un-credentialed (anonymous) requests reach the handler stack — and
// therefore the anonymous rate-limit bucket. "none" attaches an
// anonymous Subject to every request; "apikey_optional" is the freemium
// shape (anonymous floor without a key). "apikey" and "sep10" require a
// credential and reject anonymous callers with 401 before the
// rate-limit middleware runs, so their anon bucket is never exercised.
func authModeAdmitsAnonymous(mode string) bool {
	switch mode {
	case "none", "apikey_optional":
		return true
	default:
		return false
	}
}

// warnOpenCORS logs a warning at startup when CORS is set to
// allow every origin AND auth_mode permits authenticated calls.
// The combination lets any third-party site issue authenticated
// requests against the API on behalf of a logged-in browser
// user — a classic CSRF amplifier when paired with cookie-based
// auth (we use bearer tokens, which mitigates the worst of it,
// but the wide-open posture is still a smell).
//
// Default config ships AllowedOrigins=[] (same-origin only); this
// warning only fires once an operator has
// explicitly opted into the wildcard.
// parseFiatPeggedClassics resolves the operator's
// pricing_guard.fiat_pegged_classic_assets map (classic asset_key →
// ISO-4217 ticker) into the asset_id → canonical fiat Asset map the
// declared-peg price fill consumes. Config.Validate already hard-fails
// malformed entries at load; the soft-fail here is belt-and-braces for
// non-Validate construction paths, mirroring TradesConfig.USDPeggedClassics
// (a missing peg is a smaller failure than refusing to start). Keys
// are re-canonicalised via Asset.String() so lookup never depends on
// the operator's exact spelling.
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

func warnOpenCORS(logger *slog.Logger, allowedOrigins []string, authMode string) {
	// Match middleware.CORS's own wildcard test (allowed["*"] set
	// membership) rather than requiring "*" to be the ONLY entry — a
	// config like ["*", "https://evil.com"] is just as wide open, since
	// CORS() echoes "*" to every origin regardless of the extra entries.
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

// requireEmailVerifiedOrNil returns the email-verification gate
// middleware when the operator has opted in via
// `cfg.API.SignupRequireEmailVerification`; nil keeps the gate
// off so the pre-gate wire contract is preserved.
func requireEmailVerifiedOrNil(enabled bool) middleware.Middleware {
	if !enabled {
		return nil
	}
	return middleware.RequireEmailVerified()
}

// touchUsageMiddlewareOrNil returns the wired TouchUsage
// middleware when BOTH a Postgres-backed keys store AND a Redis
// client are present; otherwise nil so the server's chain
// assembly skips it.
//
// Postgres is required
// for the actual UPDATE, Redis for the SETNX debounce. Either
// missing → legacy "no last_used updates" posture, which the
// dashboard renders as "—".
func touchUsageMiddlewareOrNil(keys platform.APIKeyStore, rdb redis.UniversalClient, logger *slog.Logger) middleware.Middleware {
	if keys == nil || rdb == nil {
		return nil
	}
	debouncer := auth.NewRedisTouchDebouncer(rdb, 0)
	return middleware.TouchUsage(keys, debouncer, logger.With("component", "touch-usage"))
}

// statusIdentity projects the operator's config onto the three
// /v1/status identity inputs: the region label, the deployment TIER,
// and the background services whose heartbeats the roll-up judges.
//
// Deployment tier and service list come from config, not a hardcode: a
// lean test net runs no aggregator, so a fixed {"indexer","aggregator"}
// list would pin `overall` at "degraded" forever (inventory
// `run_aggregator: false`), and a fixed "production" tier would label a
// test net PRODUCTION on the explorer's status page. The pubnet
// defaults ("production", indexer+aggregator) keep r1's wire contract
// identical.
func statusIdentity(cfg config.Config) (region, deployment string, services []string) {
	return cfg.Region.ID, cfg.Region.Deployment, cfg.API.StatusServices
}

// sessionPeekerAdapter bridges dashboardauth.SessionFromContext
// to v1.SessionPeeker so v1's /v1/account/me handler can read
// the magic-link session without importing dashboardauth.
//
// Stateless — the lookup is a context.Value read.
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

// prewarmCaches keeps the heaviest read caches hot. The
// /v1/sources?include=stats and /v1/markets / /v1/pools queries
// run aggregations over the trades hypertable that take 5–10s on
// a cold path; cache TTLs of 30s–10min mean a single user-pageload
// with no recent neighbours always pays the full cost. This
// goroutine keeps each entry alive on a cadence sized to each
// query's cost.
//
// Two cadences (one 25s cycle running the 8s source-stats query AND 12
// market/pool variants AND an assetsReader refresh held one Postgres backend at
// 76% CPU continuously as trades grew, with knock-on memory pressure from
// ZFS-ARC-vs-shared_buffers double-caching the working set):
//
//   - **Heavy** (sources_stats + source_volume_history_24h):
//     5 min cadence, query takes ~8s on a 3-month dataset and
//     scales linearly with data depth. The 24h-window data has
//     >5min freshness tolerance, so refresh-every-5min is well
//     within product semantics.
//   - **Light** (markets/pools/assetsReader): 60s cadence, queries
//     individually sub-second under normal load.
//
// Errors get logged at debug level — a transient warmup failure
// is rare and the next cycle retries. Stops on ctx cancel.
//
// The guard is registered HERE rather than at the `go prewarmCaches(…)`
// call site because it is the callee that runs on the detached
// goroutine's stack, and a guard that travels with the function stays
// correct if it is ever started from a second place. Started as a named
// function, so the panic-guard test resolves this declaration.
//
// lightCadence and issuersCacheTTL are package-level (rather than
// local to this function or to the cachedIssuersReader construction
// site) so the TTL-headroom invariant between them has one source of
// truth to reference instead of two copies that can silently drift
// apart.
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

	// Fire both immediately on startup so the first user request
	// after a binary restart hits a warm cache — CONCURRENTLY, because
	// running them in sequence meant the user-facing pass waited on the
	// operator-facing one.
	//
	// prewarmHeavy's sources_stats query alone takes ~8s (see its own
	// note below), and it ran FIRST. So for tens of seconds after every
	// restart the listing keys were still cold while the heavy
	// diagnostic aggregate churned. Measured on r1 with the API up at
	// 05:10:16:
	//
	//	05:10:37  10025 ms  /v1/assets?include=sparkline&limit=10&order_by=…
	//	05:10:37  10026 ms  /v1/assets?limit=50
	//	05:11:02   2016 ms  both again
	//
	// Real users, 21s and 46s after boot, paying cold fills for keys the
	// prewarm had not reached yet. Steady state was already healthy (a
	// constant 11 detail-prewarms per cycle, 94% cache hit rate), so this
	// is purely a startup-window defect — and it recurs on every deploy.
	//
	// The two passes touch disjoint readers (stats vs
	// markets/assets/issuers), so there is no shared state to race; each
	// already carries its own per-call deadlines.
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
	if _, err := stats.GetSourceStats(statsCtx); err != nil {
		logger.Debug("prewarm sources stats failed", "err", err)
	}
	if _, err := stats.GetSourceVolumeHistory24h(statsCtx); err != nil {
		logger.Debug("prewarm sources volume history failed", "err", err)
	}
	// Every /dexes/<source> page requests sparkline7d; unwarmed, that read
	// costs ~8.5s on the request path against ~1.1s without it.
	if _, err := stats.GetSourceVolumeHistory7d(statsCtx); err != nil {
		logger.Debug("prewarm sources volume history 7d failed", "err", err)
	}
}

// assetsPrewarmBatchTimeout bounds each of the two independent
// assetsReader batches in prewarmLight below: the /v1/coins + /v1/assets
// listing warm, and the native + verified-asset detail fan-out. A var
// rather than an inline constant so a test can substitute a short
// deadline and exercise expiry deterministically without a real 20s
// wait.
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
	// 5-min ceiling on the whole prewarm cycle. A 60s budget shared across
	// ~25 sequential calls is too short — when the first
	// few were slow (cold cache after API restart, ~8s each), the
	// budget was exhausted and the remaining ~20 calls all aborted
	// with `context deadline exceeded`. Net: cache stayed cold for
	// hours, sustained api_cache_miss_rate_high. The next prewarm
	// cycle starts 60s after the previous one's tick (not its
	// completion), but the call site is sequential so cycles
	// can't overlap; a 5-min ceiling means a slow cycle just
	// drops a couple of subsequent ticks rather than truncating
	// the in-flight cycle's per-key warming.
	mkCtx, mkCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer mkCancel()

	// ORDER MATTERS: the /v1/assets listing keys are warmed FIRST,
	// ahead of the markets/pools work below.
	//
	// Behind the ~20 cold markets/pools reads (DistinctPairsExt,
	// AllPools) at seconds each, a browser arriving seconds after a
	// restart would pay the fill itself. Measured on r1 with the API up
	// at 06:14:37, with the passes concurrent but assets warmed last:
	//
	//	06:15:01  9903 ms  /v1/assets?include=sparkline&limit=10&order_by=…
	//	06:15:01  9905 ms  /v1/assets?limit=50
	//
	// Concurrency between the two passes was necessary but not
	// sufficient — the queue inside THIS pass was the remaining cost.
	// /v1/assets is the most-requested route in production and backs the
	// explorer's landing page, so it goes first; markets and pools are
	// secondary and can warm behind it.

	// /v1/coins?limit=200&include=sparkline backs the unified
	// currencies listing — single most-trafficked asset-catalogue read.
	//
	// Important: the handler's `prependNative` path subtracts one
	// from `limit` when cursor/issuer/q are all empty (the explorer's
	// no-filter case) so it can splice the synthetic XLM row at the
	// top without overshooting the user's requested page size. So a
	// /v1/coins?limit=200 user request actually calls
	// `ListAssetsExt(ctx, ListAssetsOptions{Limit: 199, …})` under the
	// hood — passing Limit=200 here warms a different cache key than
	// the one the user request looks up. Mirror the listingLimit the
	// handler actually uses.
	assetsReaderCtx, assetsReaderCancel := context.WithTimeout(ctx, assetsPrewarmBatchTimeout)
	defer assetsReaderCancel()
	if _, err := assetsReader.ListAssetsExt(assetsReaderCtx, timescale.ListAssetsOptions{Limit: 199}); err != nil {
		logger.Debug("prewarm asset-catalogue listing failed", "err", err)
	}

	// …and the SEPARATE /v1/assets listing, whose handler does the
	// OPPOSITE arithmetic to /v1/coins above. See prewarmAssetListings.
	prewarmAssetListings(assetsReaderCtx, logger, assetsReader, snaps, catalogueLen)

	// /v1/assets/native is the most-trafficked single-asset
	// page (XLM is the explorer's default landing) and its
	// GetNativeAssetRow hits the heavy `listAssetsBaseSelect`
	// whole-asset-universe CTE — sub-200ms when cached, ~3s cold.
	// prewarmLight alone runs only ListAssetsExt; without this, native's
	// GetNativeAssetRow cache key is never touched and every native
	// page-load cold-fills it (1-3s on rapid retries as each per-asset
	// SWR entry fills incrementally). Drift-safe: this is the EXACT method
	// the /v1/assets/native handler calls
	// (asset_catalogue_extension.go GetNativeAssetRow path).
	//
	// Runs here, immediately after the other assetsReaderCtx calls
	// above, rather than after the markets/pools/per-DEX/per-CEX
	// loops below: those loops run against the separate
	// 5-minute mkCtx and can take most or all of assetsReaderCtx's
	// 20s budget just by elapsed wall-clock time, so deferring the
	// native/verified-asset warms until after them left this block
	// racing an exhausted or near-exhausted timeout on a cold cache.
	//
	// It gets its OWN fresh deadline rather than reusing
	// assetsReaderCtx: that context's budget started ticking
	// back at the /v1/coins warm above, and the listing warm's own
	// calls (ListAssetsExt plus every entry in
	// assetListingPrewarmOptions via prewarmAssetListings) can burn a
	// meaningful chunk of it on a cold cache before this line even
	// runs. Sharing one deadline across both batches means a slow
	// listing warm silently no-ops this whole batch on context
	// deadline exceeded (swallowed at Debug) — the cache reports
	// healthy and the next /v1/assets/native or verified-asset request
	// still pays the cold read.
	assetDetailCtx, assetDetailCancel := context.WithTimeout(ctx, assetsPrewarmBatchTimeout)
	defer assetDetailCancel()
	if _, err := assetsReader.GetNativeAssetRow(assetDetailCtx); err != nil {
		logger.Debug("prewarm native asset-catalogue row failed", "err", err)
	}

	// Every verified-currency canonical
	// asset_id gets the same warm-cache treatment as native. Without
	// this, /v1/assets/USDC-GA5Z…, /v1/assets/EURC-GDH…, etc. cold-
	// fill the heavy `listAssetsBaseSelect` chain on every
	// canonical-form request — measured 3.3s on r1 for USDC's
	// canonical form. The slug-form path (/v1/assets/usdc) was
	// already fast because the explorer happens to fan out to it,
	// but programmatic clients (and the explorer's drill-out from
	// market detail) navigate by canonical asset_id, which missed
	// the warm slot. Drift-safe: GetAssetByAssetID is exactly what
	// the handler calls (asset_catalogue_extension.go line 215).
	// Errors logged at Debug — a transient miss is fine since the
	// user request still fronts the cache.
	for _, assetID := range verifiedAssetIDs {
		prewarmAssetDetail(assetDetailCtx, logger, assetsReader, assetID)
	}
	// Native gets the same full fan-out treatment as verified assets.
	// GetNativeAssetRow above warms the single asset-catalogue-row SWR slot; this
	// covers the SIX OTHER readers /v1/assets/native fans out to.
	prewarmAssetDetail(assetDetailCtx, logger, assetsReader, "native")

	// Mirrors the most-trafficked /v1/markets, /v1/pools requests
	// the explorer fires (default order, no source filter). Each limit
	// is its own cache key under [v1.CachedMarketsReader.AllPools];
	// without per-limit prewarm, anything off the warmed key 503s
	// under the pools-server-timeout. The two routes get
	// SEPARATE limit sets — see marketsPrewarmLimits and
	// poolsPrewarmLimits for which callers each one is drawn from.
	//
	// Per-handler order semantics MUST match the cache key the
	// handler will look up:
	// - /v1/markets defaults to MarketsOrderVolume24hDesc ("" → 1); "pair"
	//   → 0 is explicit-only. The cache key includes the order, so both are warmed.
	// - /v1/pools defaults to MarketsOrderVolume24hDesc (handler
	//   accepts ""|"volume_24h_usd_desc" → 1). Prewarming with 0 would be a
	//   phantom slot — every cold-
	//   cache user request would still run a 10-30s SQL scan because the
	//   warmed key never matched (/v1/pools?source=sdex took 27s, soroswap 16s,
	//   phoenix 12s, aquarius 9s, comet 11s cold). Match the handler's default
	//   explicitly so the warmed key is the one users hit.
	// Important: the unfiltered /v1/pools handler builds
	// `PoolsFilter{Sources: v1.DexSourceNames()}` (the registry's
	// DEX list) — NOT `Sources: nil`. Cache key includes the
	// stringified Sources slice; passing `PoolsFilter{}` here
	// (`Sources: nil` → key fragment `[]`) warms a different key
	// than the user request lands on (`[aquarius comet phoenix
	// sdex soroswap]`). Mirror the handler's behaviour explicitly —
	// prewarmPools does exactly that, and is where the /v1/pools half
	// of this block now lives.
	for _, lim := range marketsPrewarmLimits {
		// Alphabetical (MarketsOrderPair). NOT the /v1/markets default —
		// that is volume-desc. Still worth prewarming:
		// it is the stable-keyset order a full-catalogue walker should
		// pass explicitly, and the volume-desc default is prewarmed just
		// below.
		if _, _, err := markets.DistinctPairsExt(mkCtx, "", lim, timescale.MarketsOrderPair); err != nil {
			logger.Debug("prewarm markets failed", "limit", lim, "order", "pair", "err", err)
		}
		// /v1/markets explorer-actual order — the home page,
		// HomeTopMarkets, sitemap, and embed/pair routes ALL pass
		// `?order_by=volume_24h_usd_desc`; warming only MarketsOrderPair would
		// leave every explorer pageload to hit the
		// 8s timeout. The cache
		// key includes the order, so the volume-desc variant is a
		// separate slot and needs its own prewarm.
		if _, _, err := markets.DistinctPairsExt(mkCtx, "", lim, timescale.MarketsOrderVolume24hDesc); err != nil {
			logger.Debug("prewarm markets failed", "limit", lim, "order", "volume_24h_usd_desc", "err", err)
		}
	}
	prewarmPools(mkCtx, logger, markets)

	// Per-DEX prewarm — the explorer's /dexes/{source} pages each
	// fire `/v1/pools?source=<dex>&limit=100`, which lands on a
	// distinct cache key per source. Without this, every page click
	// missed cache and ran a 10-30s full-window trades-hypertable
	// scan; sometimes returning a 503, sometimes
	// overshooting because the driver doesn't reliably propagate
	// context cancellation mid-query. Per-DEX prewarm runs the
	// canonical limit=100 + default order the explorer hits;
	// subsequent users land on warm cache (sub-second). Errors are
	// logged at Debug — a missed cycle is fine since the user
	// request still fronts the cache.
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

	// /v1/issuers had NO prewarm at all, while CachedIssuersReader's TTL
	// is 5 minutes — so the slot expired every 5 min and the next caller
	// paid the full cold fill. Measured on r1: 1.212s cold vs
	// 0.129s warm. At production's ~0.08 rps most requests arrive AFTER
	// the TTL has lapsed, so the cold path was close to the common case
	// rather than a startup-only cost, and it showed up as recurring
	// multi-second p99 spikes.
	//
	// The underlying query cannot be indexed out of the problem — the
	// reader's own doc records why (two seq scans feeding a HashAggregate
	// over 57k groups; ~196ms at the DB alone). Keeping the slot warm is
	// the available fix.
	//
	// This runs on the 60s light cadence — 5x inside the 5-minute TTL, so
	// a dropped cycle still cannot expose a cold slot.
	prewarmIssuers(mkCtx, logger, issuers)

	// /v1/network/stats (CachedNetworkStatsReader, main.go) had no
	// prewarm at all: the reader was constructed but never
	// threaded into this goroutine, so every binary restart left the
	// explorer's network strip paying the full ~485ms p95 network-wide
	// aggregate inline on the first request instead of getting a warm
	// SWR value. Steady state is largely self-healing (the reader
	// serves stale-while-revalidate once any value exists), so this
	// closes the cold-start gap specifically.
	prewarmNetworkStats(mkCtx, logger, networkStats)
}

// marketsPrewarmLimits are the `?limit=` values the /v1/markets prewarm
// warms, for both orders. Unchanged from the single shared set these were
// split out of: 5 (audit script), 25 (the explorer's HomeTopMarkets), 100
// (OpenAPI default + MarketsTable + the asset page's markets tab), 200
// (currencies listing).
//
// HomeRecentTrades asks for `?limit=3`, which is NOT warmed and stays that
// way deliberately: measured on r1, a never-requested
// DistinctPairsExt limit costs 0.60-0.63 s cold, below the bar worth a
// permanent background query, and the live `?limit=3` slot answered in
// 0.8 ms because real traffic keeps it warm.
var marketsPrewarmLimits = []int{5, 25, 100, 200}

// poolsPrewarmLimits are the `?limit=` values the /v1/pools prewarm warms.
//
// Separate from marketsPrewarmLimits. Sharing one set
// meant the pools warm-up inherited limits chosen for /v1/markets, and the
// explorer's own pool tables ask for something else — `AllPools` keys on
// the limit, so the warmed slots were beside the point:
//
//	/v1/pools?limit=8&order_by=volume_24h_usd_desc     2.197 s   (unwarmed)
//	/v1/pools?limit=5   / 25 / 100 / 200               0.0008-0.0026 s (warmed)
//
// 8 is the missing one, and it is not an invented probe value — it is what
// the shipped explorer's /network page sends for its "Top Stellar markets"
// panel: NetworkView.tsx's TopMarkets calls usePools(8,
// 'volume_24h_usd_desc') (web/explorer/src/api/hooks.ts). The origin's own
// slow-request log recorded the shape verbatim:
//
//	{"path":"/v1/pools","latency_ms":2197.052,
//	 "query_shape":"limit=8&order_by=volume_24h_usd_desc","slow":true}
//
// It did not show up in production request logs because the site is still
// pre-launch: 24 h of non-curl /v1/pools traffic is the Next.js static
// export build plus small ?base=&quote= fetches. An external live probe
// is the right instrument for a site with no organic traffic yet.
//
// This is the THIRD instance of the same bug class in this file — see the
// MarketsOrderPair phantom slot recorded in prewarmLight and
// assetListingPrewarmLimits' missing `?limit=50`. The lesson each
// time: derive the set from what callers SEND, and keep the prewarm
// argument byte-identical to the handler's.
//
// 100 stays for /dexes (PAGE_LIMIT) and the OpenAPI default; 5/25/200 stay
// for the audit script, the Scalar default and the currencies listing.
var poolsPrewarmLimits = []int{5, 8, 25, 100, 200}

// prewarmPools warms the unfiltered /v1/pools listing slots.
//
// Split out of [prewarmLight] so the warmed cache key can be asserted
// without standing up the assets/issuers readers — the same reason
// prewarmIssuers is its own function. The point of the guard is that the
// arguments are BYTE-IDENTICAL to the ones handlePools looks up: the
// registry's DEX source set (not nil), an empty cursor, and
// MarketsOrderVolume24hDesc (the handler's default for ""|
// "volume_24h_usd_desc"). Every one of those three has been the drifted
// dimension in a real incident.
func prewarmPools(ctx context.Context, logger *slog.Logger, markets *v1.CachedMarketsReader) {
	if markets == nil {
		return
	}
	filter := timescale.PoolsFilter{Sources: v1.DexSourceNames()}
	for _, lim := range poolsPrewarmLimits {
		if _, _, err := markets.AllPools(ctx, filter, "", lim, timescale.MarketsOrderVolume24hDesc); err != nil {
			logger.Debug("prewarm pools failed", "limit", lim, "err", err)
		}
	}
}

// assetListingPrewarmLimits are the `?limit=` values /v1/assets callers
// actually send, taken from OBSERVED traffic rather than assumption.
//
// An earlier revision of this list was inferred from the explorer's
// source and would have been wrong: it warmed 1/10/100/500 and missed
// `?limit=50` entirely, which turned out to be the single most
// expensive shape in production. The query-shape logging
// is what made the real distribution visible.
//
// Measured on r1 over 100 minutes, slow
// requests (>=500 ms) grouped by shape, excluding SSE streams:
//
//	18x  avg 4053 ms   72.9 s total   real   ?limit=50
//	16x  avg 4485 ms   71.8 s total   real   ?include=sparkline&limit=10&order_by=volume_24h_usd_desc
//	12x  avg 1192 ms   14.3 s total   probe  (/v1/pairs, not this route)
//	 6x  avg  960 ms    5.8 s total   probe  (/v1/issuers, not this route)
//	 1x  avg 2008 ms                  probe  ?limit=500
//	 1x  avg 1790 ms                  real   ?limit=100
//	 1x  avg 1607 ms                  probe  ?limit=5
//
// The two real shapes are 84% of all slow time on the API.
//
// `include=sparkline` is deliberately NOT a dimension here: measured at
// the origin it costs nothing (7.8 ms with it, 8.8 ms without, on the
// same warm listing), because the sparkline attach reads its own
// already-warm per-asset slots rather than changing this cache key.
var assetListingPrewarmLimits = []int{1, 5, 10, 50, 100, 500}

// prewarmAssetListings warms the /v1/assets listing keys.
//
// The handler overfetches by one — `ListAssetsOptions{Limit: limit + 1}`
// — so a `?limit=50` request looks up the key for 51. That +1 arrived
// with cursor pagination (before it, `len(rows) >
// limit` was never true and only the first page of ~199K assets was
// reachable). The prewarm was never updated to match, so /v1/assets has
// had NO warm key of its own since.
//
// The overfetch is easy to disbelieve, so it is measured. At the r1
// origin, with `?limit=50` kept warm by live traffic:
//
//	?limit=50  → internal Limit 51   0.007 s
//	?limit=51  → internal Limit 52   1.359 s
//
// Two adjacent user-facing limits, 180x apart, differing only in which
// internal key live traffic happens to keep warm. That is also why this
// helper takes USER-facing limits and applies the +1 itself: doing the
// arithmetic in one place is the difference between warming the slot
// callers hit and warming a phantom one.
//
// Both orders are warmed. `order_by` is absent on most requests, which
// parseAssetsOrder maps to AssetsOrderObservationCountDesc, but the
// explorer's home table asks for volume_24h_usd_desc.
//
// Every other option stays at its zero value on purpose: those are the
// no-filter, no-cursor requests a first page-load makes. A filtered or
// paginated request is a deliberate narrowing by a caller already past
// the landing page.
// Each successfully warmed variant is also persisted to Redis
// (`snaps`, nil-safe) so the NEXT process can seed this cache before it
// starts listening — see assets_listing_snapshot.go. The read goes
// through ListAssetsExtAt rather than ListAssetsExt so the snapshot
// carries the rows' REAL observation time: when this cycle is itself
// served a stale entry, re-persisting must not reset the age of data
// nothing has re-observed.
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
	// The unified (asset_class=all) landing page's catalogue phase
	// (serveCatalogueUnifiedPage) fills any shortfall below the user's
	// limit from the classic phase with Limit=(userLimit-catalogueLen)+
	// AssetsListOverfetchBy, Order=Volume24hUSDDesc always — a DIFFERENT
	// cache key than the userLimit+1 set above the moment the catalogue
	// is non-empty (it is: ~45 rows). Mirror that arithmetic so the
	// landing page's first load actually lands on a warmed slot instead
	// of a phantom one.
	for _, opts := range catalogueFillPrewarmOptions(catalogueLen) {
		warmAssetListingOptions(ctx, logger, assetsReader, snaps, opts)
	}
}

// warmAssetListingOptions runs and persists one prewarm variant. Split out
// so [prewarmAssetListings] can drive both the direct-handler cache keys
// (assetListingPrewarmOptions) and the unified-listing catalogue-fill keys
// (catalogueFillPrewarmOptions) through the same read+persist path.
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

// catalogueFillPrewarmOptions is the classic-phase cache key the unified
// listing's catalogue-fill call (serveCatalogueUnifiedPage) actually
// requests for each userLimit in assetListingPrewarmLimits, given a
// catalogue of catalogueLen rows. Skips a userLimit the catalogue alone
// satisfies (remaining <= 0) — that page never reaches the classic phase.
//
// Kept separate from assetListingPrewarmOptions (same "testable without a
// database" reasoning as that function's own doc) rather than folded into
// it: the two mirror two DIFFERENT handlers' arithmetic and collapsing
// them would make a future drift in either one indistinguishable from the
// other in a diff.
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

// assetListingPrewarmOptions is the exact set of cache keys to warm.
//
// Separated from the IO so the arithmetic is testable on its own: the
// `+1` and the limit set are the two things that decide whether this
// warms the slots callers hit or a set of phantom ones, and neither
// needs a database to check.
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

// classicLakeSupplySweepGap is the pause between lake-supply prewarm
// sweeps.
//
// Sized against the cache's own 30-minute TTL rather than against the
// cost of a sweep, because those are wildly different numbers: a sweep
// with nothing expired is a dozen already-warm listing cache reads and
// ZERO ClickHouse work — every asset it looks at has a live entry, so no
// batch is cut. The expensive sweeps are the cold one at boot and the one
// that lands after a TTL generation lapses.
//
// Since the population is warmed in one pass, it also EXPIRES in one, so
// this gap is exactly the window in which a caller can still be served the
// trustline-only figure. A minute keeps that window small; a 5-minute
// cadence like the loop above would leave the understatement reachable for
// 5 minutes out of every 30, which is most of the defect back again.
const classicLakeSupplySweepGap = time.Minute

// prewarmClassicLakeSupply keeps the per-asset lake-flows supply cache
// warm for the assets the /v1/assets listing actually serves.
//
// Without it, that cache warms only from the request path — 32 assets per
// request — so on a service with no consumer traffic it is cold by
// default: entries expire unread and both /v1/assets and /v1/rwa/assets
// fall back to the trustline-only sum, which cannot see supply held in
// claimable balances, LP reserves or SAC contract_data. Measured on r1
// ~19 h after the last request, PYUSD served 3,149,454 against
// a lake reading of 11,778,001 and XRF 21,895,149 against 118,333,629.
//
// [assetListingPrewarmOptions] is passed rather than an asset list so the
// warmed population IS the population prewarmAssetListings keeps warm.
// Recomputed per sweep, not hoisted, so the two stay identical if the
// shape set ever changes.
//
// Sweep-then-sleep rather than a ticker, like the protocol sweep: a cold
// pass takes minutes and overlapping passes would double the ClickHouse
// load exactly when it is already highest.
//
// Named rather than inlined at the call site so the startup property — the
// first sweep runs immediately, not one sweep gap after boot — can be
// asserted without standing up run(). It takes no logger because it decides
// nothing worth reporting: the listing read and the lake read each log their
// own failures inside the Server, where the detail lives.
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

// prewarmIssuerLimits are the /v1/issuers limits worth keeping warm.
//
// CachedIssuersReader serves every limit from one ceiling-sized entry, so
// the first of these fills it and the rest are hits; the list stays so the
// guard test can prove each real caller's limit lands warm — the /v1/pools
// lesson in [prewarmLight], where a mismatched key left every user request
// paying 10-30s against a cache that looked warm.
//
// These are the limits real callers actually send:
//   - 1 and 100 — the explorer.
//   - 5 — scripts/dev/r1-smoke.sh.
//   - 100 — the sla-probe, the OpenAPI default, AND the value
//     handleIssuersList falls back to when `limit` is omitted, which
//     makes it the one that matters most.
var prewarmIssuerLimits = []int{1, 5, 100}

// prewarmIssuers keeps the /v1/issuers slots warm.
//
// Split out of [prewarmLight] so the limit set can be asserted without
// standing up the markets/assets readers — the point of the guard is
// that these limits track real callers, and that is exactly the kind of
// thing that silently drifts when someone changes a caller.
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

// prewarmNetworkStats keeps the single /v1/network/stats SWR slot warm.
//
// Split out of [prewarmLight] like prewarmIssuers, so the call can be
// asserted in isolation without standing up the markets/assets readers.
func prewarmNetworkStats(ctx context.Context, logger *slog.Logger, networkStats *v1.CachedNetworkStatsReader) {
	if networkStats == nil {
		return
	}
	if _, err := networkStats.GetNetworkStats(ctx); err != nil {
		logger.Debug("prewarm network stats failed", "err", err)
	}
}

// prewarmAssetDetail warms every SWR cache key the
// /v1/assets/{id} handler fans out to for a single asset.
//
// /v1/assets/{id} fires SEVEN SWR-cached reader calls per request
// (full fan-out at internal/api/v1/asset_catalogue_extension.go):
//
//	GetAssetByAssetID         — the asset-catalogue row itself
//	GetAssetTopMarkets(id, 5) — top 5 markets per asset
//	GetAssetPriceHistory24h   — 24h sparkline
//	GetAssetPriceHistory7d    — 7d sparkline
//	GetAssetMarketsCount      — total markets count
//	GetAssetTradeCount24h     — 24h trade count
//	GetAssetATH               — all-time high
//
// Warming only GetAssetByAssetID leaves the other SIX readers to cold-fill
// on first hit, costing ~2s on
// /v1/assets/USDC-GA5Z…'s first request post-restart even though
// subsequent hits serve sub-ms warm.
//
// Drift-safe: each call uses the EXACT method the handler calls
// (per asset_catalogue_extension.go), so the cache-key shapes match
// byte-for-byte. This is the lock-in that keeps the warm slot landing on the
// same key the user request hits.
//
// Errors logged at Debug — transient misses are fine because the
// user request still fronts the cache (cold-fill happens on the
// user's request path if prewarm missed).
//
// Limit `5` for GetAssetTopMarkets matches the handler's literal
// (asset_catalogue_extension.go:77 → `GetAssetTopMarkets(ctx, assetID, 5)`).
// If the handler later varies the limit (e.g. higher for verified
// currencies), this prewarm must mirror the new value — drift in
// limit means a different SWR cache key, same bug class.
func prewarmAssetDetail(ctx context.Context, logger *slog.Logger, assetsReader *v1.CachedAssetsReader, assetID string) {
	// Per-reader prewarm. We don't bail on the first failure — each
	// reader has its own cache slot and a partial prewarm still
	// helps subsequent reads.
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

// prewarmAssetCall shrinks the per-reader-Debug-log boilerplate
// into one site so adding/removing readers in [prewarmAssetDetail]
// stays a single-line change. The `any` return type allows
// uniform handling across the diverse reader signatures.
func prewarmAssetCall(ctx context.Context, logger *slog.Logger, name, assetID string, fn func() (any, error)) {
	if _, err := fn(); err != nil {
		logger.Debug("prewarm asset call failed", "reader", name, "asset_id", assetID, "err", err)
	}
	_ = ctx // each fn already captures the context; arg kept for symmetry / future timeout pattern.
}

// selfPrewarmAdmitted reports whether authMode lets the self-prewarm's
// credential-less GET /v1/assets/{id} through Auth. Under apikey and sep10
// every such request 401s and spends the loopback failed-auth budget, so
// the loop must not start. Unknown modes are refused (fail closed).
func selfPrewarmAdmitted(authMode string) bool {
	switch authMode {
	case "", "none", "apikey_optional":
		return true
	}
	return false
}

// selfPrewarmAssetEndpoints loops every 60s and HTTP-GETs
// /v1/assets/<id> for native + every verified currency, against
// our own listener. This warms ALL caches the handler touches —
// not just the 7 CachedAssetsReader SWR slots that prewarmCaches
// already covers, but also the F2-path readers (Volume24hUSDForAsset,
// supply.LatestSupply, lookupUSDPrice, populateChange24h) that
// prewarmCaches doesn't know about. Drift-safe by construction:
// the call hits the same Server.Handler the user request would,
// so every internal lookup happens with byte-identical args.
//
// Per `feedback_prewarm_handler_drift`: this is the canonical
// pattern when handler fan-out is wider than the prewarm
// goroutine's per-reader enumeration. Adding a new reader to the
// /v1/assets/{id} handler tomorrow needs zero update here —
// because we don't enumerate readers, we just exercise the
// handler.
//
// Initial 3s sleep: lets the listener bind + lets prewarmCaches'
// first cycle settle (so the user-facing latency we measure on
// first warm hit reflects steady state, not the
// boot-sequence-race window).
func selfPrewarmAssetEndpoints(ctx context.Context, logger *slog.Logger, listenAddr string, verifiedAssetIDs []string) {
	// Guarded on the callee side for the same reason as prewarmCaches:
	// this runs on a detached goroutine, and every request it issues
	// travels the full /v1/assets/{id} handler fan-out, so it exercises
	// more code than any other warmer here.
	defer recoverBackgroundWorker(logger, "self-prewarm")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return
	}

	baseURL := fmt.Sprintf("http://%s/v1/assets/", listenAddr)
	client := &http.Client{Timeout: 30 * time.Second}

	// native first — biggest cache-miss surface (explorer's default
	// landing). Then every verified currency.
	targets := append([]string{"native"}, verifiedAssetIDs...)

	// The explorer opts into include_thin, which is its own cache entry.
	queries := []string{"", "?include_thin=true"}
	warm := func(id, query string) {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+id+query, nil)
		if err != nil {
			logger.Debug("self-prewarm request build failed", "asset_id", id, "err", err)
			return
		}
		// Mark as synthetic so obs.HTTPMetrics keeps these
		// deliberately-cold warming requests out of the
		// customer-facing latency histogram + SLO. Without this
		// the prewarmer's own ~570ms cold misses dominate p95/p99.
		req.Header.Set("User-Agent", "stellarindex-prewarm/1")
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			if ctx.Err() == nil {
				logger.Debug("self-prewarm GET failed", "asset_id", id, "query", query, "err", err, "elapsed", elapsed.String())
			}
			return
		}
		_ = resp.Body.Close()
		logger.Debug("self-prewarm /v1/assets", "asset_id", id, "query", query, "status", resp.StatusCode, "elapsed", elapsed.String())
	}
	runPass := func() {
		for _, id := range targets {
			for _, query := range queries {
				if ctx.Err() != nil {
					return
				}
				warm(id, query)
			}
		}
	}

	// Initial pass + steady-state cadence. 60s matches prewarmCaches'
	// lightCadence so F2-path caches don't expire between cycles
	// (their underlying TTLs are 1–2 min).
	runPass()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runPass()
		}
	}
}

// storePriceAtReader adapts *timescale.Store to v1.PriceAtReader —
// the point-in-time lookup behind /v1/price/at AND the
// per-horizon references behind /v1/price/changes. Delegates to
// ClosedVWAPAtOrBefore, which picks the finest CAGG resolution
// (prices_1m → … → prices_1d) whose nearest at-or-before bucket is
// within maxStaleness. sql.ErrNoRows translates to the sentinel so
// the handler can 404 (or null a horizon) honestly.
// This seam reads the SAME prices_1m closed
// buckets as wiring.StorePriceReader.LatestPrice, so it must carry the SAME
// withholding gates — otherwise one extra path segment (/v1/price/at,
// /v1/price/changes) republishes every price /v1/price refuses. The
// gates live here, at the reader seam, rather than in each handler:
// both leaking routes call PriceAt, so gating once covers both and any
// future PriceAt consumer inherits it. See wiring.PriceWithheld().
//
// The thin-market half is asked about `ts`, not about now. The
// number served here is the bucket at-or-before ts, and a trailing
// window ending today decides nothing about it — it withheld the whole
// history of a market that has since gone quiet, and it passed the
// dust-seeded early history of a market that has since become real.
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
	// The ladder's FINEST rung is the same raw prices_1m
	// closed bucket wiring.StorePriceReader.LatestPrice serves — a bare
	// Σ(quote)/Σ(base) CAGG bucket with no outlier filter, no volume
	// floor and no freeze protection. Carrying only the withholding
	// gates here left /v1/price/at and every /v1/price/changes horizon
	// republishing the manipulated minute /v1/price refuses, though
	// pricingguard's package doc claimed to cover every raw-bucket
	// path. Apply the same trailing-baseline guard, on the 1m rung
	// only: the coarser rungs are hour/day bars a trailing 1-minute
	// baseline cannot judge. A candidate the guard cannot serve is
	// ErrPriceAtGuarded, not ErrPriceAtUnavailable: the bucket exists and
	// was refused, so the handler answers price-withheld (or marks the
	// horizon withheld) rather than claiming there is no data.
	if row.Resolution == timescale.Granularity1m {
		served, ok := pricingguard.GuardServedVWAP1mAt(ctx, r.s, r.logger, pair,
			timescale.Vwap1mRow{Bucket: row.Bucket, VWAP: row.VWAP}, ts, maxStaleness)
		if !ok {
			return "", time.Time{}, 0, v1.ErrPriceAtGuarded
		}
		row.VWAP, row.Bucket = served.VWAP, served.Bucket
	}
	// observed_at = bucket close = bucket start + resolution: the
	// instant the bucket's VWAP became final (ADR-0015). window_seconds
	// carries the resolution so the handler labels it honestly.
	return row.VWAP, row.Bucket.Add(row.Resolution.BucketDuration()), row.Resolution.Seconds(), nil
}
