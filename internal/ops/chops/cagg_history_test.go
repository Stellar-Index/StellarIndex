// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestRenderCAGGHistoryTextfile_Golden(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	src := now.Add(-30 * 24 * time.Hour)
	rows := []timescale.CAGGHistoryStart{
		// Emptied by a WITH NO DATA rebuild: no start sample, gap = the whole history.
		{View: "prices_1m", Source: "trades", SourceStart: src, ExpectedStart: src, Gap: 30 * 24 * time.Hour},
		{View: "prices_1mo", Source: "trades", MaterialisedStart: src, SourceStart: src, ExpectedStart: src},
		// Short by exactly the legacy day of slack: not yet missing.
		{View: "twap_1h", Source: "prices_1m", MaterialisedStart: src.Add(24 * time.Hour), SourceStart: src, ExpectedStart: src, Gap: 24 * time.Hour},
		{View: "twap_1d", Source: "prices_1m", MaterialisedStart: src.Add(25 * time.Hour), SourceStart: src, ExpectedStart: src, Gap: 25 * time.Hour},
		// Not a legacy view: only the new families carry it.
		{View: "pools_per_source_1h", Source: "trades", MaterialisedStart: now.Add(-time.Hour), SourceStart: src, ExpectedStart: src, Gap: 30*24*time.Hour - time.Hour},
		// Empty source: no expected sample, gap 0.
		{View: "supply_1d", Source: "asset_supply_history"},
	}
	var b bytes.Buffer
	renderCAGGHistoryTextfile(&b, rows, now)
	want := `# HELP stellarindex_cagg_history_start_seconds Unix time of the earliest bucket in a continuous aggregate's materialisation hypertable (its real-time tail excluded); absent when nothing is materialised.
# TYPE stellarindex_cagg_history_start_seconds gauge
stellarindex_cagg_history_start_seconds{view="prices_1mo"} 1797408000
stellarindex_cagg_history_start_seconds{view="twap_1h"} 1797494400
stellarindex_cagg_history_start_seconds{view="twap_1d"} 1797498000
stellarindex_cagg_history_start_seconds{view="pools_per_source_1h"} 1799996400
# HELP stellarindex_cagg_expected_start_seconds Unix time a continuous aggregate's history should start: its source's earliest row, raised by a documented floor or an armed retention policy (never one on trades); absent when the source is empty.
# TYPE stellarindex_cagg_expected_start_seconds gauge
stellarindex_cagg_expected_start_seconds{view="prices_1m"} 1797408000
stellarindex_cagg_expected_start_seconds{view="prices_1mo"} 1797408000
stellarindex_cagg_expected_start_seconds{view="twap_1h"} 1797408000
stellarindex_cagg_expected_start_seconds{view="twap_1d"} 1797408000
stellarindex_cagg_expected_start_seconds{view="pools_per_source_1h"} 1797408000
# HELP stellarindex_cagg_history_gap_seconds Seconds the materialised start of a continuous aggregate trails the bucket holding its expected start; a WITH NO DATA rebuild nobody refilled reads as months.
# TYPE stellarindex_cagg_history_gap_seconds gauge
stellarindex_cagg_history_gap_seconds{view="prices_1m"} 2592000
stellarindex_cagg_history_gap_seconds{view="prices_1mo"} 0
stellarindex_cagg_history_gap_seconds{view="twap_1h"} 86400
stellarindex_cagg_history_gap_seconds{view="twap_1d"} 90000
stellarindex_cagg_history_gap_seconds{view="pools_per_source_1h"} 2588400
stellarindex_cagg_history_gap_seconds{view="supply_1d"} 0
# HELP stellarindex_cagg_history_missing 1 when a price continuous aggregate (prices_1m..prices_1mo) holds less history than the trades hypertable it aggregates — a migration recreated it WITH NO DATA, or a replay rewrote its base rows, and the manual refresh_continuous_aggregate follow-up never ran. Its refresh policy re-fills only a trailing sliver, so every newest-bar and last-refresh signal reads green while the back-history serves empty.
# TYPE stellarindex_cagg_history_missing gauge
stellarindex_cagg_history_missing{view="prices_1m"} 1
stellarindex_cagg_history_missing{view="prices_1mo"} 0
# HELP stellarindex_twap_history_missing 1 when a TWAP continuous aggregate is missing the history prices_1m holds — a migration recreated/emptied it (WITH NO DATA) and the manual refresh_continuous_aggregate follow-up never ran.
# TYPE stellarindex_twap_history_missing gauge
stellarindex_twap_history_missing{view="twap_1h"} 0
stellarindex_twap_history_missing{view="twap_1d"} 1
# HELP stellarindex_trades_retention_present 1 when a retention policy exists on trades, which is kept forever; the history gauges ignore it.
# TYPE stellarindex_trades_retention_present gauge
stellarindex_trades_retention_present 0
`
	if got := b.String(); got != want {
		t.Errorf("textfile drifted from the golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestLegacyHistoryGaugesKeepTheirSeries: deploy/monitoring/rules/data-freshness.yml and its promtool tests select
// exactly these nine view labels under exactly these two families, as data-freshness.sh emitted them.
func TestLegacyHistoryGaugesKeepTheirSeries(t *testing.T) {
	want := map[string]string{}
	for _, v := range []string{"prices_1m", "prices_15m", "prices_1h", "prices_4h", "prices_1d", "prices_1w", "prices_1mo"} {
		want[v] = "stellarindex_cagg_history_missing"
	}
	want["twap_1h"] = "stellarindex_twap_history_missing"
	want["twap_1d"] = "stellarindex_twap_history_missing"
	if !maps.Equal(legacyHistoryGauge, want) {
		t.Fatalf("legacy gauge map = %v, want %v", legacyHistoryGauge, want)
	}
	registered := map[string]bool{}
	for _, s := range timescale.TradesCAGGs {
		registered[s.Name] = true
	}
	for _, v := range slices.Sorted(maps.Keys(want)) {
		if !registered[v] {
			t.Errorf("legacy view %s is no longer a registered trades aggregate, so its gauge would vanish", v)
		}
	}
}

func TestLegacyHistoryMissing_FreshDeployStaysQuiet(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	young := timescale.CAGGHistoryStart{View: "prices_1d", SourceStart: now.Add(-47 * time.Hour), ExpectedStart: now.Add(-47 * time.Hour), Gap: 47 * time.Hour}
	if got := legacyHistoryMissing(young, now); got != 0 {
		t.Errorf("source under two days old: missing = %d, want 0", got)
	}
	young.SourceStart = now.Add(-49 * time.Hour)
	if got := legacyHistoryMissing(young, now); got != 1 {
		t.Errorf("source over two days old, empty view: missing = %d, want 1", got)
	}
	if strings.Contains(legacyHistoryGauge["trade_sums_1m"], "missing") {
		t.Error("trade_sums_1m must not carry a legacy missing gauge (its 26 h floor is by design)")
	}
}
