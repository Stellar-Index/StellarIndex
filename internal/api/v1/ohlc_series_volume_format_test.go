// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// seriesBarsReader serves canned CAGG-shaped series rows per stored pair.
type seriesBarsReader struct {
	stubHistoryReader
	bars map[string][]v1.OHLCSeriesBar
}

func (r *seriesBarsReader) OHLCSeries(
	_ context.Context, pair canonical.Pair, _ string, _, _ time.Time, _ int,
) ([]v1.OHLCSeriesBar, error) {
	src := r.bars[fiatParityPairKey(pair)]
	return append([]v1.OHLCSeriesBar(nil), src...), nil
}

var integerVolumeText = regexp.MustCompile(`^[0-9]+$`)

// TestOHLCSeriesVolumeFormatMatchesAcrossPaths runs one crypto-quoted
// (native CAGG pass-through) and one fiat-quoted (combined) series over the
// SAME stored bar and holds v_base/v_quote to one shape: integer
// smallest-unit text. The CAGG's quote_vol is vwap*volume, so its NUMERIC
// text carries sub-unit fractional digits; the native path used to pass that
// through verbatim while the combined path rendered v_quote at a fixed 10dp.
func TestOHLCSeriesVolumeFormatMatchesAcrossPaths(t *testing.T) {
	usdc, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	xlm, _ := canonical.ParseAsset("native")
	pair, _ := canonical.NewPair(xlm, usdc)
	t0 := scaleNormBucketStart()

	reader := &seriesBarsReader{bars: map[string][]v1.OHLCSeriesBar{
		fiatParityPairKey(pair): {{
			T: v1.WireTime(t0), O: "0.1", H: "0.1", L: "0.1", C: "0.1",
			VBase:   "10000000000",
			VQuote:  "999999999.99999999995800000000", // vwap*volume noise just under 1e9
			N:       1,
			Sources: []string{"sdex"},
		}},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader, USDPeggedClassics: []canonical.Asset{usdc}}))

	for _, quote := range []string{usdc.String(), "fiat:USD"} {
		resp := mustGet(t, ts.URL+"/v1/ohlc?base=native&quote="+quote+"&interval=1h&limit=1"+scaleNormWindow())
		if resp.StatusCode != http.StatusOK {
			body, _ := readAll(resp)
			t.Fatalf("quote=%s: status = %d, want 200: %s", quote, resp.StatusCode, body)
		}
		var env struct {
			Data v1.OHLCSeriesResponse `json:"data"`
		}
		mustDecode(t, resp, &env)
		if len(env.Data.Intervals) != 1 {
			t.Fatalf("quote=%s: %d bars, want 1", quote, len(env.Data.Intervals))
		}
		bar := env.Data.Intervals[0]
		for _, f := range []struct{ name, got, want string }{
			{"v_base", bar.VBase, "10000000000"},
			{"v_quote", bar.VQuote, "1000000000"},
		} {
			if !integerVolumeText.MatchString(f.got) || f.got != f.want {
				t.Errorf("quote=%s: %s = %q, want integer smallest-unit text %q",
					quote, f.name, f.got, f.want)
			}
		}
	}
}
