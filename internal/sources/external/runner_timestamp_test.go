package external

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

func TestPlausibleTradeTime(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   time.Time
		want bool
	}{
		{"now", now, true},
		{"within_skew", now.Add(4 * time.Minute), true},
		{"past_skew", now.Add(6 * time.Minute), false},
		{"microseconds_read_as_millis", time.UnixMilli(now.UnixMilli() * 1000).UTC(), false},
		{"seconds_read_as_millis", time.UnixMilli(now.Unix()).UTC(), false},
		{"recent_past", now.Add(-48 * time.Hour), true},
	}
	for _, tc := range cases {
		if got := plausibleTradeTime(tc.ts, now); got != tc.want {
			t.Errorf("%s: plausibleTradeTime(%s) = %v, want %v", tc.name, tc.ts, got, tc.want)
		}
	}
}

func TestForwardTrades_DropsImplausibleTimestamp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	bad := testTrade(t, "ts-venue", 1)
	bad.Timestamp = time.Now().Add(24 * time.Hour)
	good := testTrade(t, "ts-venue", 2)

	in := make(chan canonical.Trade, 2)
	in <- bad
	in <- good
	close(in)
	sink := make(chan consumer.Event, 2)
	forwardTrades(ctx, "ts-venue", in, sink, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if len(sink) != 1 {
		t.Fatalf("forwarded %d trades, want 1 (the plausible one)", len(sink))
	}
	if te := (<-sink).(TradeEvent); te.Trade.Ledger != 2 {
		t.Errorf("forwarded ledger %d, want 2", te.Trade.Ledger)
	}
}

func TestEmitPollResults_DropsImplausibleTimestamp(t *testing.T) {
	bad := testTrade(t, "ts-venue", 1)
	bad.Timestamp = time.Now().Add(24 * time.Hour)
	good := testTrade(t, "ts-venue", 2)

	sink := make(chan consumer.Event, 2)
	emitPollResults(context.Background(), "ts-venue", sink, []canonical.Trade{bad, good}, nil)

	if len(sink) != 1 {
		t.Fatalf("emitted %d trades, want 1 (the plausible one)", len(sink))
	}
}
