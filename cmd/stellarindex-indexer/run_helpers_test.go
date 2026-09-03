// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	externalchainlink "github.com/Stellar-Index/StellarIndex/internal/sources/external/chainlink"
)

// Coverage for the indexer's run() wiring helpers (#340 item 4). All
// four ran only from run(), which no test calls.

// ─── chainlinkFeedSetFromConfig ──────────────────────────────────

// TestChainlinkFeedSetFromConfig_RejectsAMalformedPairKey — the feed map
// is operator TOML, so its keys are unvalidated input. A key that is not
// a canonical pair must abort startup, naming the key.
//
// The alternative — skipping the bad entry — is the dangerous one: the
// operator sees "chainlink enabled" in the logs, the poller runs, and
// the feed they configured is simply absent from the divergence
// cross-check. A missing reference reads exactly like an agreeing one.
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

// TestChainlinkFeedSetFromConfig_ZeroDecimalsBecomesTheChainlinkDefault
// is the scaling trap.
//
// `decimals` is a uint8, so an operator who omits it in TOML gets 0.
// Taking that literally would divide the raw int256 answer by 10^0 —
// serving a BTC/USD price 10^8 times too large into the divergence
// cross-check. BuildFeedSet substitutes DefaultDecimals (8) for 0, and
// this pins that substitution end-to-end through the adapter.
func TestChainlinkFeedSetFromConfig_ZeroDecimalsBecomesTheChainlinkDefault(t *testing.T) {
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
	if spec.Decimals != externalchainlink.DefaultDecimals {
		t.Errorf("omitted decimals produced %d, want the Chainlink default %d — a literal 0 "+
			"divides the raw int256 answer by 10^0 and feeds a price 10^8 times too large "+
			"into the divergence cross-check", spec.Decimals, externalchainlink.DefaultDecimals)
	}
}

// TestChainlinkFeedSetFromConfig_CarriesAddressAndInvertThrough — the
// adapter copies three fields by hand from config.ChainlinkFeedSetting
// into externalchainlink.FeedSpec. A field dropped in that copy is
// silent: `invert` in particular would publish the RECIPROCAL of the
// intended pair, and a 1/x price is a plausible-looking number.
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

// TestChainlinkFeedSetFromConfig_EmptyMapYieldsTheBuiltInDefaults pins a
// counter-intuitive behaviour that reads the other way at the call site.
//
// startExternalConnectors guards this helper with
//
//	if len(pairs) == 0 { logger.Warn("…feed_map is empty after parse — skipping") }
//
// which suggests an empty feed_map disables the source. It does not:
// BuildFeedSet substitutes DefaultFeedMap() for an empty input, so an
// operator who enables chainlink with no feed_map silently gets the six
// built-in mainnet feeds. That is a defensible default, but it is the
// opposite of what the guard implies, so it is pinned here — and the
// guard is consequently unreachable for an empty map (see #340).
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

// TestSetSourceEnabled_LowercasesAndSkipsEmpty pins the two
// normalisations this helper performs on its way to a Prometheus label.
//
// The `source` label is what every dashboard and alert groups on, so a
// mixed-case name would split one source into two series — the metric
// still looks healthy, but `sum by (source)` double-counts and an alert
// keyed on the lowercase spelling never fires for the other.
//
// The empty-string skip matters for the same reason: an empty label
// value is a legal but meaningless series that no query selects.
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

// TestSetSourceEnabled_UnknownSourceIsRecordedNotRejected states the
// helper's contract for a name that is not in external.Registry.
//
// It does NOT validate. That is deliberate and correct: the gauge
// reports what the OPERATOR configured, and an unregistered name is
// precisely the misconfiguration an operator needs to see on a
// dashboard. Rejecting or dropping it would hide the typo that made
// their source silently absent.
//
// The cost is unbounded label cardinality from config, which is
// acceptable because the input is a fixed operator list read once at
// startup — not request-derived. Pinned so neither property is changed
// by accident.
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

// TestSetSourceEnabled_FalseSetsZeroNotAbsent — "disabled" has to be an
// explicit 0. Leaving the series absent instead makes
// `source_enabled == 0` match nothing, so a disabled source and an
// unconfigured one become indistinguishable in an alert.
func TestSetSourceEnabled_FalseSetsZeroNotAbsent(t *testing.T) {
	obs.SourceEnabled.Reset()

	setSourceEnabled([]string{"kraken"}, true)
	setSourceEnabled([]string{"kraken"}, false)

	if got := testutil.ToFloat64(obs.SourceEnabled.WithLabelValues("kraken")); got != 0 {
		t.Errorf("source_enabled{source=\"kraken\"} = %v after disabling, want an explicit 0", got)
	}
}

// ─── startExternalConnectors ─────────────────────────────────────

// TestStartExternalConnectors_NothingEnabledStartsNothing — the
// all-disabled path must return a usable no-op wait func and an empty
// source list, without touching external.Run (which would spawn
// network goroutines).
//
// A nil wait func here would nil-panic run()'s shutdown sequence, which
// calls it unconditionally.
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

// TestStartExternalConnectors_EveryEnabledNameIsRegistered is the
// registry-gating assertion, and the one with teeth.
//
// The names this function returns become the `enabled` list that
// setSourceEnabled labels and that routerEnabled / ammSignerEnabled
// match against. More importantly they are the `Source` string stamped
// on every trade the connector emits, and the aggregator resolves that
// string through external.Lookup at VWAP compute time.
//
// Lookup FAILS OPEN on shape but CLOSED on inclusion: an unregistered
// name yields Metadata{Class: Exchange, IncludeInVWAP: false,
// AmountDecimals: 0}. AmountDecimals 0 is the dangerous one — CLAUDE.md
// is explicit that external scaling is NOT uniform (CEX 10^8, FX 10^6),
// so a name that misses the registry is read at 10^0 and its amounts
// are wrong by eight orders of magnitude.
//
// So a connector wired here without a Registry entry is a scaling
// defect, not merely a missing dashboard row.
func TestStartExternalConnectors_EveryEnabledNameIsRegistered(t *testing.T) {
	t.Parallel()

	// Only the pollers/streamers that need no credential and no network
	// at construction. Each is enabled ALONE so a failure names it.
	cases := []struct {
		name string
		cfg  config.ExternalConfig
	}{
		{"coingecko", config.ExternalConfig{CoinGecko: config.ExternalVenueConfig{Enabled: true}}},
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

// mustCatalogue loads the embedded verified-currency catalogue — the
// same one run() passes. The aggregator pair set and the CoinGecko
// ticker map are both derived from it, so a nil catalogue would exercise
// a fallback path production never takes.
func mustCatalogue(t *testing.T) *currency.Catalogue {
	t.Helper()
	c, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	return c
}
