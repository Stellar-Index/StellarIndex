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
		sel := listAssetsBaseSelectSQL("", order)
		if strings.Contains(sel, rankTierMarker) {
			t.Errorf("order %v: rendered SELECT still holds the %s marker — that is a syntax error, not a comment", order, rankTierMarker)
		}
		if !strings.Contains(sel, tier+" AS rank_tier") {
			t.Errorf("order %v: SELECT does not emit listingRankTierExpr AS rank_tier", order)
		}
		if !strings.Contains(sel, "LEFT JOIN account_directory") {
			t.Errorf("order %v: SELECT is missing the account_directory join the tier reads", order)
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
