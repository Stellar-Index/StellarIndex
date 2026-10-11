package redispub_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TestChannelDropCounter_CountsGoRedisChannelFull reproduces go-redis's silent PubSub.Channel drop
// (stalled reader, small buffer/timeout) and requires it to count as
// stellarindex_api_stream_subscribe_total{outcome="dropped_slow_consumer"}, not only a log line.
// NewSubscriber is called solely for its side effect of installing the logger; the drop happens in
// go-redis's goroutine before any Subscriber code runs.
func TestChannelDropCounter_CountsGoRedisChannelFull(t *testing.T) {
	mr, rdb := newRedis(t)
	if _, err := redispub.NewSubscriber(rdb, "install-only", &fakeHub{}, nil); err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	ctx := context.Background()
	pubsub := rdb.Subscribe(ctx, "drop-test-raw")
	t.Cleanup(func() { _ = pubsub.Close() })

	// A one-slot buffer with a short send timeout: the second published
	// message cannot be enqueued while the first sits unread, so go-redis
	// drops it after chanSendTimeout instead of blocking forever.
	_ = pubsub.Channel(redis.WithChannelSize(1), redis.WithChannelSendTimeout(20*time.Millisecond))
	time.Sleep(50 * time.Millisecond) // miniredis SUBSCRIBE registration races PUBLISH

	before := testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues("dropped_slow_consumer"))

	mr.Publish("drop-test-raw", "first")
	mr.Publish("drop-test-raw", "second")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues("dropped_slow_consumer")); got > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dropped_slow_consumer did not increment within deadline (before=%v)", before)
}
