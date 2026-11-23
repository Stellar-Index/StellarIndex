package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	spanAQUA = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	spanFOO  = "FOO-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

// spanScope is a scope over rows judged at now.
func spanScope(t *testing.T, now time.Time, rows ...timescale.SourceExclusion) assetExclusionScope {
	t.Helper()
	set, v := pricingguard.NewSourceExclusionGate(globalExclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{}).
		Snapshot(context.Background())
	if v != pricingguard.NotWithheld {
		t.Fatal(v)
	}
	return assetExclusionScope{set: set, judge: !set.Empty(), now: now}
}

func spanRow(id string) AssetDetail {
	return AssetDetail{
		AssetID: id, Type: "credit_alphanum4", PriceUSD: sptrLocal("0.5"), MarketCapUSD: sptrLocal("5000"), FDVUSD: sptrLocal("6000"),
	}
}

// A series point is labelled by its bucket's start and holds a value read inside that bucket: an exclusion 08:10-08:20
// withholds the 08:00 point and serves 09:00 (judged to now) and 07:00. The headline records sources the exclusion does not
// name, so it stands.
func TestAssetRowSeries_PointJudgedOverItsOwnBucket(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 2, 0, 0, time.UTC)
	sc := spanScope(t, now, timescale.SourceExclusion{
		ID: 1, Source: "soroswap", Base: spanAQUA, Quote: "fiat:USD",
		From: time.Date(2026, 11, 20, 8, 10, 0, 0, time.UTC), Until: time.Date(2026, 11, 20, 8, 20, 0, 0, time.UTC),
	})
	d := spanRow(spanAQUA)
	d.PriceSources = []string{"sdex"}
	for _, h := range []int{7, 8, 9} {
		d.PriceHistory24h = append(d.PriceHistory24h, AssetPricePoint{
			T: time.Date(2026, 11, 20, h, 0, 0, 0, time.UTC).Format(time.RFC3339), P: sptrLocal("0." + string(rune('0'+h))),
		})
	}
	rows := []AssetDetail{d}
	sc.judgeRows(context.Background(), &Server{}, rows, Flags{})
	got := rows[0].PriceHistory24h
	if rows[0].PriceUSD == nil {
		t.Fatal("headline on unexcluded recorded sources was withheld")
	}
	if got[0].P == nil || got[1].P != nil || got[2].P == nil {
		t.Errorf("07:00 %v, 08:00 %v, 09:00 %v; want 08:00 alone withheld", got[0].P, got[1].P, got[2].P)
	}
	if d.PriceHistory24h[1].P == nil {
		t.Error("judging wrote into the caller's series")
	}
}

// The snapshot price is the newest traded minute in a 7-day lookback, not a minute from the last 15: a manipulated 10:20
// print inside a 10:00-10:30 exclusion is still the price at 11:00 when the asset has not traded since, and is withheld. An
// exclusion that ended before the whole lookback leaves it.
func TestAssetRowHeadline_JudgedOverThePriceLookback(t *testing.T) {
	now := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)
	excl := func(from, until time.Time) timescale.SourceExclusion {
		return timescale.SourceExclusion{ID: 1, Source: "soroswap", Base: spanAQUA, Quote: "fiat:USD", From: from, Until: until}
	}
	for _, tc := range []struct {
		name     string
		from     time.Time
		withheld bool
	}{
		{"window ended 30 minutes ago", now.Add(-time.Hour), true},
		{"window six days ago", now.Add(-6 * 24 * time.Hour), true},
		{"window ended before the lookback", now.Add(-assetPriceLookback - time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := spanScope(t, now, excl(tc.from, tc.from.Add(30*time.Minute)))
			rows := []AssetDetail{spanRow(spanAQUA)}
			sc.judgeRows(context.Background(), &Server{}, rows, Flags{})
			d := rows[0]
			if got := d.PriceUSD == nil && d.MarketCapUSD == nil && d.FDVUSD == nil; got != tc.withheld {
				t.Errorf("price %v cap %v fdv %v; want withheld=%v", d.PriceUSD, d.MarketCapUSD, d.FDVUSD, tc.withheld)
			}
			if tc.withheld && d.PriceWithheldReason != PriceWithheldSourceExcluded {
				t.Errorf("reason %q", d.PriceWithheldReason)
			}
		})
	}
}
