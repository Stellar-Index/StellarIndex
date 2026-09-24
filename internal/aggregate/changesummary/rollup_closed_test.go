package changesummary

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fixedSource []TimedValue

func (f fixedSource) TimedVWAPs1m(context.Context, canonical.Pair, time.Time, time.Time) ([]TimedValue, error) {
	return f, nil
}

type recordingSink struct{ rows []Row }

func (s *recordingSink) UpsertChangeSummary(_ context.Context, row Row) error {
	s.rows = append(s.rows, row)
	return nil
}

func refreshWith(t *testing.T, src fixedSource, now time.Time) (*recordingSink, error) {
	t.Helper()
	sink := &recordingSink{}
	w, err := New(src, sink, nil, slog.New(slog.DiscardHandler), Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sink, w.refreshOne(context.Background(), Entity{Type: "coin", ID: "native"}, now.Add(-time.Hour), now)
}

// TestRefreshOne_ExcludesTheOpenBucket is the GH-757 worker half: a point
// whose bucket ends after the worker's clock is the minute still filling.
// Admitted, its fat-finger 1000 became current_value and the ATH — which
// the upsert then ratchets with GREATEST for good.
func TestRefreshOne_ExcludesTheOpenBucket(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 30, 0, time.UTC)
	minute := now.Truncate(time.Minute)
	sink, err := refreshWith(t, fixedSource{
		{At: minute.Add(-2 * time.Minute), Value: "0.1"},
		{At: minute, Value: "0.11"},
		{At: minute.Add(time.Minute), Value: "1000"}, // ends 12:01, still open at 12:00:30
	}, now)
	if err != nil {
		t.Fatalf("refreshOne: %v", err)
	}
	if len(sink.rows) != 1 {
		t.Fatalf("upserted %d rows, want 1", len(sink.rows))
	}
	row := sink.rows[0]
	if row.CurrentValue != "0.11" {
		t.Errorf("current_value = %v, want 0.11 (the newest CLOSED bucket)", row.CurrentValue)
	}
	if row.ATHValue == nil {
		t.Fatal("ath_value = nil, want 0.11")
	}
	if *row.ATHValue != "0.11" {
		t.Errorf("ath_value = %v, want 0.11 — the open-minute 1000 must not become the ATH", *row.ATHValue)
	}
}

// TestRefreshOne_RefusesAnUnpricedNewestPoint pins that a newest point with
// no positive price upserts nothing: it would be current_value 0, and the
// upsert's LEAST would pin atl_value to 0 for the life of the row.
func TestRefreshOne_RefusesAnUnpricedNewestPoint(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 30, 0, time.UTC)
	minute := now.Truncate(time.Minute)
	for _, bad := range []string{"", "0", "-1", "NaN", "not-a-number"} {
		sink, err := refreshWith(t, fixedSource{
			{At: minute.Add(-2 * time.Minute), Value: "0.1"},
			{At: minute, Value: bad},
		}, now)
		if err == nil {
			t.Errorf("newest value %q: refreshOne returned nil, want an error", bad)
		}
		if len(sink.rows) != 0 {
			t.Errorf("newest value %q: upserted %+v, want nothing", bad, sink.rows[0])
		}
	}
}
