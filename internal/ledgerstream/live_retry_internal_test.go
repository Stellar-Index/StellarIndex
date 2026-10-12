package ledgerstream

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
)

// A MinIO blip must not tear the live tail down. The SDK fetch worker spends a retry attempt on every non-NotExist
// error but none on "tip not written yet", and both sleep RetryWait, so shortening it for tip latency shrinks fault
// tolerance (5 x 500ms = 2.5s). Tolerance is a time budget with attempts derived from the wait; these tests pin that.

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

// TestApplyLiveRetryPolicy_DerivesLimitFromTheOverriddenWait pins the ordering trap: the attempt
// count must derive from the wait the worker will actually sleep, not the SDK's 30s default, or
// tolerance is 60x shorter than configured.
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
