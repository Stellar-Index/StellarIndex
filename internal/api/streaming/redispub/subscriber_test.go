package redispub_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeHub captures Hub.Publish calls.
type fakeHub struct {
	mu    sync.Mutex
	calls []hubCall
}

type hubCall struct {
	topic     string
	eventType string
	data      []byte
}

func (h *fakeHub) Publish(topic, eventType string, data []byte) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, hubCall{topic: topic, eventType: eventType, data: data})
	return "fake-id"
}

func (h *fakeHub) Calls() []hubCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hubCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// newRedis spins up an in-memory miniredis + a *redis.Client.
// miniredis supports SUBSCRIBE/PUBLISH out of the box.
func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

// TestNewSubscriber_RequiresInputs — operator misconfig must fail
// at construction.
func TestNewSubscriber_RequiresInputs(t *testing.T) {
	if _, err := redispub.NewSubscriber(nil, "", &fakeHub{}, nil); err == nil {
		t.Error("expected error for nil cache")
	}
	_, rdb := newRedis(t)
	if _, err := redispub.NewSubscriber(rdb, "", nil, nil); err == nil {
		t.Error("expected error for nil hub")
	}
}

// TestSubscriber_RoundTrip — the canonical happy path: a Publisher
// writes one event; the Subscriber decodes it and republishes on
// the Hub with the canonical topic key.
func TestSubscriber_RoundTrip(t *testing.T) {
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, "test:closed", hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	pub, err := redispub.NewPublisher(rdb, "test:closed")
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- sub.Run(ctx) }()

	// miniredis SUBSCRIBE registration is racy with PUBLISH —
	// give the subscriber a beat to bind before we publish.
	time.Sleep(50 * time.Millisecond)

	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	// A real closed-bucket event's ObservedAt is the bucket END, i.e.
	// ~now; the subscriber now enforces that freshness (F2), so the
	// fixture uses a recent timestamp rather than a fixed historical one.
	observedAt := time.Now().UTC().Add(-time.Minute)

	if err := pub.PublishClosedBucket(ctx, pair, 5*time.Minute, "0.123456789012", observedAt, nil); err != nil {
		t.Fatalf("PublishClosedBucket: %v", err)
	}

	// Wait briefly for the message to flow through.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	c := calls[0]
	wantTopic := v1.PriceStreamTopic(pair.Base, pair.Quote, 300)
	if c.topic != wantTopic {
		t.Errorf("topic = %q, want %q", c.topic, wantTopic)
	}
	if c.eventType != "price_update" {
		t.Errorf("eventType = %q, want price_update", c.eventType)
	}

	// Payload round-trip — Subscriber forwards the published
	// JSON bytes verbatim, so re-decode should match.
	got := decodeCall(t, c.data)
	if got.Data.AssetID != pair.Base.String() || got.Data.Quote != pair.Quote.String() {
		t.Errorf("payload identity = %s/%s, want %s/%s",
			got.Data.AssetID, got.Data.Quote, pair.Base.String(), pair.Quote.String())
	}
	if got.Data.PriceType != "vwap" {
		t.Errorf("price_type = %q, want vwap", got.Data.PriceType)
	}

	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
}

// TestSubscriber_TopicFormatStaysInSync — sentinel test: Subscriber's
// topic format must match v1.PriceStreamTopic since the Hub layer
// expects the exact string.
func TestSubscriber_TopicFormatStaysInSync(t *testing.T) {
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	want := v1.PriceStreamTopic(canonical.NativeAsset(), usd, 300)
	if want != "closed:"+canonical.NativeAsset().String()+"/"+usd.String()+"/300" {
		t.Fatalf("v1.PriceStreamTopic format changed; redispub Subscriber must update too. Got %q", want)
	}
}

// publishRaw sends raw bytes on the pub/sub channel, bypassing the
// typed Publisher — the exact shape a host-adjacent process with Redis
// network access (r1 Redis has NO AUTH) could inject. Used to prove the
// subscriber validates + sanitizes before fan-out (F2).
func publishRaw(t *testing.T, rdb *redis.Client, channel, payload string) {
	t.Helper()
	if err := rdb.Publish(context.Background(), channel, payload).Err(); err != nil {
		t.Fatalf("PUBLISH: %v", err)
	}
}

// wireEnvelope mirrors the envelope shape the Subscriber fans out
// (field-compatible with /v1/price responses — see subscriber.go
// closedBucketEnvelope).
type wireEnvelope struct {
	Data struct {
		AssetID       string    `json:"asset_id"`
		Quote         string    `json:"quote"`
		Price         string    `json:"price"`
		PriceType     string    `json:"price_type"`
		ObservedAt    time.Time `json:"observed_at"`
		WindowSeconds int64     `json:"window_seconds"`
	} `json:"data"`
	AsOf time.Time `json:"as_of"`
}

func decodeCall(t *testing.T, data []byte) wireEnvelope {
	t.Helper()
	var ev wireEnvelope
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("decode forwarded payload %q: %v", data, err)
	}
	return ev
}

func hasValue(t *testing.T, calls []hubCall, want string) bool {
	t.Helper()
	for _, c := range calls {
		if decodeCall(t, c.data).Data.Price == want {
			return true
		}
	}
	return false
}

func callValues(t *testing.T, calls []hubCall) string {
	t.Helper()
	vs := make([]string, len(calls))
	for i, c := range calls {
		vs[i] = decodeCall(t, c.data).Data.Price
	}
	return strings.Join(vs, ",")
}

// TestSubscriber_DropsForgedValueDecimal — a host-adjacent process
// injecting an event with a non-numeric / non-positive / absurd
// value_decimal must be DROPPED, not fanned out to SSE clients (F2).
//
// Proven by publishing a batch of forged events followed by ONE valid
// sentinel: messages on a single Redis channel are processed in order,
// so once the sentinel reaches the Hub every forged message before it
// has already been handled. A Hub holding exactly the sentinel proves
// each forged message was rejected. Against the un-fixed subscriber
// (which forwards anything with a non-empty asset/quote) every forged
// message is fanned out too and this fails.
func TestSubscriber_DropsForgedValueDecimal(t *testing.T) {
	const channel = "test:closed"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let SUBSCRIBE bind (miniredis race)

	asset := canonical.NativeAsset().String()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote := usd.String()
	observedAt := time.Now().UTC().Format(time.RFC3339)

	forged := []string{
		"-5.000000000000",                 // negative
		"0.000000000000",                  // zero
		"not-a-number",                    // non-numeric
		"1000000000000000000000000000000", // 10^30 — absurd
		"1/2",                             // fraction form big.Rat would accept
		"1e9",                             // scientific form big.Rat would accept
		"+5.000000000000",                 // explicit leading '+'
		"0x1p4",                           // hex float literal big.Rat would accept (Q171)
		"1_000.5",                         // underscore-separated literal (Q171)
		"0b101",                           // binary literal (Q171)
		"0o17",                            // octal literal (Q171)
	}
	for _, v := range forged {
		publishRaw(t, rdb, channel, fmt.Sprintf(
			`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q,"injected":"forged"}`,
			asset, quote, v, observedAt))
	}
	const sentinelValue = "7.654321000000"
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q}`,
		asset, quote, sentinelValue, observedAt))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinelValue) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if !hasValue(t, calls, sentinelValue) {
		t.Fatalf("valid sentinel never fanned out; forged batch may have stalled the loop")
	}
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1 — forged events must be dropped; forwarded values = [%s]",
			len(calls), callValues(t, calls))
	}
}

// TestSubscriber_StripsInjectedExtraFields — a VALID event carrying
// attacker-injected extra JSON fields is fanned out, but SANITIZED to
// the canonical four-field shape: the subscriber re-marshals the
// validated struct rather than forwarding the raw payload (F2), so the
// injected fields never reach SSE clients. The value_decimal survives
// unchanged as a decimal string (the money representation is not
// weakened). Against the un-fixed subscriber (which forwards the raw
// bytes) the injected fields leak through and this fails.
func TestSubscriber_StripsInjectedExtraFields(t *testing.T) {
	const channel = "test:closed"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	asset := canonical.NativeAsset().String()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	quote := usd.String()
	observedAt := time.Now().UTC().Format(time.RFC3339)

	const value = "0.123456789012"
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":%q,"observed_at":%q,`+
			`"injected_field":"pwned","evil":{"nested":true}}`,
		asset, quote, value, observedAt))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	data := calls[0].data
	for _, leak := range []string{"injected_field", "pwned", "evil", "nested"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("forwarded payload leaked injected content %q: %s", leak, data)
		}
	}
	ev := decodeCall(t, data)
	if ev.Data.AssetID != asset || ev.Data.Quote != quote {
		t.Errorf("identity = %s/%s, want %s/%s", ev.Data.AssetID, ev.Data.Quote, asset, quote)
	}
	if ev.Data.Price != value {
		t.Errorf("price = %q, want %q (must survive unchanged as a decimal string)", ev.Data.Price, value)
	}
	if ev.Data.WindowSeconds != 300 {
		t.Errorf("window_seconds = %d, want 300", ev.Data.WindowSeconds)
	}
}

// TestSubscriber_DropsNonCanonicalAssetIdentity — asset and quote are
// echoed to SSE clients as asset_id / quote and form the Hub topic key,
// so a forged event naming anything canonical.Asset.String() could not
// have produced must be DROPPED, not fanned out verbatim. Same
// batch-then-sentinel proof as TestSubscriber_DropsForgedValueDecimal.
func TestSubscriber_DropsNonCanonicalAssetIdentity(t *testing.T) {
	const channel = "test:closed"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	observedAt := time.Now().UTC().Format(time.RFC3339)
	forged := [][2]string{
		{"<script>alert(1)</script>", "fiat:USD"}, // not an asset id at all
		{"native", "injected quote text"},         // quote not an asset id
		{"XLM", "fiat:USD"},                       // parses, but not the canonical spelling
		{"NATIVE", "fiat:USD"},                    // case variant of a canonical id
		{"native", "native"},                      // degenerate self-pair
	}
	for _, f := range forged {
		publishRaw(t, rdb, channel, fmt.Sprintf(
			`{"asset":%q,"quote":%q,"window_seconds":300,"value_decimal":"1.000000000000","observed_at":%q}`,
			f[0], f[1], observedAt))
	}
	const sentinelValue = "7.654321000000"
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":"native","quote":"fiat:USD","window_seconds":300,"value_decimal":%q,"observed_at":%q}`,
		sentinelValue, observedAt))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinelValue) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if !hasValue(t, calls, sentinelValue) {
		t.Fatalf("valid sentinel never fanned out")
	}
	if len(calls) != 1 {
		topics := make([]string, len(calls))
		for i, c := range calls {
			topics[i] = c.topic
		}
		t.Fatalf("Hub.Publish called %d times, want 1 — non-canonical identities must be dropped; topics = %v",
			len(calls), topics)
	}
}

// TestSubscriber_ForwardsOneFramePerBucket — two aggregators on one
// channel (a restart overlap, a failover pair) each publish every
// bucket; a delayed message can arrive after a newer bucket. The Hub
// topic must carry each bucket once, in order: a repeat of the
// newest bucket from a second producer and an older bucket are both
// dropped, and the next bucket goes through.
func TestSubscriber_ForwardsOneFramePerBucket(t *testing.T) {
	const channel = "test:closed"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	bucket := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	publish := func(producer, value string, at time.Time) {
		publishRaw(t, rdb, channel, fmt.Sprintf(
			`{"asset":"native","quote":"fiat:USD","window_seconds":300,"value_decimal":%q,"observed_at":%q,"producer_id":%q}`,
			value, at.Format(time.RFC3339), producer))
	}
	publish("agg-a", "0.100000000000", bucket)                   // first
	publish("agg-b", "0.110000000000", bucket)                   // same bucket, second aggregator
	publish("agg-a", "0.090000000000", bucket.Add(-time.Minute)) // older bucket, delivered late
	const sentinelValue = "0.120000000000"
	publish("agg-a", sentinelValue, bucket.Add(time.Minute)) // next bucket

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinelValue) {
		time.Sleep(10 * time.Millisecond)
	}
	if got, want := callValues(t, hub.Calls()), "0.100000000000,"+sentinelValue; got != want {
		t.Fatalf("forwarded values = [%s], want [%s] — one frame per bucket, in order", got, want)
	}
}

// subscribeOutcomes is every outcome label the subscriber can emit. A
// responder reading `malformed` must not be looking at a clock-skew drop.
var subscribeOutcomes = []string{"ok", "decode_error", "malformed", "future_observed_at", "stale_observed_at", "duplicate", "dropped_slow_consumer"}

// TestNewSubscriber_SeedsEveryOutcome pins that a wired subscriber
// exports every outcome at zero before its first message, so a rule can
// read "no ok events" as a real zero rather than an absent series.
func TestNewSubscriber_SeedsEveryOutcome(t *testing.T) {
	_, rdb := newRedis(t)
	if _, err := redispub.NewSubscriber(rdb, "test:seed", &fakeHub{}, nil); err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	mfs, err := obs.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, mf := range mfs {
		if mf.GetName() != "stellarindex_api_stream_subscribe_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "outcome" {
					seen[lp.GetValue()] = true
				}
			}
		}
	}
	for _, o := range subscribeOutcomes {
		if !seen[o] {
			t.Errorf("outcome %q not exported after NewSubscriber; exported = %v", o, seen)
		}
	}
}

// TestSubscriber_CountsRejectionsByCause — an API host whose clock lags
// the aggregator by more than the forward-skew tolerance drops every
// event. That must be countable as clock skew, distinct from a stale
// replay and from a malformed payload.
func TestSubscriber_CountsRejectionsByCause(t *testing.T) {
	const channel = "test:outcomes"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let SUBSCRIBE bind (miniredis race)

	before := map[string]float64{}
	for _, o := range subscribeOutcomes {
		before[o] = testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues(o))
	}

	asset := canonical.NativeAsset().String()
	const quote = "fiat:USD"
	event := func(window int, value string, at time.Time) string {
		return fmt.Sprintf(`{"asset":%q,"quote":%q,"window_seconds":%d,"value_decimal":%q,"observed_at":%q}`,
			asset, quote, window, value, at.UTC().Format(time.RFC3339))
	}
	now := time.Now()
	publishRaw(t, rdb, channel, event(300, "1.0", now.Add(6*time.Minute)))
	publishRaw(t, rdb, channel, event(300, "1.0", now.Add(-25*time.Hour)))
	publishRaw(t, rdb, channel, event(0, "1.0", now))
	const sentinel = "7.654321000000"
	publishRaw(t, rdb, channel, event(300, sentinel, now))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hasValue(t, hub.Calls(), sentinel) {
		time.Sleep(10 * time.Millisecond)
	}
	if !hasValue(t, hub.Calls(), sentinel) {
		t.Fatalf("valid sentinel never fanned out")
	}

	want := map[string]float64{"ok": 1, "decode_error": 0, "malformed": 1, "future_observed_at": 1, "stale_observed_at": 1}
	for _, o := range subscribeOutcomes {
		if got := testutil.ToFloat64(obs.APIStreamSubscribeTotal.WithLabelValues(o)) - before[o]; got != want[o] {
			t.Errorf("outcome %q incremented by %v, want %v", o, got, want[o])
		}
	}
}

// runSubscriber starts a Subscriber on channel and returns its hub, its
// Redis client and a raw-payload publisher.
func runSubscriber(t *testing.T, channel string) (*fakeHub, *redis.Client, func(payload string)) {
	t.Helper()
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // let SUBSCRIBE bind (miniredis race)
	return hub, rdb, func(payload string) { publishRaw(t, rdb, channel, payload) }
}

func waitForCalls(hub *fakeHub, n int) []hubCall {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) < n {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // catch a frame that should not have come
	return hub.Calls()
}

// A frozen bucket reaches SSE clients as a value-less price_frozen frame in
// the 60-second series' price_frozen shape, plus frozen_since.
func TestSubscriber_FrozenEventFansOutAsPriceFrozen(t *testing.T) {
	const channel = "test:closed"
	hub, rdb, _ := runSubscriber(t, channel)
	pub, err := redispub.NewPublisher(rdb, channel)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	pair := canonical.Pair{Base: canonical.NativeAsset(), Quote: usd}
	bucket := time.Now().UTC().Truncate(time.Minute)
	since := bucket.Add(-3 * time.Minute)
	if err := pub.PublishFrozenBucket(context.Background(), pair, 5*time.Minute, bucket, since); err != nil {
		t.Fatalf("PublishFrozenBucket: %v", err)
	}

	calls := waitForCalls(hub, 1)
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	if calls[0].eventType != "price_frozen" {
		t.Errorf("event type = %q, want price_frozen", calls[0].eventType)
	}
	if want := "closed:native/fiat:USD/300"; calls[0].topic != want {
		t.Errorf("topic = %q, want %q", calls[0].topic, want)
	}
	at, from := bucket.Format(time.RFC3339), since.Format(time.RFC3339)
	want := fmt.Sprintf(`{"data":{"asset_id":"native","quote":"fiat:USD","observed_at":%q,"window_seconds":300,"frozen_since":%q},"as_of":%q,"flags":{"frozen":true,"frozen_checked":true}}`,
		at, from, at)
	if got := string(calls[0].data); got != want {
		t.Errorf("frame =\n  %s\nwant\n  %s", got, want)
	}
}

// A frozen event carrying any part of a value, an impossible frozen_since,
// or an unknown kind was not produced by an aggregator and is dropped.
func TestSubscriber_DropsMalformedFrozenEvents(t *testing.T) {
	hub, _, publish := runSubscriber(t, "test:closed")
	bucket := time.Now().UTC().Truncate(time.Minute)
	at, later := bucket.Format(time.RFC3339), bucket.Add(time.Minute).Format(time.RFC3339)
	head := `{"asset":"native","quote":"fiat:USD","window_seconds":300,"observed_at":"` + at + `"`
	for _, forged := range []string{
		head + `,"kind":"frozen","value_decimal":"0.100000000000"}`,
		head + `,"kind":"frozen","truncated":false}`,
		head + `,"kind":"frozen","truncated":true,"covered_from":"` + at + `"}`,
		head + `,"kind":"frozen","frozen_since":"` + later + `"}`,
		head + `,"kind":"thawed"}`,
		head + `,"value_decimal":"0.100000000000","frozen_since":"` + at + `"}`,
	} {
		publish(forged)
	}
	publish(head + `,"kind":"frozen","frozen_since":"` + at + `"}`)

	calls := waitForCalls(hub, 1)
	if len(calls) != 1 || calls[0].eventType != "price_frozen" {
		t.Fatalf("forwarded %d frames (%+v), want only the valid price_frozen", len(calls), calls)
	}
}

// A topic carries one frame per bucket whatever its kind, in order: a price
// for a bucket already forwarded as frozen is a duplicate, and the next
// bucket's price goes out as a price_update.
func TestSubscriber_OneFramePerBucketAcrossKinds(t *testing.T) {
	hub, _, publish := runSubscriber(t, "test:closed")
	bucket := time.Now().UTC().Truncate(time.Minute)
	event := func(at time.Time, rest string) string {
		return `{"asset":"native","quote":"fiat:USD","window_seconds":300,"observed_at":"` + at.Format(time.RFC3339) + `",` + rest + `}`
	}
	publish(event(bucket.Add(-time.Minute), `"kind":"frozen"`))
	publish(event(bucket.Add(-time.Minute), `"value_decimal":"0.100000000000"`))
	publish(event(bucket, `"value_decimal":"0.200000000000"`))

	calls := waitForCalls(hub, 2)
	if len(calls) != 2 {
		t.Fatalf("forwarded %d frames, want 2", len(calls))
	}
	if calls[0].eventType != "price_frozen" || calls[1].eventType != "price_update" {
		t.Errorf("event types = %q, %q; want price_frozen, price_update", calls[0].eventType, calls[1].eventType)
	}
	if got := decodeCall(t, calls[1].data).Data.Price; got != "0.200000000000" {
		t.Errorf("second frame price = %q, want the next bucket's", got)
	}
}

// TestSubscriber_FrameTimestampsRenderUTC pins that the /v1/price/stream
// frame renders as_of and observed_at with a literal Z even when the
// producer's event carried a local offset: a client bucketing on the
// string would otherwise mis-bucket by the offset.
func TestSubscriber_FrameTimestampsRenderUTC(t *testing.T) {
	const channel = "test:closed:utc"
	_, rdb := newRedis(t)
	hub := &fakeHub{}
	sub, err := redispub.NewSubscriber(rdb, channel, hub, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	bucket := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	local := bucket.In(time.FixedZone("CEST", 2*60*60))
	publishRaw(t, rdb, channel, fmt.Sprintf(
		`{"asset":"native","quote":"fiat:USD","window_seconds":300,"value_decimal":"0.100000000000","observed_at":%q}`,
		local.Format(time.RFC3339)))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d frames, want 1", len(calls))
	}
	want := `"` + bucket.Format("2006-01-02T15:04:05Z") + `"`
	for _, key := range []string{`"as_of":`, `"observed_at":`} {
		if !bytes.Contains(calls[0].data, []byte(key+want)) {
			t.Errorf("frame %s = %s, want %s%s (UTC with a Z offset)", key, calls[0].data, key, want)
		}
	}
}
