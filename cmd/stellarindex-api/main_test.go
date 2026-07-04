package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/StellarIndex/stellar-index/internal/canonical"
	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/divergence"
	"github.com/StellarIndex/stellar-index/internal/usage"
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
