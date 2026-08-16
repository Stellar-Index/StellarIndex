package timescale

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// The ?asset= filter on /v1/markets must match EVERY alias form the
// requested asset's trades keyed under (F-1340): XLM lives under
// `native`, `crypto:XLM`, and its SAC C-address depending on the venue,
// so the pre-fix scalar `p.base_asset = $5 OR p.quote_asset = $5`
// structurally omitted the crypto:XLM-keyed CEX markets from an
// ?asset=native query (and vice-versa). The fix binds $5 as the full
// alias text[] and matches with ANY-membership on each leg.
//
// bindArrayValue renders whatever $5 arg the builder bound to its
// Postgres array literal. On the pre-fix builder $5 is a bare string
// (not a driver.Valuer), which fails the assertion cleanly — that is
// the redness proof.
func bindArrayValue(t *testing.T, arg any) string {
	t.Helper()
	v, ok := arg.(driver.Valuer)
	if !ok {
		t.Fatalf("$5 asset arg is %T (%v), want a bound text[] (driver.Valuer) — "+
			"a scalar bind matches only the literal spelling and omits the alias forms", arg, arg)
	}
	dv, err := v.Value()
	if err != nil {
		t.Fatalf("$5 Value(): %v", err)
	}
	return fmt.Sprintf("%s", dv)
}

func TestBuildDistinctPairsQuery_AssetFilterUsesAnyMembership(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildDistinctPairsQuery(since, "", "native", "", 100, order)
			// Each leg must be an ANY-membership test, not a scalar `= $5`.
			for _, want := range []string{
				"p.base_asset = ANY($5)",
				"p.quote_asset = ANY($5)",
			} {
				if !strings.Contains(q, want) {
					t.Errorf("query missing %q — an aliased asset can't match its other forms:\n%s", want, q)
				}
			}
			if strings.Contains(q, "p.base_asset = $5") {
				t.Errorf("query still binds the scalar `= $5` asset predicate (pre-fix shape)")
			}
		})
	}
}

func TestBuildDistinctPairsQuery_AssetFilterBindsFullAliasSet(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	wantForms := canonical.AssetAliasStrings(native)
	if len(wantForms) < 3 {
		t.Fatalf("expected XLM to expand to >=3 alias forms, got %v", wantForms)
	}

	_, args := buildDistinctPairsQuery(since, "", "native", "", 100, MarketsOrderVolume24hDesc)
	if len(args) != 5 {
		t.Fatalf("args = %d, want 5 ($1..$5)", len(args))
	}
	bound := bindArrayValue(t, args[4])
	// Every alias form (native, crypto:XLM, the SAC C-address) must be
	// bound so ANY-membership can reach the rows keyed under each.
	for _, form := range wantForms {
		if !strings.Contains(bound, form) {
			t.Errorf("bound $5 %q missing alias form %q — that form's markets stay invisible", bound, form)
		}
	}
}

func TestBuildDistinctPairsQuery_NoAssetBindsEmptyArray(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)

	// DistinctPairsExt / SourceMarkets pass asset="" — $5 must bind an
	// EMPTY array so the cardinality($5)=0 short-circuit disables the
	// filter (an unfiltered directory scan, unchanged from pre-fix).
	_, args := buildDistinctPairsQuery(since, "", "", "", 100, MarketsOrderPair)
	bound := bindArrayValue(t, args[4])
	if strings.ContainsAny(bound, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		t.Errorf("empty-asset $5 = %q, want an empty array {}", bound)
	}
}
