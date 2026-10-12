package v1

import (
	"sync"
	"testing"
	"time"
)

// TestTipStreamDivergenceBudgetHoldsItsTwoReferences pins the two relationships the budget constant is argued from:
// - Strictly under the tick budget: an auxiliary read must never consume the whole tick.
// - No wider than a fifth of the DEFAULT window: a stalled store may shift an emission, never replace it.
func TestTipStreamDivergenceBudgetHoldsItsTwoReferences(t *testing.T) {
	if tipStreamDivergenceBudget >= tipStreamTickTimeout {
		t.Errorf("tipStreamDivergenceBudget = %v, tipStreamTickTimeout = %v — the auxiliary lookup must be bounded strictly tighter than the computation it rides beside",
			tipStreamDivergenceBudget, tipStreamTickTimeout)
	}
	fifth := time.Duration(defaultTipWindowSeconds) * time.Second / 5
	if tipStreamDivergenceBudget > fifth {
		t.Errorf("tipStreamDivergenceBudget = %v, want <= %v (a fifth of the %ds default window) — beyond that a stalled lookup replaces the cadence rather than nudging it",
			tipStreamDivergenceBudget, fifth, defaultTipWindowSeconds)
	}
}

// TestTipDivergenceStallLogRateLimits: under a sustained stall the warning fires once per interval process-wide, each
// line carrying the count it swallowed. Unbounded, it would be a line per tick per stream. An injected clock pins policy.
func TestTipDivergenceStallLogRateLimits(t *testing.T) {
	var l tipDivergenceStallLog
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if got, ok := l.admit(base); !ok || got != 0 {
		t.Fatalf("first stall = (%d, %t), want (0, true) — the first line of a stall must not be withheld", got, ok)
	}
	for i, at := range []time.Time{
		base.Add(time.Second),
		base.Add(30 * time.Second),
		base.Add(tipStreamDivergenceStallInterval - time.Nanosecond),
	} {
		if got, ok := l.admit(at); ok {
			t.Errorf("stall %d inside the interval = (%d, true), want suppressed", i, got)
		}
	}
	got, ok := l.admit(base.Add(tipStreamDivergenceStallInterval))
	if !ok {
		t.Fatal("stall at the interval boundary was suppressed — a sustained stall must keep reporting")
	}
	if got != 3 {
		t.Errorf("suppressed_since_last = %d, want 3 — the swallowed count must be reported, not lost", got)
	}
	if got, ok := l.admit(base.Add(2 * tipStreamDivergenceStallInterval)); !ok || got != 0 {
		t.Errorf("next interval = (%d, %t), want (0, true) — the counter resets with each emitted line", got, ok)
	}
}

// TestTipDivergenceStallLogCountIntegrity: lines emitted + counts reported + residue held must equal calls made, so
// the limiter loses no stalls under contention. Runs 512 concurrent producers within one interval so exactness holds.
func TestTipDivergenceStallLogCountIntegrity(t *testing.T) {
	const (
		producers   = 512
		perProducer = 100
	)
	var l tipDivergenceStallLog
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	var (
		mu       sync.Mutex
		emitted  int
		reported uint64
		wg       sync.WaitGroup
	)
	start := make(chan struct{})
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < perProducer; i++ {
				if sup, ok := l.admit(base.Add(time.Duration(i) * time.Millisecond)); ok {
					mu.Lock()
					emitted++
					reported += sup
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	l.mu.Lock()
	residue := l.suppressed
	l.mu.Unlock()

	total := uint64(producers * perProducer)
	accounted := uint64(emitted) + reported + residue
	if accounted != total {
		t.Errorf("count loss: %d stalls in, %d accounted for (emitted %d + reported %d + residue %d)",
			total, accounted, emitted, reported, residue)
	}
	if emitted != 1 {
		t.Errorf("lines emitted = %d for stalls all inside one interval, want 1 — the rate limit leaked", emitted)
	}
}

// TestTipDivergenceStallLogAgesOutAStaleResidue — after a long quiet period the next stall is
// a NEW incident: it must be reported promptly and not stamped with a count swallowed
// earlier, which an operator would read as this incident's volume.
func TestTipDivergenceStallLogAgesOutAStaleResidue(t *testing.T) {
	var l tipDivergenceStallLog
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if _, ok := l.admit(base); !ok {
		t.Fatal("the first stall of an incident must be reported")
	}
	for i := 1; i <= 5; i++ {
		if _, ok := l.admit(base.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("stall %d inside the interval was emitted, want suppressed", i)
		}
	}
	// The incident ends here; nothing for an hour, then a fresh one.
	sup, ok := l.admit(base.Add(time.Hour))
	if !ok {
		t.Fatal("the first stall after a quiet hour was suppressed — a new incident must not be starved")
	}
	if sup != 0 {
		t.Errorf("suppressed_since_last = %d on a new incident's first line, want 0 — "+
			"the previous incident's residue must be aged out, not attributed to this one", sup)
	}
}
