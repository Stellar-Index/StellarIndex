package freeze_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// newRedis spins up an in-memory miniredis + a *redis.Client
// pointed at it. Returns both so tests can assert against the
// underlying store directly.
func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func nativeUSD(t *testing.T) (canonical.Asset, canonical.Asset) {
	t.Helper()
	usd, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("ParseAsset: %v", err)
	}
	return canonical.NativeAsset(), usd
}

// TestNewWriter_RejectsNilCache: a nil cache must fail at construction, not at first write.
func TestNewWriter_RejectsNilCache(t *testing.T) {
	if _, err := freeze.NewWriter(nil, 0); err == nil {
		t.Error("expected error for nil cache")
	}
}

// TestNewLooker_RejectsNilCache — same loud-misconfig stance.
func TestNewLooker_RejectsNilCache(t *testing.T) {
	if _, err := freeze.NewLooker(nil); err == nil {
		t.Error("expected error for nil cache")
	}
}

// TestLooker_FrozenForPair_AbsentMarker: clean state returns (false, nil), not an error.
func TestLooker_FrozenForPair_AbsentMarker(t *testing.T) {
	_, rdb := newRedis(t)
	l, _ := freeze.NewLooker(rdb)
	asset, quote := nativeUSD(t)

	frozen, err := l.FrozenForPair(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("err = %v, want nil for absent marker", err)
	}
	if frozen {
		t.Error("frozen = true for never-marked pair")
	}
}

// TestLooker_FrozenForPair_PresentMarker: a present marker returns (true, nil).
func TestLooker_FrozenForPair_PresentMarker(t *testing.T) {
	_, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	l, _ := freeze.NewLooker(rdb)
	asset, quote := nativeUSD(t)

	if err := w.Mark(context.Background(), asset, quote, "",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatal(err)
	}

	frozen, err := l.FrozenForPair(context.Background(), asset, quote)
	if err != nil {
		t.Fatalf("FrozenForPair: %v", err)
	}
	if !frozen {
		t.Error("frozen = false; marker should be present")
	}
}

// TestLooker_FrozenForPair_TTLExpiry: once the marker TTL elapses the pair reads (false, nil), like a never-marked pair.
func TestLooker_FrozenForPair_TTLExpiry(t *testing.T) {
	mr, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 30*time.Second)
	l, _ := freeze.NewLooker(rdb)
	asset, quote := nativeUSD(t)

	if err := w.Mark(context.Background(), asset, quote, "",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatal(err)
	}
	// Roll past TTL.
	mr.FastForward(60 * time.Second)

	frozen, err := l.FrozenForPair(context.Background(), asset, quote)
	if err != nil {
		t.Fatal(err)
	}
	if frozen {
		t.Error("frozen = true after TTL expiry")
	}
}

// TestLooker_DistinctPairsIsolated: freezing one (asset, quote) pair does not bleed into another.
func TestLooker_DistinctPairsIsolated(t *testing.T) {
	_, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	l, _ := freeze.NewLooker(rdb)
	xlm, usd := nativeUSD(t)
	eur, _ := canonical.ParseAsset("fiat:EUR")

	// Freeze XLM/USD only.
	if err := w.Mark(context.Background(), xlm, usd, "",
		anomaly.Decision{Action: anomaly.ActionFreeze}); err != nil {
		t.Fatal(err)
	}

	frozen, _ := l.FrozenForPair(context.Background(), xlm, usd)
	if !frozen {
		t.Error("XLM/USD should be frozen")
	}
	frozen, _ = l.FrozenForPair(context.Background(), xlm, eur)
	if frozen {
		t.Error("XLM/EUR should NOT be frozen (distinct pair)")
	}
}

// recordingSink captures every RecordFreeze call so tests can
// assert the Writer wired the sink correctly.
type recordingSink struct {
	calls []recordedFreeze
	err   error
	// failN fails the first N calls with err; 0 fails every call while err is set.
	failN int
}

type recordedFreeze struct {
	Asset       canonical.Asset
	Quote       canonical.Asset
	FrozenValue string
	Decision    anomaly.Decision
}

func (r *recordingSink) RecordFreeze(_ context.Context, asset, quote canonical.Asset, frozenValue string, decision anomaly.Decision) error {
	r.calls = append(r.calls, recordedFreeze{
		Asset:       asset,
		Quote:       quote,
		FrozenValue: frozenValue,
		Decision:    decision,
	})
	if r.failN > 0 && len(r.calls) > r.failN {
		return nil
	}
	return r.err
}

// errExploded is a sentinel for the sink-error test.
var errExploded = errSinkExploded("simulated sink failure")

type errSinkExploded string

func (e errSinkExploded) Error() string { return string(e) }

// silence unused-import warnings on platforms where this file
// is only partially read.
var (
	_ = json.Marshal
	_ = time.Now
	_ = miniredis.RunT
	_ = cachekeys.Freeze
)

// An owned ordinary ladder nobody advances is bounded like the unowned one, or a restart rehydrates a finished
// freeze. An escalated one is not: only an operator ends it (escalated_outage_test.go).

// abandonedLadder is an ordinary ladder whose last writer stopped
// hours ago: well past HoldUntil plus any grace.
func abandonedLadder(now time.Time) freeze.State {
	return freeze.State{
		FiredAt:        now.Add(-9 * time.Hour),
		HoldUntil:      now.Add(-6 * time.Hour),
		ExtensionsUsed: 2,
	}
}

// seedShortLadderBeside writes a windowed marker in which the 5m window
// holds an abandoned ordinary ladder and the 1h window holds `long`.
func seedShortLadderBeside(t *testing.T, long freeze.State) (*miniredis.Miniredis, *freeze.Writer, string) {
	t.Helper()
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	body, err := json.Marshal(freeze.Marker{
		AssetID: asset.String(), QuoteID: quote.String(), Windowed: true,
		Ladders: map[string]freeze.State{
			shortWindow.String(): abandonedLadder(time.Now().UTC()),
			longWindow.String():  long,
		},
	})
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	key := cachekeys.Freeze(asset, quote).String()
	if err := mr.Set(key, string(body)); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	return mr, w, key
}

// TestWriter_MarkerSetFailureIsCounted: a freeze decided while Redis rejects writes is never published, so the failure must be
// counted where the ladder-write-failures alert reads it, not only returned to a caller that logs and moves on.
func TestWriter_MarkerSetFailureIsCounted(t *testing.T) {
	mr, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	asset, quote := nativeUSD(t)
	c := obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("marker_set")
	before := testutil.ToFloat64(c)

	mr.SetError("READONLY You can't write against a read only replica.")
	if err := w.Mark(context.Background(), asset, quote, "", anomaly.Decision{Action: anomaly.ActionFreeze}); err == nil {
		t.Fatal("Mark succeeded against a read-only Redis")
	}
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Fatalf("marker_set failures delta = %v, want 1", got)
	}
}

// TestWriter_MarkerSetCancelledIsNotCounted: a marker write cut off by shutdown is not a Redis failure, and the zero-for alert must
// not page on it.
func TestWriter_MarkerSetCancelledIsNotCounted(t *testing.T) {
	_, rdb := newRedis(t)
	w, _ := freeze.NewWriter(rdb, 0)
	asset, quote := nativeUSD(t)
	c := obs.AnomalyFreezeLadderWriteFailuresTotal.WithLabelValues("marker_set")
	before := testutil.ToFloat64(c)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Mark(ctx, asset, quote, "", anomaly.Decision{Action: anomaly.ActionFreeze}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mark on a cancelled context: err = %v, want context.Canceled", err)
	}
	if got := testutil.ToFloat64(c) - before; got != 0 {
		t.Fatalf("marker_set failures delta = %v, want 0", got)
	}
}
