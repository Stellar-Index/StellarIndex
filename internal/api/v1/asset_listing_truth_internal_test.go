package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// truthAssetsReader answers only the reads these tests drive; any other method panics on the nil embedded interface.
type truthAssetsReader struct {
	AssetsReader
	byID       map[string]timescale.AssetRow
	byIDErr    error
	observedAt time.Time
	cacheStale bool
}

func (t *truthAssetsReader) GetAssetByAssetID(_ context.Context, id string) (timescale.AssetRow, error) {
	if t.byIDErr != nil {
		return timescale.AssetRow{}, t.byIDErr
	}
	row, ok := t.byID[id]
	if !ok {
		return timescale.AssetRow{}, sql.ErrNoRows
	}
	return row, nil
}

func (t *truthAssetsReader) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return nil, nil
}

func (t *truthAssetsReader) ListAssetsExtAt(
	context.Context, timescale.ListAssetsOptions,
) ([]timescale.AssetRow, time.Time, bool, error) {
	return nil, t.observedAt, t.cacheStale, nil
}

// TestClassicUnifiedPage_CarriesCacheFreshness: rows served past the cache TTL are flagged and dated by their read, not by now.
func TestClassicUnifiedPage_CarriesCacheFreshness(t *testing.T) {
	observed := time.Now().Add(-40 * time.Minute).UTC().Truncate(time.Second)
	s := &Server{AssetsReader: &truthAssetsReader{observedAt: observed, cacheStale: true}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all", nil)
	s.serveClassicUnifiedPage(rec, req, assetListFilters{}, 10, "")
	var got struct {
		AsOf  time.Time `json:"as_of"`
		Flags Flags     `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !got.Flags.Stale || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("stale = %v, Cache-Control = %q; want stale and no-store for rows past the cache TTL",
			got.Flags.Stale, rec.Header().Get("Cache-Control"))
	}
	if !got.AsOf.Equal(observed) {
		t.Errorf("as_of = %s, want the rows' observation time %s", got.AsOf, observed)
	}
}

// TestAssetListCost_ChargesSparklines: each requested series is a batch prices read the page cache does not share.
func TestAssetListCost_ChargesSparklines(t *testing.T) {
	cases := []struct {
		class, include string
		want           int
	}{
		{"", "", 1},
		{"", "sparkline7d", 1 + assetListSurchargeSparkline},
		{"", "sparkline24h,sparkline7d", 1 + 2*assetListSurchargeSparkline},
		{"stablecoin", "sparkline24h", 1 + assetListSurchargeSparkline},
		{"all", "sparkline7d", 1 + assetListSurchargeVolumePlan + assetListSurchargeSparkline},
	}
	for _, c := range cases {
		if got := assetListCost(assetListFilters{}, timescale.AssetsOrderObservationCountDesc, c.class, c.include, true); got != c.want {
			t.Errorf("class=%q include=%q cost = %d, want %d", c.class, c.include, got, c.want)
		}
	}
	if got := assetListCost(assetListFilters{}, timescale.AssetsOrderObservationCountDesc, "", "sparkline7d", false); got != 1 {
		t.Errorf("no store: cost = %d, want 1", got)
	}
}

// TestMergeContractArmVolume_NativeAndUnreadArm: XLM's SAC leg joins its volume, and an arm that failed to read marks a floor.
func TestMergeContractArmVolume_NativeAndUnreadArm(t *testing.T) {
	var sac string
	for _, a := range canonical.AssetAliases(canonical.Asset{Type: canonical.AssetNative}) {
		if a.Type == canonical.AssetSoroban {
			sac = a.String()
		}
	}
	if sac == "" {
		t.Fatal("native has no SAC alias")
	}
	vol := "250"
	reader := &truthAssetsReader{byID: map[string]timescale.AssetRow{sac: {AssetID: sac, Volume24hUSD: &vol}}}
	s := &Server{AssetsReader: reader}

	base := "1000"
	dst := AssetDetail{AssetID: "native", VolumeUSD24h: &base}
	s.mergeContractArmVolume(context.Background(), &dst, "native")
	if dst.VolumeUSD24h == nil || *dst.VolumeUSD24h != "1250" || dst.VolumeLowerBound {
		t.Errorf("native volume = %v lower_bound=%v, want 1250 exact", dst.VolumeUSD24h, dst.VolumeLowerBound)
	}

	reader.byIDErr = errors.New("pool timeout")
	dst = AssetDetail{AssetID: "native", VolumeUSD24h: &base}
	s.mergeContractArmVolume(context.Background(), &dst, "native")
	if *dst.VolumeUSD24h != "1000" || !dst.VolumeLowerBound {
		t.Errorf("unread arm: volume = %s lower_bound=%v, want 1000 flagged a floor", *dst.VolumeUSD24h, dst.VolumeLowerBound)
	}

	reader.byIDErr, reader.byID = nil, nil
	dst = AssetDetail{AssetID: "native", VolumeUSD24h: &base}
	s.mergeContractArmVolume(context.Background(), &dst, "native")
	if dst.VolumeLowerBound {
		t.Error("an untraded wrapper (no row) must not mark the volume a floor")
	}
}

// TestListingValuation_FloorSupplyIsALowerBound: a valuation built on a trustline sum says it is a minimum.
func TestListingValuation_FloorSupplyIsALowerBound(t *testing.T) {
	s := &Server{}
	mk := func() *AssetDetail {
		circ, b := "64690000000", string(supply.BasisClassicTrustlineSum)
		return &AssetDetail{
			Decimals: 7, CirculatingSupply: &circ, SupplyBasis: &b,
			ListingReference: &AssetListingReference{PriceUSD: "1.0000000000"},
		}
	}
	floor := mk()
	s.publishListingValuation(floor, "")
	if floor.ListingValuation == nil || floor.ListingValuation.Status != ListingValuationPublished ||
		!floor.ListingValuation.ValueLowerBound {
		t.Errorf("trustline floor: valuation = %+v, want published with value_lower_bound", floor.ListingValuation)
	}
	full := mk()
	s.publishListingValuation(full, "25810528958550")
	if full.ListingValuation == nil || full.ListingValuation.ValueLowerBound {
		t.Errorf("lake total: valuation = %+v, want no lower-bound mark", full.ListingValuation)
	}
}

// TestFillMarketCaps_ConfirmedDecimalsOnUnpricedRow: an unpriced row still publishes the token's confirmed scale.
func TestFillMarketCaps_ConfirmedDecimalsOnUnpricedRow(t *testing.T) {
	const id = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	s := &Server{NonstandardDecimals: &NonstandardDecimalsCache{
		assets: map[string]timescale.NonstandardDecimalsAsset{id: {Asset: id, Decimals: 18}},
	}}
	rows := []AssetDetail{{AssetID: id, Type: string(canonical.AssetSoroban), Decimals: 7}}
	s.fillMarketCapsFromSupply(context.Background(), rows, nil)
	if rows[0].Decimals != 18 {
		t.Errorf("decimals = %d, want the confirmed 18", rows[0].Decimals)
	}
}
