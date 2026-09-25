package timescale

import (
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Both /v1/pools orderings must read the SAME canonical CTE.
//
// The pair-ordered tail used to select `FROM pools` — the pre-collapse
// CTE — while the volume-desc tail selected `FROM canon`. So the two
// orderings disagreed about what a pool is: `?order_by=pair` returned
// both orientations of every two-sided market as separate rows, each
// carrying only its own direction's vol_24h_usd and count_24h rather
// than the summed pair, with last_price un-inverted on the flipped
// side. Measured on r1 2026-08-03: 61 duplicate both-orientation pairs
// in a 200-row page versus 0 on the default ordering, and both variants
// cache under distinct keys so the disagreement was durable.
func TestBuildPoolsQuery_BothOrderingsSelectFromCanon(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)

			// The final SELECT — everything after the last CTE — must
			// read `canon`. Checking the suffix rather than the whole
			// string, since `canon` itself legitimately selects FROM
			// pools.
			tail := q[strings.LastIndex(q, "SELECT source, base_asset"):]
			if !strings.Contains(tail, "FROM canon") {
				t.Errorf("%s ordering does not select FROM canon — it returns both "+
					"orientations of each market with un-summed volumes:\n%s", name, tail)
			}
			if strings.Contains(tail, "FROM pools") {
				t.Errorf("%s ordering selects the pre-collapse CTE:\n%s", name, tail)
			}
		})
	}
}

// /v1/pools filters run on the raw pools_per_source_1h rows, which hold
// both stored orientations of a market and every alias spelling of an
// asset. A scalar `p.base_asset = $5` bind kept only one direction and
// one spelling: the pair page dropped the reversed-orientation half of
// SDEX volume, and ?asset=native never saw a SAC-keyed Soroban pool.
func TestBuildPoolsQuery_FiltersBindAliasSets(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	xlmForms := canonical.AssetAliasStrings(native)
	if len(xlmForms) < 3 {
		t.Fatalf("expected XLM to expand to >=3 alias forms, got %v", xlmForms)
	}

	_, args := buildPoolsQuery(since, PoolsFilter{Base: "native", Quote: usdc}, "", 100, MarketsOrderVolume24hDesc)
	if len(args) != 7 {
		t.Fatalf("args = %d, want 7 ($1..$7)", len(args))
	}
	for i, want := range map[int][]string{4: xlmForms, 5: {usdc}, 6: {}} {
		got, ok := args[i].([]string)
		if !ok {
			t.Fatalf("$%d is %T (%v), want a text[] of alias forms — a scalar matches one spelling only", i+1, args[i], args[i])
		}
		if got == nil {
			t.Errorf("$%d is a nil slice: it binds SQL NULL, and cardinality(NULL) never disables the predicate", i+1)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("$%d = %v, want %v", i+1, got, want)
		}
	}

	_, args = buildPoolsQuery(since, PoolsFilter{Asset: "native"}, "", 100, MarketsOrderPair)
	if got, ok := args[6].([]string); !ok || strings.Join(got, ",") != strings.Join(xlmForms, ",") {
		t.Errorf("?asset=native $7 = %#v, want the XLM alias set %v", args[6], xlmForms)
	}
}

func TestBuildPoolsQuery_PairFilterAdmitsBothOrientations(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume desc": MarketsOrderVolume24hDesc,
		"pair":        MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _ := buildPoolsQuery(since, PoolsFilter{Base: "native", Quote: "fiat:USD"}, "", 100, order)
			// The filter precedes the fold, so it must admit the row
			// stored quote-first for `canon` to sum both directions.
			pools := q[:strings.Index(q, "canon AS")]
			for _, want := range []string{
				"(p.base_asset = ANY($5) AND p.quote_asset = ANY($6))",
				"(p.base_asset = ANY($6) AND p.quote_asset = ANY($5))",
				"p.base_asset = ANY($7) OR p.quote_asset = ANY($7)",
			} {
				if !strings.Contains(pools, want) {
					t.Errorf("pools CTE missing %q:\n%s", want, pools)
				}
			}
			for _, scalar := range []string{"p.base_asset = $5", "p.quote_asset = $6", "p.base_asset = $7"} {
				if strings.Contains(q, scalar) {
					t.Errorf("query still carries the scalar predicate %q", scalar)
				}
			}
		})
	}
}
