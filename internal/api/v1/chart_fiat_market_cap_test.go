package v1_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// windowedFXHistory answers only the days inside the asked window, as the store does.
type windowedFXHistory struct{ points []v1.FXQuotePoint }

func (s *windowedFXHistory) ListFXHistory(_ context.Context, _ string, from, to time.Time) ([]v1.FXQuotePoint, error) {
	var out []v1.FXQuotePoint
	for _, p := range s.points {
		if !p.Bucket.Before(from) && !p.Bucket.After(to) {
			out = append(out, p)
		}
	}
	return out, nil
}

// TestChart_FiatMarketCap_HasNoInventedHistory: the catalogue holds one current M2 figure, so only
// recent days are valued with it and the response says where the series starts.
func TestChart_FiatMarketCap_HasNoInventedHistory(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	day := func(back int) time.Time { return now.Truncate(24*time.Hour).AddDate(0, 0, -back) }
	fx := &windowedFXHistory{points: []v1.FXQuotePoint{
		{Bucket: day(5000), RateUSDText: "1.5", InverseUSDText: "0.66666666666666666667"},
		{Bucket: day(300), RateUSDText: "30", InverseUSDText: "0.03333333333333333333"},
		{Bucket: day(1), RateUSDText: "40", InverseUSDText: "0.025"},
	}}
	opts := v1.Options{
		History: &stubHistoryReader{}, FXHistory: fx, VerifiedCurrencies: newTestCatalogue(t),
		Clock: func() time.Time { return now },
	}
	for _, tf := range []string{"all", "1y"} {
		got := getChartOpts(t, opts, "asset=fiat:TRY&quote=fiat:USD&price_type=market_cap&granularity=1d&timeframe="+tf)
		if len(got.Points) != 1 || !got.Points[0].T.Time().Equal(day(1)) {
			t.Fatalf("timeframe=%s: points = %+v, want only yesterday's", tf, got.Points)
		}
		if !got.Truncated || got.DataStartsAt == nil || !got.DataStartsAt.Time().Equal(day(1)) {
			t.Errorf("timeframe=%s: truncated=%v data_starts_at=%v, want the series start named", tf, got.Truncated, got.DataStartsAt)
		}
	}

	// No rate in the days read: an empty series must still say older days were not read.
	fx.points = fx.points[:2]
	got := getChartOpts(t, opts, "asset=fiat:TRY&quote=fiat:USD&price_type=market_cap&granularity=1d&timeframe=all")
	if len(got.Points) != 0 || !got.Truncated {
		t.Errorf("stale rates: points=%d truncated=%v, want an empty series marked truncated", len(got.Points), got.Truncated)
	}
}
