package soroswap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

// White-box half of the RLT-416 retry regression (see
// factory_seed_retry_test.go for the fake RPC and the black-box half). These
// substitute seedSleep with a recorder, so they run instantly and can assert
// the exact backoff schedule. Every case still goes through the exported seed
// and a REAL stellarrpc.Client: the HTTP-status classification reads the
// client's error text, and only the real client can prove that still matches.

// recordSeedSleeps replaces seedSleep with a recorder for the test's lifetime.
// onWait, when non-nil, runs before each recorded wait returns (used to cancel
// a context mid-backoff). The recorder honours ctx like the real one.
func recordSeedSleeps(t *testing.T, onWait func(d time.Duration)) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := seedSleep
	seedSleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		if onWait != nil {
			onWait(d)
		}
		return ctx.Err()
	}
	t.Cleanup(func() { seedSleep = prev })
	return &waits
}

// backoffsOf drops the inter-call throttle waits, leaving the retry backoffs.
func backoffsOf(waits []time.Duration) []time.Duration {
	var out []time.Duration
	for _, w := range waits {
		if w != seedThrottle {
			out = append(out, w)
		}
	}
	return out
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An endpoint that never recovers must still fail — closed, with the cause —
// after exactly the budget, on the documented schedule.
func TestSeedFromFactoryRPC_SpentBudgetFailsClosedWithTheCause(t *testing.T) {
	factory := makeContractStrkey(t, 0x01)
	cases := []struct {
		name   string
		stuck  seedFault
		wantIn string
		check  func(t *testing.T, err error)
	}{
		{"HTTP 503", seedFault{status: http.StatusServiceUnavailable}, "HTTP 503", nil},
		{"HTTP 429", seedFault{status: http.StatusTooManyRequests}, "HTTP 429", nil},
		{"connection dropped", seedFault{drop: true}, "EOF", func(t *testing.T, err error) {
			t.Helper()
			var ne net.Error
			if !errors.As(err, &ne) {
				t.Errorf("error %q does not WRAP the transport error (errors.As net.Error failed)", err)
			}
		}},
		{"JSON-RPC internal error", seedFault{rpcCode: -32603, rpcMsg: "captive core busy"}, "captive core busy", func(t *testing.T, err error) {
			t.Helper()
			var re *stellarrpc.JSONRPCError
			if !errors.As(err, &re) || re.Code != -32603 {
				t.Errorf("error %q does not WRAP the JSON-RPC error", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordSeedSleeps(t, nil)
			rpc := &flakySorobanRPC{stuck: map[int]seedFault{0: tc.stuck}}
			srv := newFlakySorobanRPC(t, rpc)

			n, err := NewDecoder().SeedFromFactoryRPC(context.Background(), stellarrpc.New(srv.URL), factory)
			if err == nil {
				t.Fatal("seed returned nil against an endpoint that never recovers; the fail-closed outcome must survive the retry")
			}
			if n != 0 {
				t.Errorf("seeded %d pair(s); want 0", n)
			}
			if h := rpc.hitsFor(0); h != seedMaxAttempts {
				t.Errorf("call was attempted %d time(s); want exactly the budget, %d", h, seedMaxAttempts)
			}
			for _, want := range []string{"all_pairs_length", "gave up after 5 attempts", tc.wantIn} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q is missing %q", err, want)
				}
			}
			if tc.check != nil {
				tc.check(t, err)
			}
			want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
			if got := backoffsOf(*waits); !equalDurations(got, want) {
				t.Errorf("backoff schedule %v; want %v", got, want)
			}
		})
	}
}

// The budget belongs to each CALL. Two calls that each need the full budget
// both recover; a sweep-wide budget would have failed the second.
func TestSeedFromFactoryRPC_BudgetIsPerCallNotPerSweep(t *testing.T) {
	waits := recordSeedSleeps(t, nil)
	factory := makeContractStrkey(t, 0x01)
	pair, tok0, tok1 := makeContractStrkey(t, 0x02), makeContractStrkey(t, 0x03), makeContractStrkey(t, 0x04)
	fail := func(k int) []seedFault {
		out := make([]seedFault, k)
		for i := range out {
			out[i] = seedFault{status: http.StatusBadGateway}
		}
		return out
	}
	rpc := &flakySorobanRPC{
		answers: onePairAnswers(t, pair, tok0, tok1),
		faults:  map[int][]seedFault{0: fail(seedMaxAttempts - 1), 3: fail(seedMaxAttempts - 1)},
	}
	srv := newFlakySorobanRPC(t, rpc)

	dec := NewDecoder()
	n, err := dec.SeedFromFactoryRPC(context.Background(), stellarrpc.New(srv.URL), factory)
	if err != nil || n != 1 {
		t.Fatalf("seed = (%d, %v); want (1, nil) — each call failed budget-1 times and then succeeded", n, err)
	}
	if _, ok := dec.pairTokensFor(pair); !ok {
		t.Error("pair not seeded")
	}
	for call, want := range map[int]int{0: seedMaxAttempts, 1: 1, 2: 1, 3: seedMaxAttempts} {
		if h := rpc.hitsFor(call); h != want {
			t.Errorf("logical call %d issued %d time(s); want %d", call, h, want)
		}
	}
	if got := len(backoffsOf(*waits)); got != 2*(seedMaxAttempts-1) {
		t.Errorf("%d backoff wait(s); want %d", got, 2*(seedMaxAttempts-1))
	}
	// The three inter-call throttles are untouched by the retry.
	if got := len(*waits) - len(backoffsOf(*waits)); got != 3 {
		t.Errorf("%d throttle wait(s) for a one-pair sweep; want 3", got)
	}
}

// Deterministic failures are returned at once: re-asking cannot change the
// answer, and a contract error must never be papered over by a retry.
func TestSeedFromFactoryRPC_DeterministicFailuresAreNeverRetried(t *testing.T) {
	factory := makeContractStrkey(t, 0x01)
	cases := []struct {
		name   string
		fault  seedFault
		wantIn string
	}{
		{"contract rejects the call", seedFault{simErr: "host trap: contract panicked"}, "simulate rejected"},
		{"HTTP 400", seedFault{status: http.StatusBadRequest}, "HTTP 400"},
		{"HTTP 401", seedFault{status: http.StatusUnauthorized}, "HTTP 401"},
		{"HTTP 404", seedFault{status: http.StatusNotFound}, "HTTP 404"},
		// A retryable status QUOTED in the body of a non-retryable response
		// must not be read as the response's status.
		{"HTTP 400 quoting a 503", seedFault{status: http.StatusBadRequest, body: "upstream said HTTP 503"}, "HTTP 400"},
		{"JSON-RPC invalid params", seedFault{rpcCode: -32602, rpcMsg: "invalid params"}, "invalid params"},
		{"JSON-RPC method not found", seedFault{rpcCode: -32601, rpcMsg: "method not found"}, "method not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordSeedSleeps(t, nil)
			rpc := &flakySorobanRPC{stuck: map[int]seedFault{0: tc.fault}}
			srv := newFlakySorobanRPC(t, rpc)

			_, err := NewDecoder().SeedFromFactoryRPC(context.Background(), stellarrpc.New(srv.URL), factory)
			if err == nil {
				t.Fatal("seed returned nil; want the deterministic error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q is missing %q", err, tc.wantIn)
			}
			if h := rpc.hitsFor(0); h != 1 {
				t.Errorf("deterministic failure was attempted %d time(s); want 1", h)
			}
			if b := backoffsOf(*waits); len(b) != 0 {
				t.Errorf("backed off %v before a deterministic failure; want no waits", b)
			}
		})
	}
}

// A context that ends during a backoff ends the retry there: no further
// attempt, and the error carries both the ctx error and the RPC cause.
func TestSeedFromFactoryRPC_ContextEndsTheRetryMidBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recordSeedSleeps(t, func(time.Duration) { cancel() })
	rpc := &flakySorobanRPC{stuck: map[int]seedFault{0: {status: http.StatusServiceUnavailable}}}
	srv := newFlakySorobanRPC(t, rpc)

	_, err := NewDecoder().SeedFromFactoryRPC(ctx, stellarrpc.New(srv.URL), makeContractStrkey(t, 0x01))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("error %q dropped the RPC failure that was being retried", err)
	}
	if h := rpc.hitsFor(0); h != 1 {
		t.Errorf("%d attempt(s) after the context was cancelled in the first backoff; want 1", h)
	}
}

// A context that is already done when a throttle wait starts stops the sweep
// before the next call is issued.
func TestSeedFromFactoryRPC_ContextEndsTheSweepInTheThrottle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recordSeedSleeps(t, func(time.Duration) { cancel() })
	pair, tok0, tok1 := makeContractStrkey(t, 0x02), makeContractStrkey(t, 0x03), makeContractStrkey(t, 0x04)
	rpc := &flakySorobanRPC{answers: onePairAnswers(t, pair, tok0, tok1)}
	srv := newFlakySorobanRPC(t, rpc)

	n, err := NewDecoder().SeedFromFactoryRPC(ctx, stellarrpc.New(srv.URL), makeContractStrkey(t, 0x01))
	if !errors.Is(err, context.Canceled) || n != 0 {
		t.Fatalf("seed = (%d, %v); want (0, context.Canceled)", n, err)
	}
	if total := rpc.totalHits(); total != 1 {
		t.Errorf("RPC saw %d request(s); want 1 (all_pairs_length only)", total)
	}
}

// The real wait primitive: returns early with the ctx error, nil otherwise.
func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleepCtx on a cancelled ctx = %v; want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sleepCtx ignored the cancelled ctx for %v", elapsed)
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Errorf("sleepCtx on a live ctx = %v; want nil", err)
	}
}

// Pins the numbers the doc comment and the CHANGELOG state as the worst case,
// so a change to the budget has to change the stated bound with it.
func TestSeedRetryBudget_StatedWorstCase(t *testing.T) {
	if seedMaxAttempts != 5 {
		t.Errorf("seedMaxAttempts = %d; the documented budget is 5 attempts per call", seedMaxAttempts)
	}
	var total time.Duration
	for k := 1; k < seedMaxAttempts; k++ {
		total += seedBackoffBase << (k - 1)
	}
	if total != 15*time.Second {
		t.Errorf("worst-case backoff per call = %v; the documented bound is 15s", total)
	}
}
