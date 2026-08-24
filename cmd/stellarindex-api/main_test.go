package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stellar/go-stellar-sdk/keypair"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/auth/sep10"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestUsageReaderOrNil_RedisAbsent — when the usage counter is
// nil (Redis-less deployment), the helper must return a
// typed-nil v1.UsageReader rather than wrapping the nil counter
// in a non-nil adapter. The /v1/account/usage handler treats
// `usageReader == nil` as "no backend wired" and short-circuits
// to `[]`; the buggy pre-fix shape was a non-nil adapter that
// nil-deref'd on `Read`. F-1258 (codex audit-2026-05-12).
func TestUsageReaderOrNil_RedisAbsent(t *testing.T) {
	if r := usageReaderOrNil(nil); r != nil {
		t.Errorf("usageReaderOrNil(nil) = %v (non-nil), want nil — handler short-circuits on nil; non-nil wrapper would deref the inner nil counter on Read", r)
	}
}

// TestUsageReaderOrNil_RedisPresent — when the counter is
// non-nil, the helper returns a non-nil adapter that bridges
// to the v1 package. Uses miniredis so the underlying
// usage.New() doesn't itself short-circuit to nil.
func TestUsageReaderOrNil_RedisPresent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	c := usage.New(rdb)
	if c == nil {
		t.Fatal("usage.New(real rdb) returned nil — test setup invariant broken")
	}
	r := usageReaderOrNil(c)
	if r == nil {
		t.Fatal("usageReaderOrNil(real-counter) = nil, want non-nil")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildDivergenceReferences_DefaultsCoinGeckoOnly(t *testing.T) {
	cfg := config.DivergenceConfig{
		CoinGecko: config.DivergenceCoinGeckoConfig{Enabled: true},
		Chainlink: config.DivergenceChainlinkConfig{Enabled: false},
	}
	refs := buildDivergenceReferences(cfg, nil, discardLogger())
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1 (CoinGecko only)", len(refs))
	}
	if got := refs[0].Name(); got != "coingecko" {
		t.Errorf("refs[0].Name() = %q, want %q", got, "coingecko")
	}
}

func TestBuildDivergenceReferences_BothWiredWhenChainlinkConfigured(t *testing.T) {
	cfg := config.DivergenceConfig{
		CoinGecko: config.DivergenceCoinGeckoConfig{Enabled: true},
		Chainlink: config.DivergenceChainlinkConfig{
			Enabled: true,
			FeedMap: map[string]config.ChainlinkFeedConfig{
				"fiat:EUR/fiat:USD": {
					Address:  "0xb49f677943BC038e9857d61E7d053CaA2C1734C1",
					Decimals: 8,
				},
			},
		},
	}
	refs := buildDivergenceReferences(cfg, nil, discardLogger())
	// Three since PR #149: chainlink alone provides BOTH synthetic leg
	// classes (crypto/USD base feeds + fiat/USD fx feeds route by pair
	// within one FeedMap), so the USD-cross constructs even in this
	// minimal config — a chainlink-composed cross is still a second,
	// CoinGecko-independent reference.
	if len(refs) != 3 {
		t.Fatalf("len(refs) = %d, want 3", len(refs))
	}
	wantSet := map[string]bool{
		"coingecko": true, "chainlink": true,
		divergence.SyntheticCrossName: true,
	}
	for _, r := range refs {
		if !wantSet[r.Name()] {
			t.Errorf("unexpected reference: %q", r.Name())
		}
	}
}

func TestBuildDivergenceReferences_ChainlinkEnabledButEmptyFeedMap_Skips(t *testing.T) {
	cfg := config.DivergenceConfig{
		CoinGecko: config.DivergenceCoinGeckoConfig{Enabled: false},
		Chainlink: config.DivergenceChainlinkConfig{
			Enabled: true,
			FeedMap: map[string]config.ChainlinkFeedConfig{},
		},
	}
	refs := buildDivergenceReferences(cfg, nil, discardLogger())
	if len(refs) != 0 {
		t.Fatalf("len(refs) = %d, want 0 (empty FeedMap should not wire Chainlink)", len(refs))
	}
}

func TestBuildDivergenceReferences_AllDisabled(t *testing.T) {
	cfg := config.DivergenceConfig{
		CoinGecko: config.DivergenceCoinGeckoConfig{Enabled: false},
		Chainlink: config.DivergenceChainlinkConfig{Enabled: false},
	}
	refs := buildDivergenceReferences(cfg, nil, discardLogger())
	if len(refs) != 0 {
		t.Fatalf("len(refs) = %d, want 0", len(refs))
	}
}

// nopOracleReader satisfies divergence.OracleReader for wiring
// tests — never returns a row (the builder only needs non-nil).
type nopOracleReader struct{}

func (nopOracleReader) LatestOracleObservation(_ context.Context, _ string, _, _ []string) (*canonical.OracleUpdate, error) {
	return nil, nil
}

// TestBuildDivergenceReferences_OnChainOraclesWired — the default-ON
// [divergence.{reflector,redstone,band}] gates wire five on-chain
// references (Reflector expands to its three per-contract variants)
// when an oracle_updates reader is available, plus the synthetic
// USD-cross derived from them (PR #149: reflector-cex/redstone/band
// base legs × reflector-fx fx leg — both leg classes present here, so
// the cross constructs).
func TestBuildDivergenceReferences_OnChainOraclesWired(t *testing.T) {
	cfg := config.DivergenceConfig{
		Reflector: config.DivergenceOracleConfig{Enabled: true},
		Redstone:  config.DivergenceOracleConfig{Enabled: true},
		Band:      config.DivergenceOracleConfig{Enabled: true},
	}
	refs := buildDivergenceReferences(cfg, nopOracleReader{}, discardLogger())
	got := make(map[string]bool, len(refs))
	for _, r := range refs {
		got[r.Name()] = true
	}
	for _, want := range []string{
		divergence.OracleSourceReflectorDEX,
		divergence.OracleSourceReflectorCEX,
		divergence.OracleSourceReflectorFX,
		divergence.OracleSourceRedstone,
		divergence.OracleSourceBand,
		divergence.SyntheticCrossName,
	} {
		if !got[want] {
			t.Errorf("missing reference %q (got %v)", want, got)
		}
	}
	if len(refs) != 6 {
		t.Errorf("len(refs) = %d, want 6", len(refs))
	}
}

// TestBuildDivergenceReferences_SyntheticNeedsBothLegClasses — with the
// FX leg class absent (reflector disabled ⇒ no reflector-fx, chainlink
// disabled ⇒ no fiat feeds), the synthetic must NOT construct: a cross
// with a missing leg would only add a permanent failure row to every
// result. Redstone+band alone provide base legs only.
func TestBuildDivergenceReferences_SyntheticNeedsBothLegClasses(t *testing.T) {
	cfg := config.DivergenceConfig{
		Redstone: config.DivergenceOracleConfig{Enabled: true},
		Band:     config.DivergenceOracleConfig{Enabled: true},
	}
	refs := buildDivergenceReferences(cfg, nopOracleReader{}, discardLogger())
	for _, r := range refs {
		if r.Name() == divergence.SyntheticCrossName {
			t.Fatalf("synthetic cross constructed without an FX leg class")
		}
	}
	if len(refs) != 2 {
		t.Errorf("len(refs) = %d, want 2 (redstone + band only)", len(refs))
	}
}

// TestBuildDivergenceReferences_OnChainOraclesSkippedWithoutReader —
// enabled gates with a nil reader (no Postgres) skip cleanly rather
// than wiring references that would nil-deref on every tick.
func TestBuildDivergenceReferences_OnChainOraclesSkippedWithoutReader(t *testing.T) {
	cfg := config.DivergenceConfig{
		Reflector: config.DivergenceOracleConfig{Enabled: true},
		Redstone:  config.DivergenceOracleConfig{Enabled: true},
		Band:      config.DivergenceOracleConfig{Enabled: true},
	}
	if refs := buildDivergenceReferences(cfg, nil, discardLogger()); len(refs) != 0 {
		t.Fatalf("len(refs) = %d, want 0 (nil reader must skip on-chain oracles)", len(refs))
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

func (stubKeysForBuild) ListForAccount(context.Context, uuid.UUID) ([]platform.APIKey, error) {
	panic("unused")
}
func (stubKeysForBuild) Update(context.Context, platform.APIKey) error            { panic("unused") }
func (stubKeysForBuild) Revoke(context.Context, string, uuid.UUID, string) error  { panic("unused") }
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

// TestBuildAPIKeyValidator_BothFlagStates pins the auth_backend cutover
// knob: the default/"redis" backend keeps the CURRENT behaviour (legacy
// RedisAPIKeyValidator), and "postgres" selects the read-through
// validator — falling loud to the Noop (every request 503s) when a
// required dependency is missing rather than silently demoting to
// anonymous.
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

// TestResolveSEP10Validator_WiringByConfiguration pins the F-1224 SEP-10
// wiring reconciliation. The pre-fix two-phase dance (build with a nil Redis
// client at phase 1, "rebuild" behind an `if !isNoop` guard that was never
// true at phase 2) meant a CONFIGURED SEP-10 deployment NEVER got the guarded
// (replay-protected) validator — it either refused to boot (auth_mode=sep10)
// or silently 503'd every SEP-10 endpoint (Noop), and the rebuild's failure
// branch was a latent fail-open. This asserts the corrected wiring:
//
//   - configured (seed+jwt env) + Redis      → *sep10.Validator (GUARDED),
//     in every auth_mode. (Pre-fix: Noop — this case is the red one.)
//   - configured + NO Redis + auth_mode=sep10 → hard error (ErrReplayGuardUnavailable);
//     never a guard-free validator.
//   - configured + NO Redis + other mode      → Noop (503), binary still boots.
//   - unconfigured (no seed/jwt env)          → Noop (503), binary still boots
//     (the common r1 auth_mode=apikey_optional case).
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
			v, err := resolveSEP10Validator(configured, mode, rdb, discardLogger())
			if err != nil {
				t.Fatalf("mode=%s: unexpected error: %v", mode, err)
			}
			if _, ok := v.(*sep10.Validator); !ok {
				t.Fatalf("mode=%s: got %T, want *sep10.Validator (guarded, replay-protected)", mode, v)
			}
			if _, isNoop := v.(auth.NoopSEP10Validator); isNoop {
				t.Fatalf("mode=%s: a configured deployment WITH Redis must not get the Noop (503) validator", mode)
			}
		}
	})

	t.Run("configured + NO Redis + auth_mode=sep10 → fail closed", func(t *testing.T) {
		setConfiguredEnv(t)
		v, err := resolveSEP10Validator(configured, "sep10", nil, discardLogger())
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
		v, err := resolveSEP10Validator(configured, "apikey_optional", nil, discardLogger())
		if err != nil {
			t.Fatalf("must degrade to Noop (not abort boot) outside auth_mode=sep10: %v", err)
		}
		if _, ok := v.(auth.NoopSEP10Validator); !ok {
			t.Errorf("got %T, want auth.NoopSEP10Validator (503 on SEP-10 endpoints, never guard-free)", v)
		}
	})

	t.Run("unconfigured → Noop, boots", func(t *testing.T) {
		v, err := resolveSEP10Validator(config.SEP10Config{
			WebAuthDomain: "auth.stellarindex.test",
			HomeDomain:    "stellarindex.test",
		}, "apikey_optional", nil, discardLogger())
		if err != nil {
			t.Fatalf("an unconfigured SEP-10 deployment must boot with the Noop, got error: %v", err)
		}
		if _, ok := v.(auth.NoopSEP10Validator); !ok {
			t.Errorf("got %T, want auth.NoopSEP10Validator", v)
		}
	})
}

// TestWarnUnsafeBind covers the C3-18 IPv6 parse fix: a public
// all-interfaces bind must trigger the unsafe-bind warning whether it's
// written as IPv4 (0.0.0.0), IPv6 ([::]), or port-only (:3000). The
// pre-fix strings.Cut(":") split "[::]:3000" into host "[" and silently
// shipped a public IPv6 bind with no warning.
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

// TestWarnCollapsedAnonThrottle pins the boot warning for the
// anonymous-throttle collapse (REL-availability, audit-2026-08-03):
// with trusted_proxy_cidrs empty behind a reverse proxy every
// anonymous caller keys on the proxy's single address, collapsing the
// whole anon tier into ONE shared bucket (an availability self-DoS).
//
// The warning must fire when the anon tier is BOTH capped
// (anon_rate_limit_per_min > 0) AND reachable (auth_mode admits
// anonymous: none / apikey_optional) AND trusted_proxy_cidrs is empty —
// and must NOT fire when a proxy CIDR is set (the R1 shape), when the
// anon tier is disabled (0), or under a credential-required auth mode.
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

	// A configured proxy CIDR resolves each client to its real address,
	// so the throttle discriminates and there is no collapse. This is
	// the R1 shape (auth_mode=apikey_optional, anon=6000,
	// trusted_proxy_cidrs=["127.0.0.1/32"]) — it MUST stay silent.
	if warnLogged("apikey_optional", 6000, []string{"127.0.0.1/32"}) {
		t.Error("R1 shape (non-empty trusted_proxy_cidrs): must NOT warn")
	}
	if warnLogged("none", 60, []string{"10.0.0.0/8"}) {
		t.Error("non-empty trusted_proxy_cidrs: throttle discriminates; must NOT warn")
	}
}

// fakeSubscriberRunner implements subscriberRunner for
// TestRunSubscriberSupervisedWithBackoff_RestartsPastFailures. The
// first failN calls to Run return failErr immediately (mimicking
// redispub.Subscriber.Run's "subscribe channel closed unexpectedly"
// error); every call after that blocks until ctx is cancelled, like
// the real Subscriber.Run does while the connection stays healthy.
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

// TestRunSubscriberSupervisedWithBackoff_RestartsPastFailures —
// REL-supervision (audit-2026-07-23). redispub.Subscriber.Run's doc
// says an unexpected stream-end error lets "the caller decide
// whether to retry"; prior to this fix the caller (main.go) never
// did — a single Run failure logged and the goroutine exited for
// good, leaving /v1/price/stream's closed-bucket feed permanently
// silent for the rest of the process. This asserts the supervised
// loop actually restarts past MULTIPLE consecutive failures (not
// just tolerates one) and reaches a long-lived run. Against the
// pre-fix single-shot call site this times out: calls never exceeds
// 1.
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

// TestInProcessLoginThrottle_EnforcesPerEmailCap — NTF-08
// (audit-2026-07-23). A single victim inbox must be capped even
// when every send comes from a DIFFERENT IP (the inbox-bomb
// dimension a per-IP-only cap can't catch). Asserts the corrected
// value: the (max+1)th send to the same email is denied.
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

// TestInProcessSignupIPThrottle_EnforcesPerIPCap — NTF-08
// (audit-2026-07-23). Asserts the corrected value: the (max+1)th
// signup from one IP within the window is denied with
// auth.ErrSignupRateLimited (what signupIPThrottleOK translates into
// a 429), while a distinct IP is unaffected.
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
