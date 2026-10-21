package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type countingMarketSources struct{ pair, asset atomic.Int64 }

func (r *countingMarketSources) PairSourceStats(context.Context, []string, []string) ([]timescale.SourceStats, error) {
	r.pair.Add(1)
	return []timescale.SourceStats{{Source: "sdex", TradeCount24h: 1}}, nil
}

func (r *countingMarketSources) AssetSourceStats(context.Context, []string) ([]timescale.SourceStats, error) {
	r.asset.Add(1)
	return []timescale.SourceStats{{Source: "sdex", TradeCount24h: 2}}, nil
}

// Repeat reads of one asset or pair share a fill, an asset and a pair never share a key, and an expired entry is served at once,
// flagged stale and dated by its fill time, while one refresh runs.
func TestMarketSources_CachedServesStaleWithFillTime(t *testing.T) {
	up := &countingMarketSources{}
	c := NewCachedMarketSourceReader(up, time.Minute)
	h := New(Options{MarketSources: c}).Handler()
	const asset = "/v1/markets/sources?asset=native"
	serveThrough(t, h, asset, "")
	serveThrough(t, h, asset, "")
	serveThrough(t, h, "/v1/markets/sources?base=native&quote=USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", "")
	if a, p := up.asset.Load(), up.pair.Load(); a != 1 || p != 1 {
		t.Fatalf("upstream calls asset=%d pair=%d; want 1 each", a, p)
	}

	old := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	c.cache.mu.Lock()
	for _, e := range c.cache.entries {
		e.at = old
	}
	c.cache.mu.Unlock()
	rec := serveThrough(t, h, asset, "")
	assertAsOf(t, rec, old)
	var body struct {
		Flags struct {
			Stale bool `json:"stale"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.Flags.Stale {
		t.Fatalf("flags.stale = %v (err %v); want true for an expired entry", body.Flags.Stale, err)
	}
	if !waitFor(time.Second, func() bool { return up.asset.Load() == 2 }) {
		t.Fatalf("asset upstream calls = %d; want one background refresh", up.asset.Load())
	}
}

// Client-chosen keys cannot grow the map past its cap; the oldest fill goes first.
func TestSWRCache_EvictsOldestAtCapacity(t *testing.T) {
	c := newSWRCache[int](nil, "test", "test-fill", time.Minute, 2)
	for i := range 3 {
		if _, _, _, err := c.get(context.Background(), "op", fmt.Sprint(i), func(context.Context) (int, error) { return i, nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries["0"]; ok || len(c.entries) != 2 {
		t.Fatalf("entries = %d, has oldest = %v; want 2 without key 0", len(c.entries), ok)
	}
}

// A failed refresh is logged, else the refresh_error alert's runbook has nothing to grep.
func TestSWRCache_LogsFailedRefresh(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	c := newSWRCache[int](logger, "test", "test-fill", time.Nanosecond, 0)
	if _, _, _, err := c.get(context.Background(), "op", "k", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.get(context.Background(), "op", "k", func(context.Context) (int, error) { return 0, errors.New("statement timeout") }); err != nil {
		t.Fatal(err)
	}
	logged := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(buf.String(), "statement timeout") && strings.Contains(buf.String(), "op=op")
	}
	if !waitFor(time.Second, logged) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("log = %q; want the refresh error with its op", buf.String())
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
