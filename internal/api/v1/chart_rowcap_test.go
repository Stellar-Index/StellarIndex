package v1_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// limitStore is the fixture store with a reader that honours the row
// limit the way the CAGG reader does: it keeps the OLDEST `limit` buckets.
type limitStore struct{ *chartOHLCStore }

func (s limitStore) HistoryPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	pts, err := s.chartOHLCStore.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
	if limit > 0 && len(pts) > limit {
		pts = pts[:limit]
	}
	return pts, err
}

func (s limitStore) TWAPPointsInRange(
	ctx context.Context, pair canonical.Pair, gran string, from, to time.Time, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, from, to, limit)
}

func (s limitStore) HistoryPoints(
	ctx context.Context, pair canonical.Pair, gran string, limit int,
) ([]v1.HistoryPoint, error) {
	return s.HistoryPointsInRange(ctx, pair, gran, time.Time{}, time.Time{}, limit)
}

func minuteRun(start time.Time, n int) map[time.Time]string {
	m := make(map[time.Time]string, n)
	for i := 0; i < n; i++ {
		m[start.Add(time.Duration(i)*time.Minute)] = "1.0000000000"
	}
	return m
}

func rowCapServer(t *testing.T, byPair map[string]map[time.Time]string) *testServer {
	t.Helper()
	usdc := installUSDCSACRegistry(t)
	return httpTestServer(t, v1.New(v1.Options{
		History:           limitStore{newChartOHLCStore(byPair)},
		USDPeggedClassics: []canonical.Asset{usdc},
	}))
}

// A merge of uncapped alias reads can exceed the response cap and still be
// complete and current; it must not be flagged.
func TestChart_RowCap_UncappedMergeIsNotFlagged(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Minute).Add(-100 * time.Hour)
	ts := rowCapServer(t, map[string]map[time.Time]string{
		"native/fiat:USD":     minuteRun(start, 30_000),
		"crypto:XLM/fiat:USD": minuteRun(start.Add(30_000*time.Minute), 25_000),
	})
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1m")
	if got := len(env.Data.Points); got < 50_000 {
		t.Fatalf("points = %d, want >= 50000 for the merge", got)
	}
	if env.Data.RowCapTruncated || env.Data.DataEndsAt != nil {
		t.Errorf("row_cap_truncated=%v data_ends_at=%v on a complete merge of uncapped reads",
			env.Data.RowCapTruncated, env.Data.DataEndsAt)
	}
}

// A capped leg of the XLM cross leaves an intersection under the cap that
// still stops far from the present; the response must say so.
func TestChart_RowCap_CappedCrossLegIsFlagged(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Minute).Add(-100 * 24 * time.Hour)
	ts := rowCapServer(t, map[string]map[time.Time]string{
		pegAliasUSDCClassic + "/native": minuteRun(start, 55_000),
		"crypto:XLM/fiat:USD":           minuteRun(start.Add(25_000*time.Minute), 55_000),
	})
	env := getChart(t, ts.URL+"/v1/chart?asset="+pegAliasUSDCClassic+"&quote=fiat:USD&timeframe=all&granularity=1m")
	n := len(env.Data.Points)
	if n == 0 || n >= 50_000 {
		t.Fatalf("points = %d, want a non-empty cross under the cap", n)
	}
	if !env.Data.RowCapTruncated || env.Data.DataEndsAt == nil {
		t.Fatalf("row_cap_truncated=%v data_ends_at=%v, want true + set", env.Data.RowCapTruncated, env.Data.DataEndsAt)
	}
	if want := start.Add(49_999 * time.Minute); !time.Time(*env.Data.DataEndsAt).Equal(want) {
		t.Errorf("data_ends_at = %v, want %v (earliest capped leg end)", env.Data.DataEndsAt, want)
	}
}

// DataEndsAt means "complete up to here": a capped read plus a recent
// uncapped alias reaches the present, yet the flag and cut-off must stay.
func TestChart_RowCap_CappedReadWithCurrentAliasKeepsCompleteUpTo(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-100 * 24 * time.Hour)
	ts := rowCapServer(t, map[string]map[time.Time]string{
		"native/fiat:USD":     minuteRun(start, 55_000),
		"crypto:XLM/fiat:USD": minuteRun(now.Add(-99*time.Minute), 100),
	})
	env := getChart(t, ts.URL+"/v1/chart?asset=native&quote=fiat:USD&timeframe=all&granularity=1m")
	pts := env.Data.Points
	if len(pts) <= 50_000 {
		t.Fatalf("points = %d, want > 50000 (capped read + alias)", len(pts))
	}
	if !env.Data.RowCapTruncated || env.Data.DataEndsAt == nil {
		t.Fatalf("row_cap_truncated=%v data_ends_at=%v, want true + set", env.Data.RowCapTruncated, env.Data.DataEndsAt)
	}
	want := start.Add(49_999 * time.Minute)
	if got := time.Time(*env.Data.DataEndsAt); !got.Equal(want) {
		t.Errorf("data_ends_at = %v, want %v (capped read's last bucket)", got, want)
	}
	if last := time.Time(pts[len(pts)-1].T); !last.After(want) {
		t.Errorf("last point %v not after data_ends_at; fixture does not reach the present", last)
	}
}
