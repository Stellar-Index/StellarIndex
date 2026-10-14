// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// White-box tests of the topic-map bound: they read h.topics under h.mu because asserting through TopicCount() would
// stop compiling against a Hub without the bound, and a regression test that cannot run red proves nothing.

// TestHub_TopicMapDoesNotGrowWithChurn pins the bound: streaming one made-up pair after another must not leave a
// permanent topic and ring buffer per pair, since unauthenticated input would otherwise exhaust memory.
func TestHub_TopicMapDoesNotGrowWithChurn(t *testing.T) {
	const churn = 500

	hub := NewHub(0)
	for i := 0; i < churn; i++ {
		// Subscribe + immediately cancel: the shape of a client that
		// opens a stream for a pair nobody publishes and hangs up.
		_, cancel, err := hub.Subscribe([]string{fmt.Sprintf("closed:MADEUP%d/USD", i)}, "")
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		cancel()
	}

	hub.mu.RLock()
	got := len(hub.topics)
	hub.mu.RUnlock()

	// The reaper drops subscriber-less, never-published topics every topicSweepGrowth (64) creations, so the map holds
	// one sweep of churn. The bound is a literal so this file still compiles against a Hub without the bound and can
	// run RED.
	const wantMax = 128
	if got > wantMax {
		t.Fatalf("topic map holds %d topics after %d one-shot subscriptions, want <= %d "+
			"(idle zero-subscriber topics must be reaped)", got, churn, wantMax)
	}
}

// TestHub_FullSubscribedCeilingDoesNotSweepEveryInsert pins that a ceiling where every topic is subscribed does not
// re-sweep on each insert (O(n) per Subscribe under h.mu; 20k topics took ~234 s under -race), while an unsubscribe
// re-arms the sweep.
func TestHub_FullSubscribedCeilingDoesNotSweepEveryInsert(t *testing.T) {
	const ceiling, extra = 8, 20

	hub := NewHub(0)
	hub.SetMaxTopics(ceiling)
	cancels := make([]func(), 0, ceiling+1)
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for i := range ceiling + extra {
		_, cancel, err := hub.Subscribe([]string{fmt.Sprintf("closed:HELD%d/USD", i)}, "")
		switch {
		case i < ceiling && err != nil:
			t.Fatalf("Subscribe %d under the ceiling: %v", i, err)
		case i >= ceiling && !errors.Is(err, ErrTopicCapacity):
			t.Fatalf("Subscribe %d over a fully-subscribed ceiling: err = %v, want ErrTopicCapacity", i, err)
		case err == nil:
			cancels = append(cancels, cancel)
		}
	}

	hub.mu.RLock()
	since, n := hub.sinceSweep, len(hub.topics)
	hub.mu.RUnlock()
	if n != ceiling {
		t.Fatalf("topic map holds %d topics, want %d: refused subscribes must not insert", n, ceiling)
	}
	// One sweep at the first attempt over the ceiling finds it futile; the
	// remaining extra-1 attempts must not sweep.
	if since != extra-1 {
		t.Fatalf("sinceSweep = %d after %d attempts over a fully-subscribed ceiling, want %d "+
			"(a futile ceiling must not be re-swept on every insert)", since, extra, extra-1)
	}

	cancels[0]()
	_, cancel, err := hub.Subscribe([]string{"closed:AFTER/USD"}, "")
	if err != nil {
		t.Fatalf("Subscribe after an unsubscribe freed a slot: %v", err)
	}
	cancels = append(cancels, cancel)

	hub.mu.RLock()
	_, stillThere := hub.topics["closed:HELD0/USD"]
	hub.mu.RUnlock()
	if stillThere {
		t.Fatal("topic that lost its last subscriber survived the next insert at the ceiling; " +
			"an unsubscribe must re-arm the ceiling sweep")
	}
}

// TestHub_SubscribeRefusesPastMaxTopics pins that, with every held topic subscribed, a Subscribe minting a new
// client-supplied topic is refused without leaving a registration, while existing topics stay subscribable.
func TestHub_SubscribeRefusesPastMaxTopics(t *testing.T) {
	const ceiling = 4

	hub := NewHub(0)
	hub.SetMaxTopics(ceiling)
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for i := range ceiling {
		_, cancel, err := hub.Subscribe([]string{fmt.Sprintf("closed:FULL%d/USD", i)}, "")
		if err != nil {
			t.Fatalf("Subscribe %d under the ceiling: %v", i, err)
		}
		cancels = append(cancels, cancel)
	}

	// The first topic exists, the second would be new: the whole
	// subscription is refused and the existing topic keeps one subscriber.
	ch, cancel, err := hub.Subscribe([]string{"closed:FULL0/USD", "closed:NEW/USD"}, "")
	if !errors.Is(err, ErrTopicCapacity) {
		t.Fatalf("Subscribe past the ceiling: err = %v, want ErrTopicCapacity", err)
	}
	if ch != nil || cancel != nil {
		t.Fatal("refused Subscribe returned a channel or cancel func")
	}

	hub.mu.RLock()
	n := len(hub.topics)
	_, minted := hub.topics["closed:NEW/USD"]
	full0 := hub.topics["closed:FULL0/USD"]
	hub.mu.RUnlock()
	if n > ceiling || minted {
		t.Fatalf("topic map holds %d topics (minted NEW: %v), want <= %d", n, minted, ceiling)
	}
	full0.mu.Lock()
	subs := len(full0.subs)
	full0.mu.Unlock()
	if subs != 1 {
		t.Fatalf("closed:FULL0/USD holds %d subscribers after a refused Subscribe, want 1", subs)
	}

	_, cancel, err = hub.Subscribe([]string{"closed:FULL1/USD"}, "")
	if err != nil {
		t.Fatalf("Subscribe to an existing topic at the ceiling: %v", err)
	}
	cancels = append(cancels, cancel)
}

// TestStream_TopicCapRefusedWith503: a Stream whose Subscribe is refused
// answers 503 before any SSE header is sent.
func TestStream_TopicCapRefusedWith503(t *testing.T) {
	hub := NewHub(0)
	hub.SetMaxTopics(1)
	_, cancelHeld, err := hub.Subscribe([]string{"closed:HELD/USD"}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelHeld()

	// Bounded so an admitted stream ends and reports 200 instead of hanging.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/?pair=NEW/USD", nil)
	Stream(rec, req, hub, []string{"closed:NEW/USD"}, StreamOptions{HeartbeatInterval: 30 * time.Second})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("stream status = %d, want 429", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Fatal("refused stream sent SSE headers")
	}
}

// TestStream_CapRejectsBeforeTopicCreation pins that a connection refused by the concurrency caps never allocates a
// Hub topic; subscribing first would mint a permanent topic from client-controlled input on every rejection.
func TestStream_CapRejectsBeforeTopicCreation(t *testing.T) {
	SetMaxStreamsPerIP(1)
	defer SetMaxStreamsPerIP(0)
	// Collapse every httptest connection into one per-IP bucket so the
	// cap trips deterministically.
	SetStreamClientIPResolver(func(*http.Request) string { return "flooder" })
	defer SetStreamClientIPResolver(nil)

	hub := NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirrors internal/api/v1.handlePriceStream: the topic key comes
		// straight from the request.
		Stream(w, r, hub, []string{"closed:" + r.URL.Query().Get("pair")}, StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hold the single allowed slot. Do() returns once the handler wrote
	// its 200, which is after the slot is taken.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?pair=XLM/USD", nil)
	held, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer held.Body.Close()
	if held.StatusCode != http.StatusOK {
		t.Fatalf("first stream status = %d, want 200", held.StatusCode)
	}

	// Over-cap connection asking for a brand-new topic key.
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?pair=MINTED/USD", nil)
	resp, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-cap stream status = %d, want 429", resp.StatusCode)
	}

	hub.mu.RLock()
	_, minted := hub.topics["closed:MINTED/USD"]
	total := len(hub.topics)
	hub.mu.RUnlock()
	if minted {
		t.Fatalf("refused connection allocated topic %q (map holds %d topics) — "+
			"the concurrency caps must be enforced before Subscribe can create one",
			"closed:MINTED/USD", total)
	}
}
