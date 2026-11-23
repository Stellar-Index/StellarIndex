package main

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// An unmeasured substance verdict on a push surface is neither cleared nor a deliberate withholding:
// the notification is retried, never sent unguarded and never silently dropped.

var (
	pushThick = timescale.MarketSubstance{VolumeUSD: "250000.5", Buckets: 900, SpanSeconds: 23 * 3600}
	pushDust  = timescale.MarketSubstance{VolumeUSD: "8.57", Buckets: 1}
)

// flakySubstanceStore fails its first `fails` reads (and any read under an ended ctx), then answers sub.
type flakySubstanceStore struct {
	mu    sync.Mutex
	fails int
	reads int
	sub   timescale.MarketSubstance
}

func (s *flakySubstanceStore) read(ctx context.Context) (timescale.MarketSubstance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if err := ctx.Err(); err != nil {
		return timescale.MarketSubstance{}, err
	}
	if s.fails > 0 {
		s.fails--
		return timescale.MarketSubstance{}, errors.New("substance scan failed")
	}
	return s.sub, nil
}

func (s *flakySubstanceStore) PairMarketSubstance(ctx context.Context, _, _ []canonical.Asset, _ time.Duration) (timescale.MarketSubstance, error) {
	return s.read(ctx)
}

func (s *flakySubstanceStore) PairMarketSubstanceAt(ctx context.Context, _, _ []canonical.Asset, _ time.Time, _ time.Duration, _ timescale.HistoryGranularity) (timescale.MarketSubstance, error) {
	return s.read(ctx)
}

func substanceGates(store *flakySubstanceStore) pricingguard.Gate {
	policy := pricingguard.SubstancePolicy{
		MinVolumeUSD: new(big.Rat).SetInt64(1000), MinBuckets: 20, MinSpan: 6 * time.Hour, Window: 24 * time.Hour,
	}
	return pricingguard.Gate{Substance: pricingguard.NewSubstanceGate(store, pricingguard.SubstanceGateOptions{Policy: policy})}
}

func pushPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, quote := alertUSDAssets(t)
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func firingResult() divergence.CachedResult {
	return divergence.CachedResult{OurPrice: 0.42, OurPriceExact: "0.42", Median: 0.3, ComputedAt: time.Now()}
}

func TestDivergenceFiringHook_UnmeasuredReleasesTheLatch(t *testing.T) {
	pair := pushPair(t)
	pub := &recordingPublisher{}
	hook := divergenceFiringHook(discardLogger(), pub, substanceGates(&flakySubstanceStore{fails: 1, sub: pushThick}))
	err := hook(context.Background(), pair, firingResult())
	if !errors.Is(err, pricingguard.ErrSubstanceUnmeasured) {
		t.Fatalf("unmeasured: hook err = %v, want ErrSubstanceUnmeasured so the worker re-arms the episode", err)
	}
	if len(pub.payloads) != 0 {
		t.Fatal("unmeasured: divergence.firing was sent unguarded")
	}
	if err := hook(context.Background(), pair, firingResult()); err != nil {
		t.Fatalf("next refresh, read healthy: hook err = %v", err)
	}
	if len(pub.payloads) != 1 {
		t.Fatalf("next refresh delivered %d events, want 1", len(pub.payloads))
	}
}

func TestDivergenceFiringHook_MeasuredVerdicts(t *testing.T) {
	pair := pushPair(t)
	for _, tc := range []struct {
		name string
		sub  timescale.MarketSubstance
		want int
	}{{"measured thin is withheld", pushDust, 0}, {"measured fine is delivered", pushThick, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &recordingPublisher{}
			hook := divergenceFiringHook(discardLogger(), pub, substanceGates(&flakySubstanceStore{sub: tc.sub}))
			if err := hook(context.Background(), pair, firingResult()); err != nil {
				t.Fatalf("hook err = %v, want nil", err)
			}
			if len(pub.payloads) != tc.want {
				t.Fatalf("delivered %d, want %d", len(pub.payloads), tc.want)
			}
		})
	}
}

// A shutdown cancellation is not a withholding: the episode stays undelivered for the next run.
func TestDivergenceFiringHook_CancelledIsNotAWithholding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pub := &recordingPublisher{}
	hook := divergenceFiringHook(discardLogger(), pub, substanceGates(&flakySubstanceStore{sub: pushThick}))
	if err := hook(ctx, pushPair(t), firingResult()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: hook err = %v, want it to wrap context.Canceled", err)
	}
	if len(pub.payloads) != 0 {
		t.Fatal("cancelled: sent unguarded")
	}
}

func freezeUnmeasuredLosses() float64 {
	return testutil.ToFloat64(obs.CustomerWebhookFanoutFailuresTotal.WithLabelValues(
		string(platform.WebhookEventAnomalyFreeze), obs.FanoutFailurePriceUnmeasured))
}

func TestAnomalyFreezeHook_UnmeasuredIsRetriedThenCounted(t *testing.T) {
	pair := pushPair(t)
	for _, tc := range []struct {
		name       string
		store      *flakySubstanceStore
		cancelled  bool
		wantSent   int
		wantLosses float64
	}{
		{"one failed read is retried", &flakySubstanceStore{fails: 1, sub: pushThick}, false, 1, 0},
		{"retry outlives a cancelled tick", &flakySubstanceStore{sub: pushThick}, true, 1, 0},
		{"still unmeasured is a counted loss", &flakySubstanceStore{fails: 2, sub: pushThick}, false, 0, 1},
		{"measured thin is withheld", &flakySubstanceStore{sub: pushDust}, false, 0, 0},
		{"measured fine is delivered", &flakySubstanceStore{sub: pushThick}, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancelled {
				cancel()
			}
			defer cancel()
			before := freezeUnmeasuredLosses()
			pub := &recordingPublisher{}
			anomalyFreezeHook(discardLogger(), pub, substanceGates(tc.store))(ctx, pair.Base, pair.Quote, "0.42", anomaly.Decision{})
			if len(pub.payloads) != tc.wantSent {
				t.Fatalf("delivered %d, want %d", len(pub.payloads), tc.wantSent)
			}
			if d := freezeUnmeasuredLosses() - before; d != tc.wantLosses {
				t.Fatalf("price_unmeasured losses moved %v, want %v", d, tc.wantLosses)
			}
		})
	}
}

func TestPriceAlertReader_UnmeasuredIsAReadFailure(t *testing.T) {
	base, quote := alertUSDAssets(t)
	reader := priceAlertVWAPReader{
		store:  fakeAlertVWAPStore{latest: alertRow(0, "1.01"), trailing: steadyAlertRows(12)},
		logger: discardLogger(),
		gate:   substanceGates(&flakySubstanceStore{fails: 1, sub: pushThick}),
	}
	if _, _, ok, err := reader.LatestVWAP(context.Background(), base, quote); ok || !errors.Is(err, pricingguard.ErrSubstanceUnmeasured) {
		t.Fatalf("unmeasured: ok=%v err=%v, want a read failure, not a benign no-price", ok, err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, _, _, err := reader.LatestVWAP(expired, base, quote); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: err = %v, want it to wrap DeadlineExceeded so the worker classes a timeout", err)
	}
	if _, _, ok, err := reader.LatestVWAP(context.Background(), base, quote); !ok || err != nil {
		t.Fatalf("measured fine: ok=%v err=%v, want a served price", ok, err)
	}
	thin := reader
	thin.gate = substanceGates(&flakySubstanceStore{sub: pushDust})
	if _, _, ok, err := thin.LatestVWAP(context.Background(), base, quote); ok || err != nil {
		t.Fatalf("measured thin: ok=%v err=%v, want withheld (ok=false, nil)", ok, err)
	}
}
