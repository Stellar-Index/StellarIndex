package pricealerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// stallingPrices blocks the VWAP read for one base asset until the
// caller's context ends — a never-traded pair whose probe walks evicted
// chunks — and answers every other pair at once.
type stallingPrices struct {
	stallBase string
	price     string
}

func (p stallingPrices) LatestVWAP(ctx context.Context, base, _ canonical.Asset) (string, time.Time, bool, error) {
	if base.String() == p.stallBase {
		<-ctx.Done()
		return "", time.Time{}, false, ctx.Err()
	}
	return p.price, time.Date(2026, 7, 5, 11, 59, 0, 0, time.UTC), true, nil
}

func evaluated(outcome string) float64 {
	return testutil.ToFloat64(obs.PriceAlertEvaluatedTotal.WithLabelValues(outcome))
}

// GH-749 (2): the sweep is serial on the process context, so one alert
// whose price read never returns held every later alert — every other
// tenant's — for up to the 30 m background statement timeout, and no
// sweep outcome was emitted meanwhile. Each alert now has its own
// deadline; the stalled one is recorded as a timeout and the next alert
// still fires within the same sweep.
func TestSweep_StalledAlertTimesOutAndOthersStillFire(t *testing.T) {
	acct := uuid.New()
	stalled := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "crypto:BTC", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "1", Enabled: true,
	}
	healthy := platform.PriceAlert{
		ID: uuid.New(), AccountID: acct,
		BaseAsset: "native", QuoteAsset: "fiat:USD",
		Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
	}
	alerts := &fakeAlertStore{enabled: []platform.PriceAlert{stalled, healthy}}
	hooks := &fakeWebhooks{byAcct: map[uuid.UUID][]platform.CustomerWebhook{
		acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
	}}
	w := New(alerts, hooks, stallingPrices{stallBase: "crypto:BTC", price: "0.20"}, Options{
		Interval: 300 * time.Millisecond,
		Logger:   quietLogger(),
	})

	timeoutBefore, firedBefore := evaluated(outcomeTimeout), evaluated(outcomeFired)
	done := make(chan struct{})
	go func() {
		w.Sweep(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep still blocked on the stalled alert after 5s: no per-alert deadline")
	}

	if len(hooks.enqueued) != 1 {
		t.Errorf("want the healthy alert's delivery enqueued, got %d", len(hooks.enqueued))
	}
	if got := evaluated(outcomeTimeout) - timeoutBefore; got != 1 {
		t.Errorf("evaluated_total{timeout} moved by %v, want 1", got)
	}
	if got := evaluated(outcomeFired) - firedBefore; got != 1 {
		t.Errorf("evaluated_total{fired} moved by %v, want 1", got)
	}
}

// GH-749 (1): a sweep where every alert fails and one where a single alert
// fails both emit one partial_error sample. The per-alert counter must
// tell them apart, and the last-sweep gauge must advance either way.
func TestSweep_CountsEveryFailingAlert(t *testing.T) {
	acct := uuid.New()
	var enabled []platform.PriceAlert
	for range 3 {
		enabled = append(enabled, platform.PriceAlert{
			ID: uuid.New(), AccountID: acct,
			BaseAsset: "native", QuoteAsset: "fiat:USD",
			Condition: platform.AlertAbove, Threshold: "0.15", Enabled: true,
		})
	}
	alerts := &fakeAlertStore{enabled: enabled}
	hooks := &fakeWebhooks{
		byAcct: map[uuid.UUID][]platform.CustomerWebhook{
			acct: {priceAlertWebhook(acct, true, string(platform.WebhookEventPriceAlert))},
		},
		enqErr: errors.New("permission denied for table webhook_deliveries"),
	}
	obs.PriceAlertLastSweepUnix.Set(0)
	errBefore := evaluated(outcomeError)

	w := buildWorker(alerts, hooks, fakePrices{price: "0.20", bucket: time.Date(2026, 7, 5, 11, 59, 0, 0, time.UTC), ok: true})
	w.Sweep(context.Background())

	if got := evaluated(outcomeError) - errBefore; got != 3 {
		t.Errorf("evaluated_total{error} moved by %v, want 3 — one per failing alert", got)
	}
	if got, want := testutil.ToFloat64(obs.PriceAlertLastSweepUnix), float64(w.now().Unix()); got != want {
		t.Errorf("last_sweep_unix = %v, want %v", got, want)
	}
}
