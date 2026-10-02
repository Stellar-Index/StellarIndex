package forex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func TestListAggBarsKeepsCloseText(t *testing.T) {
	start := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			if !strings.HasPrefix(r.URL.Path, "/v2/aggs/ticker/C:USDEUR/range/1/hour/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = fmt.Fprintf(w, `{"results":[{"T":"C:USDEUR","t":%d,"c":0.7412345678901234567}],"next_url":"%s/v2/aggs/next?cursor=p2"}`,
				start.UnixMilli(), base)
			return
		}
		_, _ = fmt.Fprintf(w, `{"results":[{"t":%d,"c":0.92}]}`, start.Add(time.Hour).UnixMilli())
	}))
	defer srv.Close()
	base = srv.URL

	bars, err := NewClient("k").WithBase(srv.URL).ListAggBars(context.Background(), "eur", GrainHour, start, start.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ListAggBars: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("got %d bars across two pages, want 2", len(bars))
	}
	b := bars[0]
	if b.CloseText != "0.7412345678901234567" {
		t.Errorf("CloseText = %q, want the vendor's text verbatim", b.CloseText)
	}
	if b.Ticker != "EUR" || b.Grain != GrainHour || b.Source != fxSource {
		t.Errorf("bar identity = %s/%s/%s", b.Ticker, b.Grain, b.Source)
	}
	if !b.BarStart.Equal(start) || !b.BarEnd.Equal(start.Add(time.Hour)) {
		t.Errorf("bar span = %s..%s, want %s..+1h", b.BarStart, b.BarEnd, start)
	}
	if !bars[1].BarStart.Equal(start.Add(time.Hour)) || bars[1].CloseText != "0.92" {
		t.Errorf("second page bar = %+v", bars[1])
	}

	offHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"results":[],"next_url":"https://elsewhere.invalid/v2/aggs?cursor=x"}`)
	}))
	defer offHost.Close()
	if _, err := NewClient("k").WithBase(offHost.URL).ListAggBars(context.Background(), "EUR", GrainHour, start, start); err == nil ||
		!strings.Contains(err.Error(), "off-host") {
		t.Fatalf("off-host next_url: err = %v, want a refusal", err)
	}
}

// hourlyBars is an hourly EUR series starting at start, one bar per close.
func hourlyBars(start time.Time, closes ...string) []FXBar {
	out := make([]FXBar, len(closes))
	for i, c := range closes {
		s := start.Add(time.Duration(i) * time.Hour)
		out[i] = FXBar{Ticker: "EUR", Grain: GrainHour, BarStart: s, BarEnd: s.Add(time.Hour), CloseText: c, Source: fxSource}
	}
	return out
}

func repeat(v string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func allBars(FXBar) bool { return true }

func TestFixingGateMedianReference(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	t.Run("first bar has no reference and is accepted", func(t *testing.T) {
		acc, ref := GateFixings(hourlyBars(t0, "1000"), allBars)
		if len(acc) != 1 || len(ref) != 0 {
			t.Fatalf("accepted %d refused %d, want 1/0", len(acc), len(ref))
		}
	})

	t.Run("band edges are inclusive", func(t *testing.T) {
		for _, tc := range []struct {
			close string
			ok    bool
		}{{"1.49", true}, {"1.5", true}, {"1.51", false}, {"0.5", true}, {"0.49", false}} {
			series := hourlyBars(t0, append(repeat("1", 24), tc.close)...)
			acc, _ := GateFixings(series, func(b FXBar) bool { return b.BarStart.Equal(series[24].BarStart) })
			if got := len(acc) == 1; got != tc.ok {
				t.Errorf("close %s after 24×1: accepted=%v, want %v", tc.close, got, tc.ok)
			}
		}
	})

	t.Run("a spike is refused and the next bar is not", func(t *testing.T) {
		series := hourlyBars(t0, append(repeat("1", 24), "3", "1")...)
		acc, ref := GateFixings(series, allBars)
		if len(ref) != 1 || ref[0].CloseText != "3" {
			t.Fatalf("refused = %+v, want only the spike", ref)
		}
		if last := acc[len(acc)-1]; !last.BarStart.Equal(series[25].BarStart) {
			t.Fatalf("bar after the spike not accepted")
		}
	})

	// Refused bars stay in the reference, so a real level shift is
	// accepted once it is half of the last 24 raw bars.
	t.Run("a level shift is accepted after twelve bars", func(t *testing.T) {
		series := hourlyBars(t0, append(repeat("1", 24), repeat("3", 14)...)...)
		_, ref := GateFixings(series, func(b FXBar) bool { return !b.BarStart.Before(series[24].BarStart) })
		if len(ref) != 12 {
			t.Fatalf("refused %d shifted bars, want 12", len(ref))
		}
	})

	// The live appender sees 144h of bars and gates the last 48h; the
	// backfill sees the whole series. Both must reach the same verdicts.
	t.Run("live and paged inputs agree", func(t *testing.T) {
		closes := make([]string, 240)
		for i := range closes {
			closes[i] = "1." + strconv.Itoa(10+i%7)
			if i%37 == 0 {
				closes[i] = "5"
			}
		}
		full := hourlyBars(t0, closes...)
		now := full[len(full)-1].BarEnd.Add(10 * time.Minute)
		var live []FXBar
		for _, b := range full {
			if !b.BarStart.Before(now.Add(-fixingLiveWindow - FixingReferenceWindow)) {
				live = append(live, b)
			}
		}
		accLive, refLive := liveFixingCandidates(live, now)
		accFull, refFull := liveFixingCandidates(full, now)
		if fmt.Sprint(starts(accLive)) != fmt.Sprint(starts(accFull)) || fmt.Sprint(starts(refLive)) != fmt.Sprint(starts(refFull)) {
			t.Fatalf("live verdicts differ from the full series:\nlive acc=%v ref=%v\nfull acc=%v ref=%v",
				starts(accLive), starts(refLive), starts(accFull), starts(refFull))
		}
		if len(refLive) == 0 || len(accLive) == 0 {
			t.Fatalf("fixture exercises nothing: accepted %d refused %d", len(accLive), len(refLive))
		}
	})
}

func starts(bars []FXBar) []int64 {
	out := make([]int64, len(bars))
	for i, b := range bars {
		out[i] = b.BarStart.Unix()
	}
	return out
}

type recordingFixingWriter struct {
	mu       sync.Mutex
	bars     []FXBar
	onInsert func()
}

func (r *recordingFixingWriter) InsertFXFixingBatch(_ context.Context, bars []FXBar) error {
	if r.onInsert != nil {
		r.onInsert()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bars = append(r.bars, bars...)
	return nil
}

// The UZS current-feed incident replayed on the hourly series: a lone
// broken bar is refused by the gate; a vendor that serves the broken level
// for long enough passes the gate but disagrees with the guarded daily rate.
func TestFixingGateUZSBrokenBar(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := today.Add(-24 * time.Hour)
	guarded := guardedDailyRates(&Snapshot{History7d: map[string][]HistoryPoint{"UZS": uzsBars(today)}})["UZS"]
	w := &Worker{logger: discardLogger(), fixingWriter: &recordingFixingWriter{}}
	uzs := func(closes ...string) []FXBar {
		out := hourlyBars(day, closes...)
		for i := range out {
			out[i].Ticker = "UZS"
		}
		return out
	}

	refusedBefore := testutil.ToFloat64(obs.FXFixingsBarsRefusedTotal.WithLabelValues("UZS"))
	acc, ref := GateFixings(uzs(append(repeat("11800", 20), "1820")...), allBars)
	w.writeFixings(context.Background(), "UZS", acc, ref, guarded)
	if len(ref) != 1 || ref[0].CloseText != "1820" {
		t.Fatalf("refused = %+v, want the lone 1820 bar", ref)
	}
	if got := testutil.ToFloat64(obs.FXFixingsBarsRefusedTotal.WithLabelValues("UZS")) - refusedBefore; got != 1 {
		t.Errorf("bars_refused_total delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(obs.FXFixingsQuoteDisagrees.WithLabelValues("UZS")); got != 0 {
		t.Errorf("quote_disagrees = %v after only agreeing bars were accepted, want 0", got)
	}

	acc, ref = GateFixings(uzs(repeat("1820", 20)...), allBars)
	if len(ref) != 0 {
		t.Fatalf("a self-consistent series was refused: %d bars", len(ref))
	}
	w.writeFixings(context.Background(), "UZS", acc, ref, guarded)
	if got := testutil.ToFloat64(obs.FXFixingsQuoteDisagrees.WithLabelValues("UZS")); got != 1 {
		t.Errorf("quote_disagrees = %v, want 1 against the guarded ~11,800", got)
	}
}

// aggsServer answers the per-ticker aggregates endpoint with one bar per
// hour in the requested range, closing at closeFor(ticker), or with status.
func aggsServer(hits *map[string]int, mu *sync.Mutex, status int, closeFor func(string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/") // "", v2, aggs, ticker, C:USDXXX, range, 1, hour, from, to
		ticker := strings.TrimPrefix(parts[4], "C:USD")
		mu.Lock()
		(*hits)[ticker]++
		mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		from, _ := strconv.ParseInt(parts[8], 10, 64)
		to, _ := strconv.ParseInt(parts[9], 10, 64)
		var rows []string
		if closeFor != nil {
			for s := time.UnixMilli(from).UTC().Truncate(time.Hour); !s.After(time.UnixMilli(to)); s = s.Add(time.Hour) {
				if s.Before(time.UnixMilli(from)) {
					continue
				}
				rows = append(rows, fmt.Sprintf(`{"t":%d,"c":%s}`, s.UnixMilli(), closeFor(ticker)))
			}
		}
		_, _ = fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(rows, ","))
	}
}

func newFixingsWorker(t *testing.T, status int, closeFor func(string) string) (*Worker, *recordingFixingWriter, map[string]int, time.Time) {
	t.Helper()
	hits := map[string]int{}
	var mu sync.Mutex
	up := &fakeMassive{
		current: map[string]float64{"UZS": 11800, "EUR": 0.92},
		history: map[string]float64{"UZS": 11790, "EUR": 0.92},
		names:   guardedNames,
		aggs:    aggsServer(&hits, &mu, status, closeFor),
	}
	w := newGuardedWorker(t, up, nil)
	now := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)
	w.now = func() time.Time { return now }
	rec := &recordingFixingWriter{}
	w.WithFixingWriter(rec)
	return w, rec, hits, now
}

func TestAppendFixings_InsertsSettledLiveWindowAfterInstall(t *testing.T) {
	w, rec, _, now := newFixingsWorker(t, 0, func(ticker string) string {
		if ticker == "UZS" {
			return "11800"
		}
		return "0.92"
	})
	rec.onInsert = func() {
		if w.cache.Latest() == nil {
			t.Error("fx_fixings written before the served snapshot was installed")
		}
	}
	w.refreshOnce(context.Background())

	perTicker := map[string]int{}
	for _, b := range rec.bars {
		perTicker[b.Ticker]++
		if b.BarStart.Before(now.Add(-fixingLiveWindow)) {
			t.Errorf("%s bar %s predates the live window", b.Ticker, b.BarStart)
		}
		if b.BarEnd.After(now.Add(-FixingSettle)) {
			t.Errorf("%s bar %s..%s is still open", b.Ticker, b.BarStart, b.BarEnd)
		}
	}
	// now is hh:30, so the window holds 47 whole closed hours.
	for _, ticker := range []string{"EUR", "UZS"} {
		if perTicker[ticker] != 47 {
			t.Errorf("%s: inserted %d bars, want 47", ticker, perTicker[ticker])
		}
	}
}

func TestAppendFixings_FetchFailures(t *testing.T) {
	t.Run("a 500 skips the ticker and the cycle continues", func(t *testing.T) {
		w, rec, hits, _ := newFixingsWorker(t, http.StatusInternalServerError, nil)
		before := testutil.ToFloat64(obs.FXFixingsFetchErrorsTotal.WithLabelValues("EUR"))
		w.refreshOnce(context.Background())
		if hits["EUR"] == 0 || hits["UZS"] == 0 {
			t.Fatalf("hits = %v, want both tickers tried", hits)
		}
		if len(rec.bars) != 0 {
			t.Fatalf("inserted %d bars from failed fetches", len(rec.bars))
		}
		if testutil.ToFloat64(obs.FXFixingsFetchErrorsTotal.WithLabelValues("EUR")) <= before {
			t.Errorf("fetch_errors_total{EUR} did not move")
		}
	})
	t.Run("a 429 ends the cycle", func(t *testing.T) {
		w, _, hits, _ := newFixingsWorker(t, http.StatusTooManyRequests, nil)
		w.refreshOnce(context.Background())
		if tried := len(hits); tried != 1 {
			t.Fatalf("tickers tried after a 429 = %d (%v), want 1", tried, hits)
		}
	})
}

func TestAppendFixings_EmptyCycleStampsLastRefresh(t *testing.T) {
	w, rec, _, _ := newFixingsWorker(t, 0, nil)
	obs.FXFixingsLastRefreshUnix.Set(0)
	w.refreshOnce(context.Background())
	if len(rec.bars) != 0 {
		t.Fatalf("inserted %d bars from an empty series", len(rec.bars))
	}
	if testutil.ToFloat64(obs.FXFixingsLastRefreshUnix) == 0 {
		t.Fatal("last_refresh not stamped on an empty cycle")
	}
}

// A snapshot a fallback provider served spends no primary-vendor call.
func TestAppendFixings_FallbackSnapshotQueriesNothing(t *testing.T) {
	hits := map[string]int{}
	var mu sync.Mutex
	up := &fakeMassive{
		names: guardedNames, groupedStatus: http.StatusInternalServerError,
		aggs: aggsServer(&hits, &mu, 0, func(string) string { return "1" }),
	}
	w := newGuardedWorker(t, up, nil).WithFallbacks(ECBProvider{Endpoint: ecbServer(t, ecbDailyXML, http.StatusOK).URL})
	rec := &recordingFixingWriter{}
	w.WithFixingWriter(rec)
	w.refreshOnce(context.Background())
	if w.cache.Latest() == nil {
		t.Fatal("fallback did not serve; the test proves nothing")
	}
	if len(hits) != 0 || len(rec.bars) != 0 {
		t.Fatalf("aggregates hits = %v, bars = %d; want none for a fallback snapshot", hits, len(rec.bars))
	}
}
