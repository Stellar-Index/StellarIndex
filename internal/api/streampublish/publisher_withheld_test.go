package streampublish_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

func (r *fakeReader) SetErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func nextEvent(t *testing.T, ch <-chan streaming.Event, within time.Duration) streaming.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %s", within)
	}
	return streaming.Event{}
}

// TestPublisher_WithheldPairPublishesTheWithholding — a pair that becomes
// withheld while subscribers are attached must say so on the wire once,
// not fall silent like a pair with no trades, and its bucket must be
// republished when it is served again.
func TestPublisher_WithheldPairPublishesTheWithholding(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07", PriceType: "vwap",
		ObservedAt: v1.WireTime(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)), WindowSeconds: 60,
	})
	ch, cancelSub := hub.Subscribe([]string{v1.PriceStreamTopic(asset, quote, 60)}, "")
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	if ev := nextEvent(t, ch, 2*time.Second); ev.Type != "price_update" {
		t.Fatalf("first event = %s, want price_update", ev.Type)
	}

	reader.SetErr(v1.PriceWithheldError(pricingguard.WithheldThinMarket))
	ev := nextEvent(t, ch, 2500*time.Millisecond)
	if ev.Type != "price_withheld" {
		t.Fatalf("event after the pair was withheld = %s, want price_withheld", ev.Type)
	}
	var body struct {
		AssetID string `json:"asset_id"`
		Quote   string `json:"quote"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		t.Fatalf("unmarshal price_withheld: %v", err)
	}
	if body.AssetID != "native" || body.Quote != "fiat:USD" || body.Reason != "substance" {
		t.Fatalf("price_withheld data = %+v, want native/fiat:USD reason substance", body)
	}

	// Still withheld on the next tick: the marker is not repeated.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected %s while the verdict was unchanged", ev.Type)
	case <-time.After(1500 * time.Millisecond):
	}

	reader.SetErr(nil)
	if ev := nextEvent(t, ch, 2500*time.Millisecond); ev.Type != "price_update" {
		t.Fatalf("event once served again = %s, want the bucket republished as price_update", ev.Type)
	}
}
