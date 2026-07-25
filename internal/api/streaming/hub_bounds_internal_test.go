// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// White-box regressions for the topic-map bound (REL-05). They read
// h.topics under h.mu on purpose: what these tests pin is what the MAP
// does, and asserting it through the exported TopicCount() would stop
// them compiling against the pre-fix Hub — a regression test that
// can't be run red proves nothing.

// TestHub_TopicMapDoesNotGrowWithChurn pins REL-05: a client that
// streams one made-up pair after another must not leave a permanent
// topic (with its ring buffer) behind for each one. Before the fix
// getOrCreateTopic only ever inserted, so h.topics grew by one entry
// per distinct topic name ever seen and nothing ever removed it —
// memory exhaustion driven entirely by unauthenticated input.
func TestHub_TopicMapDoesNotGrowWithChurn(t *testing.T) {
	const churn = 500

	hub := NewHub(0)
	for i := 0; i < churn; i++ {
		// Subscribe + immediately cancel: the shape of a client that
		// opens a stream for a pair nobody publishes and hangs up.
		_, cancel := hub.Subscribe([]string{fmt.Sprintf("closed:MADEUP%d/USD", i)}, "")
		cancel()
	}

	hub.mu.RLock()
	got := len(hub.topics)
	hub.mu.RUnlock()

	// The reaper sweeps every topicSweepGrowth (64) creations and drops
	// every subscriber-less, never-published topic, so the map holds at
	// most one sweep's worth of churn plus slack — NOT one entry per
	// pair ever streamed. The bound is spelled as a literal, not as
	// 2*topicSweepGrowth, so this file still compiles against the
	// pre-fix Hub and can be run RED.
	const wantMax = 128
	if got > wantMax {
		t.Fatalf("topic map holds %d topics after %d one-shot subscriptions, want <= %d "+
			"(idle zero-subscriber topics must be reaped)", got, churn, wantMax)
	}
}

// TestStream_CapRejectsBeforeTopicCreation pins
// REL-05-resource-exhaustion: a connection the concurrency caps refuse
// must never allocate a Hub topic. Before the fix Stream() called
// hub.Subscribe FIRST and only then entered StreamFromChannel where
// the caps live, so every rejected connection still minted a permanent
// topic keyed by client-controlled input — the caps bounded sockets
// but not Hub memory.
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
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503", resp.StatusCode)
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
