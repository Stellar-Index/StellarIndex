// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// A request made only of raw: assets must return before touching the
// database: the Store here has no connection, so a query would panic.
func TestDailyOraclePrices_RawOnlyAssetsAreNotQueried(t *testing.T) {
	raw, err := canonical.NewOracleRawAsset("USDT0")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	pts, err := (&Store{}).DailyOraclePrices(context.Background(), []canonical.Asset{raw}, usd, time.Time{}, time.Now())
	if err != nil || pts != nil {
		t.Fatalf("DailyOraclePrices(raw only) = (%v, %v), want (nil, nil)", pts, err)
	}
}

// An unvalidated Asset passes IsMapped yet stringifies to a raw: key, so
// the key filter alone cannot keep raw CAGG rows out; the SQL must.
func TestDailyOraclePrices_SQLRefusesRawRows(t *testing.T) {
	spoof := canonical.Asset{Type: canonical.AssetClassic, Code: "raw:FOO", Issuer: "BAR"}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"bucket", "source", "asset", "last_price", "last_decimals", "observation_count"},
	})
	if _, err := store.DailyOraclePrices(context.Background(), []canonical.Asset{spoof}, usd, time.Time{}, time.Now()); err != nil {
		t.Fatalf("DailyOraclePrices: %v", err)
	}
	stmt := conn.only(t)
	if keys, ok := stmt.arg(t, 1).([]string); !ok || len(keys) != 1 || keys[0] != "raw:FOO-BAR" {
		t.Fatalf("$1 = %#v, want []string{\"raw:FOO-BAR\"}", stmt.arg(t, 1))
	}
	if !strings.Contains(stmt.sql, "asset NOT LIKE 'raw:%'") {
		t.Fatalf("DailyOraclePrices SQL lacks the raw: exclusion:\n%s", stmt.sql)
	}
}
