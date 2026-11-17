package ingest

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeBackfillTradeStore is a fakeTradeInserter that also records the hull of each view's refresh calls.
type fakeBackfillTradeStore struct {
	fakeTradeInserter
	refreshed map[string][2]time.Time
}

func (f *fakeBackfillTradeStore) Prices1mRetentionArmed(context.Context) (bool, error) {
	return false, nil
}

func (f *fakeBackfillTradeStore) RefreshContinuousAggregate(_ context.Context, view string, from, to time.Time) error {
	if f.refreshed == nil {
		f.refreshed = map[string][2]time.Time{}
	}
	w, ok := f.refreshed[view]
	if !ok || from.Before(w[0]) {
		w[0] = from
	}
	if !ok || to.After(w[1]) {
		w[1] = to
	}
	f.refreshed[view] = w
	return nil
}

func (f *fakeBackfillTradeStore) RefreshContinuousAggregateForced(ctx context.Context, view string, from, to time.Time) error {
	return f.RefreshContinuousAggregate(ctx, view, from, to)
}

// Rows older than every refresh policy's start offset are never materialised by the policies, so the backfill must refresh every
// trades continuous aggregate over them itself and name the window it refreshed.
func TestInsertRefreshingCAGGs_RefreshesEveryViewOverBackfilledRows(t *testing.T) {
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Hour).Add(17 * time.Minute)
	a, b := tradeWithHash("a"), tradeWithHash("b")
	a.Timestamp, b.Timestamp = old, old.Add(26*time.Hour)
	store := &fakeBackfillTradeStore{}
	var log bytes.Buffer

	err := insertRefreshingCAGGs(store, &log, func(w tradeInserter) error {
		return insertBackfilledTrades(context.Background(), w, []canonical.Trade{a, b}, 0, io.Discard, time.Now())
	})
	if err != nil {
		t.Fatalf("insertRefreshingCAGGs: %v", err)
	}
	if len(store.got) != 2 {
		t.Fatalf("inserted %v, want both trades", store.got)
	}
	for _, c := range timescale.TradesCAGGs {
		w, ok := store.refreshed[c.Name]
		if !ok {
			t.Errorf("%s: not refreshed over the backfilled rows", c.Name)
			continue
		}
		if w[0].After(a.Timestamp) || !w[1].After(b.Timestamp) {
			t.Errorf("%s: refreshed [%s, %s), does not cover [%s, %s]", c.Name, w[0], w[1], a.Timestamp, b.Timestamp)
		}
	}
	if !strings.Contains(log.String(), a.Timestamp.Format(time.RFC3339)) || !strings.Contains(log.String(), b.Timestamp.Format(time.RFC3339)) {
		t.Errorf("refresh window not printed: %q", log.String())
	}
}

// Nothing offered, nothing refreshed: an empty walk must not issue a refresh over a zero window.
func TestInsertRefreshingCAGGs_NoRowsNoRefresh(t *testing.T) {
	store := &fakeBackfillTradeStore{}
	if err := insertRefreshingCAGGs(store, io.Discard, func(tradeInserter) error { return nil }); err != nil {
		t.Fatalf("insertRefreshingCAGGs: %v", err)
	}
	if len(store.refreshed) != 0 {
		t.Fatalf("refreshed %v with no rows written", store.refreshed)
	}
}
