package orchestrator

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
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// oldestOfCappedRead is the coveredFrom of the plain capped read: the oldest of the newest limit rows.
func oldestOfCappedRead(t *testing.T, s *sumsStore, pair canonical.Pair, from, to time.Time, limit int) time.Time {
	t.Helper()
	rows, err := s.TradesInRange(context.Background(), pair, from, to, limit)
	if err != nil || len(rows) != limit {
		t.Fatalf("fixture read: %d rows, %v", len(rows), err)
	}
	s.rawReads = nil
	return rows[0].Timestamp
}

// A failed live-tail read is a refusal, never an empty tail: the leg keeps the capped read, flagged with the reason.
func TestFillFromMinuteSums_FailedTailReadStaysTruncated(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 22, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	s := &sumsStore{raw: burstWindow(t, pair, from), watermark: to.Add(-150 * time.Second), liveErr: errors.New("conn reset")}
	want := oldestOfCappedRead(t, s, pair, from, to, 95*6)
	o := coverageOrch(t, s, 95*6, 0)
	var logs bytes.Buffer
	o.logger = slog.New(slog.NewTextHandler(&logs, nil))
	counter := obs.AggregatorWindowTruncatedTotal.WithLabelValues(pair.String(), "5m")
	before := testutil.ToFloat64(counter)

	got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !coveredFrom.Equal(want) || len(got) != 95*6 || weights != nil {
		t.Errorf("coveredFrom=%v rows=%d weights=%d; want the capped read from %v", coveredFrom, len(got), len(weights), want)
	}
	if d := testutil.ToFloat64(counter) - before; d != 1 {
		t.Errorf("truncated counter advanced by %v, want 1", d)
	}
	if !strings.Contains(logs.String(), "unmaterialised-tail read failed: conn reset") {
		t.Errorf("truncation log lacks the tail-read reason:\n%s", logs.String())
	}
}

// A view lagging past the window start puts the whole pre-raw part in the live tail, which starts at the window, not before it.
func TestFillFromMinuteSums_WatermarkBeforeWindowStart(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 22, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	window := burstWindow(t, pair, from)
	before := burstWindow(t, pair, from.Add(-5*time.Minute))
	for i := range before {
		before[i].OpIndex += 1_000_000
	}
	s := &sumsStore{raw: append(before, window...), watermark: from.Add(-2 * time.Minute)}
	o := coverageOrch(t, s, 95*6, 0)

	got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !coveredFrom.IsZero() {
		t.Fatalf("coveredFrom = %v; want the whole window covered", coveredFrom)
	}
	if len(s.liveReads) != 1 || s.liveReads[0] != [2]time.Time{from, from.Add(4 * time.Minute)} {
		t.Errorf("live reads %v, want one from the window start %v", s.liveReads, from)
	}
	if n := tradeCount(got, weights); n != len(window) {
		t.Errorf("trade count = %d, want %d", n, len(window))
	}
	if have, want := sumRatio(got, pair), sumRatio(window, pair); have.Cmp(want) != 0 {
		t.Errorf("VWAP = %s, want %s over the window's trades only", have.FloatString(18), want.FloatString(18))
	}
}

// A wide seam minute (the one the cap cut inside) that cannot be re-read would end coverage after the plain capped read; the
// leg keeps the capped read instead, so the fill never serves less than it.
func TestFillFromMinuteSums_UnresolvedSeamMinuteKeepsCappedRead(t *testing.T) {
	pair := mkPair(t, "crypto", "XLM", "fiat", "USD")
	from := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	s := &sumsStore{raw: busyWindow(t, pair, from), watermark: to, rereadErr: errors.New("timeout")}
	cf := oldestOfCappedRead(t, s, pair, from, to, 100)
	if !cf.After(cf.Truncate(time.Minute)) {
		t.Fatalf("fixture: the cap must cut inside a minute, got %v", cf)
	}
	evil := makeTradeOn(t, pair, "sdex", 1_0000000, 3_000_000, cf.Truncate(time.Minute).Add(time.Millisecond))
	evil.OpIndex = 999999
	s.raw = append(s.raw, evil)
	o := coverageOrch(t, s, 100, 4)
	var logs bytes.Buffer
	o.logger = slog.New(slog.NewTextHandler(&logs, nil))
	wideCtr := obs.AggregatorWindowWideMinutesUnresolvedTotal.WithLabelValues(pair.String(), "1h")
	before := testutil.ToFloat64(wideCtr)

	got, weights, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, time.Hour, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if d := testutil.ToFloat64(wideCtr) - before; d != 1 {
		t.Errorf("wide-minutes-unresolved counter advanced by %v, want 1", d)
	}
	if !coveredFrom.Equal(cf) || len(got) != 100 || weights != nil {
		t.Errorf("coveredFrom=%v rows=%d weights=%d; want the capped read from %v", coveredFrom, len(got), len(weights), cf)
	}
	if !strings.Contains(logs.String(), "wide seam minute") {
		t.Errorf("truncation log lacks the seam reason:\n%s", logs.String())
	}
}

// Within one tick, a pair's windows sharing a live tail read it once; outside a tick there is no memo.
func TestFillFromMinuteSums_LiveTailMemoisedPerTick(t *testing.T) {
	pair := mkPair(t, "crypto", "BTC", "fiat", "USD")
	from := time.Date(2026, 11, 27, 2, 22, 0, 0, time.UTC)
	to := from.Add(5 * time.Minute)
	for _, tc := range []struct {
		inTick bool
		reads  int
	}{{false, 2}, {true, 1}} {
		s := &sumsStore{raw: burstWindow(t, pair, from), watermark: to.Add(-150 * time.Second)}
		o := coverageOrch(t, s, 95*6, 0)
		if tc.inTick {
			o.tickLiveTails = map[string][]timescale.TradeMinuteSum{}
		}
		var vwaps []string
		for i := 0; i < 2; i++ {
			got, _, coveredFrom, err := o.fetchTradesDetectTruncation(context.Background(), pair, pair, 5*time.Minute, from, to)
			if err != nil || !coveredFrom.IsZero() {
				t.Fatalf("read %d: coveredFrom=%v err=%v", i, coveredFrom, err)
			}
			vwaps = append(vwaps, sumRatio(got, pair).RatString())
		}
		if len(s.liveReads) != tc.reads || vwaps[0] != vwaps[1] {
			t.Errorf("inTick=%v: %d live reads (want %d), VWAPs %v", tc.inTick, len(s.liveReads), tc.reads, vwaps)
		}
	}
}
