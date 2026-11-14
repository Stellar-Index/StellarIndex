package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// windowFXHistoryReader applies the inclusive [from, to] window the store applies.
type windowFXHistoryReader struct{ points []v1.FXQuotePoint }

func (s *windowFXHistoryReader) ListFXHistory(_ context.Context, _ string, from, to time.Time) ([]v1.FXQuotePoint, error) {
	var out []v1.FXQuotePoint
	for _, p := range s.points {
		if !p.Bucket.Before(from) && !p.Bucket.After(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

// TestChart_Fiat_ServesClosedDaysOnly: today's fx_quotes row is rewritten on every refresh, so no
// fiat chart serves it; a 24h window serves yesterday's closed day.
func TestChart_Fiat_ServesClosedDaysOnly(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	fx := &windowFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: today.AddDate(0, 0, -1), RateUSDText: "0.92", InverseUSDText: "1.08"},
		{Bucket: today, RateUSDText: "0.50", InverseUSDText: "2.00"},
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: &stubHistoryReader{}, FXHistory: fx}))
	for _, q := range []string{
		"asset=fiat:EUR&quote=fiat:USD&timeframe=24h",
		"asset=fiat:EUR&quote=fiat:USD&timeframe=1w",
		"asset=fiat:EUR&quote=fiat:GBP&timeframe=1w",
	} {
		resp := mustGet(t, ts.URL+"/v1/chart?"+q)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status=%d", q, resp.StatusCode)
		}
		var body struct {
			Data v1.ChartSeries `json:"data"`
		}
		mustDecode(t, resp, &body)
		if len(body.Data.Points) != 1 || !time.Time(body.Data.Points[0].T).Equal(today.AddDate(0, 0, -1)) {
			t.Errorf("%s: points = %+v, want yesterday's closed day only", q, body.Data.Points)
		}
	}
}
