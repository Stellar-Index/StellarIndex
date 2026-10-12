// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalchainlink "github.com/Stellar-Index/StellarIndex/internal/sources/external/chainlink"
)

// Coverage for the indexer's run() wiring helpers. All
// four ran only from run(), which no test calls.

// ─── chainlinkFeedSetFromConfig ──────────────────────────────────

// TestChainlinkFeedSetFromConfig_RejectsAMalformedPairKey pins that a non-canonical pair key aborts startup naming the
// key; skipping it would leave the feed silently absent from the divergence cross-check, which reads like agreement.
func TestChainlinkFeedSetFromConfig_RejectsAMalformedPairKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
	}{
		{"no-separator", "BTCUSD"},
		{"empty-key", ""},
		{"empty-leg", "crypto:BTC/"},
		{"three-legs", "crypto:BTC/fiat:USD/fiat:EUR"},
		{"whitespace", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := map[string]config.ChainlinkFeedSetting{
				tc.key: {Address: "0x0000000000000000000000000000000000000001", Decimals: 8},
			}
			feeds, pairs, err := chainlinkFeedSetFromConfig(in)
			if err == nil {
				t.Fatalf("feed_map key %q was accepted (feeds=%v pairs=%v); a key that is not a "+
					"canonical pair must abort startup rather than be silently dropped — a "+
					"missing divergence reference is indistinguishable from an agreeing one",
					tc.key, feeds, pairs)
			}
			if !strings.Contains(err.Error(), tc.key) && tc.key != "" {
				t.Errorf("error %q does not name the offending key %q; the operator has to be "+
					"able to find it in their TOML", err, tc.key)
			}
			if feeds != nil || pairs != nil {
				t.Errorf("a rejected feed map returned feeds=%v pairs=%v, want both nil — a "+
					"partial map would wire some feeds and drop others", feeds, pairs)
			}
		})
	}
}

// TestChainlinkFeedSetFromConfig_ZeroDecimalsStaysAbsentForOnChainResolution is the scaling trap: an omitted `decimals`
// is 0, and BuildFeedSet must carry that 0 through rather than substitute DefaultDecimals (8), since the poller adopts
// the on-chain decimals() for an omitted value and a substituted 8 would wrongly refuse an 18-decimal feed.
func TestChainlinkFeedSetFromConfig_ZeroDecimalsStaysAbsentForOnChainResolution(t *testing.T) {
	t.Parallel()

	feeds, _, err := chainlinkFeedSetFromConfig(map[string]config.ChainlinkFeedSetting{
		"crypto:BTC/fiat:USD": {Address: "0x0000000000000000000000000000000000000001"}, // Decimals omitted → 0
	})
	if err != nil {
		t.Fatalf("chainlinkFeedSetFromConfig: %v", err)
	}
	spec, ok := feeds["crypto:BTC/fiat:USD"]
	if !ok {
		t.Fatalf("feed absent from %v", feeds)
	}
	if spec.Decimals != 0 {
		t.Errorf("omitted decimals produced %d, want 0 (absent) — the poller adopts the feed's "+
			"on-chain decimals() for an absent value; a substituted %d would be verified as if "+
			"the operator had asserted it and refuse a non-8-decimal feed instead of adopting "+
			"its real scale", spec.Decimals, externalchainlink.DefaultDecimals)
	}
}

// TestChainlinkFeedSetFromConfig_CarriesAddressAndInvertThrough pins the hand-copied fields from
// config.ChainlinkFeedSetting into FeedSpec. A dropped `invert` would silently publish the
// reciprocal price, which looks plausible.
func TestChainlinkFeedSetFromConfig_CarriesAddressAndInvertThrough(t *testing.T) {
	t.Parallel()

	const addr = "0x00000000000000000000000000000000000000ab"
	feeds, _, err := chainlinkFeedSetFromConfig(map[string]config.ChainlinkFeedSetting{
		"fiat:EUR/fiat:USD": {Address: addr, Decimals: 6, Invert: true},
	})
	if err != nil {
		t.Fatalf("chainlinkFeedSetFromConfig: %v", err)
	}
	spec := feeds["fiat:EUR/fiat:USD"]
	if spec.Address != addr {
		t.Errorf("Address = %q, want %q — a dropped address points the poller at the zero "+
			"contract", spec.Address, addr)
	}
	if spec.Decimals != 6 {
		t.Errorf("Decimals = %d, want the operator's explicit 6 (not the default)", spec.Decimals)
	}
	if !spec.Invert {
		t.Error("Invert was dropped — the poller would publish the reciprocal pair, and 1/x " +
			"is a plausible-looking price rather than an obvious error")
	}
}

// TestChainlinkFeedSetFromConfig_CarriesMaxAgeThrough — a dropped
// max_age_hours would silently fall back to the class default, and an
// omitted one must resolve per pair (FX 76h), not to one constant.
func TestChainlinkFeedSetFromConfig_CarriesMaxAgeThrough(t *testing.T) {
	t.Parallel()

	feeds, _, err := chainlinkFeedSetFromConfig(map[string]config.ChainlinkFeedSetting{
		"crypto:BTC/fiat:USD": {Address: "0x00000000000000000000000000000000000000ab", MaxAgeHours: 5},
		"fiat:CHF/fiat:USD":   {Address: "0x00000000000000000000000000000000000000ac"},
	})
	if err != nil {
		t.Fatalf("chainlinkFeedSetFromConfig: %v", err)
	}
	if got := feeds["crypto:BTC/fiat:USD"].MaxAge; got != 5*time.Hour {
		t.Errorf("BTC MaxAge = %s, want the operator's 5h", got)
	}
	if got := feeds["fiat:CHF/fiat:USD"].MaxAge; got != externalchainlink.DefaultMaxAgeFX {
		t.Errorf("CHF MaxAge = %s, want the FX default %s", got, externalchainlink.DefaultMaxAgeFX)
	}
}

// TestChainlinkFeedSetFromConfig_EmptyMapYieldsTheBuiltInDefaults pins a counter-intuitive behaviour: BuildFeedSet
// substitutes DefaultFeedMap() for an empty input, so startExternalConnectors' `len(pairs) == 0` skip guard is
// unreachable.
func TestChainlinkFeedSetFromConfig_EmptyMapYieldsTheBuiltInDefaults(t *testing.T) {
	t.Parallel()

	for _, in := range []map[string]config.ChainlinkFeedSetting{nil, {}} {
		feeds, pairs, err := chainlinkFeedSetFromConfig(in)
		if err != nil {
			t.Fatalf("chainlinkFeedSetFromConfig(%v): %v", in, err)
		}
		if len(pairs) == 0 {
			t.Fatalf("an empty feed_map produced no pairs; if this is now the intent, the "+
				"defaults were removed and startExternalConnectors' skip-warning became "+
				"reachable — reconcile the two. feeds=%v", feeds)
		}
		if len(feeds) != len(externalchainlink.DefaultFeedMap()) {
			t.Errorf("an empty feed_map produced %d feeds, want the %d built-in defaults",
				len(feeds), len(externalchainlink.DefaultFeedMap()))
		}
	}
}

// ─── setSourceEnabled ────────────────────────────────────────────

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
