package ledgerstream

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
)

// ─── a MinIO blip must not tear the live tail down ──────────
//
// The SDK's fetch worker consumes a retry ATTEMPT for every non-
// NotExist datastore error and gives up after RetryLimit of them, but
// it consumes NOTHING for "the tip isn't written yet" (os.ErrNotExist
// on an unbounded range). Both sleep the same RetryWait. So dropping
// RetryWait to 500ms for tip latency silently cut fault tolerance to
// 5 × 500ms = 2.5s, and a MinIO restart exited the indexer over and
// over until systemd's StartLimit parked the unit in `failed`.
//
// The fix expresses tolerance as a TIME budget and derives the attempt
// count from the wait actually in force. These tests pin both halves:
// the arithmetic, and the end-to-end behaviour against a datastore that
// faults N times and then succeeds.

func TestLiveRetryLimit_DerivesAttemptsFromBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wait   time.Duration
		budget time.Duration
		want   uint32
	}{
		{
			// The production pairing: 500ms re-check for tip latency,
			// 5 minutes of fault tolerance.
			name: "production_live_tail", wait: 500 * time.Millisecond,
			budget: 5 * time.Minute, want: 600,
		},
		{
			// Rounds UP: the budget is a floor, never a ceiling.
			name: "rounds_up", wait: 700 * time.Millisecond,
			budget: 2 * time.Second, want: 3,
		},
		{
			// A budget shorter than one wait still buys one attempt —
			// never zero, which would mean "give up immediately".
			name: "sub_wait_budget", wait: time.Second,
			budget: 10 * time.Millisecond, want: 1,
		},
		// Zero on either input means "leave the SDK default alone".
		{name: "no_budget", wait: time.Second, budget: 0, want: 0},
		{name: "no_wait", wait: 0, budget: time.Minute, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := liveRetryLimit(tc.wait, tc.budget); got != tc.want {
				t.Errorf("liveRetryLimit(%v, %v) = %d, want %d", tc.wait, tc.budget, got, tc.want)
			}
		})
	}
}

// TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait pins the
// ordering trap: the attempt count must be derived from the wait the
// worker will ACTUALLY sleep, not from the SDK's 30s default. Getting
// this backwards yields a tolerance 60× shorter than configured — the
// same coupling defect in a new costume.
func TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait(t *testing.T) {
	buffered := ingest.DefaultBufferedStorageBackendConfig(1)
	if buffered.RetryWait != 30*time.Second || buffered.RetryLimit != 5 {
		t.Fatalf("SDK defaults moved (RetryWait=%v RetryLimit=%d) — re-derive this test",
			buffered.RetryWait, buffered.RetryLimit)
	}

	applyLiveRetryPolicy(Config{
		LiveRetryWait:   500 * time.Millisecond,
		LiveRetryBudget: 5 * time.Minute,
	}, &buffered)

	if buffered.RetryWait != 500*time.Millisecond {
		t.Errorf("RetryWait = %v, want 500ms (tip-latency override)", buffered.RetryWait)
	}
	if buffered.RetryLimit != 600 {
		t.Errorf("RetryLimit = %d, want 600 (5min / 500ms)", buffered.RetryLimit)
	}
	tolerated := time.Duration(buffered.RetryLimit) * buffered.RetryWait
	if tolerated < 5*time.Minute {
		t.Errorf("fault tolerance = %v, want >= 5m — a MinIO restart must not exit the indexer", tolerated)
	}
}

// ─── fixture helpers (internal-package copies; the external test
// package has its own, and Go will not share across packages) ────────
