package coinbase

import (
	"errors"
	"testing"
	"time"
)

// TestNewStreamer_DefaultInitialBackoffIs5s pins NewStreamer's 5 s default backoff, shared with Binance/Bitstamp, against
// drive-by edits.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029/G10-03)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}

// TestClassifyDisconnect_BoundedReasonLabels keeps disconnect metric label cardinality bounded; Coinbase adds
// subscription_rejected so a config-reject loop is distinguishable from wire drops.
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
