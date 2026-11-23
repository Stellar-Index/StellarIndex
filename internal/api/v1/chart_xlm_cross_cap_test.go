package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// oldestFirstHistoryReader applies from/to/limit on both reads as the store does: oldest rows first, `limit` of them.
type oldestFirstHistoryReader struct{ rangeHonouringHistoryReader }

func (r oldestFirstHistoryReader) HistoryPoints(ctx context.Context, p canonical.Pair, g string, limit int) ([]v1.HistoryPoint, error) {
	return r.HistoryPointsInRange(ctx, p, g, time.Time{}, time.Time{}, limit)
}

func hourlyVWAP(t0 time.Time, n int, vwap string) []v1.HistoryPoint {
	pts := chartRowCapSeries(t0, n, time.Hour)
	for i := range pts {
		pts[i].VWAP = vwap
	}
	return pts
}

func xlmCrossLegsReader(assetLeg, pivot []v1.HistoryPoint) oldestFirstHistoryReader {
	return oldestFirstHistoryReader{rangeHonouringHistoryReader{&pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		pegAliasUSDCSAC + "/" + canonical.XLMSacContractID: assetLeg,
		"crypto:XLM/fiat:USD":                              pivot,
	}}}}
}

func getHistorySeries(t *testing.T, opts v1.Options, query string) v1.HistorySeries {
	t.Helper()
	ts := httpTestServer(t, v1.New(opts))
	resp := mustGet(t, ts.URL+"/v1/history/since-inception?"+query)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Data v1.HistorySeries `json:"data"`
	}
	mustDecode(t, resp, &body)
	return body.Data
}

// The XLM pivot holds more history than one read returns; read oldest-first from genesis it ends before a younger asset
// leg begins, so the cross must read the pivot over the asset leg's span rather than serve an empty, unflagged series.
func TestXLMCross_SinceInception_PivotReadCoversTheAssetLeg(t *testing.T) {
	const capN = 50_000 // historyMaxPoints
	usdc := installPegAliasRegistry(t)
	t0 := time.Now().UTC().Add(-(capN + 2000) * time.Hour).Truncate(time.Hour)
	reader := xlmCrossLegsReader(hourlyVWAP(t0.Add((capN+1000)*time.Hour), 100, "5"), hourlyVWAP(t0, capN+2000, "0.2"))
	opts := v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}}

	env := getChart(t, httpTestServer(t, v1.New(opts)).URL+"/v1/chart?asset=USDC:"+pegAliasUSDCIssuer+
		"&quote=fiat:USD&timeframe=all&granularity=1h")
	if len(env.Data.Points) != 100 || env.Data.RowCapTruncated {
		t.Errorf("/v1/chart: %d points row_cap_truncated=%v, want the asset leg's 100 buckets crossed", len(env.Data.Points), env.Data.RowCapTruncated)
	}
	h := getHistorySeries(t, opts, "asset=USDC:"+pegAliasUSDCIssuer+"&quote=fiat:USD&granularity=1h")
	if len(h.Points) != 100 || h.RowCapTruncated {
		t.Errorf("since-inception: %d points row_cap_truncated=%v, want 100 uncapped", len(h.Points), h.RowCapTruncated)
	}
}

// A declared peg derived through XLM must not read its XLM/USD pivot from its own book: USDC/XLM x XLM/USDC is 1.0 by
// construction and hides a depeg. Buckets only the peg's own book prices are a miss; a direct XLM/USD bucket still crosses.
func TestXLMCross_PivotNeverReadsThePegsOwnBook(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3)
	d1, d2 := day, day.AddDate(0, 0, 1)
	reader := &pairKeyedHistoryReader{byPair: map[string][]v1.HistoryPoint{
		usdc.String() + "/native": {{Bucket: d1, VWAP: "3.3333333333"}, {Bucket: d2, VWAP: "3.3333333333"}},
		"native/" + usdc.String(): {{Bucket: d1, VWAP: "0.3"}, {Bucket: d2, VWAP: "0.3"}},
		"crypto:XLM/fiat:USD":     {{Bucket: d1, VWAP: "0.27"}},
	}}
	series := getChartOpts(t, v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}},
		"asset="+usdc.String()+"&quote=fiat:USD&timeframe=1mo&granularity=1d")
	if len(series.Points) != 1 || !strings.HasPrefix(series.Points[0].P, "0.8999") {
		t.Fatalf("points=%+v, want only the directly pivoted bucket (~0.90), never the self-pivoted 1.0", series.Points)
	}
}

// A capped leg cuts the cross short of the cap's point count, so the row-cap flag must come from the walk, not len(points).
func TestXLMCross_SinceInception_CappedLegFlagsRowCap(t *testing.T) {
	const capN = 50_000 // historyMaxPoints
	usdc := installPegAliasRegistry(t)
	t0 := time.Now().UTC().Add(-(capN + 100) * time.Hour).Truncate(time.Hour)
	asset := hourlyVWAP(t0, capN+50, "5")
	reader := xlmCrossLegsReader(asset, hourlyVWAP(t0.Add(10*time.Hour), capN+40, "0.2"))
	h := getHistorySeries(t, v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}},
		"asset=USDC:"+pegAliasUSDCIssuer+"&quote=fiat:USD&granularity=1h")
	if !h.RowCapTruncated || h.DataEndsAt == nil || !h.DataEndsAt.Time().Equal(asset[capN-1].Bucket) {
		t.Errorf("%d points row_cap_truncated=%v data_ends_at=%v, want flagged at the asset leg's cut %v",
			len(h.Points), h.RowCapTruncated, h.DataEndsAt, asset[capN-1].Bucket)
	}
}
