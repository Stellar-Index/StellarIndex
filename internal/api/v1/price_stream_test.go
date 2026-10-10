package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// TestPriceStream_RejectsBadRequests: no hub is 503 stream-unavailable; a
// granularity, missing asset or negative window is 400.
func TestPriceStream_RejectsBadRequests(t *testing.T) {
	for _, tc := range []struct {
		name, query, wantBody string
		noHub                 bool
		want                  int
	}{
		{name: "no hub", noHub: true, query: "?asset=native&quote=fiat:USD", want: http.StatusServiceUnavailable, wantBody: "stream-unavailable"},
		{name: "granularity", query: "?asset=native&quote=fiat:USD&granularity=1m", want: http.StatusBadRequest, wantBody: "invalid-stream-param"},
		{name: "missing asset", query: "", want: http.StatusBadRequest},
		{name: "negative window", query: "?asset=native&quote=fiat:USD&window_seconds=-5", want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := v1.Options{Hub: streaming.NewHub(0)}
			if tc.noHub {
				opts = v1.Options{}
			}
			ts := httptest.NewServer(v1.New(opts).Handler())
			defer ts.Close()
			resp, err := http.Get(ts.URL + "/v1/price/stream" + tc.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.wantBody != "" {
				if body, _ := readAll(resp); !strings.Contains(body, tc.wantBody) {
					t.Errorf("error type %q missing: %s", tc.wantBody, body)
				}
			}
		})
	}
}

// TestPriceStream_HubPublishReachesSubscriber — drive the full path:
// connect, publish into the Hub on the right topic, observe the
// SSE frame on the wire.
func TestPriceStream_HubPublishReachesSubscriber(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	topic := v1.PriceStreamTopic(xlm, usd, 300)

	// Open the stream. The handler subscribes synchronously before
	// calling streaming.Stream's loop.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Brief sleep for the handler's Subscribe to register before we
	// publish — without this, the publish can race and reach the
	// buffer alone without a live subscriber.
	time.Sleep(50 * time.Millisecond)

	hub.Publish(topic, "price_update", []byte(`{"price":"0.42"}`))

	br := bufio.NewReader(resp.Body)
	frame := readPriceStreamFrame(t, br, 2*time.Second)
	if frame == "" {
		t.Fatal("no frame received")
	}
	if !strings.Contains(frame, `data: {"price":"0.42"}`) {
		t.Errorf("frame missing payload: %q", frame)
	}
	if !strings.Contains(frame, "event: price_update\n") {
		t.Errorf("frame missing event type: %q", frame)
	}
	if !strings.Contains(frame, "id: ") {
		t.Errorf("frame missing id: %q", frame)
	}
}

// TestPriceStream_TopicIsolation — a publish on a DIFFERENT pair's
// topic doesn't reach this subscriber. Sanity check that the handler
// computes the topic key correctly per (asset, quote).
//
// The foreign publish is followed by one on the subscriber's own topic,
// so the first frame decides it: a leaked foreign event would arrive
// ahead of the own-topic one. The handler subscribes before it writes
// headers, so both publishes land after the subscription exists.
func TestPriceStream_TopicIsolation(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	ownTopic := v1.PriceStreamTopic(xlm, usd, 300)
	otherTopic := ownTopic + "-different-pair-suffix"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	hub.Publish(otherTopic, "price_update", []byte(`{"x":"foreign"}`))
	hub.Publish(ownTopic, "price_update", []byte(`{"x":"own"}`))

	br := bufio.NewReader(resp.Body)
	frame := readPriceStreamFrame(t, br, 5*time.Second)
	if strings.Contains(frame, "foreign") {
		t.Errorf("subscriber received foreign-topic event: %q", frame)
	}
	if !strings.Contains(frame, `"own"`) {
		t.Errorf("first frame = %q, want the own-topic event", frame)
	}
}

// TestPriceStream_LastEventIDResume — publish 3 events into the Hub
// before any subscriber connects, then connect with Last-Event-ID
// pointing at the first one. Subscriber sees only the 2 newer events
// replayed from the buffer.
func TestPriceStream_LastEventIDResume(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	topic := v1.PriceStreamTopic(xlm, usd, 300)

	id1 := hub.Publish(topic, "price_update", []byte(`{"p":"0.10"}`))
	hub.Publish(topic, "price_update", []byte(`{"p":"0.11"}`))
	hub.Publish(topic, "price_update", []byte(`{"p":"0.12"}`))

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	req.Header.Set("Last-Event-ID", id1) // resume after first
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	frames := make([]string, 0, 2)
	for len(frames) < 2 {
		f := readPriceStreamFrame(t, br, 2*time.Second)
		if f == "" {
			break
		}
		frames = append(frames, f)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2 (replay of 0.11 + 0.12)", len(frames))
	}
	if !strings.Contains(frames[0], "0.11") {
		t.Errorf("first replay frame = %q, want 0.11", frames[0])
	}
	if !strings.Contains(frames[1], "0.12") {
		t.Errorf("second replay frame = %q, want 0.12", frames[1])
	}
}

// readPriceStreamFrame reads one full SSE frame (id/event/data
// terminated by a blank line) from body, skipping comment lines.
// Returns "" on EOF or timeout.
func readPriceStreamFrame(t *testing.T, br *bufio.Reader, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var sb strings.Builder
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		// retry: is a standalone prelude field (no data line) —
		// never a client-visible frame on its own; skip it like a
		// comment.
		if strings.HasPrefix(line, "retry:") {
			continue
		}
		if line == "\n" {
			if sb.Len() > 0 {
				return sb.String()
			}
			continue
		}
		sb.WriteString(line)
	}
	return sb.String()
}

// TestPriceStream_AliasSubscriptionReceivesCryptoXLMPublishes — the
// alias fan-out regression: the
// aggregator publishes XLM's CEX-fed VWAP under `crypto:XLM/fiat:USD`,
// a `?asset=native` subscriber would otherwise get a healthy 200 and zero
// frames forever. The handler subscribes to every alias spelling
// of the pair.
func TestPriceStream_AliasSubscriptionReceivesCryptoXLMPublishes(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	// Publish under the ALIAS spelling — the one the aggregator uses.
	hub.Publish(v1.PriceStreamTopic(cryptoXLM, usd, 300), "price_update", []byte(`{"price":"0.17"}`))

	br := bufio.NewReader(resp.Body)
	frame := readPriceStreamFrame(t, br, 2*time.Second)
	if !strings.Contains(frame, `data: {"price":"0.17"}`) {
		t.Fatalf("native subscriber missed the crypto:XLM publish; frame = %q", frame)
	}
}

// closedFrame is the envelope the redispub subscriber fans out.
func closedFrame(assetID, price string, at time.Time) []byte {
	return []byte(`{"data":{"asset_id":"` + assetID + `","quote":"fiat:USD","price":"` + price +
		`","price_type":"vwap","observed_at":"` + at.Format(time.RFC3339) +
		`","window_seconds":300},"as_of":"` + at.Format(time.RFC3339) + `"}`)
}

// openClosedStream opens /v1/price/stream for (asset, fiat:USD) and
// returns a reader positioned after the subscription has registered.
func openClosedStream(t *testing.T, hub *streaming.Hub, asset string) *bufio.Reader {
	t.Helper()
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset="+asset+"&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	return bufio.NewReader(resp.Body)
}

// framesUntil reads frames until one carries sentinel and returns the
// asset_id/price of each, in order.
func framesUntil(t *testing.T, br *bufio.Reader, sentinel string) []string {
	t.Helper()
	var got []string
	for {
		frame := readPriceStreamFrame(t, br, 2*time.Second)
		if frame == "" {
			t.Fatalf("stream ended before the sentinel frame; got %v", got)
		}
		var body struct {
			Data struct {
				AssetID string `json:"asset_id"`
				Price   string `json:"price"`
			} `json:"data"`
		}
		for _, line := range strings.Split(frame, "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				if err := json.Unmarshal([]byte(rest), &body); err != nil {
					t.Fatalf("frame data: %v (%q)", err, rest)
				}
			}
		}
		got = append(got, body.Data.AssetID+"@"+body.Data.Price)
		if body.Data.Price == sentinel {
			return got
		}
	}
}

// TestPriceStream_OneSeriesPerConnection is the one-series regression. The
// aggregator prices XLM as both `native` (SDEX) and `crypto:XLM` (CEX),
// and publishes each on its own topic every bucket. A connection
// subscribes to both spellings, so it could receive two price_update
// frames per bucket from two independent series — a sawtooth between
// SDEX and CEX prices. It must follow ONE series: the caller's own
// spelling first, the order /v1/price?window= reads the cache in.
func TestPriceStream_OneSeriesPerConnection(t *testing.T) {
	bucket := time.Now().UTC().Truncate(time.Minute)
	cases := []struct {
		asset, preferred, other string
	}{
		{"native", "native", "crypto:XLM"},
		{"crypto:XLM", "crypto:XLM", "native"},
	}
	for _, tc := range cases {
		t.Run(tc.asset, func(t *testing.T) {
			hub := streaming.NewHub(0)
			br := openClosedStream(t, hub, tc.asset)
			usd, _ := canonical.ParseAsset("fiat:USD")
			pref, _ := canonical.ParseAsset(tc.preferred)
			other, _ := canonical.ParseAsset(tc.other)
			prefTopic := v1.PriceStreamTopic(pref, usd, 300)
			otherTopic := v1.PriceStreamTopic(other, usd, 300)

			hub.Publish(prefTopic, "price_update", closedFrame(tc.preferred, "0.10", bucket))
			hub.Publish(otherTopic, "price_update", closedFrame(tc.other, "0.20", bucket))
			hub.Publish(otherTopic, "price_update", closedFrame(tc.other, "0.21", bucket.Add(time.Minute)))
			hub.Publish(prefTopic, "price_update", closedFrame(tc.preferred, "0.11", bucket.Add(time.Minute)))
			hub.Publish(prefTopic, "price_update", closedFrame(tc.preferred, "0.12", bucket.Add(2*time.Minute)))

			got := strings.Join(framesUntil(t, br, "0.12"), ",")
			want := tc.preferred + "@0.10," + tc.preferred + "@0.11," + tc.preferred + "@0.12"
			if got != want {
				t.Fatalf("frames = %s, want %s — the %s series must not interleave", got, want, tc.other)
			}
		})
	}
}

// TestPriceStream_FallsBackWhenPreferredSeriesGoesQuiet — the other half
// of the one-series contract: once the preferred spelling has published nothing
// for cachekeys.VWAPMaxAge, /v1/price?window= serves the alias key, and
// so does the stream (the zero-frames fix must survive series selection).
func TestPriceStream_FallsBackWhenPreferredSeriesGoesQuiet(t *testing.T) {
	bucket := time.Now().UTC().Truncate(time.Minute)
	hub := streaming.NewHub(0)
	br := openClosedStream(t, hub, "native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")

	hub.Publish(v1.PriceStreamTopic(canonical.NativeAsset(), usd, 300), "price_update",
		closedFrame("native", "0.10", bucket))
	hub.Publish(v1.PriceStreamTopic(cryptoXLM, usd, 300), "price_update",
		closedFrame("crypto:XLM", "0.20", bucket.Add(6*time.Minute)))

	got := strings.Join(framesUntil(t, br, "0.20"), ",")
	if want := "native@0.10,crypto:XLM@0.20"; got != want {
		t.Fatalf("frames = %s, want %s", got, want)
	}
}

// TestPriceStream_WithheldMarkerFollowsTheSelectedSeries — a publisher's
// price_withheld names one alias spelling. A crypto:XLM connection whose
// own series is live must not see native's marker, which would read as
// its own pair being withheld; with crypto:XLM quiet it falls back to it.
func TestPriceStream_WithheldMarkerFollowsTheSelectedSeries(t *testing.T) {
	bucket := time.Now().UTC().Truncate(time.Minute)
	usd, _ := canonical.ParseAsset("fiat:USD")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	nativeTopic := v1.PriceStreamTopic(canonical.NativeAsset(), usd, 300)
	cryptoTopic := v1.PriceStreamTopic(cryptoXLM, usd, 300)
	nativeWithheld := []byte(`{"asset_id":"native","quote":"fiat:USD","reason":"substance","as_of":"` +
		bucket.Add(time.Minute).Format(time.RFC3339) + `"}`)

	t.Run("own series live", func(t *testing.T) {
		hub := streaming.NewHub(0)
		br := openClosedStream(t, hub, "crypto:XLM")
		hub.Publish(cryptoTopic, "price_update", closedFrame("crypto:XLM", "0.10", bucket))
		hub.Publish(nativeTopic, "price_withheld", nativeWithheld)
		hub.Publish(cryptoTopic, "price_update", closedFrame("crypto:XLM", "0.11", bucket.Add(time.Minute)))

		got := strings.Join(framesUntil(t, br, "0.11"), ",")
		if want := "crypto:XLM@0.10,crypto:XLM@0.11"; got != want {
			t.Fatalf("frames = %s, want %s — native's price_withheld leaked into the crypto:XLM series", got, want)
		}
	})
	t.Run("own series quiet", func(t *testing.T) {
		hub := streaming.NewHub(0)
		br := openClosedStream(t, hub, "crypto:XLM")
		hub.Publish(nativeTopic, "price_withheld", nativeWithheld)
		frame := readPriceStreamFrame(t, br, 2*time.Second)
		if !strings.Contains(frame, "event: price_withheld") || !strings.Contains(frame, `"asset_id":"native"`) {
			t.Fatalf("frame = %q, want native's price_withheld once it is the served series", frame)
		}
	})
}

// TestPriceStream_WindowSeparation — the window-interleave regression:
// the aggregator
// publishes one bucket per (pair, window) and without separation all three land
// on ONE topic. A subscriber following the default 300s series must
// NOT receive the 3600s or 86400s publishes.
func TestPriceStream_WindowSeparation(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD&window_seconds=300", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	hub.Publish(v1.PriceStreamTopic(xlm, usd, 3600), "price_update", []byte(`{"w":"3600"}`))
	hub.Publish(v1.PriceStreamTopic(xlm, usd, 86400), "price_update", []byte(`{"w":"86400"}`))
	hub.Publish(v1.PriceStreamTopic(xlm, usd, 300), "price_update", []byte(`{"w":"300"}`))

	br := bufio.NewReader(resp.Body)
	frame := readPriceStreamFrame(t, br, 2*time.Second)
	if !strings.Contains(frame, `data: {"w":"300"}`) {
		t.Fatalf("first frame should be the 300s bucket only; frame = %q", frame)
	}
}

// TestPriceStream_AliasFanOut_MergesDistinctVenuesIntoOneStream is a
// documented-defect reproduction, not a green regression
// guard: it records CURRENT behaviour so the fix (tracked separately —
// see the finding) has a concrete before/after to work from, rather
// than landing on top of an un-derived symptom.
//
// cmd/stellarindex-aggregator/main.go's defaultPairs() computes XLM
// under BOTH `native` and `crypto:XLM` as independent (base, quote)
// pairs — deliberately, so a future CEX connector populates the
// abstract side without a config change (see that function's own
// comment). internal/api/v1/price_stream.go's alias fan-out then
// subscribes a single client to every alias topic of its requested
// asset, so a `?asset=native` subscriber's ONE connection receives
// BOTH pairs' publishes, presented as sequential updates on what looks
// like one series. On r1 today this is latent: publishToStream in
// internal/aggregate/orchestrator only fires on a successful VWAP
// write, and crypto:XLM has no venue wired, so it never publishes.
// This test proves the client-visible merge itself, independent of
// whether the aggregator is currently driving it — the trigger the
// finding names ("a future deployment with Binance/Coinbase running")
// is a config change away, not a code change away.
//
// The fix is NOT implemented here: it requires either collapsing
// alias topics to one canonical topic per market on the subscribe
// side, or a producer id/sequence on the wire event
// (internal/api/streaming/redispub/event.go) enforced by the
// redispub Subscriber (internal/api/streaming/redispub/subscriber.go)
// — both outside this fix's file scope, and a product decision on
// which venue should win a merged tick belongs with whoever owns that
// scope, not a silent default here.
func TestPriceStream_AliasFanOut_MergesDistinctVenuesIntoOneStream(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	native, _ := canonical.ParseAsset("native")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	// Two independent venues' closed buckets for the SAME nominal
	// tick, published under their own (base, quote) pairs exactly as
	// defaultPairs() configures the aggregator to compute them.
	hub.Publish(v1.PriceStreamTopic(native, usd, 300), "price_update", []byte(`{"price":"0.1050","venue":"onchain"}`))
	hub.Publish(v1.PriceStreamTopic(cryptoXLM, usd, 300), "price_update", []byte(`{"price":"0.1200","venue":"cex"}`))

	br := bufio.NewReader(resp.Body)
	first := readPriceStreamFrame(t, br, 2*time.Second)
	second := readPriceStreamFrame(t, br, 2*time.Second)

	if !strings.Contains(first, `"venue":"onchain"`) {
		t.Fatalf("first frame = %q, want the native/onchain publish", first)
	}
	if !strings.Contains(second, `"venue":"cex"`) {
		t.Fatalf("second frame = %q, want the crypto:XLM/cex publish — a SINGLE "+
			"`?asset=native` subscription received a SECOND, independently-sourced "+
			"price for what it presented as one series, with nothing on the wire "+
			"(or in this handler) to tell the client the two frames came from "+
			"different venues (RLT-345)", second)
	}
}

// TestPriceStream_SubstanceWithheld_RefusesConnect — a pair below the
// thin-market serve floor must get the same 404 + `price-withheld`
// problem type from the stream that /v1/price gives it, BEFORE the
// response switches into SSE mode (once the SSE headers are out it is
// too late to say 404).
func TestPriceStream_SubstanceWithheld_RefusesConnect(t *testing.T) {
	hub := streaming.NewHub(0)
	gate := newClosedStreamGate(true)
	srv := v1.New(v1.Options{Hub: hub, Substance: gate})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — the stream must not open on a withheld pair", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if strings.Contains(string(body), "text/event-stream") ||
		strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		t.Errorf("response switched into SSE mode on a withheld pair: %s / %s",
			resp.Header.Get("Content-Type"), body)
	}
	if got := gate.seenSurfaces(); len(got) == 0 || got[0] != "price_stream" {
		t.Errorf("substance gate surface label = %v, want first call \"price_stream\" "+
			"(the metric must name WHICH surface withheld)", got)
	}
}

// TestPriceStream_ScamFlaggedIssuer_RefusesConnect — the scam gate is
// the SECOND gate, and a hand-written call site that consults one and
// forgets the other is the drift shape. With the substance gate
// disabled (nil, as an operator diagnosing a coverage complaint would
// leave it), a directory-scam-flagged issuer must still be refused.
func TestPriceStream_ScamFlaggedIssuer_RefusesConnect(t *testing.T) {
	hub := streaming.NewHub(0)
	gate := newClosedStreamGate(true)
	srv := v1.New(v1.Options{Hub: hub, Scam: gate})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset="+flaggedIssuerAsset+"&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a flagged issuer's closed bucket must not stream", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if got := gate.seenSurfaces(); len(got) == 0 || got[0] != "price_stream" {
		t.Errorf("scam gate surface label = %v, want first call \"price_stream\"", got)
	}
}

// TestPriceStream_GateFlipMidStreamWithholdsBucket — the half that a
// connect-time-only check would miss, and the half that matters most.
//
// An SSE connection lives for hours. A connection opened while a pair
// still cleared the serve floor — including the attacker's own, opened
// before the dust market it authored was measured or before its issuer
// was flagged — must stop being served the moment the verdict flips,
// because the aggregator keeps publishing that pair's closed bucket
// regardless (nothing on the producer side consults a gate).
//
// The assertion is on the CONTENT of the next frames, not on silence: a
// withheld bucket is published between two servable ones, and the
// subscriber must see a price_withheld marker in its place — never the
// withheld price, and never nothing, which reads as a quiet market —
// then the later servable bucket.
func TestPriceStream_GateFlipMidStreamWithholdsBucket(t *testing.T) {
	hub := streaming.NewHub(0)
	gate := newClosedStreamGate(false) // servable at connect
	srv := v1.New(v1.Options{Hub: hub, Substance: gate})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	topic := v1.PriceStreamTopic(xlm, usd, 300)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the pair clears the floor at connect)", resp.StatusCode)
	}
	if v, asked := gate.awaitConsultation(gateSyncBudget); asked && v {
		t.Fatal("connect-time consultation withheld, but the gate was set to allow")
	}

	// Let the handler's Subscribe register before publishing.
	time.Sleep(50 * time.Millisecond)
	br := bufio.NewReader(resp.Body)

	// Bucket 1: servable, and it must arrive — otherwise every later
	// assertion about a bucket NOT arriving would be vacuous.
	hub.Publish(topic, "price_update", []byte(`{"price":"0.42"}`))
	gate.awaitConsultation(gateSyncBudget)
	if frame := readPriceStreamFrame(t, br, 3*time.Second); !strings.Contains(frame, `"price":"0.42"`) {
		t.Fatalf("servable bucket did not reach the subscriber; frame = %q", frame)
	}

	// The verdict flips: this pair is now withheld (dust market measured
	// / issuer flagged). Bucket 2 must never reach the wire. Wait for the
	// forwarder to have reached bucket 2 before restoring the verdict, so
	// the bucket is decided under the withholding verdict and not by a
	// race with the line below.
	gate.setWithhold(true)
	hub.Publish(topic, "price_update", []byte(`{"price":"999.99","as_of":"2026-05-02T12:05:00Z"}`))
	gate.awaitConsultation(gateSyncBudget)

	// Verdict flips back; bucket 3 is servable again. The subscriber must
	// see the marker for bucket 2 and then bucket 3.
	gate.setWithhold(false)
	hub.Publish(topic, "price_update", []byte(`{"price":"0.43"}`))

	frame := readPriceStreamFrame(t, br, 3*time.Second)
	if strings.Contains(frame, "999.99") {
		t.Fatalf("withheld closed bucket was fanned out to the subscriber: %q", frame)
	}
	if !strings.Contains(frame, "event: price_withheld") || !strings.Contains(frame, `"reason":"substance"`) ||
		!strings.Contains(frame, `"as_of":"2026-05-02T12:05:00Z"`) {
		t.Fatalf("withheld bucket frame = %q, want a price_withheld marker with reason substance and the bucket's as_of", frame)
	}
	frame = readPriceStreamFrame(t, br, 3*time.Second)
	if strings.Contains(frame, "999.99") {
		t.Fatalf("withheld closed bucket was fanned out to the subscriber: %q", frame)
	}
	if !strings.Contains(frame, `"price":"0.43"`) {
		t.Fatalf("next frame after the withheld bucket = %q, want the later servable bucket", frame)
	}
}

// TestPriceStream_NoGatesWired_StillStreams — nil gates mean the
// operator disabled [pricing_guard]; that must keep today's behaviour,
// not turn into a deny-everything outage on the stream path.
func TestPriceStream_NoGatesWired_StillStreams(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no gates wired", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)
	hub.Publish(v1.PriceStreamTopic(xlm, usd, 300), "price_update", []byte(`{"price":"0.11"}`))

	br := bufio.NewReader(resp.Body)
	if frame := readPriceStreamFrame(t, br, 3*time.Second); !strings.Contains(frame, `"price":"0.11"`) {
		t.Fatalf("ungated deployment stopped streaming; frame = %q", frame)
	}
}

// TestPriceStream_StalledGateCoalescesBacklog — buckets that queue while
// the forwarder is inside a slow gate call must share ONE fresh verdict,
// not pay the gate budget each. Serial per-event gating lets a stalled DB
// back the Hub queue up until Publish evicts the subscriber.
func TestPriceStream_StalledGateCoalescesBacklog(t *testing.T) {
	hub := streaming.NewHub(0)
	gate := &stallingGate{entered: make(chan struct{}, 1), hold: make(chan struct{})}
	srv := v1.New(v1.Options{Hub: hub, Substance: gate})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	topic := v1.PriceStreamTopic(xlm, usd, 300)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	const buckets = 10
	hub.Publish(topic, "price_update", []byte(`{"price":"b1"}`))
	select {
	case <-gate.entered:
	case <-time.After(gateSyncBudget):
		t.Fatal("forwarder never consulted the gate for the first bucket")
	}
	for i := 2; i <= buckets; i++ {
		hub.Publish(topic, "price_update", []byte(fmt.Sprintf(`{"price":"b%d"}`, i)))
	}
	close(gate.hold)

	br := bufio.NewReader(resp.Body)
	for i := 1; i <= buckets; i++ {
		want := fmt.Sprintf(`"price":"b%d"`, i)
		if frame := readPriceStreamFrame(t, br, 3*time.Second); !strings.Contains(frame, want) {
			t.Fatalf("frame %d = %q, want %s", i, frame, want)
		}
	}
	// connect + the stalled first bucket + one verdict for the queued rest.
	if got := gate.calls.Load(); got != 3 {
		t.Errorf("gate consultations = %d, want 3: the queued buckets must share one verdict", got)
	}
}

// closedStreamGate stands in for both pricingguard gates on the stream
// path. One struct implements PriceSubstanceGate and PriceScamGate so a
// test can flip the verdict mid-stream, and every consultation is
// published on `calls` so a test can synchronise on the gate actually
// having been asked — rather than sleeping and hoping.
//
// Mutex-guarded because the per-bucket re-check runs on the forwarder
// goroutine while the test drives the verdict from its own.
type closedStreamGate struct {
	mu       sync.Mutex
	withhold bool
	surfaces []string
	calls    chan bool // verdict, one per consultation (buffered)
}

// gateSyncBudget is how long the mid-stream test waits for the
// forwarder goroutine to consult the gate about a bucket it has just
// been handed. Generous: overrunning it on FIXED code would restore the
// servable verdict before the withheld bucket was decided and flake the
// test, while on un-fixed code it is simply dead time before the real
// assertion fires.
const gateSyncBudget = 2 * time.Second

func newClosedStreamGate(withhold bool) *closedStreamGate {
	return &closedStreamGate{withhold: withhold, calls: make(chan bool, 32)}
}

func (g *closedStreamGate) record(surface string) bool {
	g.mu.Lock()
	w := g.withhold
	g.surfaces = append(g.surfaces, surface)
	g.mu.Unlock()
	select {
	case g.calls <- w:
	default:
	}
	return w
}

// Allowed implements v1.PriceSubstanceGate (thin-market floor).
func (g *closedStreamGate) Allowed(_ context.Context, _, _ canonical.Asset, surface string) bool {
	return !g.record(surface)
}

func (g *closedStreamGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}

// Withheld implements v1.PriceScamGate (flagged issuer).
func (g *closedStreamGate) Withheld(_ context.Context, _ canonical.Asset, surface string) bool {
	return g.record(surface)
}

func (g *closedStreamGate) setWithhold(v bool) {
	g.mu.Lock()
	g.withhold = v
	g.mu.Unlock()
}

func (g *closedStreamGate) seenSurfaces() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.surfaces...)
}

// awaitConsultation waits for the gate to be asked once more and
// reports the verdict it gave, plus whether it was asked at all inside
// the budget.
//
// Deliberately NON-fatal: it is a synchronisation aid, not the
// assertion. On un-fixed code the gate is never consulted from this
// path, and a t.Fatal here would make the mid-stream test fail for
// "nobody asked" — masking the assertion that actually matters, which
// is that the withheld bucket reached the subscriber's wire.
func (g *closedStreamGate) awaitConsultation(budget time.Duration) (verdict, asked bool) {
	select {
	case v := <-g.calls:
		return v, true
	case <-time.After(budget):
		return false, false
	}
}

// stallingGate allows every pair but blocks its second consultation (the
// first bucket after connect) until hold is closed, standing in for a
// stalled directory/substance query.
type stallingGate struct {
	calls   atomic.Int32
	entered chan struct{}
	hold    chan struct{}
}

func (g *stallingGate) Allowed(_ context.Context, _, _ canonical.Asset, _ string) bool {
	if g.calls.Add(1) == 2 {
		g.entered <- struct{}{}
		<-g.hold
	}
	return true
}

func (g *stallingGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}
