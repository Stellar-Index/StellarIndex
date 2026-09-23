package redispub_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// subscribeOutcomes is every outcome label the subscriber can emit. A
// responder reading `malformed` must not be looking at a clock-skew drop.
var subscribeOutcomes = []string{"ok", "decode_error", "malformed", "future_observed_at", "stale_observed_at"}

// TestNewSubscriber_SeedsEveryOutcome pins that a wired subscriber
// exports every outcome at zero before its first message, so a rule can
// read "no ok events" as a real zero rather than an absent series.
func TestNewSubscriber_SeedsEveryOutcome(t *testing.T) {
	_, rdb := newRedis(t)
	if _, err := redispub.NewSubscriber(rdb, "test:seed", &fakeHub{}, nil); err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	mfs, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "stellarindex_api_stream_subscribe_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "outcome" {
					seen[lp.GetValue()] = true
				}
			}
		}
	}
	for _, o := range subscribeOutcomes {
		if !seen[o] {
			t.Errorf("outcome %q not exported after NewSubscriber; exported = %v", o, seen)
		}
	}
}

// TestSubscriber_CountsRejectionsByCause — an API host whose clock lags
// the aggregator by more than the forward-skew tolerance drops every
// event. That must be countable as clock skew, distinct from a stale
// replay and from a malformed payload.
func TestSubscriber_CountsRejectionsByCause(t *testing.T) {
	const channel = "test:outcomes"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let SUBSCRIBE bind (miniredis race)

	before := map[string]float64{}
	for _, o := range subscribeOutcomes {
		before[o] = testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues(o))
	}

	asset := canonical.NativeAsset().String()
	const quote = "fiat:USD"
	event := func(window int, value string, at time.Time) string {
		return fmt.Sprintf(`{"asset":%q,"quote":%q,"window_seconds":%d,"value_decimal":%q,"observed_at":%q}`,
			asset, quote, window, value, at.UTC().Format(time.RFC3339))
	}
	now := time.Now()
	publishRaw(t, rdb, channel, event(300, "1.0", now.Add(6*time.Minute)))
	publishRaw(t, rdb, channel, event(300, "1.0", now.Add(-25*time.Hour)))
	publishRaw(t, rdb, channel, event(0, "1.0", now))
	const sentinel = "7.654321000000"
	publishRaw(t, rdb, channel, event(300, sentinel, now))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinel) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hasValue(t, hub.Calls(), sentinel) {
		t.Fatalf("valid sentinel never fanned out")
	}

	want := map[string]float64{"ok": 1, "decode_error": 0, "malformed": 1, "future_observed_at": 1, "stale_observed_at": 1}
	for _, o := range subscribeOutcomes {
		if got := testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues(o)) - before[o]; got != want[o] {
			t.Errorf("outcome %q incremented by %v, want %v", o, got, want[o])
		}
	}
}
