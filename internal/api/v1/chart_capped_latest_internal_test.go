package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestChartSeriesPoints_LatestBooksAreTheNewestServedBucket: a valuation guard measures the book under the newest SERVED
// bucket. When the row cap trims a later bucket another book owned, that book does not stand under any served price, and the
// book that does (the capped source's) is the one measured.
func TestChartSeriesPoints_LatestBooksAreTheNewestServedBucket(t *testing.T) {
	asset := mustAsset(t, "TKN-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	usd, xlm := mustAsset(t, "fiat:USD"), canonical.NativeAsset()
	t0 := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	series := func(from, n int) []HistoryPoint {
		out := make([]HistoryPoint, n)
		for i := range out {
			out[i] = HistoryPoint{Bucket: t0.Add(time.Duration(from+i) * time.Minute), VWAP: "1"}
		}
		return out
	}
	// The capped source reads historyMaxPoints+1 rows, so its cut is bucket historyMaxPoints-1; the late book owns the next one.
	capped, late := series(0, historyMaxPoints+1), series(historyMaxPoints, 1)
	s := &Server{Options: Options{VerifiedCurrencies: mustEmbeddedCatalogue(t)}}
	win := chartWindow{gran: "1m"}
	for _, tc := range []struct {
		name string
		read chartRead
		want []canonical.Pair
	}{
		{
			name: "observed",
			want: []canonical.Pair{{Base: asset, Quote: usd}},
			read: func(_ context.Context, p canonical.Pair, _, _ time.Time, _ int) ([]HistoryPoint, error) {
				switch {
				case p.Base.Equal(asset) && p.Quote.Equal(usd):
					return capped, nil
				case p.Base.Equal(asset):
					return late, nil
				}
				return nil, nil
			},
		},
		{
			name: "derived through XLM",
			want: []canonical.Pair{{Base: asset, Quote: xlm}, {Base: xlm, Quote: usd}},
			read: func(_ context.Context, p canonical.Pair, _, _ time.Time, _ int) ([]HistoryPoint, error) {
				switch {
				case p.Base.Equal(asset) && p.Quote.Equal(xlm):
					return series(historyMaxPoints-2, 3), nil
				case p.Base.Equal(xlm) && p.Quote.Equal(usd):
					return capped, nil
				case p.Base.Equal(xlm):
					return late, nil
				}
				return nil, nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			points, walk, err := s.chartSeriesPoints(context.Background(), canonical.Pair{Base: asset, Quote: usd}, win, tc.read)
			if err != nil {
				t.Fatal(err)
			}
			if len(points) == 0 {
				t.Fatal("no points served")
			}
			served := points[len(points)-1].Bucket
			if !walk.latestAt.Equal(served) {
				t.Errorf("latestAt = %v, want the newest served bucket %v", walk.latestAt, served)
			}
			if len(walk.latestBooks) != len(tc.want) {
				t.Fatalf("latestBooks = %v, want %v", walk.latestBooks, tc.want)
			}
			for i, b := range walk.latestBooks {
				if !b.Base.Equal(tc.want[i].Base) || !b.Quote.Equal(tc.want[i].Quote) {
					t.Errorf("latestBooks = %v, want %v", walk.latestBooks, tc.want)
				}
			}
		})
	}
}
