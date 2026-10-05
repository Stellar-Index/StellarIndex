package changesummary

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type pairSource map[string][]TimedValue

func (s pairSource) TimedVWAPs1m(_ context.Context, p canonical.Pair, _, _ time.Time) ([]TimedValue, error) {
	v, ok := s[p.String()]
	if !ok {
		return nil, errors.New("source down")
	}
	return v, nil
}

// TestRefresh_PublishesPassOutcomeAndHeartbeat: a total outage must be
// visible as a `failed` pass that leaves the heartbeat alone; any upsert
// advances it.
func TestRefresh_PublishesPassOutcomeAndHeartbeat(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	good := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetCrypto, Code: "XLM"}, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}
	bad := canonical.Pair{Base: canonical.Asset{Type: canonical.AssetCrypto, Code: "BTC"}, Quote: canonical.Asset{Type: canonical.AssetFiat, Code: "USD"}}
	src := pairSource{good.String(): {{At: now.Add(-time.Minute), Value: "0.1"}}}

	counter := func(outcome string) float64 {
		return testutil.ToFloat64(obs.ChangeSummaryPassesTotal.WithLabelValues(outcome))
	}
	run := func(ents ...Entity) {
		w, err := New(src, &recordingSink{}, ents, slog.New(slog.DiscardHandler), Options{Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		w.refresh(context.Background())
	}
	goodEnt := Entity{Type: "coin", ID: good.Base.String(), Pair: good}
	badEnt := Entity{Type: "coin", ID: bad.Base.String(), Pair: bad}

	obs.ChangeSummaryLastSuccessUnix.Set(1)
	beforeFailed := counter("failed")
	run(badEnt)
	if got := counter("failed") - beforeFailed; got != 1 {
		t.Fatalf("failed passes +%v, want +1", got)
	}
	if got := testutil.ToFloat64(obs.ChangeSummaryLastSuccessUnix); got != 1 {
		t.Fatalf("heartbeat moved to %v on a total outage", got)
	}

	beforePartial := counter("partial")
	run(goodEnt, badEnt)
	if got := counter("partial") - beforePartial; got != 1 {
		t.Fatalf("partial passes +%v, want +1", got)
	}
	if got := testutil.ToFloat64(obs.ChangeSummaryLastSuccessUnix); got != float64(now.Unix()) {
		t.Fatalf("heartbeat = %v, want %d after a partial pass", got, now.Unix())
	}

	beforeOK := counter("ok")
	run(goodEnt)
	if got := counter("ok") - beforeOK; got != 1 {
		t.Fatalf("ok passes +%v, want +1", got)
	}
}
