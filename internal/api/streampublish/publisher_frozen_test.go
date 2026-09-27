package streampublish_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeFreeze is a v1.FrozenLooker keyed by "<asset>/<quote>".
type fakeFreeze struct {
	frozen map[string]bool
	err    error
}

func (f fakeFreeze) FrozenForPair(_ context.Context, asset, quote canonical.Asset) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.frozen[asset.String()+"/"+quote.String()], nil
}

// firstEvent runs the publisher over one pair with a fresh bucket and
// returns the first event it publishes.
func firstEvent(t *testing.T, looker v1.FrozenLooker, stale bool) streaming.Event {
	t.Helper()
	hub := streaming.NewHub(0)
	reader := &fakeReader{stale: stale}
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
	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{Frozen: looker})
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s of publisher start")
	}
	return streaming.Event{}
}

type streamedPrice struct {
	Data  map[string]any  `json:"data"`
	Flags map[string]bool `json:"flags"`
}

func decodeStreamed(t *testing.T, ev streaming.Event) streamedPrice {
	t.Helper()
	var p streamedPrice
	if err := json.Unmarshal(ev.Data, &p); err != nil {
		t.Fatalf("unmarshal %s payload: %v", ev.Type, err)
	}
	return p
}

// TestPublisher_FrozenPairPublishesTheFreezeNotTheRefusedBucket — under
// an ADR-0019 freeze the raw prices_1m bucket is the value the freeze
// refused, and /v1/price never serves it under the flag. The stream
// used to publish it every minute as a price_update with flags
// {stale:false} and no frozen key — the same bytes as a healthy pair.
// The marker of ANY spelling governs (the reader does not report which
// alias its bucket came from), so a freeze on crypto:XLM covers native.
func TestPublisher_FrozenPairPublishesTheFreezeNotTheRefusedBucket(t *testing.T) {
	ev := firstEvent(t, fakeFreeze{frozen: map[string]bool{"crypto:XLM/fiat:USD": true}}, true)
	if ev.Type != "price_frozen" {
		t.Fatalf("event type = %q, want price_frozen", ev.Type)
	}
	p := decodeStreamed(t, ev)
	if _, hasPrice := p.Data["price"]; hasPrice {
		t.Errorf("price_frozen carries the refused bucket's price: %s", ev.Data)
	}
	if p.Data["observed_at"] != "2026-05-02T12:00:00Z" {
		t.Errorf("observed_at = %v, want the refused bucket's 2026-05-02T12:00:00Z", p.Data["observed_at"])
	}
	if !p.Flags["frozen"] || !p.Flags["frozen_checked"] {
		t.Errorf("flags = %v, want frozen and frozen_checked true", p.Flags)
	}
	// The reader called the refused bucket stale; the freeze must not
	// launder that into stale:false.
	if stale, present := p.Flags["stale"]; !present || !stale {
		t.Errorf("flags = %v, want stale true (the reader's verdict on the refused bucket)", p.Flags)
	}
}

// TestPublisher_UnfrozenPairSaysTheFreezeWasChecked — a healthy pair's
// price_update must be distinguishable from one whose freeze was never
// evaluated: frozen_checked is present only when every marker was read.
func TestPublisher_UnfrozenPairSaysTheFreezeWasChecked(t *testing.T) {
	ev := firstEvent(t, fakeFreeze{}, false)
	if ev.Type != "price_update" {
		t.Fatalf("event type = %q, want price_update", ev.Type)
	}
	p := decodeStreamed(t, ev)
	if p.Data["price"] != "0.07" {
		t.Errorf("price = %v, want 0.07", p.Data["price"])
	}
	if !p.Flags["frozen_checked"] {
		t.Errorf("flags = %v, want frozen_checked true", p.Flags)
	}
	if _, present := p.Flags["frozen"]; present {
		t.Errorf("flags = %v, want no frozen key on an unfrozen pair", p.Flags)
	}

	failed := decodeStreamed(t, firstEvent(t, fakeFreeze{err: errors.New("redis down")}, false))
	if _, present := failed.Flags["frozen_checked"]; present {
		t.Errorf("flags = %v after a failed marker read, want frozen_checked absent (not evaluated)", failed.Flags)
	}
}
