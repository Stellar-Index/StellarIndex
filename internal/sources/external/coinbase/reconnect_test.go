package coinbase

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// G10-03 (F-0029 port): Coinbase shares the same healthy-connection
// backoff drift that Binance/Bitstamp were fixed for. NewStreamer's
// default backoff is 5 s. This test pins the defaults so a drive-by
// edit can't silently regress them.
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
// metric's label cardinality bounded. Coinbase adds the
// subscription_rejected reason on top of the shared set so a config-
// reject loop is distinguishable from transient wire drops.
func TestClassifyDisconnect_BoundedReasonLabels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "other"},
		{"subscription_rejected", ErrSubscriptionRejected, "subscription_rejected"},
		{"reset", errors.New("read: failed to read frame payload: read tcp 1.2.3.4:443: read: connection reset by peer"), "reset"},
		{"broken_pipe", errors.New("write: broken pipe"), "broken_pipe"},
		{"timeout", errors.New("read: i/o timeout"), "timeout"},
		{"dial", errors.New("dial: lookup ws-feed.exchange.coinbase.com: no such host"), "dial"},
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
// that sets TCP keepalive. F-0029.
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
