package redispub_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming/redispub"
)

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
