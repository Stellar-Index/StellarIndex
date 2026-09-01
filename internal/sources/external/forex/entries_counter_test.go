package forex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// This worker writes fx_quotes directly and never passes through
// internal/pipeline's sink — the only other place SourceEventsTotal is
// incremented. So `massive` had NO series in that metric at all, while
// writing 116 rows a day on r1 and anchoring every non-USD price on the
// platform (per-trade usd_volume and the ADR-0051 local-currency
// derivation both hang off it).
//
// The status page's per-source "entries (24h)" column reads exactly
// that counter, so the most load-bearing off-chain feed we run rendered
// as `0` — indistinguishable from a dead connector, next to sources
// that genuinely were dead.
//
// These tests share the process-global counter, so they run
// sequentially (no t.Parallel) like the liveness suite beside them.

func entriesFor(source string) float64 {
	return testutil.ToFloat64(obs.SourceEventsTotal.WithLabelValues(source))
}

// TestPersistSnapshot_countsRowsAsSourceEntries is the headline: a
// committed non-empty write must advance the universal per-source entry
// counter by the number of rows written.
func TestPersistSnapshot_countsRowsAsSourceEntries(t *testing.T) {
	obs.SourceEventsTotal.Reset()

	w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
	snap := &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies: []Currency{
			{Ticker: "EUR", RateUSD: 1.08},
			{Ticker: "BRL", RateUSD: 5.18},
			{Ticker: "JPY", RateUSD: 147.2},
		},
		History7d: map[string][]HistoryPoint{},
	}

	w.persistSnapshot(context.Background(), snap)

	got := entriesFor("massive")
	if got == 0 {
		t.Fatal("massive recorded 0 entries after a committed write — this is the " +
			"defect: the status page's entries column reads this counter, so the " +
			"active FX feed displayed as dead")
	}
	if got != 3 {
		t.Errorf("entries = %v, want 3 (one per row written) — the count must be the "+
			"number of DATA POINTS recorded, not one per batch", got)
	}
}

// TestPersistSnapshot_doesNotCountFailedOrEmptyWrites is the honesty
// half, and it matters more than the headline. A counter that advanced
// on a failed insert would make a wedged feed look productive — the
// exact failure the liveness gauge beside it is documented to avoid.
func TestPersistSnapshot_doesNotCountFailedOrEmptyWrites(t *testing.T) {
	t.Run("failed insert", func(t *testing.T) {
		obs.SourceEventsTotal.Reset()
		w := &Worker{
			writer: &fakeFXWriter{err: errors.New("db down")},
			logger: discardLogger(),
		}
		w.persistSnapshot(context.Background(), &Snapshot{
			PublishedAt: time.Now().UTC(),
			Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
			History7d:   map[string][]HistoryPoint{},
		})
		if got := entriesFor("massive"); got != 0 {
			t.Errorf("entries = %v after a FAILED insert, want 0 — counting rows we "+
				"did not persist reports a broken feed as healthy", got)
		}
	})

	t.Run("empty batch", func(t *testing.T) {
		obs.SourceEventsTotal.Reset()
		w := &Worker{writer: &fakeFXWriter{}, logger: discardLogger()}
		w.persistSnapshot(context.Background(), &Snapshot{
			PublishedAt: time.Now().UTC(),
			Currencies:  []Currency{},
			History7d:   map[string][]HistoryPoint{},
		})
		if got := entriesFor("massive"); got != 0 {
			t.Errorf("entries = %v for an EMPTY batch, want 0 — an upstream that "+
				"returned no usable rates has produced no data points", got)
		}
	})
}

// TestPersistSnapshot_attributesEntriesToTheProviderThatAnswered — when
// a fallback serves the round, its rows belong to the fallback, not to
// massive. Attributing them to the primary would report a feed as
// healthy while it was in fact down, which is the same class of lie as
// counting a failed write.
func TestPersistSnapshot_attributesEntriesToTheProviderThatAnswered(t *testing.T) {
	obs.SourceEventsTotal.Reset()

	w := &Worker{
		writer:       &fakeFXWriter{},
		logger:       discardLogger(),
		activeSource: "exchangeratesapi",
	}
	w.persistSnapshot(context.Background(), &Snapshot{
		PublishedAt: time.Now().UTC(),
		Currencies:  []Currency{{Ticker: "EUR", RateUSD: 1.08}},
		History7d:   map[string][]HistoryPoint{},
	})

	if got := entriesFor("exchangeratesapi"); got != 1 {
		t.Errorf("entries{exchangeratesapi} = %v, want 1 — a fallback's rows must be "+
			"attributed to the provider that actually answered", got)
	}
	if got := entriesFor("massive"); got != 0 {
		t.Errorf("entries{massive} = %v, want 0 — attributing a fallback's rows to "+
			"the primary reports a down feed as healthy", got)
	}
}
