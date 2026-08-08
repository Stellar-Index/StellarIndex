package redispub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// fakeHub captures Hub.Publish calls.
type fakeHub struct {
	mu    sync.Mutex
	calls []hubCall
}

type hubCall struct {
	topic     string
	eventType string
	data      []byte
}

func (h *fakeHub) Publish(topic, eventType string, data []byte) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, hubCall{topic: topic, eventType: eventType, data: data})
	return "fake-id"
}

func (h *fakeHub) Calls() []hubCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hubCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// newRedis spins up an in-memory miniredis + a *redis.Client.
// miniredis supports SUBSCRIBE/PUBLISH out of the box.
func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// TestNewSubscriber_RequiresInputs — operator misconfig must fail
// at construction.
func TestNewSubscriber_RequiresInputs(t *testing.T) {
	if _, err := redispub.NewSubscriber(nil, "", &fakeHub{}, nil); err == nil {
		t.Error("expected error for nil cache")
	}
	_, rdb := newRedis(t)
	if _, err := redispub.NewSubscriber(rdb, "", nil, nil); err == nil {
		t.Error("expected error for nil hub")
	}
}

// TestSubscriber_RoundTrip — the canonical happy path: a Publisher
// writes one event; the Subscriber decodes it and republishes on
// the Hub with the canonical topic key.
func TestSubscriber_RoundTrip(t *testing.T) {
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, "test:closed", hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	pub, err := redispub.NewPublisher(rdb, "test:closed")
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- sub.Run(ctx) }()

	// miniredis SUBSCRIBE registration is racy with PUBLISH —
	// give the subscriber a beat to bind before we publish.
	time.Sleep(50 * time.Millisecond)

	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// A real closed-bucket event's ObservedAt is the bucket END, i.e.
	// ~now; the subscriber now enforces that freshness (F2), so the
	// fixture uses a recent timestamp rather than a fixed historical one.
	observedAt := time.Now().UTC().Add(-time.Minute)

	if err := pub.PublishClosedBucket(ctx, pair, 5*time.Minute, "0.123456789012", observedAt); err != nil {
		t.Fatalf("PublishClosedBucket: %v", err)
	}

	// Wait briefly for the message to flow through.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	c := calls[0]
	wantTopic := v1.PriceStreamTopic(pair.Base, pair.Quote, 300)
	if c.topic != wantTopic {
		t.Errorf("topic = %q, want %q", c.topic, wantTopic)
	}
	if c.eventType != "price_update" {
		t.Errorf("eventType = %q, want price_update", c.eventType)
	}

	// Payload round-trip — Subscriber forwards the published
	// JSON bytes verbatim, so re-decode should match.
	var got redispub.ClosedBucketEvent
	if err := json.Unmarshal(c.data, &got); err != nil {
		t.Fatalf("decode forwarded payload: %v", err)
	}
	if got.Asset != pair.Base.String() || got.Quote != pair.Quote.String() {
		t.Errorf("payload identity = %s/%s, want %s/%s",
			got.Asset, got.Quote, pair.Base.String(), pair.Quote.String())
	}

	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

// TestSubscriber_TopicFormatStaysInSync — sentinel test: Subscriber's
// topic format must match v1.PriceStreamTopic since the Hub layer
// expects the exact string.
func TestSubscriber_TopicFormatStaysInSync(t *testing.T) {
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	want := v1.PriceStreamTopic(canonical.NativeAsset(), usd, 300)
	if want != "closed:"+canonical.NativeAsset().String()+"/"+usd.String()+"/300" {
		t.Fatalf("v1.PriceStreamTopic format changed; redispub Subscriber must update too. Got %q", want)
	}
}

// publishRaw sends raw bytes on the pub/sub channel, bypassing the
// typed Publisher — the exact shape a host-adjacent process with Redis
// network access (r1 Redis has NO AUTH) could inject. Used to prove the
// subscriber validates + sanitizes before fan-out (F2).
func publishRaw(t *testing.T, rdb *redis.Client, channel, payload string) {
	t.Helper()
	if err := rdb.Publish(context.Background(), channel, payload).Err(); err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
}

func decodeCall(t *testing.T, data []byte) redispub.ClosedBucketEvent {
	t.Helper()
	var ev redispub.ClosedBucketEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode forwarded payload %q: %v", data, err)
	}
	return ev
}

func hasValue(t *testing.T, calls []hubCall, want string) bool {
	t.Helper()
	for _, c := range calls {
		if decodeCall(t, c.data).ValueDecimal == want {
			return true
		}
	}
	return false
}

func callValues(t *testing.T, calls []hubCall) string {
	t.Helper()
	vs := make([]string, len(calls))
	for i, c := range calls {
		vs[i] = decodeCall(t, c.data).ValueDecimal
	}
	return strings.Join(vs, ",")
}

// TestSubscriber_DropsForgedValueDecimal — a host-adjacent process
// injecting an event with a non-numeric / non-positive / absurd
// value_decimal must be DROPPED, not fanned out to SSE clients (F2).
//
// Proven by publishing a batch of forged events followed by ONE valid
// sentinel: messages on a single Redis channel are processed in order,
// so once the sentinel reaches the Hub every forged message before it
// has already been handled. A Hub holding exactly the sentinel proves
// each forged message was rejected. Against the un-fixed subscriber
// (which forwards anything with a non-empty asset/quote) every forged
// message is fanned out too and this fails.
func TestSubscriber_DropsForgedValueDecimal(t *testing.T) {
	const channel = "test:closed"
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

	asset := canonical.NativeAsset().String()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote := usd.String()
	observedAt := time.Now().UTC().Format(time.RFC3339)

	forged := []string{
		"-5.000000000000",                 // negative
		"0.000000000000",                  // zero
		"not-a-number",                    // non-numeric
		"1000000000000000000000000000000", // 10^30 — absurd
		"1/2",                             // fraction form big.Rat would accept
	}
	for _, v := range forged {
		publishRaw(t, rdb, channel, fmt.Sprintf(
			`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q,"injected":"forged"}`,
			asset, quote, v, observedAt))
	}
	const sentinelValue = "7.654321000000"
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q}`,
		asset, quote, sentinelValue, observedAt))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinelValue) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if !hasValue(t, calls, sentinelValue) {
		t.Fatalf("valid sentinel never fanned out; forged batch may have stalled the loop")
	}
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1 — forged events must be dropped; forwarded values = [%s]",
			len(calls), callValues(t, calls))
	}
}

// TestSubscriber_StripsInjectedExtraFields — a VALID event carrying
// attacker-injected extra JSON fields is fanned out, but SANITIZED to
// the canonical four-field shape: the subscriber re-marshals the
// validated struct rather than forwarding the raw payload (F2), so the
// injected fields never reach SSE clients. The value_decimal survives
// unchanged as a decimal string (the money representation is not
// weakened). Against the un-fixed subscriber (which forwards the raw
// bytes) the injected fields leak through and this fails.
func TestSubscriber_StripsInjectedExtraFields(t *testing.T) {
	const channel = "test:closed"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	asset := canonical.NativeAsset().String()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote := usd.String()
	observedAt := time.Now().UTC().Format(time.RFC3339)

	const value = "0.123456789012"
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q,`+
			`"injected_field":"pwned","evil":{"nested":true}}`,
		asset, quote, value, observedAt))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	data := calls[0].data
	for _, leak := range []string{"injected_field", "pwned", "evil", "nested"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("forwarded payload leaked injected content %q: %s", leak, data)
		}
	}
	ev := decodeCall(t, data)
	if ev.Asset != asset || ev.Quote != quote {
		t.Errorf("identity = %s/%s, want %s/%s", ev.Asset, ev.Quote, asset, quote)
	}
	if ev.ValueDecimal != value {
		t.Errorf("value_decimal = %q, want %q (must survive unchanged as a decimal string)", ev.ValueDecimal, value)
	}
	if ev.WindowSeconds != 300 {
		t.Errorf("window_seconds = %d, want 300", ev.WindowSeconds)
	}
}
