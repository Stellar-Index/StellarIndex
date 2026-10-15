package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageReaderOrNil_RedisAbsent pins that a nil usage counter yields a nil v1.UsageReader, not a
// non-nil adapter, because the handler short-circuits on nil and the adapter would nil-deref.
func TestUsageReaderOrNil_RedisAbsent(t *testing.T) {
	if r := wiring.UsageReaderOrNil(nil); r != nil {
		t.Errorf("wiring.UsageReaderOrNil(nil) = %v (non-nil), want nil — handler short-circuits on nil; non-nil wrapper would deref the inner nil counter on Read", r)
	}
}

// TestUsageReaderOrNil_RedisPresent: with a non-nil counter the helper returns a non-nil adapter bridging to the v1
// package; miniredis keeps usage.New() from short-circuiting to nil.
func TestUsageReaderOrNil_RedisPresent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := usage.New(rdb)
	if c == nil {
		t.Fatal("usage.New(real rdb) returned nil — test setup invariant broken")
	}
	r := wiring.UsageReaderOrNil(c)
	if r == nil {
		t.Fatal("wiring.UsageReaderOrNil(real-counter) = nil, want non-nil")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDivergenceServiceHasNoReferences guards that the API binary never builds divergence.References,
// since it only calls Service.LookupCached. It source-scans main.go because main() needs live Postgres.
func TestDivergenceServiceHasNoReferences(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	forbidden := []string{
		"buildDivergenceReferences",
		"divergence.NewCoinGeckoReference",
		"divergence.NewOracleReference",
	}
	for _, sym := range forbidden {
		if bytes.Contains(src, []byte(sym)) {
			t.Errorf("main.go references %q — the API binary must build only the cache-reading divergence.Service, never a References list (GH-1004b)", sym)
		}
	}
}

// ── auth_backend flag selection (X6 read-through cutover) ───────────

// stubKeysForBuild / stubAccountsForBuild satisfy the platform stores
// the Postgres validator constructor requires. It never calls them
// (construction only nil-checks), so every method panics.
type stubKeysForBuild struct{}

func (stubKeysForBuild) Create(context.Context, platform.APIKey, int) (platform.APIKey, error) {
	panic("unused")
}
func (stubKeysForBuild) Get(context.Context, string) (platform.APIKey, error) { panic("unused") }
func (stubKeysForBuild) GetByHash(context.Context, []byte) (platform.APIKey, error) {
	panic("unused")
}

func (stubKeysForBuild) CountActiveForAccount(context.Context, uuid.UUID) (int, error) {
	panic("unused")
}

func (stubKeysForBuild) ListActiveForAccount(context.Context, uuid.UUID) ([]platform.APIKey, error) {
	panic("unused")
}

func (stubKeysForBuild) ListForAccount(context.Context, uuid.UUID, int) ([]platform.APIKey, bool, error) {
	panic("unused")
}
func (stubKeysForBuild) Update(context.Context, uuid.UUID, platform.APIKey) error { panic("unused") }
func (stubKeysForBuild) Revoke(context.Context, uuid.UUID, string, uuid.UUID, string) error {
	panic("unused")
}
func (stubKeysForBuild) TouchUsage(context.Context, string, net.IP, string) error { panic("unused") }

type stubAccountsForBuild struct{}

func (stubAccountsForBuild) Create(context.Context, platform.Account) (platform.Account, error) {
	panic("unused")
}

func (stubAccountsForBuild) Get(context.Context, uuid.UUID) (platform.Account, error) {
	panic("unused")
}

func (stubAccountsForBuild) GetBySlug(context.Context, string) (platform.Account, error) {
	panic("unused")
}

func (stubAccountsForBuild) Update(context.Context, platform.Account) error   { panic("unused") }
func (stubAccountsForBuild) Suspend(context.Context, uuid.UUID, string) error { panic("unused") }
func (stubAccountsForBuild) Unsuspend(context.Context, uuid.UUID) error       { panic("unused") }

// TestBuildAPIKeyValidator_BothFlagStates pins auth_backend: "redis" or default keeps the legacy
// validator, and "postgres" selects the read-through one, failing loud to Noop (503s) when a
// dependency is missing rather than demoting to anonymous.
func TestBuildAPIKeyValidator_BothFlagStates(t *testing.T) {
	logger := discardLogger()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	pgV, err := auth.NewPostgresAPIKeyValidator(auth.PostgresValidatorOptions{
		Keys:     stubKeysForBuild{},
		Accounts: stubAccountsForBuild{},
		Cache:    rdb,
	})
	if err != nil {
		t.Fatalf("build postgres validator: %v", err)
	}

	t.Run("redis backend (current default) → RedisAPIKeyValidator", func(t *testing.T) {
		v := buildAPIKeyValidator(authValidatorOptions{Backend: "redis", Rdb: rdb, PostgresValidator: pgV}, logger, "apikey")
		if _, ok := v.(*auth.RedisAPIKeyValidator); !ok {
			t.Fatalf("got %T, want *auth.RedisAPIKeyValidator", v)
		}
	})

	t.Run("empty backend defaults to redis", func(t *testing.T) {
		v := buildAPIKeyValidator(authValidatorOptions{Backend: "", Rdb: rdb}, logger, "apikey")
		if _, ok := v.(*auth.RedisAPIKeyValidator); !ok {
			t.Fatalf("got %T, want *auth.RedisAPIKeyValidator (empty == redis)", v)
		}
	})

	t.Run("redis backend without Redis → Noop (fail-loud)", func(t *testing.T) {
		v := buildAPIKeyValidator(authValidatorOptions{Backend: "redis", Rdb: nil}, logger, "apikey")
		if _, ok := v.(auth.NoopAPIKeyValidator); !ok {
			t.Fatalf("got %T, want auth.NoopAPIKeyValidator", v)
		}
	})

	t.Run("postgres backend wired → the read-through validator", func(t *testing.T) {
		v := buildAPIKeyValidator(authValidatorOptions{Backend: "postgres", Rdb: rdb, PostgresValidator: pgV}, logger, "apikey")
		got, ok := v.(*auth.PostgresAPIKeyValidator)
		if !ok {
			t.Fatalf("got %T, want *auth.PostgresAPIKeyValidator", v)
		}
		if got != pgV {
			t.Error("postgres backend must return the pre-built validator, not a fresh one")
		}
	})

	t.Run("postgres backend unwired → Noop (fail-loud)", func(t *testing.T) {
		v := buildAPIKeyValidator(authValidatorOptions{Backend: "postgres", Rdb: rdb, PostgresValidator: nil}, logger, "apikey")
		if _, ok := v.(auth.NoopAPIKeyValidator); !ok {
			t.Fatalf("got %T, want auth.NoopAPIKeyValidator (postgres backend without the dashboard bundle must fail loud)", v)
		}
	})
}

// A configured SEP-10 deployment always gets the replay-guarded validator, never a guard-free one: Redis present gives
// *sep10.Validator in every auth_mode. Configured but no Redis or no account lake: hard error under auth_mode=sep10,
// else Noop (404). Unconfigured: Noop, binary still boots.
func TestResolveSEP10Validator_WiringByConfiguration(t *testing.T) {
	server, err := keypair.Random()
	if err != nil {
		t.Fatalf("keypair.Random: %v", err)
	}
	const (
		seedEnv = "TEST_SEP10_SEED"
		jwtEnv  = "TEST_SEP10_JWT"
	)
	configured := config.SEP10Config{
		SeedEnv:       seedEnv,
		JWTSecretEnv:  jwtEnv,
		WebAuthDomain: "auth.stellarindex.test",
		HomeDomain:    "stellarindex.test",
		ChallengeTTL:  15 * time.Minute,
		JWTTTL:        time.Hour,
	}
	setConfiguredEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv(seedEnv, server.Seed())
		t.Setenv(jwtEnv, "test-jwt-secret-must-be-32-bytes-or-more!!")
	}

	t.Run("configured + Redis → guarded validator (every auth_mode)", func(t *testing.T) {
		setConfiguredEnv(t)
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })

		for _, mode := range []string{"apikey_optional", "sep10"} {
			v, err := resolveSEP10Validator(configured, mode, config.Default().Stellar.Passphrase(), rdb, &lakeAccountSigners{}, discardLogger())
			if err != nil {
				t.Fatalf("mode=%s: unexpected error: %v", mode, err)
			}
			if _, ok := v.(*sep10.Validator); !ok {
				t.Fatalf("mode=%s: got %T, want *sep10.Validator (guarded, replay-protected)", mode, v)
			}
			if _, isNoop := v.(auth.NoopSEP10Validator); isNoop {
				t.Fatalf("mode=%s: a configured deployment WITH Redis must not get the Noop (404) validator", mode)
			}
		}
	})

	t.Run("configured + NO Redis + auth_mode=sep10 → fail closed", func(t *testing.T) {
		setConfiguredEnv(t)
		v, err := resolveSEP10Validator(configured, "sep10", config.Default().Stellar.Passphrase(), nil, &lakeAccountSigners{}, discardLogger())
		if err == nil {
			t.Fatalf("expected fail-closed error, got validator %T", v)
		}
		if !errors.Is(err, sep10.ErrReplayGuardUnavailable) {
			t.Errorf("err = %v, want wrap of sep10.ErrReplayGuardUnavailable", err)
		}
		if v != nil {
			t.Errorf("must return a nil validator on the error path (never a guard-free one), got %T", v)
		}
	})

	t.Run("configured + NO Redis + auth_mode=apikey_optional → Noop, boots", func(t *testing.T) {
		setConfiguredEnv(t)
		v, err := resolveSEP10Validator(configured, "apikey_optional", config.Default().Stellar.Passphrase(), nil, &lakeAccountSigners{}, discardLogger())
		if err != nil {
			t.Fatalf("must degrade to Noop (not abort boot) outside auth_mode=sep10: %v", err)
		}
		if _, ok := v.(auth.NoopSEP10Validator); !ok {
			t.Errorf("got %T, want auth.NoopSEP10Validator (404 on SEP-10 endpoints, never guard-free)", v)
		}
	})

	t.Run("configured + Redis + NO account lake → fail closed / Noop", func(t *testing.T) {
		setConfiguredEnv(t)
		mr := miniredis.RunT(t)
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		t.Cleanup(func() { _ = rdb.Close() })

		v, err := resolveSEP10Validator(configured, "sep10", config.Default().Stellar.Passphrase(), rdb, nil, discardLogger())
		if !errors.Is(err, sep10.ErrAccountLoaderUnavailable) || v != nil {
			t.Fatalf("auth_mode=sep10 without a signer lookup: got (%T, %v), want (nil, ErrAccountLoaderUnavailable)", v, err)
		}
		v, err = resolveSEP10Validator(configured, "apikey_optional", config.Default().Stellar.Passphrase(), rdb, nil, discardLogger())
		if err != nil {
			t.Fatalf("must degrade to Noop outside auth_mode=sep10: %v", err)
		}
		if _, ok := v.(auth.NoopSEP10Validator); !ok {
			t.Errorf("got %T, want auth.NoopSEP10Validator (never a threshold-blind validator)", v)
		}
	})

	t.Run("unconfigured → Noop, boots", func(t *testing.T) {
		v, err := resolveSEP10Validator(config.SEP10Config{
			WebAuthDomain: "auth.stellarindex.test",
			HomeDomain:    "stellarindex.test",
		}, "apikey_optional", config.Default().Stellar.Passphrase(), nil, nil, discardLogger())
		if err != nil {
			t.Fatalf("an unconfigured SEP-10 deployment must boot with the Noop, got error: %v", err)
		}
		if _, ok := v.(auth.NoopSEP10Validator); !ok {
			t.Errorf("got %T, want auth.NoopSEP10Validator", v)
		}
	})
}

// Before the explorer reader is bound (or when its dial failed) the
// SEP-10 signer lookup must error, so verification fails closed rather
// than treating every account as not-yet-on-chain.
func TestLakeAccountSignersUnboundFailsClosed(t *testing.T) {
	_, err := (&lakeAccountSigners{}).LoadAccountSigners(context.Background(), keypair.MustRandom().Address())
	if err == nil {
		t.Fatal("unbound lake lookup returned no error")
	}
}

// A public all-interfaces bind must warn whether written as 0.0.0.0, [::] or :3000.
// strings.Cut(":") splits "[::]:3000" into host "[" and would silently ship a public IPv6 bind.
func TestWarnUnsafeBind(t *testing.T) {
	// warnLogged runs warnUnsafeBind against a captured logger and
	// reports whether the SECURITY warning fired.
	warnLogged := func(listenAddr string, cidrs []string) bool {
		var buf bytes.Buffer
		// Level Warn so the SECURITY warning is emitted; a captured
		// handler lets us assert on presence/absence of output.
		lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		warnUnsafeBind(lg, listenAddr, cidrs)
		return strings.Contains(buf.String(), "SECURITY")
	}

	// Public all-interfaces binds WITHOUT trusted proxies must warn.
	for _, addr := range []string{"0.0.0.0:3000", "[::]:3000", ":3000", "::"} {
		if addr == "::" {
			// "::" alone has no port; SplitHostPort rejects it, so it must
			// NOT warn (can't classify) — assert that separately below.
			continue
		}
		if !warnLogged(addr, nil) {
			t.Errorf("warnUnsafeBind(%q, no CIDRs): expected SECURITY warning, got none", addr)
		}
	}

	// Loopback binds must stay quiet — IPv4, IPv6, and hostname forms.
	for _, addr := range []string{"127.0.0.1:3000", "[::1]:3000", "localhost:3000"} {
		if warnLogged(addr, nil) {
			t.Errorf("warnUnsafeBind(%q): loopback must NOT warn", addr)
		}
	}

	// A public bind WITH trusted proxies configured takes the softer
	// (non-SECURITY) branch — no hard SECURITY warning.
	if warnLogged("[::]:3000", []string{"10.0.0.0/8"}) {
		t.Error("warnUnsafeBind([::]:3000, with CIDRs): must not emit the hard SECURITY warning when proxies are configured")
	}

	// Unparseable listen addr can't be classified → stay silent.
	if warnLogged("::", nil) {
		t.Error(`warnUnsafeBind("::"): bare host with no port is unparseable; must stay silent`)
	}
}

// TestWarnCollapsedAnonThrottle pins the boot warning for an empty trusted_proxy_cidrs, which
// collapses every anonymous caller into one shared bucket. It fires only when the anon tier is
// capped, reachable (auth_mode none or apikey_optional) and no proxy CIDR is set.
func TestWarnCollapsedAnonThrottle(t *testing.T) {
	warnLogged := func(authMode string, anonPerMin int, cidrs []string) bool {
		var buf bytes.Buffer
		lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		warnCollapsedAnonThrottle(lg, authMode, anonPerMin, cidrs)
		return strings.Contains(buf.String(), "SECURITY")
	}

	// Empty CIDR + active anon bucket + an anon-admitting auth mode
	// must warn — the collapse case, regardless of bind (bind is not
	// consulted, so this fires for the loopback R1 shape too).
	for _, mode := range []string{"none", "apikey_optional"} {
		if !warnLogged(mode, 60, nil) {
			t.Errorf("auth_mode=%q, anon active, empty CIDR: expected SECURITY warning, got none", mode)
		}
	}

	// Credential-required auth modes reject anonymous requests before
	// the limiter — the anon bucket is never exercised, so no collapse
	// and no warning.
	for _, mode := range []string{"apikey", "sep10"} {
		if warnLogged(mode, 60, nil) {
			t.Errorf("auth_mode=%q: anonymous requests are 401'd before the limiter; must NOT warn", mode)
		}
	}

	// Anon tier disabled (0) → no anon bucket → nothing to collapse.
	if warnLogged("none", 0, nil) {
		t.Error("anon_rate_limit_per_min=0: no anon bucket exists; must NOT warn")
	}

	// A configured proxy CIDR resolves each client to its real address, so the throttle discriminates and nothing collapses.
	// This is the R1 shape (apikey_optional, anon=6000, trusted_proxy_cidrs=["127.0.0.1/32"]) and MUST stay silent.
	if warnLogged("apikey_optional", 6000, []string{"127.0.0.1/32"}) {
		t.Error("R1 shape (non-empty trusted_proxy_cidrs): must NOT warn")
	}
	if warnLogged("none", 60, []string{"10.0.0.0/8"}) {
		t.Error("non-empty trusted_proxy_cidrs: throttle discriminates; must NOT warn")
	}
}

// fakeSubscriberRunner implements subscriberRunner: the first failN calls return failErr at once,
// and later calls block until ctx is cancelled like a healthy subscriber.
type fakeSubscriberRunner struct {
	mu      sync.Mutex
	calls   int
	failN   int
	failErr error
}

func (f *fakeSubscriberRunner) Run(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call <= f.failN {
		return f.failErr
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSubscriberRunner) Channel() string { return "fake-channel" }

func (f *fakeSubscriberRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRunSubscriberSupervisedWithBackoff_RestartsPastFailures asserts the supervisor restarts past
// several consecutive failures, since a single-shot caller would leave /v1/price/stream silent for
// the rest of the process.
func TestRunSubscriberSupervisedWithBackoff_RestartsPastFailures(t *testing.T) {
	fake := &fakeSubscriberRunner{
		failN:   3,
		failErr: errors.New("redispub: subscribe channel closed unexpectedly"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	done := make(chan struct{})
	go func() {
		runSubscriberSupervisedWithBackoff(ctx, fake, logger, time.Millisecond, 2*time.Millisecond)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for {
		if fake.callCount() > fake.failN {
			break // restarted past every induced failure into the long-lived run
		}
		select {
		case <-deadline:
			t.Fatalf("subscriber did not restart past %d induced failures within the deadline (calls=%d)",
				fake.failN, fake.callCount())
		case <-time.After(time.Millisecond):
		}
	}

	if !strings.Contains(buf.String(), "restarting") {
		t.Error(`expected a "restarting" log line for each induced failure`)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSubscriberSupervisedWithBackoff did not return after ctx cancellation")
	}
}

// TestInProcessLoginThrottle_EnforcesPerEmailCap: one victim inbox must be capped even when every send comes from a
// different IP (the inbox-bomb case a per-IP cap misses); the (max+1)th send to the same email is denied.
func TestInProcessLoginThrottle_EnforcesPerEmailCap(t *testing.T) {
	th := newInProcessLoginThrottle()
	ctx := context.Background()
	const email = "victim@example.com"
	for i := 0; i < inProcessLoginThrottleMaxPerEmail; i++ {
		allowed, err := th.Allow(ctx, fmt.Sprintf("203.0.113.%d", i), email)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("call %d: expected allowed within the per-email cap (max=%d)", i, inProcessLoginThrottleMaxPerEmail)
		}
	}
	allowed, err := th.Allow(ctx, "203.0.113.250", email)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected send #%d to %q (beyond the per-email cap of %d) to be DENIED, got allowed",
			inProcessLoginThrottleMaxPerEmail+1, email, inProcessLoginThrottleMaxPerEmail)
	}
	// A different target email, same window, is unaffected.
	allowed, err = th.Allow(ctx, "203.0.113.250", "someone-else@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("a different target email must not be throttled by another email's exhausted cap")
	}
}

// TestInProcessLoginThrottle_EnforcesPerIPCap — the spray-many-
// addresses dimension: one IP sending to many DIFFERENT emails must
// still be capped.
func TestInProcessLoginThrottle_EnforcesPerIPCap(t *testing.T) {
	th := newInProcessLoginThrottle()
	ctx := context.Background()
	const ip = "198.51.100.9"
	for i := 0; i < inProcessLoginThrottleMaxPerIP; i++ {
		allowed, err := th.Allow(ctx, ip, fmt.Sprintf("user%d@example.com", i))
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("call %d: expected allowed within the per-IP cap (max=%d)", i, inProcessLoginThrottleMaxPerIP)
		}
	}
	allowed, err := th.Allow(ctx, ip, "spray-target@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected send #%d from %q (beyond the per-IP cap of %d) to be DENIED, got allowed",
			inProcessLoginThrottleMaxPerIP+1, ip, inProcessLoginThrottleMaxPerIP)
	}
}

// TestInProcessLoginThrottle_ExhaustedIPDoesNotSpendEmailBudget pins that a call denied by an
// exhausted per-IP bucket never spends the per-email budget, or an attacker could lock a victim out.
func TestInProcessLoginThrottle_ExhaustedIPDoesNotSpendEmailBudget(t *testing.T) {
	th := newInProcessLoginThrottle()
	ctx := context.Background()
	const attackerIP = "203.0.113.77"
	const victim = "victim@example.com"

	// Exhaust the attacker IP's own per-IP cap first, using distinct
	// filler emails so none of these touches the victim's budget.
	for i := 0; i < inProcessLoginThrottleMaxPerIP; i++ {
		allowed, err := th.Allow(ctx, attackerIP, fmt.Sprintf("filler%d@example.com", i))
		if err != nil {
			t.Fatalf("filler call %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("filler call %d: expected allowed while filling the per-IP cap", i)
		}
	}

	// The attacker IP's per-IP bucket is now exhausted. Every further
	// call from it targeting the victim must be denied on IP alone, and
	// must not spend the victim's separate per-email budget.
	for i := 0; i < inProcessLoginThrottleMaxPerEmail; i++ {
		allowed, err := th.Allow(ctx, attackerIP, victim)
		if err != nil {
			t.Fatalf("attack call %d: unexpected error: %v", i, err)
		}
		if allowed {
			t.Fatalf("attack call %d: expected denied — attacker IP's per-IP cap is exhausted", i)
		}
	}

	// The victim's email budget must still be fully intact: a
	// legitimate send to the same address from a FRESH IP must be
	// allowed.
	allowed, err := th.Allow(ctx, "198.51.100.200", victim)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("an attacker repeatedly hitting their own exhausted per-IP cap must not spend the victim email's per-email budget")
	}
}

// TestInProcessLoginThrottle_MasksIPv6ToSlash64 asserts two /128s in one /64 share a budget, or a
// caller could mint a fresh bucket per request and bypass the cap.
func TestInProcessLoginThrottle_MasksIPv6ToSlash64(t *testing.T) {
	th := newInProcessLoginThrottle()
	ctx := context.Background()
	for i := 0; i < inProcessLoginThrottleMaxPerIP; i++ {
		// A distinct /128 each call, all inside 2001:db8:1234::/64.
		ip := fmt.Sprintf("2001:db8:1234::%x", i+1)
		allowed, err := th.Allow(ctx, ip, fmt.Sprintf("user%d@example.com", i))
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("call %d (ip=%s): expected allowed within the per-IP cap (max=%d)", i, ip, inProcessLoginThrottleMaxPerIP)
		}
	}
	// One more /128 from the SAME /64 must be denied — proves the /64 is
	// one shared budget, not a fresh one per address.
	allowed, err := th.Allow(ctx, "2001:db8:1234::ffff", "spray-target@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected a %d-th distinct /128 inside the same /64 to be DENIED (cap=%d), got allowed",
			inProcessLoginThrottleMaxPerIP+1, inProcessLoginThrottleMaxPerIP)
	}
	// A /128 in a DIFFERENT /64 must be unaffected.
	allowed, err = th.Allow(ctx, "2001:db8:5678::1", "other@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !allowed {
		t.Error("a distinct /64 must not be throttled by another /64's exhausted cap")
	}
}

// TestInProcessLoginThrottle_CanonicalizesEmailBeforeHashing. The
// fallback must share auth.RedisLoginThrottle's email identity: a +tag
// re-spelling of the same inbox must not get its own throttle budget.
func TestInProcessLoginThrottle_CanonicalizesEmailBeforeHashing(t *testing.T) {
	th := newInProcessLoginThrottle()
	ctx := context.Background()
	for i := 0; i < inProcessLoginThrottleMaxPerEmail; i++ {
		// Every send re-spells the SAME inbox via a different +tag.
		email := fmt.Sprintf("victim+%d@gmail.com", i)
		allowed, err := th.Allow(ctx, fmt.Sprintf("203.0.113.%d", i), email)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("call %d: expected allowed within the per-email cap (max=%d)", i, inProcessLoginThrottleMaxPerEmail)
		}
	}
	allowed, err := th.Allow(ctx, "203.0.113.250", "victim@gmail.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowed {
		t.Errorf("expected a %d-th +tag re-spelling of the same inbox to be DENIED (cap=%d), got allowed",
			inProcessLoginThrottleMaxPerEmail+1, inProcessLoginThrottleMaxPerEmail)
	}
}

// TestInProcessSignupIPThrottle_MasksIPv6ToSlash64: same class of
// bug as on the signup-IP fallback: CheckIP keyed on the raw address.
func TestInProcessSignupIPThrottle_MasksIPv6ToSlash64(t *testing.T) {
	th := newInProcessSignupIPThrottle()
	ctx := context.Background()
	for i := 0; i < inProcessSignupIPThrottleMaxPerHour; i++ {
		ip := fmt.Sprintf("2001:db8:9999::%x", i+1)
		if err := th.CheckIP(ctx, ip); err != nil {
			t.Fatalf("call %d (ip=%s): expected nil error within the cap (max=%d), got %v",
				i, ip, inProcessSignupIPThrottleMaxPerHour, err)
		}
	}
	err := th.CheckIP(ctx, "2001:db8:9999::ffff")
	if !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Errorf("expected a distinct /128 inside the same exhausted /64 to hit auth.ErrSignupRateLimited, got %v", err)
	}
}

// TestInProcessSignupIPThrottle_EnforcesPerIPCap: the (max+1)th signup from one IP in the window is denied with
// auth.ErrSignupRateLimited (signupIPThrottleOK turns it into a 429), while a distinct IP is unaffected.
func TestInProcessSignupIPThrottle_EnforcesPerIPCap(t *testing.T) {
	th := newInProcessSignupIPThrottle()
	ctx := context.Background()
	const ip = "198.51.100.5"
	for i := 0; i < inProcessSignupIPThrottleMaxPerHour; i++ {
		if err := th.CheckIP(ctx, ip); err != nil {
			t.Fatalf("call %d: expected nil error within the cap (max=%d), got %v",
				i, inProcessSignupIPThrottleMaxPerHour, err)
		}
	}
	err := th.CheckIP(ctx, ip)
	if !errors.Is(err, auth.ErrSignupRateLimited) {
		t.Errorf("expected auth.ErrSignupRateLimited beyond the per-IP cap of %d, got %v",
			inProcessSignupIPThrottleMaxPerHour, err)
	}
	if err := th.CheckIP(ctx, "198.51.100.6"); err != nil {
		t.Errorf("a different IP must not be throttled by another IP's exhausted cap: %v", err)
	}
}

// TestRecoverBackgroundWorker_CountsThePanic pins that the panic moves the same counter the
// indexer's workers move, because one page rule reads it for every binary.
func TestRecoverBackgroundWorker_CountsThePanic(t *testing.T) {
	const name = "test-api-worker"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(name))

	func() {
		defer recoverBackgroundWorker(slog.New(slog.NewTextHandler(io.Discard, nil)), name)
		panic("simulated worker fault")
	}()

	if got := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(name)) - before; got != 1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} rose by %v, want 1", name, got)
	}
}

// TestAssertRedisOrSingleInstance pins the fail-closed startup gate: a Redis-less deployment must not boot
// unless the operator has explicitly asserted single-instance, because the
// per-process auth fallbacks are unsafe across multiple instances.
func TestAssertRedisOrSingleInstance(t *testing.T) {
	cases := []struct {
		name           string
		redisEnabled   bool
		singleInstance bool
		wantErr        bool
	}{
		{"redis present, multi-instance assumed — OK (fleet-safe backends)", true, false, false},
		{"redis present, single-instance asserted — OK", true, true, false},
		{"redis absent, single-instance asserted — OK (per-process is correct)", false, true, false},
		{"redis absent, NOT asserted — REFUSE (the unsafe case)", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertRedisOrSingleInstance(tc.redisEnabled, tc.singleInstance)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected refuse-to-start error, got nil")
				}
				// The message must name the escape hatches so an operator can act.
				if !strings.Contains(err.Error(), "single_instance") || !strings.Contains(err.Error(), "Redis") {
					t.Errorf("error must name Redis + single_instance remedies, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected OK, got error: %v", err)
			}
		})
	}
}

// warnOpenCORS must treat AllowedOrigins as wildcard-open whenever "*" appears anywhere, matching middleware.CORS's set-
// membership check. A single-entry check would skip the SECURITY warning for ["*", "https://evil.com"], though CORS()
// echoes "*" to everyone.
func TestWarnOpenCORS_WildcardWithExtraEntries(t *testing.T) {
	warnLogged := func(allowedOrigins []string, authMode string) bool {
		var buf bytes.Buffer
		lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		warnOpenCORS(lg, allowedOrigins, authMode)
		return strings.Contains(buf.String(), "SECURITY")
	}

	// Exact single-wildcard case: must warn.
	if !warnLogged([]string{"*"}, "apikey") {
		t.Error(`warnOpenCORS([]string{"*"}, "apikey"): expected SECURITY warning`)
	}

	// Wildcard alongside another explicit entry: CORS() still allows
	// every origin (wildcard := allowed["*"]) so this must ALSO warn.
	if !warnLogged([]string{"*", "https://evil.com"}, "apikey") {
		t.Error(`warnOpenCORS([]string{"*", "https://evil.com"}, "apikey"): expected SECURITY warning, wildcard is still in effect`)
	}

	// No wildcard present at all: must stay quiet.
	if warnLogged([]string{"https://stellarindex.io"}, "apikey") {
		t.Error(`warnOpenCORS without "*": must not warn`)
	}
}

// The one secret keys both the login-code HMAC and the passkey ceremony
// cookie, so the unset-env warning must name both casualties: an operator
// reading only "codes" would not connect it to looping passkey sign-ins.
func TestBuildDashboardGenerator_UnsetSecretWarningNamesPasskeyCeremonies(t *testing.T) {
	const env = "STELLARINDEX_TEST_DASHBOARD_CODE_SECRET_UNSET"
	t.Setenv(env, "")
	var buf bytes.Buffer
	g := buildDashboardGenerator(config.DashboardConfig{CodeSecretEnv: env}, slog.New(slog.NewTextHandler(&buf, nil)))
	if len(g.Secret) != 0 {
		t.Fatal("no env set, yet the generator carries a secret")
	}
	out := buf.String()
	for _, want := range []string{"sign-in codes", "passkey ceremonies", "more than one API instance"} {
		if !strings.Contains(out, want) {
			t.Errorf("unset-secret warning does not mention %q; got %q", want, out)
		}
	}
}

// /v1/status deployment tier and background-service list must come from config, not be hardcoded: a lean test net would
// otherwise show a PRODUCTION tag and report an aggregator it doesn't run, holding overall at degraded forever.
func TestStatusIdentity_PlumbedFromConfig(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*config.Config)
		wantRegion     string
		wantDeployment string
		wantServices   []string
	}{
		{
			// Pubnet is unchanged — the defaults ARE r1's shape.
			name:           "pubnet defaults",
			mutate:         func(*config.Config) {},
			wantRegion:     "r1",
			wantDeployment: "production",
			wantServices:   []string{"indexer", "aggregator"},
		},
		{
			// The lean test net: no aggregator, and not production.
			name: "lean testnet",
			mutate: func(c *config.Config) {
				c.Region.ID = "testnet"
				c.Region.Deployment = "testnet"
				c.API.StatusServices = []string{"indexer"}
			},
			wantRegion:     "testnet",
			wantDeployment: "testnet",
			wantServices:   []string{"indexer"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(&cfg)

			region, deployment, services := statusIdentity(cfg)

			if region != tc.wantRegion {
				t.Errorf("region = %q, want %q", region, tc.wantRegion)
			}
			if deployment != tc.wantDeployment {
				t.Errorf("deployment = %q, want %q — the tier is an operator "+
					"fact, not a literal", deployment, tc.wantDeployment)
			}
			if !reflect.DeepEqual(services, tc.wantServices) {
				t.Errorf("services = %v, want %v", services, tc.wantServices)
			}
		})
	}
}

// TestClickhouseReadyChecks_ConfiguredButUnreachableStillPublishes: the "ClickHouse was
// already down when the API started" state must still publish the
// `stellarindex_dependency_up{dependency="clickhouse"}` series; registering the checker only
// in the success branch of the boot dial would leave it absent. The alert is
// `stellarindex_dependency_up == 0`, deliberately not absent(), so an absent series means
// every lake-backed endpoint 503s and nothing pages.
func TestClickhouseReadyChecks_ConfiguredButUnreachableStillPublishes(t *testing.T) {
	dialErr := errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
	checks := wiring.ClickhouseReadyChecks("localhost:9000", nil, dialErr, clickhouseBootDialBudget)
	if len(checks) != 1 {
		t.Fatalf("a configured but unreachable ClickHouse registered %d readiness check(s), want 1 — with none, the gauge is never published and the == 0 alert has nothing to match", len(checks))
	}
	c := checks[0]
	if got := c.Name(); got != "clickhouse" {
		t.Errorf("checker name = %q, want clickhouse (the dependency label the alert selects on)", got)
	}
	if c.Critical() {
		t.Error("the ClickHouse checker is critical — a lake outage would drain the whole backend, where the rest of the API keeps serving from Postgres")
	}
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping on a ClickHouse that never dialled returned success — the gauge would publish 1 for a dependency that is gone")
	}
	if !errors.Is(err, dialErr) {
		t.Errorf("Ping error does not wrap the boot dial failure: %v", err)
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("Ping error does not say a restart is required, which is what recovers the ten un-re-dialled lake seams: %v", err)
	}
}

// A deployment with no lake configured has no such dependency, and must
// publish nothing rather than a 0 — a 0 there pages for a component the
// host does not run.
func TestClickhouseReadyChecks_UnconfiguredPublishesNothing(t *testing.T) {
	if got := wiring.ClickhouseReadyChecks("", nil, nil, clickhouseBootDialBudget); len(got) != 0 {
		t.Fatalf("an unconfigured ClickHouse registered %d readiness check(s), want 0", len(got))
	}
}
