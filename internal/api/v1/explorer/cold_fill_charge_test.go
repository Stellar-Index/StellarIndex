package explorer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

var coldFillClock = ratelimit.WithClock(func() time.Time { return time.Unix(1_700_000_000, 0) })

// coldFillServer is a cold-path route in production shape: the anonymous limiter in front, the shared contract-detail cache, and
// the handlers' error mapping (retryableColdMiss, then writeRetryable). nil b disables the limiter.
func coldFillServer(h *Handler, b *ratelimit.Bucket, compute func(context.Context) (any, error)) http.Handler {
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) { w.WriteHeader(status) }
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _, _, err := h.contractDetailCached(r.Context(), r.URL.Query().Get("k"), compute)
		if err != nil {
			if retryableColdMiss(r.Context(), err) {
				h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/contract-detail-timeout", "Contract detail timed out")
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return middleware.RateLimitBySubject(b, nil, nil, nil)(inner)
}

func getKey(srv http.Handler, key string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?k="+key, nil))
	return w
}

func countingCompute(reads *atomic.Int64) func(context.Context) (any, error) {
	return func(context.Context) (any, error) {
		reads.Add(1)
		return "payload", nil
	}
}

func flightIdle(f *perKeyFlight) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inGoing) == 0
}

// TestColdFill_DistinctColdKeysThrottledAtAdmission: one anonymous client walking distinct cold keys gets budget/(1+ColdFillCost)
// fills, then a 429 with Retry-After that starts no read, claims no flight and never reaches the refresh gate.
func TestColdFill_DistinctColdKeysThrottledAtAdmission(t *testing.T) {
	const budget = 100
	admitted := budget / (1 + ratelimit.ColdFillCost)
	b := ratelimit.New(nil, budget, time.Minute, coldFillClock)
	var reads atomic.Int64
	compute := countingCompute(&reads)

	h1, _ := newSWRHandler()
	srv1 := coldFillServer(h1, b, compute)
	for i := range admitted {
		if w := getKey(srv1, fmt.Sprintf("ev:100:C%d", i)); w.Code != http.StatusOK {
			t.Fatalf("cold key %d within budget: status %d, want 200", i, w.Code)
		}
	}
	if got := reads.Load(); got != int64(admitted) {
		t.Fatalf("reads after the admitted phase = %d, want %d", got, admitted)
	}

	// Same client and budget, a fresh cache whose class share is held: a request that reached the gate would park, then 503.
	h2, _ := newSWRHandler()
	gate := h2.detachedGate()
	class := detachedClassForKey("ev:100:C")
	held := 0
	for gate.TryAcquireClass(class) {
		held++
	}
	refusedBefore := testutil.ToFloat64(obs.RateLimitColdFillRefusedTotal)
	srv2 := coldFillServer(h2, b, compute)
	for i := range 3 {
		w := getKey(srv2, fmt.Sprintf("ev:100:C%d", admitted+i))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("cold key past budget (%d): status %d, want 429", i, w.Code)
		}
		if w.Header().Get("Retry-After") == "" {
			t.Fatalf("cold key past budget (%d): 429 without Retry-After", i)
		}
	}
	if got := reads.Load(); got != int64(admitted) {
		t.Fatalf("reads = %d after refusals, want %d: a refused request started a lake read", got, admitted)
	}
	// Only the first refusal is the cold surcharge; the budget is then spent, so later ones stop at the base charge.
	if got := testutil.ToFloat64(obs.RateLimitColdFillRefusedTotal) - refusedBefore; got != 1 {
		t.Fatalf("cold-fill refusals counted = %v, want 1", got)
	}
	if !flightIdle(&h2.contractDetail.flight) {
		t.Fatal("a refused cold read left its flight claimed")
	}
	for range held {
		gate.ReleaseClass(class)
	}
	for i := range held {
		if !gate.TryAcquireClass(class) {
			t.Fatalf("class slot %d not free after the refusals: a refused read holds a gate slot", i)
		}
	}
}

// TestColdFill_WarmReadCostsOne: the surcharge is paid once, by the fill; every later read of the warm key costs one token.
func TestColdFill_WarmReadCostsOne(t *testing.T) {
	b := ratelimit.New(nil, 100, time.Minute, coldFillClock)
	var reads atomic.Int64
	h, _ := newSWRHandler()
	srv := coldFillServer(h, b, countingCompute(&reads))
	if w := getKey(srv, "ev:100:CWARM"); w.Code != http.StatusOK {
		t.Fatalf("cold fill: status %d", w.Code)
	}
	cold := 1 + ratelimit.ColdFillCost
	for i, want := range []int{100 - cold - 1, 100 - cold - 2} {
		w := getKey(srv, "ev:100:CWARM")
		if w.Code != http.StatusOK {
			t.Fatalf("warm read %d: status %d", i, w.Code)
		}
		if got := w.Header().Get("X-RateLimit-Remaining"); got != fmt.Sprint(want) {
			t.Fatalf("warm read %d: X-RateLimit-Remaining = %s, want %d (fill %d tokens, each warm read 1)", i, got, want, cold)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("reads = %d, want 1", reads.Load())
	}
}

// doneSignalCtx closes joined on the first Done call: on the cold path only a request waiting on a flight selects on ctx.Done.
type doneSignalCtx struct {
	context.Context
	once   sync.Once
	joined chan struct{}
}

func (c *doneSignalCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

// TestColdFill_JoiningAnInFlightFillIsNotSurcharged: only the flight's owner pays; a request that joins it pays nothing extra.
func TestColdFill_JoiningAnInFlightFillIsNotSurcharged(t *testing.T) {
	h, _ := newSWRHandler()
	var charged atomic.Int64
	base := ratelimit.WithColdFillCharge(context.Background(), func(n int) error {
		charged.Add(int64(n))
		return nil
	})
	ctx := &doneSignalCtx{Context: base, joined: make(chan struct{})}
	const key = "ev:100:CJOIN"
	fl, owner := h.contractDetail.flight.begin(key) // another request's fill, up for the whole test
	if !owner {
		t.Fatal("setup: flight already up")
	}
	errc := make(chan error, 1)
	go func() {
		_, _, _, err := h.contractDetailCached(ctx, key, func(context.Context) (any, error) {
			return nil, errors.New("a joiner must not compute")
		})
		errc <- err
	}()
	<-ctx.joined
	h.contractDetail.put(key, "payload")
	h.contractDetail.flight.end(key, fl, nil)
	if err := <-errc; err != nil {
		t.Fatalf("joiner: %v", err)
	}
	if got := charged.Load(); got != 0 {
		t.Fatalf("joiner charged %d extra tokens, want 0", got)
	}
}

// TestColdFill_NoChargeInContextUnchanged: with the limiter disabled (no bucket, so no charge in context) every cold key fills as before.
func TestColdFill_NoChargeInContextUnchanged(t *testing.T) {
	if err := ratelimit.ChargeColdFill(context.Background()); err != nil {
		t.Fatalf("ChargeColdFill without a charge = %v, want nil", err)
	}
	var reads atomic.Int64
	h, _ := newSWRHandler()
	srv := coldFillServer(h, nil, countingCompute(&reads))
	const n = 40 // past any surcharge budget the tests above use
	for i := range n {
		if w := getKey(srv, fmt.Sprintf("ev:100:C%d", i)); w.Code != http.StatusOK {
			t.Fatalf("cold key %d without a limiter: status %d, want 200", i, w.Code)
		}
	}
	if reads.Load() != n {
		t.Fatalf("reads = %d, want %d", reads.Load(), n)
	}
}

// TestColdFill_HoldersRefusalStartsNoScan: the holders board's cold path pays the same surcharge, and a refusal returns before any
// scan or flight.
func TestColdFill_HoldersRefusalStartsNoScan(t *testing.T) {
	h, sr := newSWRHandler()
	ctx := ratelimit.WithColdFillCharge(context.Background(), func(int) error {
		return fmt.Errorf("budget spent: %w", ratelimit.ErrColdFillRefused)
	})
	_, _, _, _, err := h.assetHoldersCached(ctx, "USDC-"+validTestAccount, 10)
	if !errors.Is(err, ratelimit.ErrColdFillRefused) {
		t.Fatalf("err = %v, want ErrColdFillRefused", err)
	}
	if !retryableColdMiss(ctx, err) {
		t.Fatal("a refused cold fill must take the handlers' retryable branch, not a 500")
	}
	if got := sr.holdersCalls.Load(); got != 0 {
		t.Fatalf("holders scans = %d, want 0", got)
	}
	if !flightIdle(&h.assetHolders.flight) {
		t.Fatal("a refused holders read left its flight claimed")
	}
}
