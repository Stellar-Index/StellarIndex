package redispub_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// runSubscriber starts a Subscriber on channel and returns its hub, its
// Redis client and a raw-payload publisher.
func runSubscriber(t *testing.T, channel string) (*fakeHub, *redis.Client, func(payload string)) {
	t.Helper()
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let SUBSCRIBE bind (miniredis race)
	return hub, rdb, func(payload string) { publishRaw(t, rdb, channel, payload) }
}

func waitForCalls(hub *fakeHub, n int) []hubCall {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) < n {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // catch a frame that should not have come
	return hub.Calls()
}

// A frozen bucket reaches SSE clients as a value-less price_frozen frame in
// the 60-second series' price_frozen shape, plus frozen_since.
func TestSubscriber_FrozenEventFansOutAsPriceFrozen(t *testing.T) {
	const channel = "test:closed"
	hub, rdb, _ := runSubscriber(t, channel)
	pub, err := redispub.NewPublisher(rdb, channel)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair := canonical.Pair{Base: canonical.NativeAsset(), Quote: usd}
	bucket := time.Now().UTC().Truncate(time.Minute)
	since := bucket.Add(-3 * time.Minute)
	if err := pub.PublishFrozenBucket(context.Background(), pair, 5*time.Minute, bucket, since); err != nil {
		t.Fatalf("PublishFrozenBucket: %v", err)
	}

	calls := waitForCalls(hub, 1)
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	if calls[0].eventType != "price_frozen" {
		t.Errorf("event type = %q, want price_frozen", calls[0].eventType)
	}
	if want := "closed:native/fiat:USD/300"; calls[0].topic != want {
		t.Errorf("topic = %q, want %q", calls[0].topic, want)
	}
	at, from := bucket.Format(time.RFC3339), since.Format(time.RFC3339)
	want := fmt.Sprintf(`{"data":{"asset_id":"native","quote":"fiat:USD","observed_at":%q,"window_seconds":300,"frozen_since":%q},"as_of":%q,"flags":{"frozen":true,"frozen_checked":true}}`,
		at, from, at)
	if got := string(calls[0].data); got != want {
		t.Errorf("frame =\n  %s\nwant\n  %s", got, want)
	}
}

// A frozen event carrying any part of a value, an impossible frozen_since,
// or an unknown kind was not produced by an aggregator and is dropped.
func TestSubscriber_DropsMalformedFrozenEvents(t *testing.T) {
	hub, _, publish := runSubscriber(t, "test:closed")
	bucket := time.Now().UTC().Truncate(time.Minute)
	at, later := bucket.Format(time.RFC3339), bucket.Add(time.Minute).Format(time.RFC3339)
	head := `{"asset":"native","quote":"fiat:USD","window_seconds":300,"observed_at":"` + at + `"`
	for _, forged := range []string{
		head + `,"kind":"frozen","value_decimal":"0.100000000000"}`,
		head + `,"kind":"frozen","truncated":false}`,
		head + `,"kind":"frozen","truncated":true,"covered_from":"` + at + `"}`,
		head + `,"kind":"frozen","frozen_since":"` + later + `"}`,
		head + `,"kind":"thawed"}`,
		head + `,"value_decimal":"0.100000000000","frozen_since":"` + at + `"}`,
	} {
		publish(forged)
	}
	publish(head + `,"kind":"frozen","frozen_since":"` + at + `"}`)

	calls := waitForCalls(hub, 1)
	if len(calls) != 1 || calls[0].eventType != "price_frozen" {
		t.Fatalf("forwarded %d frames (%+v), want only the valid price_frozen", len(calls), calls)
	}
}

// A topic carries one frame per bucket whatever its kind, in order: a price
// for a bucket already forwarded as frozen is a duplicate, and the next
// bucket's price goes out as a price_update.
func TestSubscriber_OneFramePerBucketAcrossKinds(t *testing.T) {
	hub, _, publish := runSubscriber(t, "test:closed")
	bucket := time.Now().UTC().Truncate(time.Minute)
	event := func(at time.Time, rest string) string {
		return `{"asset":"native","quote":"fiat:USD","window_seconds":300,"observed_at":"` + at.Format(time.RFC3339) + `",` + rest + `}`
	}
	publish(event(bucket.Add(-time.Minute), `"kind":"frozen"`))
	publish(event(bucket.Add(-time.Minute), `"value_decimal":"0.100000000000"`))
	publish(event(bucket, `"value_decimal":"0.200000000000"`))

	calls := waitForCalls(hub, 2)
	if len(calls) != 2 {
		t.Fatalf("forwarded %d frames, want 2", len(calls))
	}
	if calls[0].eventType != "price_frozen" || calls[1].eventType != "price_update" {
		t.Errorf("event types = %q, %q; want price_frozen, price_update", calls[0].eventType, calls[1].eventType)
	}
	if got := decodeCall(t, calls[1].data).Data.Price; got != "0.200000000000" {
		t.Errorf("second frame price = %q, want the next bucket's", got)
	}
}
