// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package wsclient

import (
	"errors"
	"net/http"
	"testing"
	"time"
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
