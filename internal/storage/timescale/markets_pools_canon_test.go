package timescale

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Both /v1/pools orderings must read the same canonical CTE; otherwise `order_by=pair` returns both
// orientations of a market as separate rows with partial volumes, and the disagreement is cached.
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

			// The final SELECT (after the last CTE) must read `canon`. Check the suffix, not the whole string, since
			// `canon` itself legitimately selects FROM pools.
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

// Grouping the hourly rows on the COALESCEd alias fold spilled a 218 MB sort on r1 (2.6 s per sdex page); both per-venue readers must
// aggregate on the stored columns first. last() with a ROW comparator fails as a parallel partial aggregate, so neither stage may use it.
func TestPerSourcePools_AggregateBeforeFold(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	pools, _ := buildPoolsQuery(since, PoolsFilter{Sources: []string{"sdex"}}, "", 100, MarketsOrderVolume24hDesc)
	venue, _ := buildSourceMarketsQuery(since, "sdex", "", 100, MarketsOrderVolume24hDesc)
	for name, q := range map[string]string{"pools": pools, "source markets": venue} {
		raw := q[strings.Index(q, "pools_raw AS ("):strings.Index(q, "pools AS (")]
		if !strings.Contains(raw, "FROM pools_per_source_1h r") || !strings.Contains(raw, "GROUP BY r.source, r.base_asset, r.quote_asset") {
			t.Errorf("%s: pools_raw does not aggregate the stored columns:\n%s", name, raw)
		}
		if strings.Contains(raw, "alias_fold") {
			t.Errorf("%s: pools_raw folds aliases before grouping:\n%s", name, raw)
		}
		if !strings.Contains(raw, "r.source = ") {
			t.Errorf("%s: the source filter does not reach the raw scan:\n%s", name, raw)
		}
		if strings.Contains(q, ", ROW(") {
			t.Errorf("%s: last() with a ROW comparator cannot run as a parallel partial aggregate", name)
		}
	}
}

// /v1/pools filters run on raw pools_per_source_1h rows, which hold both orientations and every alias
// spelling. A scalar `p.base_asset = $5` keeps one of each: the pair page would drop reversed SDEX volume and
// ?asset=native would miss SAC-keyed pools.
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
	if len(args) != 10 {
		t.Fatalf("args = %d, want 10 ($1..$7 + the $8/$9 alias fold + the $10 native SAC)", len(args))
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

func TestBuildPoolsQuery_UnsetSourcesBindsEmptyArray(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for name, order := range map[string]MarketsOrder{
		"volume_24h_desc": MarketsOrderVolume24hDesc,
		"pair":            MarketsOrderPair,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, args := buildPoolsQuery(since, PoolsFilter{}, "", 100, order)
			got, ok := args[3].([]string)
			if !ok {
				t.Fatalf("$4 is %T (%v), want []string", args[3], args[3])
			}
			if got == nil || len(got) != 0 {
				t.Errorf("$4 = %#v, want []string{}: a nil slice binds SQL NULL and PoolsFilter{} returns no rows", got)
			}
		})
	}
}

// The raw prefilter is exact only if every SAC spelling that folds into an asset is in that asset's alias bind.
func TestBuildPoolsQuery_RawPrefilterCoversFoldedSpellings(t *testing.T) {
	t.Parallel()
	forms, canons := sacAliasFoldBind()
	if len(forms) == 0 {
		t.Fatal("no SAC folds registered: the check below would pass vacuously")
	}
	for i, f := range forms {
		if !slices.Contains(assetAliasBind(canons[i]), f) {
			t.Errorf("%s folds to %s but is missing from assetAliasBind(%s): the prefilter would drop its rows", f, canons[i], canons[i])
		}
	}
	q, _ := buildPoolsQuery(time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC), PoolsFilter{}, "", 100, MarketsOrderPair)
	pre, post := strings.Index(q, "r.base_asset = ANY($7)"), strings.Index(q, ") p")
	if pre < 0 || post < 0 || pre > post {
		t.Errorf("raw prefilter must sit inside the fold subquery (prefilter at %d, subquery end at %d)", pre, post)
	}
}
