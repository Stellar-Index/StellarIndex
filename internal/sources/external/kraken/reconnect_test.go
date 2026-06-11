package kraken

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// G10-03 (F-0029 port): Kraken shares the same healthy-connection
// backoff drift that Binance/Bitstamp were fixed for. NewStreamer's
// default backoff is 5 s — large enough to avoid hammering Kraken on a
// venue-wide outage, small enough that the per-cycle data-loss window
// is ~5 s on a healthy connection. This test pins the defaults so a
// drive-by edit can't silently regress them.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029/G10-03)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}

// TestClassifyDisconnect_BoundedReasonLabels — keeps the disconnect
// metric's label cardinality bounded. Add to this table when adding a
// new reason; that's the operator contract.
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
		{"dial", errors.New("dial: lookup ws.kraken.com: no such host"), "dial"},
		{"other", errors.New("EOF"), "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDisconnect(tc.err); got != tc.want {
				t.Errorf("classifyDisconnect(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestKeepAliveHTTPClient_HasKeepaliveDialer — the *http.Client we hand
// to websocket.Dial must have a Transport with a custom DialContext
// that sets TCP keepalive. Without this, dead TCP connections take
// Linux's default (~2h) to be detected, surfacing as "connection reset
// by peer" reads instead of being preempted in the dialer. F-0029.
func TestKeepAliveHTTPClient_HasKeepaliveDialer(t *testing.T) {
	c := keepAliveHTTPClient()
	if c == nil {
		t.Fatal("keepAliveHTTPClient returned nil")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("Transport.DialContext is nil — would fall back to no-keepalive default")
	}
}
