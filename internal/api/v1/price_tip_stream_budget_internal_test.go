package v1

import (
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestTipStreamDivergenceBudgetHoldsItsTwoReferences pins the two
// relationships the constant's value is argued from, so a later change
// to either reference cannot leave the rationale describing a budget
// that no longer follows from it.
//
//   - Strictly under the tick budget: an auxiliary read that could
//     consume the whole tick is the defect this constant exists to
//     remove.
//   - No wider than a fifth of the DEFAULT window: a stalled store may
//     shift an emission within its period, never replace it.
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

// TestTipDivergenceStallLogRateLimits — under a sustained stall the
// warning fires once per interval for the whole process, and each line
// it does emit carries the count it swallowed, so the volume is visible
// rather than hidden. Without the bound this is one line per tick per
// stream: at the 512-producer ceiling with a one-second window, tens of
// thousands of lines a minute at exactly the moment an operator is
// reading the log.
//
// Driven by an injected clock, so it pins the policy rather than the
// wall clock.
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

// TestTipDivergenceStallLogCountIntegrity — every stall must be
// accounted for: lines emitted, plus counts those lines reported, plus
// the residue still held, must equal the calls made. A rate limiter that
// loses stalls under contention is worse than none, because the number
// it prints is then quietly wrong.
//
// Driven at the stated ceiling of 512 concurrent producers, all inside
// one interval so the age-out cannot fire and conservation is exact.
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

// TestTipDivergenceStallLogAgesOutAStaleResidue — a residue belongs to
// the incident that produced it. After a long quiet period the next
// stall is a NEW incident: it must be reported promptly, and it must not
// be stamped with a count swallowed an hour earlier. Attributing an old
// number to a new incident is worse than reporting none, because an
// operator reads it as this incident's volume.
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

// TestTipStreamDivergenceBudgetCoversTheAliasWalk pins the arithmetic
// the budget's rationale is written from. The lookup is not one record
// read: [Server.lookupDivergenceFlag] walks every canonical spelling
// SEQUENTIALLY under the single sub-budget, so the per-spelling
// tolerance is the budget divided by the family size. If a family grows,
// that tolerance shrinks and the documented figure stops being true —
// this fails rather than letting the comment rot.
func TestTipStreamDivergenceBudgetCoversTheAliasWalk(t *testing.T) {
	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	spellings := len(assetAliases(native))
	if spellings != 3 {
		t.Fatalf("XLM family = %d spellings, but the budget's rationale states 3 (and ~333ms per spelling); "+
			"per-spelling tolerance is now %v — update the comment on tipStreamDivergenceBudget",
			spellings, tipStreamDivergenceBudget/time.Duration(spellings))
	}
	perSpelling := tipStreamDivergenceBudget / time.Duration(spellings)
	if perSpelling < 300*time.Millisecond {
		t.Errorf("per-spelling tolerance = %v, below the ~333ms the rationale claims", perSpelling)
	}
	// A base outside any alias family costs one spelling and gets the
	// whole budget — the rationale says so, so pin it.
	btc, err := canonical.ParseAsset("crypto:BTC")
	if err != nil {
		t.Fatalf("parse crypto:BTC: %v", err)
	}
	if n := len(assetAliases(btc)); n != 1 {
		t.Errorf("crypto:BTC = %d spellings, want 1 — the single-spelling claim in the rationale no longer holds", n)
	}
}
