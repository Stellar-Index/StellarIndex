package timescale

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
)

func prefetchPairs(t *testing.T, n int) []canonical.Pair {
	t.Helper()
	out := make([]canonical.Pair, n)
	for i := range out {
		base, err := canonical.NewClassicAsset(fmt.Sprintf("T%d", i), "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatal(err)
		}
		p, err := canonical.NewPair(base, canonical.Asset{Type: canonical.AssetFiat, Code: "USD"})
		if err != nil {
			t.Fatal(err)
		}
		out[i] = p
	}
	return out
}

func latestCount(result string) float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues("latest_vwap_1m", result))
}

// noRecentBucket is the existence statement's answer for pairs with no recent bucket: no rows.
var noRecentBucket = scriptedResult{cols: []string{"ord", "one"}, rows: [][]driver.Value{}}

func TestPrefetchStore_AbsentAnswersAreServedWithoutAStatement(t *testing.T) {
	store, conn := newScriptedStore(t, noRecentBucket, noRecentBucket, noRecentBucket)
	store.db.SetMaxOpenConns(1)
	p := PrefetchStore{Store: store}
	ctx := readset.With(context.Background(), readset.New())
	pairs := prefetchPairs(t, 250)
	p.PrefetchLatest(ctx, pairs, 40)
	if got := len(conn.statements()); got != 3 {
		t.Fatalf("250 pairs took %d statements, want 3 of at most %d", got, prefetchChunk)
	}
	for _, s := range conn.stmts {
		if n := len(s.args[0].([]string)); n > prefetchChunk {
			t.Errorf("a statement carried %d pairs", n)
		}
	}
	hits := latestCount("hit")
	for _, pair := range pairs {
		if _, err := p.LatestClosedVWAP1mForPair(ctx, pair); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%v: err = %v, want the single read's sql.ErrNoRows", pair, err)
		}
	}
	if got := len(conn.statements()); got != 3 {
		t.Errorf("prefetched reads issued %d more statements, want none", got-3)
	}
	if d := latestCount("hit") - hits; d != 250 {
		t.Errorf("hits = %v, want 250", d)
	}
}

func TestPrefetchStore_FailedSetReadFallsThroughForEveryKey(t *testing.T) {
	boom, again := errors.New("set read failed"), errors.New("single read failed")
	store, conn := newScriptedStore(t, scriptedResult{err: boom},
		scriptedResult{err: again}, scriptedResult{err: again}, scriptedResult{err: again})
	store.db.SetMaxOpenConns(1)
	p := PrefetchStore{Store: store}
	ctx := readset.With(context.Background(), readset.New())
	pairs := prefetchPairs(t, 3)
	failed := latestCount("prefetch_error")
	p.PrefetchLatest(ctx, pairs, 40)
	if d := latestCount("prefetch_error") - failed; d != 3 {
		t.Errorf("prefetch_error = %v, want one per key (3)", d)
	}
	for _, pair := range pairs {
		// Memoised as absent, this would be sql.ErrNoRows with no statement.
		if _, err := p.LatestClosedVWAP1mForPair(ctx, pair); !errors.Is(err, again) {
			t.Errorf("%v: err = %v, want the per-key read's own error", pair, err)
		}
	}
	if got := len(conn.statements()); got != 4 {
		t.Errorf("statements = %d, want the set read and one per-key read per pair (4)", got)
	}
}

func TestPrefetchStore_NoMemoReadsAsStore(t *testing.T) {
	store, conn := newScriptedStore(t, noRecentBucket)
	p := PrefetchStore{Store: store}
	p.PrefetchLatest(context.Background(), prefetchPairs(t, 2), 40)
	if len(conn.statements()) != 0 {
		t.Fatal("a prefetch with no memo read the store")
	}
	if _, err := p.LatestClosedVWAP1mForPair(context.Background(), prefetchPairs(t, 1)[0]); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v", err)
	}
	if len(conn.statements()) != 1 {
		t.Fatal("the single read did not reach the store")
	}
}
