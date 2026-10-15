// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/rwa"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externaltiingo "github.com/Stellar-Index/StellarIndex/internal/sources/external/tiingo"
	soroswap_router "github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
)

// Coverage for the indexer's run() wiring helpers. Each
// ran only from run(), which no test calls.

// TestSetSourceEnabled_LowercasesAndSkipsEmpty pins label normalisation: a mixed-case name would split one source into
// two series (double-counting in `sum by (source)`), and an empty label value is a meaningless series.
func TestSetSourceEnabled_LowercasesAndSkipsEmpty(t *testing.T) {
	obs.SourceEnabled.Reset()

	setSourceEnabled([]string{"CoinGecko", "", "BINANCE", "sdex"}, true)

	for _, want := range []string{"coingecko", "binance", "sdex"} {
		if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues(want)); got != 1 {
			t.Errorf("source_enabled{source=%q} = %v, want 1 — the label must be lowercased "+
				"or dashboards grouping on it split one source into two series", want, got)
		}
	}
	// The mixed-case spellings must NOT exist as their own series.
	for _, unwanted := range []string{"CoinGecko", "BINANCE"} {
		if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues(unwanted)); got != 0 {
			t.Errorf("source_enabled{source=%q} exists with value %v; the mixed-case spelling "+
				"must not be its own series", unwanted, got)
		}
	}
	if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues("")); got != 0 {
		t.Errorf("source_enabled{source=\"\"} = %v, want no series — an empty label value is "+
			"legal but meaningless and no query selects it", got)
	}
}

// TestSetSourceEnabled_UnknownSourceIsRecordedNotRejected pins that a name missing from external.Registry is recorded,
// not validated: the gauge reports what the operator configured, and cardinality is bounded by a fixed startup list.
func TestSetSourceEnabled_UnknownSourceIsRecordedNotRejected(t *testing.T) {
	obs.SourceEnabled.Reset()

	const typo = "coingeko" // a real, plausible operator typo
	if _, registered := external.Registry[typo]; registered {
		t.Fatalf("%q is now a registered source; pick another unregistered name for this test", typo)
	}

	setSourceEnabled([]string{typo}, true)

	if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues(typo)); got != 1 {
		t.Errorf("source_enabled{source=%q} = %v, want 1 — an unregistered name must still be "+
			"reported, because it is exactly the misconfiguration the operator has to see; "+
			"dropping it hides the typo that left their source absent", typo, got)
	}
}

// TestSetSourceEnabled_FalseSetsZeroNotAbsent: "disabled" must be an explicit 0, or `source_enabled == 0` matches
// nothing and a disabled source is indistinguishable from an unconfigured one.
func TestSetSourceEnabled_FalseSetsZeroNotAbsent(t *testing.T) {
	obs.SourceEnabled.Reset()

	setSourceEnabled([]string{"kraken"}, true)
	setSourceEnabled([]string{"kraken"}, false)

	if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues("kraken")); got != 0 {
		t.Errorf("source_enabled{source=\"kraken\"} = %v after disabling, want an explicit 0", got)
	}
}

// ─── startExternalConnectors ─────────────────────────────────────

// TestStartExternalConnectors_NothingEnabledStartsNothing pins that the all-disabled path returns a no-op wait func and
// an empty source list without touching external.Run; a nil wait func would panic run()'s unconditional shutdown call.
func TestStartExternalConnectors_NothingEnabledStartsNothing(t *testing.T) {
	t.Parallel()

	wait, enabled, err := startExternalConnectors(
		context.Background(),
		config.ExternalConfig{},
		mustCatalogue(t),
		make(chan consumer.Event, 1),
		discardIndexerLogger(),
	)
	if err != nil {
		t.Fatalf("startExternalConnectors with nothing enabled: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("enabled = %v, want empty — no connector was configured", enabled)
	}
	if wait == nil {
		t.Fatal("nil wait func — run()'s shutdown sequence calls it unconditionally and " +
			"would nil-panic on a deployment with no external sources")
	}
	wait() // must not block or panic
}

// TestStartExternalConnectors_EveryEnabledNameIsRegistered is the registry-gating assertion: returned names are the
// trade `Source`, resolved via external.Lookup at VWAP time. An unregistered name yields AmountDecimals 0, but scaling
// is not uniform (CEX 10^8, FX 10^6), so amounts would be wrong by eight orders of magnitude.
func TestStartExternalConnectors_EveryEnabledNameIsRegistered(t *testing.T) {
	t.Parallel()

	// Only the pollers/streamers that need no credential and no network
	// at construction. Each is enabled ALONE so a failure names it.
	cases := []struct {
		name string
		cfg  config.ExternalConfig
	}{
		{"coingecko", config.ExternalConfig{CoinGecko: config.CoinGeckoVenueConfig{Enabled: true}}},
		{"ecb", config.ExternalConfig{ECB: config.ExternalVenueConfig{Enabled: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Cancel before starting: external.Run's goroutines observe a
			// dead context immediately, so nothing reaches the network.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			wait, enabled, err := startExternalConnectors(
				ctx, tc.cfg, mustCatalogue(t),
				make(chan consumer.Event, 1), discardIndexerLogger(),
			)
			if err != nil {
				t.Fatalf("startExternalConnectors: %v", err)
			}
			if wait != nil {
				wait()
			}
			if len(enabled) == 0 {
				t.Fatalf("%s was enabled in config but reported no source name — it would be "+
					"invisible to setSourceEnabled and to every gate that matches on the "+
					"enabled list", tc.name)
			}
			for _, name := range enabled {
				md, ok := external.Registry[name]
				if !ok {
					t.Errorf("startExternalConnectors reports source %q, which is NOT in "+
						"external.Registry.\nregistered: %v\n\n"+
						"external.Lookup then returns AmountDecimals 0 for it, so the "+
						"aggregator reads its amounts at 10^0 instead of the venue's real "+
						"scale (CEX 10^8, FX 10^6) — eight orders of magnitude, silently.",
						name, registrySourceNames())
				}
				if ok && name != strings.ToLower(name) {
					t.Errorf("source name %q is not lowercase; setSourceEnabled lowercases it "+
						"for the metric label, so the gauge and the registry key would "+
						"disagree (md=%+v)", name, md)
				}
			}
		})
	}
}

func registrySourceNames() []string {
	out := make([]string, 0, len(external.Registry))
	for name := range external.Registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func discardIndexerLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// mustCatalogue loads the embedded verified-currency catalogue that run() passes; a nil one would exercise a fallback
// path production never takes.
func mustCatalogue(t *testing.T) *currency.Catalogue {
	t.Helper()
	c, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	return c
}

// TestNewECBPoller_UsesConfiguredInterval is the regression test for `[external.ecb] poll_interval` being validated and
// then ignored, so the poller always ran at its 6h default.
func TestNewECBPoller_UsesConfiguredInterval(t *testing.T) {
	t.Parallel()

	p := newECBPoller(config.ExternalVenueConfig{PollInterval: 30 * time.Minute})
	if got, want := p.PollInterval(), 30*time.Minute; got != want {
		t.Errorf("PollInterval() = %v, want %v (config override was not applied)", got, want)
	}
}

func TestNewECBPoller_DefaultsWhenUnset(t *testing.T) {
	t.Parallel()

	p := newECBPoller(config.ExternalVenueConfig{})
	if got, want := p.PollInterval(), 6*time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want connector default %v", got, want)
	}
}

func TestNewTiingoPoller_TickersFromBindings(t *testing.T) {
	t.Parallel()

	p, err := newTiingoPoller(config.TiingoVenueConfig{APIKey: "k"})
	if err != nil {
		t.Fatalf("newTiingoPoller: %v", err)
	}
	if !slices.Equal(p.Tickers, rwa.FundNAVTickers()) {
		t.Errorf("Tickers = %v, want the fund bindings' %v", p.Tickers, rwa.FundNAVTickers())
	}
	if got, want := p.PollInterval(), time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want connector default %v", got, want)
	}

	p, err = newTiingoPoller(config.TiingoVenueConfig{APIKey: "k", PollInterval: 2 * time.Hour})
	if err != nil {
		t.Fatalf("newTiingoPoller: %v", err)
	}
	if got, want := p.PollInterval(), 2*time.Hour; got != want {
		t.Errorf("PollInterval() = %v, want override %v", got, want)
	}
}

func TestNewTiingoPoller_RefusesMissingKey(t *testing.T) {
	t.Parallel()

	if _, err := newTiingoPoller(config.TiingoVenueConfig{Enabled: true}); !errors.Is(err, externaltiingo.ErrAPIKeyRequired) {
		t.Errorf("err = %v, want ErrAPIKeyRequired", err)
	}
}

// TestRouterEnabled_FoldsWhitespaceAndCase pins that routerEnabled folds enabled_sources with ToLower(TrimSpace) like
// config validation and the dispatcher; an entry like " Soroswap-Router " must match, or the sweeper silently never
// starts.
func TestRouterEnabled_FoldsWhitespaceAndCase(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"exact", []string{soroswap_router.SourceName}, true},
		{"mixed case + padding", []string{"  Soroswap-Router  "}, true},
		{"absent", []string{"comet"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routerEnabled(tc.in); got != tc.want {
				t.Errorf("routerEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAmmSignerEnabled_FoldsWhitespaceAndCase is the ammSignerEnabled analog: a fold mismatch against
// timescale.AMMSignerSources silently disables signer attribution for a correctly configured source.
func TestAmmSignerEnabled_FoldsWhitespaceAndCase(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want bool
	}{
		{"exact", []string{"comet"}, true},
		{"mixed case + padding", []string{"  Comet  "}, true},
		{"absent", []string{"reflector-dex"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ammSignerEnabled(tc.in); got != tc.want {
				t.Errorf("ammSignerEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
