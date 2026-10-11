// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardkeys"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardpricealerts"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardwebhooks"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

// Coverage for the run() wiring helpers, which run() alone calls. The assertions are structural:
// which object came back, and whether the caller's nil-check on it behaves as intended.

// ─── logging capture ─────────────────────────────────────────────

// logRecorder captures slog output so a test can assert on the WARN /
// ERROR an operator would see. Several helpers below degrade rather
// than fail; on those paths the log line is the only signal the
// degrade happened, so it is part of the contract.
type logRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *logRecorder) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Split(strings.TrimSpace(r.buf.String()), "\n")
}

// hasLevelWithText reports whether any record at the given level
// contains text.
func (r *logRecorder) hasLevelWithText(level, text string) bool {
	for _, ln := range r.lines() {
		if strings.Contains(ln, "level="+level) && strings.Contains(ln, text) {
			return true
		}
	}
	return false
}

func recordingLogger() (*slog.Logger, *logRecorder) {
	rec := &logRecorder{}
	return slog.New(slog.NewTextHandler(rec, &slog.HandlerOptions{Level: slog.LevelDebug})), rec
}

func testRedisClient(t *testing.T) redis.UniversalClient {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// ─── nilOrMounter ────────────────────────────────────────────────
//
// The typed-nil trap: v1.Server gates each dashboard route family on a nil check, and an
// interface holding a typed-nil pointer is not nil. Without this helper the routes would go
// live with a nil receiver behind them, answering on a route the operator believes is disabled.

// TestNilOrMounter_TypedNilBecomesUntypedNil is the direct assertion:
// what comes back must compare == nil, which the typed nil handed in
// does not.
func TestNilOrMounter_TypedNilBecomesUntypedNil(t *testing.T) {
	t.Parallel()

	// The trap itself, stated as an assertion so the test documents
	// what it guards. If this stops being true, Go's interface
	// semantics changed and the helper is unnecessary.
	var naked v1.DashboardAuthMounter = (*dashboardkeys.Handlers)(nil)
	if naked == nil {
		t.Fatal("a typed-nil *dashboardkeys.Handlers in a DashboardAuthMounter compared == nil; " +
			"the premise of nilOrMounter no longer holds and the helper should be re-derived")
	}

	// Every concrete type the four call sites instantiate T with.
	if got := nilOrMounter((*dashboardauth.Handlers)(nil)); got != nil {
		t.Errorf("nilOrMounter((*dashboardauth.Handlers)(nil)) = %#v, want a nil interface — "+
			"server.go mounts /v1/auth/* on `!= nil` and would serve them with a nil receiver", got)
	}
	if got := nilOrMounter((*dashboardkeys.Handlers)(nil)); got != nil {
		t.Errorf("nilOrMounter((*dashboardkeys.Handlers)(nil)) = %#v, want a nil interface", got)
	}
	if got := nilOrMounter((*dashboardwebhooks.Handlers)(nil)); got != nil {
		t.Errorf("nilOrMounter((*dashboardwebhooks.Handlers)(nil)) = %#v, want a nil interface", got)
	}
	if got := nilOrMounter((*dashboardpricealerts.Handlers)(nil)); got != nil {
		t.Errorf("nilOrMounter((*dashboardpricealerts.Handlers)(nil)) = %#v, want a nil interface", got)
	}
}

// TestNilOrMounter_NonNilPassesThroughUnchanged — the other half. A
// helper that returned nil for everything would also pass the test
// above, and would silently disable the whole dashboard.
func TestNilOrMounter_NonNilPassesThroughUnchanged(t *testing.T) {
	t.Parallel()

	h, err := dashboardkeys.NewHandlers(dashboardkeys.Config{
		Keys:   stubKeysForBuild{},
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("dashboardkeys.NewHandlers: %v", err)
	}
	got := nilOrMounter(h)
	if got == nil {
		t.Fatal("nilOrMounter dropped a live *dashboardkeys.Handlers — /v1/dashboard/keys would 404")
	}
	if got != v1.DashboardAuthMounter(h) {
		t.Errorf("nilOrMounter returned %#v, want the same handlers it was given", got)
	}
}

// TestNilOrMounter_UnwiredBundleLeavesDashboardRoutesUnmounted threads an empty dashboardBundle
// through nilOrMounter into v1.Options, as run() does, and asserts the routes 404 instead of
// reaching a nil receiver and panicking.
func TestNilOrMounter_UnwiredBundleLeavesDashboardRoutesUnmounted(t *testing.T) {
	t.Parallel()

	// Exactly what run() does at main.go:1449-1452, over the zero
	// bundle buildDashboardBundle returns for an unconfigured dashboard.
	var bundle dashboardBundle
	srv := v1.New(v1.Options{
		Logger:               discardLogger(),
		DashboardAuth:        nilOrMounter(bundle.auth),
		DashboardKeys:        nilOrMounter(bundle.keys),
		DashboardWebhooks:    nilOrMounter(bundle.webhooks),
		DashboardPriceAlerts: nilOrMounter(bundle.priceAlerts),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// One GET route per mounter, taken from each package's Mount.
	for _, path := range []string{
		"/v1/auth/callback",          // dashboardauth
		"/v1/dashboard/keys",         // dashboardkeys
		"/v1/dashboard/webhooks",     // dashboardwebhooks
		"/v1/dashboard/price-alerts", // dashboardpricealerts
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s returned %d, want 404 — the dashboard is unwired, so this "+
				"route must not be mounted at all. A non-404 means a typed-nil handler "+
				"was mounted and is now serving with a nil receiver.",
				path, resp.StatusCode)
		}
	}
}

// ─── buildAuthMiddleware ─────────────────────────────────────────

// TestBuildAuthMiddleware_ModeDispatch pins the auth_mode to middleware mapping. A mode that
// should return a middleware but returns nil is an auth bypass, since the stack omits absent
// middleware. The unknown-mode arm logs and falls through to no-auth (fail-open) on purpose;
// the companion test pins the log, which is the operator's only warning.
func TestBuildAuthMiddleware_ModeDispatch(t *testing.T) {
	t.Parallel()

	// A Redis-backed validator needs a client; nil is fine for the
	// modes under test because buildAPIKeyValidator falls back to the
	// Noop validator (and logs) rather than panicking.
	opts := authValidatorOptions{}

	cases := []struct {
		mode      string
		wantNil   bool
		rationale string
	}{
		{"", true, "unset auth_mode is the documented no-auth default"},
		{"none", true, "explicit no-auth"},
		{"sep10", false, "SEP-10 must be enforced"},
		{"apikey", false, "API-key auth must be enforced"},
		{"apikey_optional", false, "optional mode still attaches the subject-resolving middleware"},
		{"APIKEY", true, "mode match is case-SENSITIVE: an operator typo falls through to no-auth"},
		{"api-key", true, "an unknown mode falls through to no-auth (logged as an error)"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			got := buildAuthMiddleware(tc.mode, opts, discardLogger())
			if tc.wantNil && got != nil {
				t.Errorf("buildAuthMiddleware(%q) returned a middleware, want nil — %s", tc.mode, tc.rationale)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("buildAuthMiddleware(%q) returned nil, want a middleware — %s. "+
					"A nil here is an AUTH BYPASS: the server omits absent middleware, so "+
					"every route serves unauthenticated.", tc.mode, tc.rationale)
			}
		})
	}
}

// TestBuildAuthMiddleware_UnknownModeIsNotSilent — the fall-through arm
// is fail-open, so the error log is the only signal an operator gets
// that their auth_mode never took effect.
func TestBuildAuthMiddleware_UnknownModeIsNotSilent(t *testing.T) {
	t.Parallel()

	logger, records := recordingLogger()
	if got := buildAuthMiddleware("apikeys", authValidatorOptions{}, logger); got != nil {
		t.Fatalf("buildAuthMiddleware(%q) = %#v, want nil", "apikeys", got)
	}
	if !records.hasLevelWithText("ERROR", "unknown auth_mode") {
		t.Errorf("an unknown auth_mode logged %v; want an ERROR naming it — this arm serves "+
			"the whole API unauthenticated, so the log is the operator's only warning",
			records.lines())
	}
}

// ─── buildDashboardBundle ────────────────────────────────────────

// TestBuildDashboardBundle_UnconfiguredIsNotAnError pins that no base_url means the dashboard is
// not mounted and the API still serves. The all-nil bundle is what the nilOrMounter test above
// threads into v1.Options.
func TestBuildDashboardBundle_UnconfiguredIsNotAnError(t *testing.T) {
	t.Parallel()

	bundle, err := buildDashboardBundle(config.DashboardConfig{}, nil, nil, discardLogger())
	if err != nil {
		t.Fatalf("buildDashboardBundle with an empty base_url errored: %v — an unconfigured "+
			"dashboard must degrade, not refuse to start", err)
	}
	if bundle.auth != nil || bundle.keys != nil || bundle.webhooks != nil || bundle.priceAlerts != nil {
		t.Errorf("unconfigured bundle carries handlers %+v; every mounter field must be nil so "+
			"nilOrMounter leaves the routes unmounted", bundle)
	}
	if bundle.middleware != nil {
		t.Error("unconfigured bundle carries a session-resolver middleware; it would run on every " +
			"request with nil stores behind it")
	}
	if bundle.pgValidator != nil {
		t.Error("unconfigured bundle carries a Postgres API-key validator; auth_backend=postgres " +
			"must fall back to the Noop (503) rather than borrow a half-built validator")
	}
}

// TestDashboardKeyMirror_WiredWheneverRedisExists pins that the
// dashboard key mirror depends on Redis alone, never on auth_backend, so a
// key minted under redis still dies when a postgres-backend instance
// revokes it.
func TestDashboardKeyMirror_WiredWheneverRedisExists(t *testing.T) {
	t.Parallel()

	if m := dashboardKeyMirror(nil); m != nil {
		t.Errorf("no redis: mirror = %T, want an untyped nil", m)
	}
	rdb := testRedisClient(t)
	m := dashboardKeyMirror(rdb)
	if m == nil {
		t.Fatal("redis configured but no mirror: dashboard keys 401 on the redis validator")
	}
	ctx := context.Background()
	minted := "sip_" + strings.Repeat("ab", 32)
	ident := auth.AccountIdentifier("example")
	err := m.CreateWithSecret(ctx, auth.MirroredKey{Plaintext: minted, Record: auth.APIKeyRecord{
		KeyID: "kid_mirror", Identifier: ident, PermissionsAll: true, MonthlyQuota: 1_000,
	}})
	if err != nil {
		t.Fatalf("CreateWithSecret: %v", err)
	}
	validator := auth.NewRedisAPIKeyValidator(rdb)
	if _, err := validator.Lookup(ctx, minted); err != nil {
		t.Fatalf("mirrored key does not authenticate on the redis validator: %v", err)
	}
	if err := m.RevokeKeyByID(ctx, ident, "kid_mirror"); err != nil {
		t.Fatalf("RevokeKeyByID: %v", err)
	}
	if _, err := validator.Lookup(ctx, minted); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Lookup after mirror revoke = %v, want ErrUnauthorized", err)
	}
}

// TestBuildDashboardBundle_ConfiguredWithoutPostgresFailsClosed — the
// opposite arm. An operator who set base_url expects the dashboard to
// work; wiring it against a nil *sql.DB would build handlers that
// nil-panic on the first request instead of failing at startup.
func TestBuildDashboardBundle_ConfiguredWithoutPostgresFailsClosed(t *testing.T) {
	t.Parallel()

	_, err := buildDashboardBundle(
		config.DashboardConfig{BaseURL: "https://dashboard.example.test"},
		nil, nil, discardLogger(),
	)
	if err == nil {
		t.Fatal("buildDashboardBundle accepted base_url with a nil *sql.DB — the handlers it " +
			"would return nil-panic on the first request; this must fail at startup")
	}
}

// TestBuildDashboardBundle_ExposesUserStoreForSessionRetention pins bundle.users being set. The
// retention reaper is skipped when it is nil, which would keep session PII forever silently.
func TestBuildDashboardBundle_ExposesUserStoreForSessionRetention(t *testing.T) {
	// The bundle wires passkeys, which refuse to start without the secret.
	const secretEnv = "STELLARINDEX_TEST_BUNDLE_CODE_SECRET"
	t.Setenv(secretEnv, "bundle-test-secret")

	// sql.Open is lazy: no connection is attempted at construction.
	db, err := sql.Open("pgx", "postgres://unused.invalid/none")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := config.Default().API.Dashboard
	cfg.BaseURL = "https://dashboard.example.test"
	cfg.CodeSecretEnv = secretEnv
	bundle, err := buildDashboardBundle(cfg, db, nil, discardLogger())
	if err != nil {
		t.Fatalf("buildDashboardBundle: %v", err)
	}
	if _, ok := bundle.users.(*postgresstore.UserStore); !ok {
		t.Fatalf("bundle.users = %T, want *postgresstore.UserStore — the session retention reaper would never start", bundle.users)
	}
}

// ─── buildWebhookHandlers / buildPriceAlertHandlers ──────────────
//
// Neither constructor touches the *sql.DB at construction, so a nil handle reaches them. Pinning
// that matters because a dropped store would otherwise fail on the first request, not at startup.

func TestBuildWebhookHandlers_ConstructsAndReturnsItsStore(t *testing.T) {
	t.Parallel()

	store, h, err := buildWebhookHandlers((*sql.DB)(nil), nil, discardLogger())
	if err != nil {
		t.Fatalf("buildWebhookHandlers: %v", err)
	}
	if store == nil {
		t.Error("nil WebhookStore — the bundle threads this to the delivery worker, which would " +
			"then drain nothing while customers' webhooks queue up")
	}
	if h == nil {
		t.Error("nil webhook handlers")
	}
}

func TestBuildPriceAlertHandlers_Constructs(t *testing.T) {
	t.Parallel()

	h, err := buildPriceAlertHandlers(nil, discardLogger())
	if err != nil {
		t.Fatalf("buildPriceAlertHandlers: %v", err)
	}
	if h == nil {
		t.Error("nil price-alert handlers")
	}
}

// ─── wireDashboardAuthThrottles ──────────────────────────────────

// TestWireDashboardAuthThrottles_RedisAbsentDegradesToInProcess pins the single-instance
// fallback. The login throttle stays set, since nil would make magic-link requests an open mail
// relay. The two Redis-only guards stay unset, since in-process ones cannot give a fleet-wide
// guarantee. The operator is warned, because the fallback is only safe on one instance.
func TestWireDashboardAuthThrottles_RedisAbsentDegradesToInProcess(t *testing.T) {
	t.Parallel()

	logger, records := recordingLogger()
	var cfg dashboardauth.Config
	wireDashboardAuthThrottles(&cfg, nil, logger)

	if cfg.LoginThrottle == nil {
		t.Error("no Redis and no login throttle — the magic-link endpoint has NO per-email or " +
			"per-IP cap, which makes it an open mail relay")
	}
	if cfg.EmailLocker != nil {
		t.Error("an EmailLocker was wired without Redis; a per-process lock cannot make the " +
			"cross-instance guarantee its callers assume")
	}
	if cfg.PasskeyCeremonyGuard != nil {
		t.Error("a PasskeyCeremonyGuard was wired without Redis; a per-process replay guard " +
			"lets a spent challenge be replayed against a sibling instance")
	}
	if !records.hasLevelWithText("WARN", "in-process") {
		t.Errorf("the Redis-less fallback logged %v; want a WARN naming the single-instance "+
			"limitation", records.lines())
	}
}

// TestWireDashboardAuthThrottles_RedisPresentWiresAllThree — with Redis
// every guard is fleet-safe and all three must be set. A missing
// PasskeyCeremonyGuard here is a replay hole the Redis-less test cannot
// see, because there the same field is correctly nil.
func TestWireDashboardAuthThrottles_RedisPresentWiresAllThree(t *testing.T) {
	t.Parallel()

	rdb := testRedisClient(t)
	logger, records := recordingLogger()
	var cfg dashboardauth.Config
	wireDashboardAuthThrottles(&cfg, rdb, logger)

	if cfg.EmailLocker == nil {
		t.Error("Redis present but no EmailLocker — signup email locking is off")
	}
	if cfg.LoginThrottle == nil {
		t.Error("Redis present but no LoginThrottle")
	}
	if cfg.PasskeyCeremonyGuard == nil {
		t.Error("Redis present but no PasskeyCeremonyGuard — a spent WebAuthn challenge can be " +
			"replayed to mint a second session")
	}
	// The single-instance warnings belong to the fallback path only;
	// emitting them with Redis wired would train operators to ignore them.
	if records.hasLevelWithText("WARN", "in-process") {
		t.Errorf("wired against Redis but still warned about the in-process fallback: %v", records.lines())
	}
}
