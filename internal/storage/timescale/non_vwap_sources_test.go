package timescale

import (
	"context"
	"database/sql/driver"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

func TestNonVWAPSourcesArg(t *testing.T) {
	names := strings.Split(nonVWAPSourcesArg(), ",")
	if !sort.StringsAreSorted(names) {
		t.Errorf("not sorted: %v", names)
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
		if external.IncludeInVWAP(n) {
			t.Errorf("%s is in the VWAP and must not be in the non-VWAP set", n)
		}
	}
	for _, want := range []string{"coingecko_daily", "poloniex_via_btc"} {
		if !set[want] {
			t.Errorf("missing %s", want)
		}
	}
	for _, venue := range []string{"kraken", "sdex", "soroswap"} {
		if set[venue] {
			t.Errorf("%s is a VWAP venue and must not be in the set", venue)
		}
	}
}

// Every rung of the closed-VWAP read binds the non-VWAP set as $3 and applies it to the latest-bucket pick as well as
// the row read; otherwise a vendor-only day becomes the served point-in-time price.
func TestClosedVWAPAtOrBefore_BindsNonVWAPSources(t *testing.T) {
	pair := testXLMUSDCPair(t)
	ts := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	bucket := ts.Truncate(time.Minute).Add(-time.Minute)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"bucket", "base_asset", "vwap", "volume"},
		rows: [][]driver.Value{{bucket, pair.Base.String(), "0.5", "100"}},
	})
	defer func() { _ = store.db.Close() }()

	if _, err := store.ClosedVWAPAtOrBefore(context.Background(), pair, ts, time.Hour); err != nil {
		t.Fatalf("ClosedVWAPAtOrBefore: %v", err)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want 1", len(conn.stmts))
	}
	stmt := conn.stmts[0]
	const pred = "NOT COALESCE(sources <@ string_to_array($3, ','), false)"
	if n := strings.Count(stmt.sql, pred); n != 4 {
		t.Errorf("predicate appears %d times, want 4 (both latest arms and both row arms)\nSQL: %s", n, stmt.sql)
	}
	if got := stmt.arg(t, 3); got != nonVWAPSourcesArg() {
		t.Errorf("$3 = %v, want the non-VWAP source set", got)
	}
}

// The raw trade window behind /v1/vwap, /v1/twap and the single-bar /v1/ohlc binds the non-VWAP set as $6 in both
// direction arms; otherwise a vendor-only month answers a window VWAP with carrier amounts as volume.
func TestTradesInRange_BindsNonVWAPSources(t *testing.T) {
	pair := testXLMUSDCPair(t)
	to := time.Now().UTC().Truncate(time.Second)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "ledger", "tx_hash", "op_index", "ts", "base_asset", "quote_asset", "base_amount", "quote_amount", "maker", "taker", "routed_via"},
	})
	defer func() { _ = store.db.Close() }()

	if _, err := store.TradesInRange(context.Background(), pair, to.Add(-time.Hour), to, 100); err != nil {
		t.Fatalf("TradesInRange: %v", err)
	}
	if len(conn.stmts) != 1 {
		t.Fatalf("issued %d statements, want 1", len(conn.stmts))
	}
	stmt := conn.stmts[0]
	const pred = "NOT (source = ANY(string_to_array($6, ',')))"
	if n := strings.Count(stmt.sql, pred); n != 2 {
		t.Errorf("predicate appears %d times, want 2 (both direction arms)\nSQL: %s", n, stmt.sql)
	}
	if got := stmt.arg(t, 6); got != nonVWAPSourcesArg() {
		t.Errorf("$6 = %v, want the non-VWAP source set", got)
	}
}
