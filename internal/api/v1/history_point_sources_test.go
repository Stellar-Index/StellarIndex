package v1_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// A point that includes a non-VWAP venue names it on both price-series
// surfaces; an ordinary VWAP point carries no `sources` at all.
func TestHistoryPoints_NameDerivedSourcePerPoint(t *testing.T) {
	t0 := time.Unix(1_770_000_000, 0).UTC()
	reader := &stubHistoryReader{
		points: []v1.HistoryPoint{
			{Bucket: t0, VWAP: "0.0019", Sources: []string{"poloniex_via_btc"}},
			{Bucket: t0.Add(24 * time.Hour), VWAP: "0.124", Sources: []string{"binance", "kraken"}},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{History: reader}))

	for _, path := range []string{
		"/v1/history/since-inception?asset=native&quote=fiat:USD",
		"/v1/chart?asset=native&quote=fiat:USD",
	} {
		resp := mustGet(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", path, resp.StatusCode)
		}
		var env struct {
			Data struct {
				Points []v1.HistoryPointWire `json:"points"`
			} `json:"data"`
		}
		mustDecode(t, resp, &env)
		pts := env.Data.Points
		if len(pts) != 2 {
			t.Fatalf("%s: got %d points, want 2", path, len(pts))
		}
		if len(pts[0].Sources) != 1 || pts[0].Sources[0] != "poloniex_via_btc" {
			t.Errorf("%s: derived point sources = %v, want [poloniex_via_btc]", path, pts[0].Sources)
		}
		if pts[1].Sources != nil {
			t.Errorf("%s: VWAP point sources = %v, want none", path, pts[1].Sources)
		}
	}
}
