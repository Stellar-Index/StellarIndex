// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// assertYieldWindowBounded guards the bounded-scan rule for defindex_flows.
func assertYieldWindowBounded(t *testing.T, name, q string) {
	t.Helper()
	if !strings.Contains(q, "ledger_close_time > now() - $1::interval") {
		t.Errorf("%s must be window-bounded on ledger_close_time", name)
	}
}

// TestDefindexSeriesQueryShapes guards the yield series SQL: hourly buckets
// at the 24h window, daily otherwise; vault-count series scoped to the
// vault layer, strategy series to the strategy layer; all window-bounded.
func TestDefindexSeriesQueryShapes(t *testing.T) {
	for name, build := range map[string]func(int) string{
		"vault counts":    defindexVaultCountSeriesQuery,
		"strategy volume": defindexStrategyVolumeSeriesQuery,
		"strategy events": defindexStrategyEventsSeriesQuery,
	} {
		hourly := build(1)
		if !strings.Contains(hourly, "date_trunc('hour', ledger_close_time)") || !strings.Contains(hourly, "HH24:00") {
			t.Errorf("%s 24h query must bucket by hour with the hour in the timestamp format", name)
		}
		daily := build(90)
		if !strings.Contains(daily, "date_trunc('day', ledger_close_time)") || strings.Contains(daily, "HH24:00") {
			t.Errorf("%s 90d query must bucket by day with a date-only timestamp format", name)
		}
		for _, q := range []string{hourly, daily} {
			assertYieldWindowBounded(t, name, q)
		}
	}

	if q := defindexVaultCountSeriesQuery(90); !strings.Contains(q, "layer = 'vault'") || strings.Contains(q, "sum(") {
		t.Error("vault count series must scope to the vault layer and count events (vault-layer scalar amount is NULL; summing it would be silently zero)")
	}
	for _, q := range []string{defindexStrategyVolumeSeriesQuery(90), defindexStrategyEventsSeriesQuery(90)} {
		if !strings.Contains(q, "layer = 'strategy'") {
			t.Error("strategy series must scope to the strategy layer")
		}
	}
}

// TestDefindexKPIQueriesShape guards the KPI splits: the window query is
// bounded, keeps the capital-volume figures on the strategy layer (where
// the scalar amount truthfully lives) and the who/when counts on the vault
// layer; the all-time query is deliberately unbounded and counts only.
func TestDefindexKPIQueriesShape(t *testing.T) {
	w := defindexWindowKPIQuery()
	assertYieldWindowBounded(t, "window KPIs", w)
	if !strings.Contains(w, "AND layer = 'strategy'),0)") {
		t.Error("window volume KPIs must sum the strategy-layer scalar amounts")
	}
	if !strings.Contains(w, "count(DISTINCT actor) FILTER (WHERE layer = 'vault' AND direction = 'deposit')") {
		t.Error("unique depositors must count distinct vault-layer deposit actors (the end-user owner column)")
	}
	if !strings.Contains(w, "count(DISTINCT contract_id) FILTER (WHERE layer = 'vault')") {
		t.Error("active vaults must count distinct vault-layer contracts")
	}

	a := defindexAllTimeKPIQuery()
	if strings.Contains(a, "$1") || strings.Contains(a, "interval") {
		t.Error("all-time KPI query must not be window-bounded (it is the retained-history total)")
	}
	if strings.Contains(a, "sum(") {
		t.Error("all-time KPIs must be counts + first-seen date only — a cross-layer/cross-asset all-time amount sum would be meaningless")
	}
	if !strings.Contains(a, "min(ledger_close_time)") {
		t.Error("all-time KPIs must carry the first-observation date so the retained-history claim is concrete")
	}
}

// TestDefindexVaultBreakdownShape guards the flows-by-vault donut: vault-
// layer COUNTS only (never token amounts — different vaults hold different
// assets), count-descending, labelled via the curated router name when
// seeded.
func TestDefindexVaultBreakdownShape(t *testing.T) {
	q := defindexVaultBreakdownQuery()
	assertYieldWindowBounded(t, "vault breakdown", q)
	if !strings.Contains(q, "layer = 'vault'") {
		t.Error("vault breakdown must scope to the vault layer")
	}
	if strings.Contains(q, "amounts_vec") || strings.Contains(q, "sum(") {
		t.Error("vault breakdown must be counts only — summing amounts across vaults mixes different assets")
	}
	if !strings.Contains(q, "COALESCE(r.name, f.contract_id)") {
		t.Error("vault breakdown must label via the curated router name where seeded, contract id otherwise")
	}
	if !strings.Contains(q, "ORDER BY count(*) DESC") {
		t.Error("vault breakdown must be count-descending (the Go fold keeps the head + Others)")
	}
}

// TestDefindexVaultFlowsTableShape guards the mixed-asset-sum ban on the
// per-vault table: counts always; deposited/withdrawn amounts come from
// the single-entry amounts_vec and appear ONLY when every one of the
// vault's window flows carries a single-asset vector — anything else must
// render '—' rather than a fabricated cross-asset sum.
func TestDefindexVaultFlowsTableShape(t *testing.T) {
	q := defindexVaultFlowsTableQuery()
	assertYieldWindowBounded(t, "vault flows table", q)
	if !strings.Contains(q, "layer = 'vault'") {
		t.Error("vault flows table must scope to the vault layer")
	}
	if !strings.Contains(q, "max(cardinality(f.amounts_vec)) = 1") {
		t.Error("per-vault amounts must be gated on single-entry vectors across the whole window (multi-asset vaults show '—')")
	}
	if !strings.Contains(q, "count(*) = count(f.amounts_vec)") {
		t.Error("per-vault amounts must also require every window row to carry a vector — a vault with vector-less rows would silently undercount")
	}
	if !strings.Contains(q, "sum(f.amounts_vec[1])") {
		t.Error("per-vault amounts must sum the single-entry vector element (the vault's own asset)")
	}
	if !strings.Contains(q, "ELSE '—' END") {
		t.Error("vaults failing the single-asset gate must render an honest em dash, not a number")
	}
	if strings.Contains(q, "LIMIT 25") == false {
		t.Error("vault flows table must cap its rows")
	}
}

// TestDefindexStrategyTableShape pins the long-standing per-strategy net
// flow table to the strategy layer's scalar amounts.
func TestDefindexStrategyTableShape(t *testing.T) {
	q := defindexStrategyTableQuery()
	assertYieldWindowBounded(t, "strategy table", q)
	if !strings.Contains(q, "layer = 'strategy'") {
		t.Error("strategy table must scope to the strategy layer")
	}
	if strings.Contains(q, "amounts_vec") {
		t.Error("strategy table must use the strategy-layer scalar amount, not the vault vector")
	}
}

// TestYieldOracleNoFloatLiterals guards ADR-0003 across every yield +
// oracle query builder: all arithmetic stays in exact NUMERIC/text — no
// float literals, no float casts.
func TestYieldOracleNoFloatLiterals(t *testing.T) {
	queries := map[string]string{
		"yield window KPIs":     defindexWindowKPIQuery(),
		"yield all-time KPIs":   defindexAllTimeKPIQuery(),
		"yield vault series":    defindexVaultCountSeriesQuery(90),
		"yield strategy volume": defindexStrategyVolumeSeriesQuery(90),
		"yield strategy events": defindexStrategyEventsSeriesQuery(90),
		"yield breakdown":       defindexVaultBreakdownQuery(),
		"yield vault table":     defindexVaultFlowsTableQuery(),
		"yield strategy table":  defindexStrategyTableQuery(),
		"oracle window KPIs":    oracleWindowKPIQuery(),
		"oracle median":         oracleMedianIntervalQuery(),
		"oracle all-time KPIs":  oracleAllTimeKPIQuery(),
		"oracle series":         oracleUpdatesSeriesQuery(90),
		"oracle per-feed":       oraclePerFeedSeriesQuery(90),
		"oracle breakdown":      oracleFeedBreakdownQuery(),
		"oracle feeds table":    oracleFeedsTableQuery(),
		"oracle latest prices":  oracleLatestPricesQuery(),
	}
	for name, q := range queries {
		for _, bad := range []string{"::float", "::double", "1e6", "1e+06", "1e7", "1e+07", "0.5 *"} {
			if strings.Contains(q, bad) {
				t.Errorf("%s contains %q — amounts must stay exact NUMERIC (ADR-0003)", name, bad)
			}
		}
	}
}
