package v1

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// panicOnceOracle panics on its first LatestOracleStreams call and
// answers normally afterward, so a test can drive the exact failure
// a read that blows up mid-fill.
type panicOnceOracle struct {
	calls   int
	streams []canonical.OracleUpdate
}

func (o *panicOnceOracle) LatestOracleUpdatesForAsset(context.Context, canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *panicOnceOracle) LatestOracleUpdatesForAssets(context.Context, []canonical.Asset, string) ([]canonical.OracleUpdate, error) {
	return nil, nil
}

func (o *panicOnceOracle) LatestOracleStreams(context.Context) ([]canonical.OracleUpdate, error) {
	o.calls++
	if o.calls == 1 {
		panic("boom")
	}
	return o.streams, nil
}

// TestCachedRWAReferences_PanicDoesNotWedgeTheFlight pins that a panic
// out of the oracle read must not leave rwaRefFlight latched on a channel
// nobody closes. The outer recover simulates net/http's own per-request
// panic recovery, which is what actually catches the panic in production
// — the bug was never a process crash, it was the single-flight marker
// surviving that recovery in a state no later caller could clear.
func TestCachedRWAReferences_PanicDoesNotWedgeTheFlight(t *testing.T) {
	oracle := &panicOnceOracle{
		streams: []canonical.OracleUpdate{
			refUpdate(t, "redstone", "rwa:USTRY", "fiat:USD", "104000000", 8, time.Now()),
		},
	}
	s := &Server{Options: Options{Oracle: oracle}, logger: slog.Default()}

	func() {
		defer func() { _ = recover() }()
		s.cachedRWAReferences(context.Background())
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := s.cachedRWAReferences(ctx)
	if !got.available {
		t.Fatalf("second call after a panicked read got no reference: the single-flight marker is wedged (GH-587)")
	}
}
