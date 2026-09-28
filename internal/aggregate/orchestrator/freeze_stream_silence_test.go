package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestTick_AnomalyFreeze_StreamCarriesAMarker is the reproduction for
// #721's remaining half: a pair that freezes mid-stream publishes nothing
// on the closed-bucket channel, so /v1/price/stream subscribers see only
// keepalives while /v1/price serves the last-known-good with
// flags.frozen. A frozen stream is indistinguishable from a quiet market.
//
// Skipped until the wire shape is decided: a frozen marker needs a new
// field or event kind on redispub.ClosedBucketEvent, its subscriber
// validation, and a documented SSE event on /v1/price/stream.
func TestTick_AnomalyFreeze_StreamCarriesAMarker(t *testing.T) {
	t.Skip("#721: frozen-mid-stream marker needs a closed-bucket wire/SSE contract decision")

	pair := xlmUsdtPair(t)
	cache, _ := newTestRedis(t)
	stream := &recordingStreamPublisher{}
	o := New(nil, cache, Config{
		Pairs:           []canonical.Pair{pair},
		Windows:         []time.Duration{5 * time.Minute},
		Anomaly:         newAnomalyChecker(t, pair),
		FreezeWriter:    &recordingFreezeMarker{},
		StreamPublisher: stream,
	})
	o.prevVWAPs[pair.String()+":"+(5*time.Minute).String()] = big.NewRat(1, 1)
	o.store = &mockStore{trades: []canonical.Trade{
		buildTrade(t, big.NewInt(100_000_000), big.NewInt(210_000_000), time.Now()),
	}}

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(stream.calls) == 0 {
		t.Fatal("a frozen tick published nothing to the stream: subscribers cannot tell a freeze from a quiet market")
	}
}
