// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"database/sql"
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
	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// Coverage for the run() wiring helpers (#340 item 3). Every one of
// these ran only inside run(), which no test calls, so a regression in
// any of them shipped uncaught.
//
// The helpers are pure wiring: their job is to translate config into
// the objects the server stack holds. That makes the interesting
// assertions structural — WHICH object came back, and whether the
// caller's nil-check on it does what the author believed — rather than
// behavioural. Pattern follows TestBuildDivergenceReferences_* in
// main_test.go.

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
// The typed-nil trap, and the reason this helper exists at all.
//
// v1.Server gates every dashboard route family on `if s.dashboardX !=
// nil` (server.go:2030-2048). A naked `opts.DashboardKeys =
// bundle.keys` assigns a *dashboardkeys.Handlers into a
// DashboardAuthMounter INTERFACE — and an interface holding a typed-nil
// pointer is NOT nil. The guard passes, Mount runs, and the routes go
// live with a nil receiver behind them: every request to
// /v1/dashboard/keys then dereferences h.cfg and panics.
//
// That is the worst shape this failure takes. It is not a 500 on a
// misconfigured deployment; it is a route the operator believes is
// disabled, answering.

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

// TestNilOrMounter_UnwiredBundleLeavesDashboardRoutesUnmounted is the
// end-to-end proof, and the one that actually catches the regression.
// It reproduces run()'s exact wiring — an EMPTY dashboardBundle (what
// buildDashboardBundle returns when base_url is unset) threaded through
// nilOrMounter into v1.Options — and asserts the routes are absent
// rather than present-and-panicking.
//
// Without nilOrMounter these requests do not 404: they reach a nil
// receiver and panic the handler goroutine.
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

// TestBuildAuthMiddleware_ModeDispatch pins the auth_mode → middleware
// mapping. The two directions matter for different reasons:
//
//   - a mode that should return nil returning a middleware would make
//     every anonymous request pay (and possibly fail) an auth check;
//   - a mode that should return a middleware returning nil is an AUTH
//     BYPASS: the server stack omits absent middleware entirely, so the
//     whole API then serves unauthenticated.
//
// The unknown-mode arm is deliberately the second shape — it logs and
// falls through to no-auth, i.e. fail-OPEN. It is pinned here so it
// stays a visible decision rather than drifting silently, and the
// companion test below pins the log that is an operator's only warning.
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

// TestBuildDashboardBundle_UnconfiguredIsNotAnError pins the documented
// degrade: no base_url means the dashboard simply is not mounted, and
// the API still serves. Returning an error here would refuse to start
// every deployment that does not run a dashboard.
//
// The all-nil bundle is load-bearing: it is exactly what
// TestNilOrMounter_UnwiredBundleLeavesDashboardRoutesUnmounted then
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

// ─── buildWebhookHandlers / buildPriceAlertHandlers ──────────────
//
// Both CRUD constructors are reached only from buildDashboardBundle,
// which needs a live Postgres. Neither touches the *sql.DB at
// construction, so the construction arm is reachable with a nil handle
// — and that is the arm worth pinning, because both wrap a NewHandlers
// validate() and a dropped store would surface as a nil dereference on
// the first customer request rather than at startup.

func TestBuildWebhookHandlers_ConstructsAndReturnsItsStore(t *testing.T) {
	t.Parallel()

	store, h, err := buildWebhookHandlers((*sql.DB)(nil), discardLogger())
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

// TestWireDashboardAuthThrottles_RedisAbsentDegradesToInProcess pins
// the single-instance fallback. Three things must hold:
//
//   - the login throttle is still SET. Leaving it nil would remove the
//     per-email/per-IP cap on magic-link requests entirely, turning the
//     endpoint into an open mail relay against arbitrary addresses;
//   - the two Redis-ONLY guards stay unset, because an in-process email
//     lock or ceremony guard would claim a fleet-wide guarantee it
//     cannot make (assertRedisOrSingleInstance is the hard gate);
//   - the operator is WARNED, because the fallback is only safe on one
//     instance.
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
