package streampublish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/api/streampublish"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeReader returns canned snapshots keyed by pair string. The
// returned snapshot is mutable between ticks via SetSnapshot —
// tests advance the clock-equivalent (ObservedAt) to drive the
// publisher's bucket-change detection.
type fakeReader struct {
	mu        sync.Mutex
	snapshots map[string]v1.PriceSnapshot
	err       error
	stale     bool
}

func (r *fakeReader) LatestPrice(_ context.Context, asset, quote canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return v1.PriceSnapshot{}, nil, false, r.err
	}
	key := asset.String() + "/" + quote.String()
	snap, ok := r.snapshots[key]
	if !ok {
		return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
	}
	return snap, []string{"binance"}, r.stale, nil
}

func (r *fakeReader) SetSnapshot(asset, quote canonical.Asset, snap v1.PriceSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapshots == nil {
		r.snapshots = map[string]v1.PriceSnapshot{}
	}
	r.snapshots[asset.String()+"/"+quote.String()] = snap
}

func mustParse(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

// TestPublisher_PublishesOnNewBucket — the publisher polls,
// detects a fresh ObservedAt, and fans out a single event per
// bucket close. Pinned to catch a regression that re-publishes the
// same bucket on every tick (every-tick republish would burn
// subscriber budgets in seconds).
func TestPublisher_PublishesOnNewBucket(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	bucket1 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07",
		PriceType: "vwap", ObservedAt: v1.WireTime(bucket1), WindowSeconds: 60,
	})

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})

	// Subscribe BEFORE Run starts so we don't miss the immediate
	// poll-once tick.
	ch, cancel, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	done := make(chan struct{})
	go func() {
		_ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}})
		close(done)
	}()

	// Expect exactly one event for bucket1.
	select {
	case ev := <-ch:
		if ev.Type != "price_update" {
			t.Errorf("event type = %q, want price_update", ev.Type)
		}
		var payload struct {
			Data    v1.PriceSnapshot `json:"data"`
			Sources []string         `json:"sources"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if !payload.Data.ObservedAt.Time().Equal(bucket1) {
			t.Errorf("payload ObservedAt = %v, want %v", payload.Data.ObservedAt, bucket1)
		}
		if payload.Data.Price != "0.07" {
			t.Errorf("payload Price = %q, want 0.07", payload.Data.Price)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within 2s of publisher start")
	}

	// Second tick at the same bucket — must NOT republish.
	select {
	case ev := <-ch:
		t.Errorf("unexpected republish at same bucket: %+v", ev)
	case <-time.After(1500 * time.Millisecond):
		// Good — no event.
	}

	// Advance the bucket. Next tick should publish.
	bucket2 := bucket1.Add(time.Minute)
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.0712",
		PriceType: "vwap", ObservedAt: v1.WireTime(bucket2), WindowSeconds: 60,
	})

	select {
	case ev := <-ch:
		var payload struct {
			Data v1.PriceSnapshot `json:"data"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal payload (bucket2): %v", err)
		}
		if !payload.Data.ObservedAt.Time().Equal(bucket2) {
			t.Errorf("bucket2 ObservedAt = %v, want %v", payload.Data.ObservedAt, bucket2)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received for bucket2 within 2s")
	}

	cancelRun()
	<-done
}

// TestPublisher_TwoSubscribersIdenticalPayload — the byte-identical
// fanout property is the whole point of the Hub-driven surface.
// If two clients on the same topic see different bytes, the
// cross-region consistency contract /v1/price/stream is meant to
// inherit from /v1/price (ADR-0015) is broken.
func TestPublisher_TwoSubscribersIdenticalPayload(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07",
		PriceType:  "vwap",
		ObservedAt: v1.WireTime(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)),
	})

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})

	chA, cancelA, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelA()
	chB, cancelB, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelB()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	var evA, evB streaming.Event
	select {
	case evA = <-chA:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber A timed out")
	}
	select {
	case evB = <-chB:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber B timed out")
	}
	if evA.ID != evB.ID {
		t.Errorf("event IDs differ: A=%q B=%q (Hub fanout must assign one ID per Publish)", evA.ID, evB.ID)
	}
	if string(evA.Data) != string(evB.Data) {
		t.Errorf("payload bytes differ:\n  A=%s\n  B=%s", evA.Data, evB.Data)
	}
}

// TestPublisher_ErrPriceNotFoundIsSilent — pairs without a
// closed bucket (fresh deploy, asset below operator coverage)
// emit no events. The publisher MUST NOT log a stack of errors
// for pairs that legitimately have no data yet.
func TestPublisher_ErrPriceNotFoundIsSilent(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{} // no snapshots → ErrPriceNotFound
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})
	ch, cancel, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	select {
	case ev := <-ch:
		t.Errorf("unexpected event for unknown pair: %+v", ev)
	case <-time.After(1500 * time.Millisecond):
		// Good — silent.
	}
}

// TestPublisher_ReaderErrorContinues — a non-NotFound reader
// error (postgres unreachable) should not take the publisher
// down. The next tick after the error clears must publish
// normally. Mirrors the divergence-refresh / volume-reader
// best-effort posture.
func TestPublisher_ReaderErrorContinues(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{err: errors.New("postgres unreachable")}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})
	ch, cancel, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	// Wait through one error tick.
	time.Sleep(1500 * time.Millisecond)

	// Clear the error and provide a snapshot. Next tick should
	// publish.
	reader.mu.Lock()
	reader.err = nil
	reader.mu.Unlock()
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07",
		ObservedAt: v1.WireTime(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)),
	})

	select {
	case <-ch:
		// Good — publisher recovered.
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not recover from reader error within 2s")
	}
}

// TestPublisher_NoPairsBlocksUntilCancel — empty Pairs must not
// busy-spin or panic; Run blocks on ctx and returns ctx.Err() on
// cancel. Regression against an earlier draft that returned
// immediately on len(pairs)==0.
func TestPublisher_NoPairsBlocksUntilCancel(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx, nil) }()

	// Confirm Run is still blocked.
	select {
	case err := <-done:
		t.Fatalf("Run returned early on empty pairs: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Good.
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// panicReader panics on every LatestPrice call — simulates a
// read/decode fault surfacing inside a per-pair poll loop.
type panicReader struct{}

func (panicReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	panic("simulated reader fault in tickOnce")
}

// TestPublisher_Run_RecoversPanickingPollLoop proves the fix:
// a panic in one pair's poll goroutine (the Run fan-out at publisher.go)
// is CONTAINED. The API binary's outer recoverBackgroundWorker wraps only
// the goroutine that CALLS Run — it cannot catch a panic in the per-pair
// goroutines Run itself spawns, which is exactly the gap this closes.
//
// Proven red: without the `defer worker.Recover(...)` in the per-pair
// goroutine, the panic below is unrecovered in a DETACHED goroutine and
// takes the whole test binary down. With the guard, the poll goroutine
// unwinds cleanly and Run returns. (The recover's Error-level log is
// asserted deterministically in internal/worker's helper test — asserting
// it here would race Run's wg.Wait against the deferred log write.)
func TestPublisher_Run_RecoversPanickingPollLoop(t *testing.T) {
	hub := streaming.NewHub(0)
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	pub := streampublish.New(hub, panicReader{}, time.Second, nil, streampublish.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	select {
	case <-done:
		// Run returned — the per-pair goroutine recovered its panic and
		// exited instead of crashing the process.
	case <-time.After(3 * time.Second):
		t.Fatal("Publisher.Run did not return after its poll goroutine panicked — the per-pair recover is missing")
	}
}

// stallReader blocks past its pollCtx deadline on every call, so the
// error tickOnce sees is pollCtx's own DeadlineExceeded rather than a
// signal that the parent ctx (and therefore the publisher) is
// shutting down.
type stallReader struct{}

func (stallReader) LatestPrice(ctx context.Context, _, _ canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	<-ctx.Done()
	return v1.PriceSnapshot{}, nil, false, ctx.Err()
}

// TestPublisher_PollTimeoutIsNotShutdown proves CA2-A33-correct-0: a
// reader that outlives the poll interval must be logged and counted
// as a stall, not treated as if the parent ctx were cancelled. Before
// the fix, tickOnce's error branch matched context.DeadlineExceeded
// unconditionally and returned silently even with a live parent ctx —
// this test fails against that code because neither the WARN log nor
// obs.StreamPublishStallTotal ever fires.
func TestPublisher_PollTimeoutIsNotShutdown(t *testing.T) {
	hub := streaming.NewHub(0)
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	// New clamps interval to a 1s floor, so this is also the
	// effective poll cadence — not a request for tighter polling.
	const interval = time.Second
	pub := streampublish.New(hub, stallReader{}, interval, logger, streampublish.Options{})

	before := testutil.ToFloat64(obs.StreamPublishStallTotal.WithLabelValues("price_stream"))

	// Parent ctx stays live for the whole test — only pollCtx (scoped
	// to interval) ever expires.
	ctx, cancelRun := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}})
		close(done)
	}()

	// Two poll intervals' worth of stalls (immediate poll-once tick +
	// one ticker fire), then stop the loop.
	time.Sleep(2500 * time.Millisecond)
	cancelRun()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	after := testutil.ToFloat64(obs.StreamPublishStallTotal.WithLabelValues("price_stream"))
	if after <= before {
		t.Errorf("StreamPublishStallTotal did not increment: before=%v after=%v", before, after)
	}
	if !strings.Contains(logBuf.String(), "reader missed poll deadline") {
		t.Errorf("expected a poll-deadline WARN log, got: %s", logBuf.String())
	}
}

// TestPublisher_NormalizesNonstandardDecimals proves the SSE closed-bucket
// producer applies the SAME dex-nonstandard-decimals correction
// /v1/price applies before serving: reader.LatestPrice returns
// the RAW closed-1m ratio, and without normalization the wire payload
// would carry that raw (wrong by 10^11) value instead of the true price.
//
// Golden shape mirrors TestPriceTip_NonstandardDecimals_Normalizes: an
// 18dp Soroban base leg against a 7dp quote, ohlcPriceDigits=10 fixed
// formatting.
func TestPublisher_NormalizesNonstandardDecimals(t *testing.T) {
	sorobanContract := "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO" // gitleaks:allow — public Stellar contract id, not a secret
	decimals := v1.NewNonstandardDecimalsCache(&fakeDecimalsReader{
		rows: []timescale.NonstandardDecimalsAsset{{Asset: sorobanContract, Decimals: 18}},
	}, nil)
	if err := decimals.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, sorobanContract)
	quote := mustParse(t, "fiat:USD")
	topic := v1.PriceStreamTopic(asset, quote, 60)

	bucket := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: sorobanContract, Quote: "fiat:USD", Price: "0.00000000005",
		PriceType: "vwap", ObservedAt: v1.WireTime(bucket), WindowSeconds: 60,
	})

	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{Decimals: decimals})

	ch, cancel, err := hub.Subscribe([]string{topic}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	done := make(chan struct{})
	go func() {
		_ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}})
		close(done)
	}()

	select {
	case ev := <-ch:
		var payload struct {
			Data v1.PriceSnapshot `json:"data"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		// Raw ratio 5e-11 scaled by 10^(18-7) = 5.0000000000, ohlcPriceDigits=10.
		// Unnormalized, the wire would carry the raw "0.00000000005".
		if payload.Data.Price != "5.0000000000" {
			t.Errorf("payload Price = %q, want normalized \"5.0000000000\"", payload.Data.Price)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received within 2s of publisher start")
	}

	cancelRun()
	<-done
}

// fakeDecimalsReader satisfies v1.NonstandardDecimalsReader with a fixed
// set of confirmed non-7-decimal assets, no database required.
type fakeDecimalsReader struct {
	rows []timescale.NonstandardDecimalsAsset
}

func (r *fakeDecimalsReader) LoadNonstandardDecimalsAssets(context.Context) ([]timescale.NonstandardDecimalsAsset, error) {
	return r.rows, nil
}

// TestPublisher_FrozenPairPublishesTheFreezeNotTheRefusedBucket — under
// an ADR-0019 freeze the raw prices_1m bucket is the value the freeze
// refused, and /v1/price never serves it under the flag. The stream
// would publish it every minute as a price_update with flags
// {stale:false} and no frozen key — the same bytes as a healthy pair.
// The marker of ANY spelling governs (the reader does not report which
// alias its bucket came from), so a freeze on crypto:XLM covers native.
func TestPublisher_FrozenPairPublishesTheFreezeNotTheRefusedBucket(t *testing.T) {
	ev := firstEvent(t, fakeFreeze{frozen: map[string]bool{"crypto:XLM/fiat:USD": true}}, true)
	if ev.Type != "price_frozen" {
		t.Fatalf("event type = %q, want price_frozen", ev.Type)
	}
	p := decodeStreamed(t, ev)
	if _, hasPrice := p.Data["price"]; hasPrice {
		t.Errorf("price_frozen carries the refused bucket's price: %s", ev.Data)
	}
	if p.Data["observed_at"] != "2026-05-02T12:00:00Z" {
		t.Errorf("observed_at = %v, want the refused bucket's 2026-05-02T12:00:00Z", p.Data["observed_at"])
	}
	if !p.Flags["frozen"] || !p.Flags["frozen_checked"] {
		t.Errorf("flags = %v, want frozen and frozen_checked true", p.Flags)
	}
	// The reader called the refused bucket stale; the freeze must not
	// launder that into stale:false.
	if stale, present := p.Flags["stale"]; !present || !stale {
		t.Errorf("flags = %v, want stale true (the reader's verdict on the refused bucket)", p.Flags)
	}
}

// TestPublisher_UnfrozenPairSaysTheFreezeWasChecked — a healthy pair's
// price_update must be distinguishable from one whose freeze was never
// evaluated: frozen_checked is present only when every marker was read.
func TestPublisher_UnfrozenPairSaysTheFreezeWasChecked(t *testing.T) {
	ev := firstEvent(t, fakeFreeze{}, false)
	if ev.Type != "price_update" {
		t.Fatalf("event type = %q, want price_update", ev.Type)
	}
	p := decodeStreamed(t, ev)
	if p.Data["price"] != "0.07" {
		t.Errorf("price = %v, want 0.07", p.Data["price"])
	}
	if !p.Flags["frozen_checked"] {
		t.Errorf("flags = %v, want frozen_checked true", p.Flags)
	}
	if _, present := p.Flags["frozen"]; present {
		t.Errorf("flags = %v, want no frozen key on an unfrozen pair", p.Flags)
	}

	failed := decodeStreamed(t, firstEvent(t, fakeFreeze{err: errors.New("redis down")}, false))
	if _, present := failed.Flags["frozen_checked"]; present {
		t.Errorf("flags = %v after a failed marker read, want frozen_checked absent (not evaluated)", failed.Flags)
	}
}

// fakeFreeze is a v1.FrozenLooker keyed by "<asset>/<quote>".
type fakeFreeze struct {
	frozen map[string]bool
	err    error
}

func (f fakeFreeze) FrozenForPair(_ context.Context, asset, quote canonical.Asset) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.frozen[asset.String()+"/"+quote.String()], nil
}

// firstEvent runs the publisher over one pair with a fresh bucket and
// returns the first event it publishes.
func firstEvent(t *testing.T, looker v1.FrozenLooker, stale bool) streaming.Event {
	t.Helper()
	hub := streaming.NewHub(0)
	reader := &fakeReader{stale: stale}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07", PriceType: "vwap",
		ObservedAt: v1.WireTime(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)), WindowSeconds: 60,
	})
	ch, cancelSub, err := hub.Subscribe([]string{v1.PriceStreamTopic(asset, quote, 60)}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{Frozen: looker})
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s of publisher start")
	}
	return streaming.Event{}
}

type streamedPrice struct {
	Data  map[string]any  `json:"data"`
	Flags map[string]bool `json:"flags"`
}

func decodeStreamed(t *testing.T, ev streaming.Event) streamedPrice {
	t.Helper()
	var p streamedPrice
	if err := json.Unmarshal(ev.Data, &p); err != nil {
		t.Fatalf("unmarshal %s payload: %v", ev.Type, err)
	}
	return p
}

// TestPublisher_WithheldPairPublishesTheWithholding — a pair that becomes
// withheld while subscribers are attached must say so on the wire once,
// not fall silent like a pair with no trades, and its bucket must be
// republished when it is served again.
func TestPublisher_WithheldPairPublishesTheWithholding(t *testing.T) {
	hub := streaming.NewHub(0)
	reader := &fakeReader{}
	asset := mustParse(t, "native")
	quote := mustParse(t, "fiat:USD")
	reader.SetSnapshot(asset, quote, v1.PriceSnapshot{
		AssetID: "native", Quote: "fiat:USD", Price: "0.07", PriceType: "vwap",
		ObservedAt: v1.WireTime(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)), WindowSeconds: 60,
	})
	ch, cancelSub, err := hub.Subscribe([]string{v1.PriceStreamTopic(asset, quote, 60)}, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancelSub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pub := streampublish.New(hub, reader, time.Second, nil, streampublish.Options{})
	go func() { _ = pub.Run(ctx, []canonical.Pair{{Base: asset, Quote: quote}}) }()

	if ev := nextEvent(t, ch, 2*time.Second); ev.Type != "price_update" {
		t.Fatalf("first event = %s, want price_update", ev.Type)
	}

	reader.SetErr(v1.PriceWithheldError(pricingguard.WithheldThinMarket))
	ev := nextEvent(t, ch, 2500*time.Millisecond)
	if ev.Type != "price_withheld" {
		t.Fatalf("event after the pair was withheld = %s, want price_withheld", ev.Type)
	}
	var body struct {
		AssetID string `json:"asset_id"`
		Quote   string `json:"quote"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Data, &body); err != nil {
		t.Fatalf("unmarshal price_withheld: %v", err)
	}
	if body.AssetID != "native" || body.Quote != "fiat:USD" || body.Reason != "substance" {
		t.Fatalf("price_withheld data = %+v, want native/fiat:USD reason substance", body)
	}

	// Still withheld on the next tick: the marker is not repeated.
	select {
	case ev := <-ch:
		t.Fatalf("unexpected %s while the verdict was unchanged", ev.Type)
	case <-time.After(1500 * time.Millisecond):
	}

	reader.SetErr(nil)
	if ev := nextEvent(t, ch, 2500*time.Millisecond); ev.Type != "price_update" {
		t.Fatalf("event once served again = %s, want the bucket republished as price_update", ev.Type)
	}
}

func (r *fakeReader) SetErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func nextEvent(t *testing.T, ch <-chan streaming.Event, within time.Duration) streaming.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(within):
		t.Fatalf("no event within %s", within)
	}
	return streaming.Event{}
}
