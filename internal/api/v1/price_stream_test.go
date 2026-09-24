package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestPriceStream_NoHub_Returns503 — the endpoint is mounted but
// returns 503 until a Hub is wired (typical pre-aggregator state).
func TestPriceStream_NoHub_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/price/stream?asset=native&quote=fiat:USD")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "stream-unavailable") {
		t.Errorf("error type missing: %s", body)
	}
}

// TestPriceStream_RejectsGranularity — closed-bucket stream is fixed
// at 1m; ?granularity= returns 400.
func TestPriceStream_RejectsGranularity(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/price/stream?asset=native&quote=fiat:USD&granularity=1m")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "invalid-stream-param") {
		t.Errorf("error type missing: %s", body)
	}
}

func TestPriceStream_MissingAsset_Returns400(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/price/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
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
// alias fan-out regression (cold audit 2026-08-03 finding 2): the
// aggregator publishes XLM's CEX-fed VWAP under `crypto:XLM/fiat:USD`,
// and pre-fix a `?asset=native` subscriber got a healthy 200 and zero
// frames forever. The handler now subscribes to every alias spelling
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

// TestPriceStream_OneSeriesPerConnection is the #752 regression. The
// aggregator prices XLM as both `native` (SDEX) and `crypto:XLM` (CEX),
// and publishes each on its own topic every bucket. A connection
// subscribes to both spellings, so pre-fix it received two price_update
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
// of the #752 contract: once the preferred spelling has published nothing
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

// TestPriceStream_WindowSeparation — the window-interleave regression
// (cold audit 2026-08-03 finding 1, r1-confirmed): the aggregator
// publishes one bucket per (pair, window) and pre-fix all three landed
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

// TestPriceStream_RejectsBadWindowSeconds — a malformed window returns
// 400 pre-stream.
func TestPriceStream_RejectsBadWindowSeconds(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/price/stream?asset=native&quote=fiat:USD&window_seconds=-5")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
