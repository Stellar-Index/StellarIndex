package redispub_test

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestPriceStreamSpecExampleMatchesTheWire pins the /v1/price/stream
// OpenAPI example to the frame this bridge actually fans out (GH-751). The
// example showed `as_of` 42 s after a bucket-aligned `observed_at` and a
// `flags` object on the 300 s series; the wire carries neither — the
// aggregator stamps each event with the closed bucket's end and the bridge
// emits it as both timestamps, with no flags. A client that keyed on the
// documented discriminator was reading a contract nobody produces.
func TestPriceStreamSpecExampleMatchesTheWire(t *testing.T) {
	bridged := map[string]map[string]any{
		"price_update": bridgedFrame(t, func(ctx context.Context, pub *redispub.Publisher, pair canonical.Pair, bucketEnd time.Time) error {
			return pub.PublishClosedBucket(ctx, pair, 5*time.Minute, "0.159608357106", bucketEnd, nil)
		}),
		"price_frozen": bridgedFrame(t, func(ctx context.Context, pub *redispub.Publisher, pair canonical.Pair, bucketEnd time.Time) error {
			return pub.PublishFrozenBucket(ctx, pair, 5*time.Minute, bucketEnd, bucketEnd)
		}),
	}

	for i, sf := range specStreamFrames(t) {
		frame := sf.body
		wire, ok := bridged[sf.eventType]
		if !ok {
			t.Errorf("spec frame %d has event type %q, which the bridge never emits", i, sf.eventType)
			continue
		}
		if got, want := keySet(frame), keySet(wire); got != want {
			t.Errorf("spec frame %d top-level keys = %s, wire emits %s", i, got, want)
		}
		data, _ := frame["data"].(map[string]any)
		if got, want := keySet(data), keySet(wire["data"].(map[string]any)); got != want {
			t.Errorf("spec frame %d data keys = %s, wire emits %s", i, got, want)
		}
		if frame["as_of"] != data["observed_at"] {
			t.Errorf("spec frame %d: as_of %v != observed_at %v — on this surface both are the closed bucket's end",
				i, frame["as_of"], data["observed_at"])
		}
		observed, err := time.Parse(time.RFC3339, data["observed_at"].(string))
		if err != nil || !observed.Equal(observed.Truncate(time.Minute)) {
			t.Errorf("spec frame %d: observed_at %v is not a closed 1-minute bucket end", i, data["observed_at"])
		}
	}
}

// bridgedFrame publishes one event the way the aggregator does (stamped
// with a minute-aligned bucket end) and returns what the Hub received.
func bridgedFrame(t *testing.T, publish func(context.Context, *redispub.Publisher, canonical.Pair, time.Time) error) map[string]any {
	t.Helper()
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
	go func() { _ = sub.Run(ctx) }()
	time.Sleep(50 * time.Millisecond) // miniredis SUBSCRIBE registration races PUBLISH

	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := canonical.NewPair(canonical.NativeAsset(), usd)
	if err != nil {
		t.Fatal(err)
	}
	bucketEnd := time.Now().UTC().Truncate(time.Minute)
	if err := publish(ctx, pub, pair, bucketEnd); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(hub.Calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	calls := hub.Calls()
	if len(calls) != 1 {
		t.Fatalf("Hub.Publish called %d times, want 1", len(calls))
	}
	var frame map[string]any
	if err := json.Unmarshal(calls[0].data, &frame); err != nil {
		t.Fatalf("decode bridged frame: %v", err)
	}
	return frame
}

type specFrame struct {
	eventType string
	body      map[string]any
}

// specStreamFrames returns the decoded `data:` lines of the /price/stream
// 200 example in the source OpenAPI spec, each with its `event:` type.
func specStreamFrames(t *testing.T) []specFrame {
	t.Helper()
	raw, err := os.ReadFile("../../../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Example any `yaml:"example"`
					} `yaml:"content"`
				} `yaml:"responses"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	example, _ := spec.Paths["/price/stream"].Get.Responses["200"].Content["text/event-stream"].Example.(string)
	var frames []specFrame
	eventType := ""
	for _, line := range strings.Split(example, "\n") {
		if et, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = et
			continue
		}
		body, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(body), &frame); err != nil {
			t.Fatalf("spec example frame is not JSON: %v (%s)", err, body)
		}
		frames = append(frames, specFrame{eventType: eventType, body: frame})
	}
	if len(frames) == 0 {
		t.Fatal("no data: frames in the /price/stream example — the scan is broken")
	}
	return frames
}

func keySet(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
