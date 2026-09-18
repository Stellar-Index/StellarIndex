package v1_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Regression suite for Q162 / RLT-299: /v1/price/stream fanned out, in
// real time, the aggregated prices /v1/price answers 404
// `errors/price-withheld` for.
//
// The closed-bucket SSE path ran Redis → redispub → Hub → this handler
// with no pricingguard consultation anywhere on it: the aggregator
// publishes a closed bucket for every pair it computes a VWAP for, the
// bridge sanitises the envelope but asks no gate, and the handler
// subscribed the caller to the topic straight from the query string.
// So a dust market's attacker-authored VWAP (the 2026-08-04 valuation
// incident class) and a directory-scam-flagged issuer's price were both
// obtainable live from the surface that shares /v1/price's consistency
// contract — while /v1/price, /v1/price/tip, /v1/price/tip/stream,
// /v1/vwap, /v1/twap, /v1/chart and the SEP-40 oracle all withheld them.
//
// Both halves are pinned here: the connect-time refusal, and the
// per-bucket re-check that keeps an ALREADY-ATTACHED connection (an
// attacker's own, opened before its issuer was flagged) from being
// served for the hours an SSE connection lives.

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
// forgets the other is the MSP-07 drift shape. With the substance gate
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
// The assertion is on the CONTENT of the next frame, not on silence: a
// withheld bucket is published between two servable ones, and the
// subscriber must see the later servable bucket next — never the
// withheld one. Pre-fix the next frame is the withheld bucket.
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
	hub.Publish(topic, "price_update", []byte(`{"price":"999.99"}`))
	gate.awaitConsultation(gateSyncBudget)

	// Verdict flips back; bucket 3 is servable again. The NEXT frame the
	// subscriber sees must be bucket 3 — pre-fix it is bucket 2.
	gate.setWithhold(false)
	hub.Publish(topic, "price_update", []byte(`{"price":"0.43"}`))

	frame := readPriceStreamFrame(t, br, 3*time.Second)
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
