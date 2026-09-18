package streaming_test

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// Replay rings are allocated on first PUBLISH, not on subscribe
// (audit-2026-09-02 F058 / K010).
//
// DefaultMaxTopics was never a ceiling: getOrCreateTopic inserts
// unconditionally and the reaper evicts only SUBSCRIBER-LESS topics, so
// a caller holding subscriptions grows the map past it by design. With
// the ring allocated eagerly, that made resident memory scale with
// concurrent streams × alias fan-out — /v1/price/stream subscribes one
// connection to assetAliases(base) × assetAliases(quote), up to 9
// topics, of which the aggregator publishes to at most a few — so the
// never-published remainder alone reserved ~20 KiB apiece for rings that
// could never hold an event.
//
// Proven red against the unfixed Hub (getOrCreateTopic allocating
// `buffer: newRing(h.bufferSize)`): BufferedTopicCount reached the full
// subscribed count and the per-topic cost measured ~20.7 KiB against the
// 2 KiB budget below.

// subscriberOnlyTopics is large enough that a 20 KiB-per-topic ring is
// unmistakable against measurement noise (~400 MiB eager vs ~7 MiB
// lazy), and small enough to stay cheap once fixed.
const subscriberOnlyTopics = 20000

// lazyRingBudgetBytes is the per-subscribed-topic memory budget. A
// topicState plus its map entry and key is a few hundred bytes; an
// eagerly-allocated 256-event ring is ~20 KiB. 2 KiB sits an order of
// magnitude clear of both, so this fails on the defect without being
// sensitive to allocator noise.
const lazyRingBudgetBytes = 2048

func TestHub_SubscribedButUnpublishedTopicsAllocateNoRing(t *testing.T) {
	hub := streaming.NewHub(0)

	topics := make([]string, 0, subscriberOnlyTopics)
	for i := range subscriberOnlyTopics {
		// The client-supplied key shape: an arbitrary pair/window that
		// no publisher will ever write to.
		topics = append(topics, fmt.Sprintf("closed:native/fiat:USD/%d", i+1))
	}

	before := heapInUse()
	_, cancel := hub.Subscribe(topics, "")
	defer cancel()
	after := heapInUse()

	if got := hub.TopicCount(); got != subscriberOnlyTopics {
		t.Fatalf("TopicCount() = %d, want %d — the subscription did not mint the "+
			"topics this test is measuring", got, subscriberOnlyTopics)
	}
	if got := hub.BufferedTopicCount(); got != 0 {
		t.Errorf("BufferedTopicCount() = %d after subscribing to %d topics with no "+
			"publisher, want 0 — a topic nothing has published to has nothing to "+
			"replay, so its ring is pure cost", got, subscriberOnlyTopics)
	}

	perTopic := int64(after-before) / int64(subscriberOnlyTopics)
	if perTopic >= lazyRingBudgetBytes {
		t.Errorf("subscriber-only topics cost %d bytes each (%d topics, %d bytes total), "+
			"want under %d — at the shipped 8192-stream cap × 9 alias topics that is "+
			"%.1f GiB of rings that can never hold an event",
			perTopic, subscriberOnlyTopics, int64(after-before), lazyRingBudgetBytes,
			float64(perTopic)*8192*9/(1<<30))
	}
	t.Logf("subscriber-only topic cost: %d bytes each", perTopic)
}

// The ring must still exist — and still replay — the moment a topic
// actually carries an event. Lazy must not mean absent.
func TestHub_PublishAllocatesTheRingAndReplayStillWorks(t *testing.T) {
	hub := streaming.NewHub(0)

	quiet, cancelQuiet := hub.Subscribe([]string{"closed:native/fiat:USD/7"}, "")
	defer cancelQuiet()
	if got := hub.BufferedTopicCount(); got != 0 {
		t.Fatalf("BufferedTopicCount() = %d before any publish, want 0", got)
	}

	first := hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"1"}`))
	hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"2"}`))

	if got := hub.BufferedTopicCount(); got != 1 {
		t.Errorf("BufferedTopicCount() = %d after publishing to one topic, want 1 — "+
			"a published topic MUST hold a replay ring", got)
	}
	if got := hub.TopicCount(); got != 2 {
		t.Errorf("TopicCount() = %d, want 2", got)
	}

	// Resume from the first event: the second must replay.
	sub, cancel := hub.Subscribe([]string{"tip:native/fiat:USD/5"}, first)
	defer cancel()
	select {
	case ev := <-sub:
		if string(ev.Data) != `{"v":"2"}` {
			t.Errorf("replayed event data = %q, want %q", ev.Data, `{"v":"2"}`)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no replay after resuming from the first event id — the lazily " +
			"allocated ring did not retain the published event")
	}
	_ = quiet
}

// A never-published topic still reaps on its last unsubscribe: the
// "nothing to replay" arm must read a missing ring as empty, not panic
// and not treat it as a buffer worth keeping for the idle TTL.
func TestHub_NeverPublishedTopicStillReapsOnLastUnsubscribe(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Hour)
	// The reaper runs opportunistically on topic CREATION; a threshold of
	// 1 makes the next creation trigger a pass deterministically.
	hub.SetMaxTopics(1)

	_, cancel := hub.Subscribe([]string{"closed:native/fiat:USD/11"}, "")
	if got := hub.TopicCount(); got != 1 {
		t.Fatalf("TopicCount() = %d, want 1", got)
	}
	cancel()

	// Minting a topic triggers the pass that should drop it.
	hub.Publish("tip:native/fiat:USD/5", "tip_update", []byte(`{"v":"1"}`))
	if got := hub.TopicCount(); got != 1 {
		t.Errorf("TopicCount() = %d after the subscriber-less unpublished topic "+
			"should have been reaped, want 1 (the published topic only)", got)
	}
	if got := hub.BufferedTopicCount(); got != 1 {
		t.Errorf("BufferedTopicCount() = %d, want 1", got)
	}
}

// heapInUse returns live heap bytes after settling the collector.
func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
