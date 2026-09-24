// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

func TestCAGGRefreshWindows(t *testing.T) {
	t.Parallel()
	cols := []string{"view_name", "has_policy", "start_offset_seconds"}
	store, conn := newScriptedStore(t, scriptedResult{cols: cols, rows: [][]driver.Value{
		{"oracle_prices_1m", true, int64(300)},
		{"source_volume_1h", false, nil},
		{"supply_1d", true, nil},
	}})
	got, err := store.CAGGRefreshWindows(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []CAGGRefreshWindow{
		{View: "oracle_prices_1m", HasPolicy: true, StartOffset: 5 * time.Minute},
		{View: "source_volume_1h"},
		{View: "supply_1d", HasPolicy: true, Unbounded: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("window %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	stmt := conn.only(t)
	for _, frag := range []string{
		"timescaledb_information.continuous_aggregates",
		"LEFT JOIN timescaledb_information.jobs",
		"proc_name = 'policy_refresh_continuous_aggregate'",
		"IN (c.view_name, c.materialization_hypertable_name)",
		"config->>'start_offset'",
	} {
		if !strings.Contains(stmt.sql, frag) {
			t.Errorf("SQL lacks %q:\n%s", frag, stmt.sql)
		}
	}
}
