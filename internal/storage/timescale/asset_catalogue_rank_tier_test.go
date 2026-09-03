package timescale

import (
	"strings"
	"testing"
)

// Rank-tier lockstep (#356). listingRankTierExpr is the listing's LEADING
// ORDER BY key, and like adjustedVolume24hExpr it has to appear in three
// places that must stay identical — the SELECT (as rank_tier, so the
// cursor can encode what the query ranked on), the ORDER BY, and the
// keyset WHERE. Drop it from any one of them and pagination silently
// skips or repeats rows; drop it from the ORDER BY and a flagged scam
// token climbs back above real assets on raw volume.

func TestListingRankTierExpr_ThreeCallSitesStayInStep(t *testing.T) {
	t.Parallel()
	for _, order := range []AssetsOrder{AssetsOrderVolume24hUSDDesc, AssetsOrderObservationCountDesc} {
		tier := listingRankTierExpr(order)
		sel := listAssetsBaseSelectSQL(order)
		if strings.Contains(sel, rankTierMarker) {
			t.Errorf("order %v: rendered SELECT still holds the %s marker — that is a syntax error, not a comment", order, rankTierMarker)
		}
		if !strings.Contains(sel, tier+" AS rank_tier") {
			t.Errorf("order %v: SELECT does not emit listingRankTierExpr AS rank_tier", order)
		}
		if !strings.Contains(sel, "LEFT JOIN account_directory") {
			t.Errorf("order %v: SELECT is missing the account_directory join the tier reads", order)
		}
		// Since #331 F1 the tier's price arm reads asset_price_snapshot
		// (listingPriceUSDExpr == aps.price_usd) instead of the inline
		// COALESCE chain, so the tier now depends on that join too — and
		// on its staleness floor, which is what makes "priced too long
		// ago to serve" resolve to the same tier as "never priced".
		if !strings.Contains(sel, "LEFT JOIN asset_price_snapshot") {
			t.Errorf("order %v: SELECT is missing the asset_price_snapshot join the tier reads", order)
		}
		orderBy := assetsOrderBy(order)
		if !strings.HasPrefix(orderBy, " ORDER BY "+tier+" ASC,") {
			t.Errorf("order %v: ORDER BY must LEAD with the rank tier ascending; got %q", order, orderBy)
		}
		pred := assetsCursorPredicate(order, 3)
		if !strings.HasPrefix(pred, "("+tier+" > $1::int OR ("+tier+" = $1::int AND ") {
			t.Errorf("order %v: keyset predicate must compare the rank tier FIRST; got %q", order, pred)
		}
	}
}

// The observation-count order is an ACTIVITY ranking: it demotes flagged
// issuers (a safety property that holds whatever the sort key) but must
// NOT reorder on price, which would silently change the long-standing
// default /v1/assets contract.
func TestListingRankTierExpr_PriceTierIsVolumeOrderOnly(t *testing.T) {
	t.Parallel()
	vol := listingRankTierExpr(AssetsOrderVolume24hUSDDesc)
	obs := listingRankTierExpr(AssetsOrderObservationCountDesc)
	if !strings.Contains(vol, "IS NULL THEN 1") {
		t.Error("volume order must carry the unpriced tier (1)")
	}
	if strings.Contains(obs, "THEN 1") {
		t.Errorf("observation-count order must NOT reorder on price; got %q", obs)
	}
	for _, expr := range []string{vol, obs} {
		if !strings.Contains(expr, "THEN 2") || !strings.Contains(expr, "unnest(dir.tags)") {
			t.Errorf("every order must demote directory-flagged issuers to tier 2; got %q", expr)
		}
	}
}

// The SQL tag vocabulary is generated from the ONE Go list, so "shows the
// ⚠ Flagged pill", "has its price withheld" and "is demoted in the
// ranking" cannot disagree.
func TestScamFlagTagsSQLArray_GeneratedFromTheOneList(t *testing.T) {
	t.Parallel()
	want := "ARRAY['malicious', 'unsafe', 'fraud', 'scam', 'hack', 'phishing']::text[]"
	if scamFlagTagsSQLArray != want {
		t.Fatalf("scamFlagTagsSQLArray = %q, want %q", scamFlagTagsSQLArray, want)
	}
	for _, tag := range DirectoryScamFlagTags {
		if !strings.Contains(scamFlagTagsSQLArray, "'"+tag+"'") {
			t.Errorf("tag %q from DirectoryScamFlagTags is missing from the SQL literal", tag)
		}
	}
}

func TestMustSQLTextArrayLiteral_RejectsAnythingButLowercaseWords(t *testing.T) {
	t.Parallel()
	for _, bad := range [][]string{
		nil,
		{""},
		{"Unsafe"},
		{"un safe"},
		{"un'safe"},
		{"unsafe--"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("mustSQLTextArrayLiteral(%q) must panic", bad)
				}
			}()
			_ = mustSQLTextArrayLiteral(bad)
		}()
	}
}

// EncodeAssetsCursor is the ONLY encoder; it must emit every ORDER BY key
// (tier first) and round-trip through the validator and the parsers.
func TestEncodeAssetsCursor_RoundTrip(t *testing.T) {
	t.Parallel()
	tier := 2
	vol := "62341.98422258"
	row := AssetRow{
		AssetID:          "JFKBANK2-GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK",
		ObservationCount: 1779,
		SortVolume24hUSD: &vol,
		RankTier:         &tier,
	}

	gotVol := EncodeAssetsCursor(row, AssetsOrderVolume24hUSDDesc)
	if want := "2:62341.98422258:" + row.AssetID; gotVol != want {
		t.Fatalf("volume cursor = %q, want %q", gotVol, want)
	}
	if err := ValidateAssetsCursor(gotVol, AssetsOrderVolume24hUSDDesc); err != nil {
		t.Fatalf("emitted volume cursor rejected by its own validator: %v", err)
	}
	if tr, v, id := parseVolumeCursor(gotVol); tr != 2 || v != vol || id != row.AssetID {
		t.Fatalf("parseVolumeCursor(%q) = (%d, %q, %q), want (2, %q, %q)", gotVol, tr, v, id, vol, row.AssetID)
	}

	gotObs := EncodeAssetsCursor(row, AssetsOrderObservationCountDesc)
	if want := "2:1779:" + row.AssetID; gotObs != want {
		t.Fatalf("observation cursor = %q, want %q", gotObs, want)
	}
	if err := ValidateAssetsCursor(gotObs, AssetsOrderObservationCountDesc); err != nil {
		t.Fatalf("emitted observation cursor rejected by its own validator: %v", err)
	}
	if tr, n, id := parseAssetCursor(gotObs); tr != 2 || n != 1779 || id != row.AssetID {
		t.Fatalf("parseAssetCursor(%q) = (%d, %d, %q), want (2, 1779, %q)", gotObs, tr, n, id, row.AssetID)
	}

	// A row with no rank tier (not from the listing query) encodes as
	// tier 0 rather than dropping the field and shifting the shape.
	if got := EncodeAssetsCursor(AssetRow{AssetID: "native"}, AssetsOrderVolume24hUSDDesc); got != "0::native" {
		t.Fatalf("nil-tier / nil-volume cursor = %q, want %q", got, "0::native")
	}
}

// A cursor minted before #356 has two fields. It must resume in tier 0
// rather than 400 — an in-flight pager should not break on deploy.
func TestSplitAssetsCursor_LegacyTwoFieldCursorResumesInTierZero(t *testing.T) {
	t.Parallel()
	tier, sortKey, assetID, ok := splitAssetsCursor("41106.07558771:yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55")
	if !ok {
		t.Fatal("legacy cursor must still split")
	}
	if tier != "0" || sortKey != "41106.07558771" || assetID != "yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55" {
		t.Fatalf("legacy split = (%q, %q, %q), want (0, 41106.07558771, yXLM-…)", tier, sortKey, assetID)
	}
	if err := ValidateAssetsCursor("41106.07558771:yXLM-GARD", AssetsOrderVolume24hUSDDesc); err != nil {
		t.Errorf("legacy volume cursor must stay valid: %v", err)
	}
	if err := ValidateAssetsCursor("1779:JFKBANK2-GB7K", AssetsOrderObservationCountDesc); err != nil {
		t.Errorf("legacy observation cursor must stay valid: %v", err)
	}
	// ...but a malformed rank tier in the three-field shape is a client error.
	if err := ValidateAssetsCursor("x:100:native", AssetsOrderObservationCountDesc); err == nil {
		t.Error("non-numeric rank tier must be rejected")
	}
}

// TestAssetsCursorPredicate_MixedDirectionKeysetIsSpelledOut is the class
// guard for wave-D KP-1 / RD-01.
//
// Every /v1/assets ordering is MIXED-direction: the sort key descends,
// asset_id ascends to break ties deterministically. SQL's row-constructor
// comparison — `(a, b) < ($1, $2)` — is SAME-direction on every element,
// so using it here reads as "…AND asset_id < $2" on a tie. That selects
// rows the walk has already served and skips the ones it has not: a plain
// `GET /v1/assets` walk served some assets twice, never served others, and
// then reported has_more=false as if the walk were complete.
//
// The volume arm always spelled the comparison out correctly; the
// observation-count arm used the row constructor. The bug was therefore
// not a missing abstraction — it was one arm of one function disagreeing
// with its own sibling — so the durable fix is this invariant rather than
// a shared helper (markets and pools already use the expanded form; a
// helper across them would be churn on correct code).
//
// Derived from source, so a THIRD ordering added later is covered on the
// day it is written rather than when someone notices missing rows.
//
// Proven red: restoring `(ca.observation_count, ca.asset_id) < ($2, $3)`
// fails this test naming AssetsOrderObservationCountDesc.
func TestAssetsCursorPredicate_MixedDirectionKeysetIsSpelledOut(t *testing.T) {
	t.Parallel()
	orders := allAssetsOrders()
	if len(orders) == 0 {
		t.Fatal("no AssetsOrder values found — the enumeration is broken, and a " +
			"guard with an empty subject set passes forever")
	}
	for _, order := range orders {
		orderBy := assetsOrderBy(order)
		if !strings.HasSuffix(orderBy, "ca.asset_id ASC") {
			// Not a mixed-direction tie-break — this invariant does not apply.
			continue
		}
		pred := assetsCursorPredicate(order, 3)

		// A row-constructor compare cannot express mixed directions.
		if strings.Contains(pred, ", ca.asset_id) <") || strings.Contains(pred, ", ca.asset_id) >") {
			t.Errorf("order %v: keyset predicate uses a row-constructor compare %q, but the "+
				"ORDER BY is mixed-direction (%q). A row constructor compares every element in "+
				"the SAME direction, so on a tie in the sort key this re-selects rows already "+
				"served and skips the rest — silent truncation, reported as has_more=false. "+
				"Spell the comparison out: (key < $n OR (key = $n AND ca.asset_id > $m)).",
				order, pred, orderBy)
		}

		// The tie-break half must walk asset_id FORWARD, matching ASC.
		if !strings.Contains(pred, "ca.asset_id > $") {
			t.Errorf("order %v: ORDER BY breaks ties on ca.asset_id ASC, so the keyset predicate "+
				"must resume with `ca.asset_id > $n`; got %q", order, pred)
		}
	}
}

// allAssetsOrders enumerates every AssetsOrder the package defines.
//
// Derived by walking the values rather than listing them, because the
// guard above advertises that a THIRD ordering added later is covered on
// the day it is written — and a literal two-element slice would not have
// been. AssetsOrder is a small int enum; walking upward until
// assetsOrderBy stops producing a distinct ORDER BY finds them all
// without depending on a name.
func allAssetsOrders() []AssetsOrder {
	var out []AssetsOrder
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		o := AssetsOrder(i)
		ob := assetsOrderBy(o)
		if ob == "" || seen[ob] {
			continue
		}
		seen[ob] = true
		out = append(out, o)
	}
	return out
}
