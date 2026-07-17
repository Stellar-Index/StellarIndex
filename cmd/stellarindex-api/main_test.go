package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
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
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(refs))
	}
	names := []string{refs[0].Name(), refs[1].Name()}
	wantSet := map[string]bool{"coingecko": true, "chainlink": true}
	for _, n := range names {
		if !wantSet[n] {
			t.Errorf("unexpected reference: %q", n)
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
// when an oracle_updates reader is available.
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
	} {
		if !got[want] {
			t.Errorf("missing on-chain oracle reference %q (got %v)", want, got)
		}
	}
	if len(refs) != 5 {
		t.Errorf("len(refs) = %d, want 5", len(refs))
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

func (stubAccountsForBuild) GetByStripeCustomerID(context.Context, string) (platform.Account, error) {
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
