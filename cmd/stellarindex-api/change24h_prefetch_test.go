package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	c24None     = iota
	c24Anchor   // a bucket one minute before the instant asked
	c24TooOld   // a bucket outside Change24hAnchorTolerance
	c24Universe = 100
)

// c24Store is the anchor reads, answering from the minute of the instant asked so an answer for another anchor shows
// in the served percentage. Its set forms answer as the singles do, or fail as fault says.
type c24Store struct {
	kind map[string]int
	// "", "error", "panic" or "slow" (outlasts the planner's budget) for the anchor statements; "baseline-error" or
	// "baseline-panic" for the baseline statements.
	fault          string
	single, before atomic.Int64
}

func c24VWAP(b time.Time) string { return fmt.Sprintf("1.%02d", 10+b.Minute()) }

func (s *c24Store) anchor(p canonical.Pair, t time.Time) (timescale.Vwap1mRow, error) {
	b := t.Truncate(time.Minute).Add(-time.Minute)
	switch s.kind[p.String()] {
	case c24Anchor:
	case c24TooOld:
		b = b.Add(-timescale.Change24hAnchorTolerance - 10*time.Minute)
	default:
		return timescale.Vwap1mRow{}, sql.ErrNoRows
	}
	return timescale.Vwap1mRow{Bucket: b, VWAP: c24VWAP(b), TradeCount: 5}, nil
}

func (s *c24Store) baseline(before time.Time, limit int) []timescale.Vwap1mRow {
	rows := make([]timescale.Vwap1mRow, 0, limit)
	for i := 1; i <= 12 && i <= limit; i++ {
		rows = append(rows, timescale.Vwap1mRow{Bucket: before.Add(-time.Duration(i) * time.Minute), VWAP: c24VWAP(before), TradeCount: 5})
	}
	return rows
}

func (s *c24Store) ClosedVWAP1mAtOrBefore(_ context.Context, p canonical.Pair, t time.Time) (timescale.Vwap1mRow, error) {
	s.single.Add(1)
	return s.anchor(p, t)
}

func (s *c24Store) ClosedVWAP1mCombinedBefore(_ context.Context, _ canonical.Pair, before time.Time, limit int) ([]timescale.Vwap1mRow, error) {
	s.before.Add(1)
	return s.baseline(before, limit), nil
}

func (s *c24Store) setFault(stage string) error {
	switch s.fault {
	case stage + "error":
		return errors.New("set read failed")
	case stage + "panic":
		panic("set read panicked")
	case stage + "slow":
		time.Sleep(1500 * time.Millisecond)
	}
	return nil
}

func (s *c24Store) ClosedVWAP1mAtOrBeforeSet(_ context.Context, pairs []canonical.Pair, t time.Time) ([]timescale.KeyResult[timescale.Vwap1mRow], error) {
	if err := s.setFault(""); err != nil {
		return nil, err
	}
	out := make([]timescale.KeyResult[timescale.Vwap1mRow], len(pairs))
	for i, p := range pairs {
		out[i].Value, out[i].Err = s.anchor(p, t)
	}
	return out, nil
}

func (s *c24Store) ClosedVWAP1mCombinedBeforeSet(_ context.Context, pairs []canonical.Pair, before time.Time, limit int) ([][]timescale.Vwap1mRow, error) {
	if err := s.setFault("baseline-"); err != nil {
		return nil, err
	}
	out := make([][]timescale.Vwap1mRow, len(pairs))
	for i := range pairs {
		out[i] = s.baseline(before, limit)
	}
	return out, nil
}

// c24Fixture is 100 USD-quoted ids: an anchor in the literal USD pair, one in the peg pair only, none, and one too old.
func c24Fixture(t *testing.T) (ids []string, peg canonical.Asset, kind map[string]int) {
	t.Helper()
	peg, err := canonical.NewClassicAsset("USDC", "GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5")
	if err != nil {
		t.Fatal(err)
	}
	kind = map[string]int{}
	for i := range c24Universe {
		a, err := canonical.NewClassicAsset(fmt.Sprintf("C%d", i), "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.String())
		usd, pegged := canonical.Pair{Base: a, Quote: usdQuoteAsset}.String(), canonical.Pair{Base: a, Quote: peg}.String()
		switch i % 4 {
		case 0:
			kind[usd] = c24Anchor
		case 1:
			kind[pegged] = c24Anchor
		case 3:
			kind[usd] = c24TooOld
		}
	}
	return ids, peg, kind
}

var c24Volatile = regexp.MustCompile(`"(as_of|request_id|instance)":"[^"]*"`)

// c24Batch serves the batch through the production change_24h chain over s, its cache on a fixed minute.
func c24Batch(t *testing.T, s change24hStore, peg canonical.Asset, minute time.Time, ids []string) (string, *minuteChange24hReader) {
	t.Helper()
	cache := newMinuteChange24hReader(storeChange24hReader{s: s, pegs: []canonical.Asset{peg}})
	cache.now = func() time.Time { return minute }
	srv := httptest.NewServer(v1.New(v1.Options{Prices: closedBucketReader{price: "2.0"}, Change24h: cache}).Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/price/batch?quote=fiat:USD&asset_ids=" + strings.Join(ids, ",")) //nolint:noctx // test server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("batch: %d %v %s", resp.StatusCode, err, body)
	}
	return c24Volatile.ReplaceAllString(string(body), `"$1":""`), cache
}

func c24Count(seam, result string) float64 {
	return testutil.ToFloat64(obs.APIPriceBatchPrefetchTotal.WithLabelValues(seam, result))
}

// With the change_24h anchors prefetched, a 100-id fiat:USD batch is byte-identical to the same chain without the
// prefetch, and the process-wide cache holds exactly what it would have; a failing, panicking or late set read
// changes nothing.
func TestChange24hPrefetch_BatchAndCacheMatchThePlannerOff(t *testing.T) {
	ids, peg, kind := c24Fixture(t)
	for _, fault := range []string{"", "error", "panic", "slow", "baseline-error", "baseline-panic"} {
		t.Run("fault="+fault, func(t *testing.T) {
			// The anchor is per-minute: a run straddling a minute would read another minute's anchors on one side.
			if s := time.Now().Second(); s > 50 {
				time.Sleep(time.Duration(61-s) * time.Second)
			}
			minute := time.Now().Truncate(time.Minute)
			offStore, onStore := &c24Store{kind: kind}, &c24Store{kind: kind, fault: fault}
			hits := c24Count("anchor_vwap_1m", "hit") + c24Count("anchor_trailing_vwap_1m", "hit")
			baselineFailed := c24Count("anchor_trailing_vwap_1m", "prefetch_error")
			off, offCache := c24Batch(t, offStore, peg, minute, ids)
			on, onCache := c24Batch(t, timescale.PrefetchAnchors{AnchorReads: onStore}, peg, minute, ids)
			if on != off {
				t.Fatalf("planner on differs\n off %s\n on  %s", off, on)
			}
			if n := strings.Count(off, `"change_24h_pct"`); n != c24Universe/2 {
				t.Fatalf("%d rows carry change_24h_pct, want %d: the fixture lost its present or absent anchors", n, c24Universe/2)
			}
			if !onCache.minute.Equal(offCache.minute) || !maps.Equal(onCache.vals, offCache.vals) || len(onCache.vals) != c24Universe/2 {
				t.Fatalf("cache on %v %v, off %v %v", onCache.minute, onCache.vals, offCache.minute, offCache.vals)
			}
			served := c24Count("anchor_vwap_1m", "hit") + c24Count("anchor_trailing_vwap_1m", "hit") - hits
			rowReads := onStore.single.Load() + onStore.before.Load()
			t.Logf("rows read %d anchors and baselines from the memo, %d as before (planner off: %d)",
				int(served), rowReads, offStore.single.Load()+offStore.before.Load())
			switch fault {
			case "":
				if served == 0 || rowReads != 0 {
					t.Fatalf("%v reads served from the memo and %d made as before, want every one from the memo", served, rowReads)
				}
			case "error", "panic":
				if onStore.single.Load() != offStore.single.Load() {
					t.Fatalf("anchor reads %d, planner off %d: a failed set read must leave every key to be read as before",
						onStore.single.Load(), offStore.single.Load())
				}
			case "baseline-error", "baseline-panic":
				if onStore.single.Load() != 0 || onStore.before.Load() != offStore.before.Load() {
					t.Fatalf("anchor reads %d (want 0), baseline reads %d (planner off %d): a failed baseline statement must"+
						" leave every baseline to be read as before", onStore.single.Load(), onStore.before.Load(), offStore.before.Load())
				}
				if d := c24Count("anchor_trailing_vwap_1m", "prefetch_error") - baselineFailed; fault == "baseline-error" && d != c24Universe/2 {
					t.Fatalf("baseline prefetch_error = %v, want one per key (%d)", d, c24Universe/2)
				}
			}
		})
	}
}
