// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package streaming_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
)

// TestStream_PerIPConcurrentCap is the C3-8 regression: a single client
// IP can hold at most MaxStreamsPerIP concurrent SSE connections; the
// (N+1)th is rejected with 503. On the unfixed code (no per-IP cap) the
// (N+1)th would open with 200, so this fails RED without the fix.
func TestStream_PerIPConcurrentCap(t *testing.T) {
	const capN = 3

	streaming.SetMaxStreamsPerIP(capN)
	defer streaming.SetMaxStreamsPerIP(0)
	// Collapse every httptest connection (distinct ephemeral ports) into
	// ONE per-IP bucket so the cap is exercised deterministically.
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "test-client" })
	defer streaming.SetStreamClientIPResolver(nil)

	hub := streaming.NewHub(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Long heartbeat + no publishes → the stream just sits open,
		// holding its slot until the client disconnects.
		streaming.Stream(w, r, hub, []string{"topic"}, streaming.StreamOptions{
			HeartbeatInterval: 30 * time.Second,
		})
	}))
	defer srv.Close()

	// Each successful GET returns only AFTER the handler wrote its 200
	// header — which happens AFTER the per-IP slot is acquired. So once
	// Do() returns 200, the slot is held.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	held := make([]*http.Response, 0, capN)
	for i := 0; i < capN; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: status = %d, want 200", i, resp.StatusCode)
		}
		held = append(held, resp)
	}

	// The (N+1)th connection from the same IP must be refused.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("open over-cap stream: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-cap stream status = %d, want 503 (per-IP cap)", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Releasing a held stream must free its slot: disconnect one, then a
	// fresh connection is accepted again.
	_ = held[0].Body.Close()
	// Poll until the server-side release lands (async on ctx cancel).
	deadline := time.Now().Add(2 * time.Second)
	for streaming.ActiveStreamsForIP("test-client") >= capN && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("reopen after release: %v", err)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("reopen-after-release status = %d, want 200 (slot should have freed)", resp2.StatusCode)
	}
	_ = resp2.Body.Close()

	// Cleanup remaining held streams.
	cancel()
	for _, r := range held[1:] {
		_ = r.Body.Close()
	}
}

// TestStream_PerIPCapDisabled confirms cap 0 is a no-op: many streams
// from one IP all open (the global cap still governs total volume).
func TestStream_PerIPCapDisabled(t *testing.T) {
	streaming.SetMaxStreamsPerIP(0)
	streaming.SetStreamClientIPResolver(func(*http.Request) string { return "one-ip" })
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

	for i := 0; i < 10; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d with per-IP cap disabled: status = %d, want 200", i, resp.StatusCode)
		}
		defer resp.Body.Close() //nolint:revive // held open until test teardown
	}
}
