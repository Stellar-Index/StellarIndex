// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package wsclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Jitter scatters reconnect timers ±25% so a fleet of streamers doesn't
// thunder-herd a venue on the next tick after a shared disconnect. Verify
// the envelope is respected and degenerate inputs pass through unchanged.
func TestJitter_envelopeAndDegenerateInputs(t *testing.T) {
	base := 4 * time.Second
	low, high := base-base/4, base+base/4
	for i := 0; i < 200; i++ {
		got := Jitter(base)
		if got < low || got > high {
			t.Fatalf("Jitter(%v)=%v outside [%v,%v]", base, got, low, high)
		}
	}
	if got := Jitter(0); got != 0 {
		t.Errorf("Jitter(0)=%v, want 0", got)
	}
	if got := Jitter(-time.Second); got != -time.Second {
		t.Errorf("Jitter(-1s)=%v, want -1s", got)
	}
}

// TestClassifyDisconnect_BoundedReasonLabels keeps the disconnect metric's
// label cardinality bounded. Add to this table when adding a new reason;
// that's the operator contract.
func TestClassifyDisconnect_BoundedReasonLabels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "other"},
		// Checked before the string match: the wrapped cause is a context
		// deadline, which would otherwise land in "timeout".
		{"stall", fmt.Errorf("%w: %w", ErrStreamStalled, context.DeadlineExceeded), "stall"},
		{"reset", errors.New("read: failed to read frame payload: read tcp 1.2.3.4:443: read: connection reset by peer"), "reset"},
		{"broken_pipe", errors.New("write: broken pipe"), "broken_pipe"},
		{"timeout", errors.New("read: i/o timeout"), "timeout"},
		{"dial", errors.New("dial: lookup stream.example.com: no such host"), "dial"},
		{"other", errors.New("EOF"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyDisconnect(tc.err); got != tc.want {
				t.Errorf("ClassifyDisconnect(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestKeepAliveHTTPClient_HasKeepaliveDialer — the *http.Client we hand to
// websocket.Dial must have a Transport with a custom DialContext that sets
// TCP keepalive, and HTTP/2 must stay disabled. Without the dialer, dead
// TCP connections take Linux's default (~2h) to be detected, surfacing as
// "connection reset by peer" reads instead of being preempted. F-0029.
func TestKeepAliveHTTPClient_HasKeepaliveDialer(t *testing.T) {
	c := KeepAliveHTTPClient()
	if c == nil {
		t.Fatal("KeepAliveHTTPClient returned nil")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("Transport.DialContext is nil — would fall back to no-keepalive default")
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true, want false (WS upgrade dials must stay HTTP/1.1)")
	}
}

// TestLoop_PingStallDropsConnection pins C2-017/C2-031 (audit-2026-07-23):
// a HALF-OPEN venue socket must be detected by the loop itself, not left
// to OS TCP keepalive (minutes-to-hours on Linux defaults).
//
// The fake venue accepts the upgrade and then wedges — it never reads
// again, so the client's ping frame is never processed and no pong comes
// back, while the TCP connection stays established. That is exactly what a
// half-open venue socket looks like from the streamer's side. Before the
// ping watchdog, runOnce sat in a bare conn.Read on the long-lived
// connection context and returned nothing at all: no trades, no error, no
// reconnect, and no disconnect metric — the streamer looked healthy while
// ingesting zero.
func TestLoop_PingStallDropsConnection(t *testing.T) {
	wedged := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-wedged // never read a frame → never answer a ping
	}))
	defer func() {
		close(wedged)
		srv.Close()
	}()

	l := &Loop{
		Source:       "wedgedvenue",
		URL:          "ws" + strings.TrimPrefix(srv.URL, "http"),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		PingInterval: 50 * time.Millisecond,
		PingTimeout:  150 * time.Millisecond,
		HandleFrame: func([]byte) ([]canonical.Trade, error) {
			return nil, nil
		},
	}

	// The ctx deadline is the failure mode, not the success path: with no
	// watchdog runOnce blocks until it fires and then reports nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := make(chan canonical.Trade, 1)

	start := time.Now()
	err := l.runOnce(ctx, out)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStreamStalled) {
		t.Fatalf("runOnce on a wedged venue returned %v after %v, want ErrStreamStalled — "+
			"a half-open socket must reconnect, not block", err, elapsed)
	}
	if got := ClassifyDisconnect(err); got != "stall" {
		t.Errorf("ClassifyDisconnect(stall err) = %q, want %q — operators need the reason label "+
			"to tell a wedged socket from an ordinary read timeout", got, "stall")
	}
	// PingInterval + PingTimeout = 200ms; anything near the 3s ctx deadline
	// means the watchdog did not fire and the ctx did.
	if elapsed > time.Second {
		t.Errorf("stall detected after %v, want < 1s (PingInterval 50ms + PingTimeout 150ms)", elapsed)
	}
}

// TestLoop_DialTimeoutBoundsSilentUpgrade pins GH-996: a venue that
// completes TCP (and, in production, TLS) but never answers the HTTP
// upgrade must fail the dial attempt within DialTimeout. The transport's
// timeouts stop at TLS and the ping watchdog starts only after Dial
// returns, so before the per-attempt deadline runOnce blocked on the loop
// context indefinitely: no error, no "dial" disconnect, no reconnect.
func TestLoop_DialTimeoutBoundsSilentUpgrade(t *testing.T) {
	wedged := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select { // never write the 101 response
		case <-wedged:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(wedged)
		srv.Close()
	}()

	l := &Loop{
		Source:      "silentupgrade",
		URL:         "ws" + strings.TrimPrefix(srv.URL, "http"),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: 150 * time.Millisecond,
		HandleFrame: func([]byte) ([]canonical.Trade, error) {
			return nil, nil
		},
	}

	// The ctx deadline is the failure mode: without a per-attempt dial
	// deadline runOnce blocks until it fires.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	err := l.runOnce(ctx, make(chan canonical.Trade, 1))
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		t.Fatalf("runOnce returned %v only after the loop ctx expired (%v) — the upgrade wait is unbounded", err, elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runOnce on a silent upgrade returned %v after %v, want a dial deadline error", err, elapsed)
	}
	if got := ClassifyDisconnect(err); got != "dial" {
		t.Errorf("ClassifyDisconnect(%v) = %q, want %q", err, got, "dial")
	}
	if elapsed > time.Second {
		t.Errorf("dial failed after %v, want < 1s (DialTimeout 150ms)", elapsed)
	}
}

// TestLoop_DialTimeoutDoesNotLimitConnection guards the other side of
// GH-996: the dial deadline must cover only the handshake. A connection
// that outlives DialTimeout must keep delivering frames.
func TestLoop_DialTimeoutDoesNotLimitConnection(t *testing.T) {
	const dialTimeout = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		time.Sleep(3 * dialTimeout)
		if err := c.Write(r.Context(), websocket.MessageText, []byte("trade")); err != nil {
			return
		}
		_, _, _ = c.Read(r.Context()) // hold open until the client leaves
	}))
	defer srv.Close()

	want := canonical.Trade{Source: "latevenue"}
	l := &Loop{
		Source:      "latevenue",
		URL:         "ws" + strings.TrimPrefix(srv.URL, "http"),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialTimeout: dialTimeout,
		HandleFrame: func([]byte) ([]canonical.Trade, error) {
			return []canonical.Trade{want}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan canonical.Trade, 1)
	done := make(chan error, 1)
	go func() { done <- l.runOnce(ctx, out) }()

	select {
	case got := <-out:
		if got.Source != want.Source {
			t.Errorf("trade source = %q, want %q", got.Source, want.Source)
		}
	case err := <-done:
		t.Fatalf("runOnce returned %v before the post-DialTimeout frame arrived — the dial deadline leaked into the connection", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no frame received within 3s")
	}
	cancel()
	<-done
}

// TestLoop_ReadsFrameLargerThanDefaultLimit pins CA2-A18: coder/websocket's
// undocumented-to-callers 32 KiB default message limit must not survive
// Dial. Kraken's v2 trade channel batches every fill of one match into a
// single `update` frame array; a liquidity sweep of ~175+ fills alone
// exceeds 32 KiB. Without runOnce raising the limit, conn.Read fails with
// "read limited at 32769 bytes", the connection drops, and every fill in
// that frame — the highest-volume prints — is lost with only a disconnect
// counter as the signal.
func TestLoop_ReadsFrameLargerThanDefaultLimit(t *testing.T) {
	// One byte over coder/websocket's defaultReadLimit (32768); a real
	// Kraken burst frame is larger still, but this is the exact boundary
	// the unfixed code cannot cross.
	big := strings.Repeat("a", 32769)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if err := c.Write(r.Context(), websocket.MessageText, []byte(big)); err != nil {
			return
		}
		_, _, _ = c.Read(r.Context()) // hold open until the client leaves
	}))
	defer srv.Close()

	var gotLen int
	l := &Loop{
		Source: "bigframevenue",
		URL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HandleFrame: func(data []byte) ([]canonical.Trade, error) {
			gotLen = len(data)
			return []canonical.Trade{{Source: "bigframevenue"}}, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan canonical.Trade, 1)
	done := make(chan error, 1)
	go func() { done <- l.runOnce(ctx, out) }()

	select {
	case <-out:
		if gotLen != len(big) {
			t.Errorf("HandleFrame saw %d bytes, want %d — frame truncated", gotLen, len(big))
		}
	case err := <-done:
		t.Fatalf("runOnce returned %v before the oversized frame was parsed — the default 32 KiB "+
			"read limit dropped the connection instead of delivering the frame", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no frame received within 3s")
	}
	cancel()
	<-done
}
