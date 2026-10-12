package bitstamp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

func TestStreamer_Name(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestStreamer_Class(t *testing.T) {
	s := NewStreamer(map[string]canonical.Pair{})
	if got := s.Class(); got != external.ClassExchange {
		t.Errorf("Class() = %q, want %q", got, external.ClassExchange)
	}
}

func TestDefaultPairList_matchesDefaultPairs(t *testing.T) {
	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	list, err := DefaultPairList()
	if err != nil {
		t.Fatalf("DefaultPairList: %v", err)
	}
	if len(list) != len(m) {
		t.Errorf("list len = %d, want %d", len(list), len(m))
	}
	if len(list) == 0 {
		t.Error("DefaultPairList returned empty slice")
	}
}

// Streamer.Start has two synchronous reject paths that never touch the network (empty pairs, pair not in PairMap); both are
// pinned so no goroutine spins dialling a venue we cannot subscribe to.

func TestStreamer_Start_emptyPairsRejected(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	if _, err := s.Start(context.Background(), nil); err == nil {
		t.Error("expected error on empty pairs, got nil")
	}
	if _, err := s.Start(context.Background(), []canonical.Pair{}); err == nil {
		t.Error("expected error on zero-length pairs, got nil")
	}
}

func TestStreamer_Start_unknownPairRejected(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	// MATIC: in allow-list, intentionally not in DefaultPairs.
	matic, _ := canonical.NewCryptoAsset("MATIC")
	usd, _ := canonical.NewFiatAsset("USD")
	missing, err := canonical.NewPair(matic, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	_, err = s.Start(context.Background(), []canonical.Pair{missing})
	if err == nil {
		t.Fatal("expected error for unknown MATIC/USD pair, got nil")
	}
	if !strings.Contains(err.Error(), "MATIC") {
		t.Errorf("error %q should cite the offending asset", err.Error())
	}
}

// Bitstamp shares the same backoff drift as Binance — see
// the equivalent test in the binance package for full context.
func TestNewStreamer_DefaultInitialBackoffIs5s(t *testing.T) {
	s := NewStreamer(nil)
	if got, want := s.InitialBackoff, 5*time.Second; got != want {
		t.Errorf("InitialBackoff = %v, want %v (F-0029)", got, want)
	}
	if got, want := s.MaxBackoff, 60*time.Second; got != want {
		t.Errorf("MaxBackoff = %v, want %v", got, want)
	}
}

// TestClassifyDisconnect — covers the additional server_requested
// reason that bitstamp uses for the bts:request_reconnect path.
func TestClassifyDisconnect(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "other"},
		{"server_requested", ErrRequestedReconnect, "server_requested"},
		{"reset", errors.New("read: ...: read: connection reset by peer"), "reset"},
		{"broken_pipe", errors.New("write: broken pipe"), "broken_pipe"},
		{"timeout", errors.New("read: i/o timeout"), "timeout"},
		{"dial", errors.New("dial: lookup ws.bitstamp.net: no such host"), "dial"},
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

func TestRestBase_emptyEndpointFallsBackToREST(t *testing.T) {
	s := &Streamer{}
	if got := s.restBase(); got != RESTEndpoint {
		t.Errorf("restBase() = %q, want %q", got, RESTEndpoint)
	}
}

func TestRestBase_wsEndpointFallsBackToREST(t *testing.T) {
	for _, prefix := range []string{"ws://example.com", "wss://example.com"} {
		s := &Streamer{Endpoint: prefix}
		if got := s.restBase(); got != RESTEndpoint {
			t.Errorf("restBase(%q) = %q, want %q", prefix, got, RESTEndpoint)
		}
	}
}

func TestRestBase_httpEndpointPassesThrough(t *testing.T) {
	s := &Streamer{Endpoint: "http://127.0.0.1:9999"}
	if got := s.restBase(); got != "http://127.0.0.1:9999" {
		t.Errorf("restBase() = %q, want passthrough", got)
	}
}

// TestRecordSubscriptionAck_RejectionSetsGaugeAndLogs: a rejected bts:error subscription must set
// obs.CEXStreamSubscriptionRejected and log.
func TestRecordSubscriptionAck_RejectionSetsGaugeAndLogs(t *testing.T) {
	// Distinct pair from other bitstamp tests so the shared gauge's
	// state can't collide across parallel test runs.
	const rejectedSymbol = "xlmbtc_ca2a18h3"

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	recordSubscriptionAck(logger, &subscriptionAck{
		Channel:  ChannelPrefix + rejectedSymbol,
		Message:  "Bad subscription string.",
		Accepted: false,
	})

	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, rejectedSymbol)); v != 1 {
		t.Errorf("subscription_rejected{symbol=%s} = %v, want 1", rejectedSymbol, v)
	}
	if got := logs.String(); !strings.Contains(got, "rejected") {
		t.Errorf("expected a rejection log line, got %q", got)
	}

	recordSubscriptionAck(logger, &subscriptionAck{
		Channel:  ChannelPrefix + rejectedSymbol,
		Accepted: true,
	})
	if v := testutil.ToFloat64(obs.CEXStreamSubscriptionRejected.WithLabelValues(SourceName, rejectedSymbol)); v != 0 {
		t.Errorf("subscription_rejected{symbol=%s} = %v, want 0 after acceptance clears it", rejectedSymbol, v)
	}
}

// symbolsFor inverts PairMap into the Bitstamp wire symbols to subscribe to; a pair missing from the map is fatal, not skipped.

func TestSymbolsFor_happyPath(t *testing.T) {
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	xlmusd := pm["xlmusd"]
	btcusd := pm["btcusd"]
	got, err := s.symbolsFor([]canonical.Pair{xlmusd, btcusd})
	if err != nil {
		t.Fatalf("symbolsFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want := map[string]bool{"xlmusd": true, "btcusd": true}
	for _, sym := range got {
		if !want[sym] {
			t.Errorf("unexpected symbol %q", sym)
		}
	}
}

func TestSymbolsFor_unknownPairRejected(t *testing.T) {
	// A pair with no PairMap entry must error, not be silently omitted, or the venue would look quiet downstream.
	pm, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(pm)

	// MATIC: in allow-list, intentionally not in DefaultPairs.
	matic, err := canonical.NewCryptoAsset("MATIC")
	if err != nil {
		t.Fatalf("NewCryptoAsset(MATIC): %v", err)
	}
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("NewFiatAsset(USD): %v", err)
	}
	missing, err := canonical.NewPair(matic, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	if _, err := s.symbolsFor([]canonical.Pair{missing}); err == nil {
		t.Fatal("expected error for MATIC/USD (not in DefaultPairs), got nil")
	}
}
