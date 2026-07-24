// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// churnTopics opens and immediately cancels a subscription on n
// distinct never-published topics — the shape of a client cycling
// through made-up pairs.
func churnTopics(hub *streaming.Hub, n int) {
	for i := 0; i < n; i++ {
		_, cancel := hub.Subscribe([]string{fmt.Sprintf("closed:CHURN%d/USD", i)}, "")
		cancel()
	}
}

// TestHub_ReapDropsMintedTopicsButKeepsReplayBuffers pins the halves
// of the retention policy against each other (REL-05): topics minted
// by a client and never published to are dropped as soon as they have
// no subscriber, while a real topic's ring buffer survives the churn
// so a reconnecting client still gets its Last-Event-ID replay.
//
// The cruder "delete the topic whenever its subscriber count hits 0"
// fix also bounds the map, but fails the replay half of this test.
func TestHub_ReapDropsMintedTopicsButKeepsReplayBuffers(t *testing.T) {
	hub := streaming.NewHub(0)

	id1 := hub.Publish("closed:XLM/USD", "price_update", []byte("first"))
	hub.Publish("closed:XLM/USD", "price_update", []byte("second"))

	churnTopics(hub, 300)

	if got := hub.TopicCount(); got > 128 {
		t.Fatalf("TopicCount = %d after 300 minted topics, want <= 128", got)
	}
	if got := hub.TopicsReaped(); got < 128 {
		t.Fatalf("TopicsReaped = %d, want >= 128 (the reaper should have run several sweeps)", got)
	}

	// The real topic kept its buffer: resuming from id1 replays the
	// event that followed it.
	sub, cancel := hub.Subscribe([]string{"closed:XLM/USD"}, id1)
	defer cancel()
	got := drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || string(got[0].Data) != "second" {
		t.Fatalf("replay after churn = %v, want the buffered \"second\" event "+
			"(a published topic's replay window must survive topic reaping)", got)
	}
}

// TestHub_ReapDropsBufferedTopicPastIdleTTL — once a published topic
// has been subscriber-less for longer than the idle TTL, its replay
// buffer is released too. Otherwise a pair that trades once and goes
// quiet holds its ring for the life of the process.
func TestHub_ReapDropsBufferedTopicPastIdleTTL(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Nanosecond)

	id1 := hub.Publish("closed:XLM/USD", "price_update", []byte("first"))
	hub.Publish("closed:XLM/USD", "price_update", []byte("second"))

	churnTopics(hub, 128) // forces at least one sweep

	if got := hub.TopicsReaped(); got == 0 {
		t.Fatal("TopicsReaped = 0, want > 0 (the reaper never ran)")
	}
	sub, cancel := hub.Subscribe([]string{"closed:XLM/USD"}, id1)
	defer cancel()
	if got := drainNonblocking(t, sub, 1, 200*time.Millisecond); len(got) != 0 {
		t.Fatalf("replay after idle TTL = %v, want none (the buffer should have been released)", got)
	}
}

// TestHub_ReapNeverDropsSubscribedTopic — reaping must never detach a
// live stream from its fanout. Run with the reaper at maximum pressure
// (tiny ceiling, nanosecond TTL): the subscribed topic still delivers.
func TestHub_ReapNeverDropsSubscribedTopic(t *testing.T) {
	hub := streaming.NewHub(0)
	hub.SetTopicIdleTTL(time.Nanosecond)
	hub.SetMaxTopics(4)

	sub, cancel := hub.Subscribe([]string{"closed:XLM/USD"}, "")
	defer cancel()

	churnTopics(hub, 300)

	hub.Publish("closed:XLM/USD", "price_update", []byte("live"))
	got := drainNonblocking(t, sub, 1, time.Second)
	if len(got) != 1 || string(got[0].Data) != "live" {
		t.Fatalf("subscribed topic delivered %v after churn, want the live event "+
			"(a topic with subscribers must never be reaped)", got)
	}
	if n := hub.TopicCount(); n > 8 {
		t.Errorf("TopicCount = %d with MaxTopics(4), want the ceiling to hold it small", n)
	}
}

// TestStream_RejectedStreamsCounter — a connection refused by the caps
// is counted, so a flood is visible in diagnostics rather than silent.
func TestStream_RejectedStreamsCounter(t *testing.T) {
	streaming.SetMaxStreamsPerIP(1)
	defer streaming.SetMaxStreamsPerIP(0)
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "counter-client" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := streaming.StreamsRejected()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	held, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer held.Body.Close()

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap status = %d, want 503", resp.StatusCode)
	}

	if got := streaming.StreamsRejected() - before; got != 1 {
		t.Errorf("StreamsRejected delta = %d, want 1", got)
	}
}
