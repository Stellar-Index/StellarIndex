package orchestrator

import "testing"

// TestPhase2FreezeFires_AllThree — the 3-signal AND fires when
// every input crosses its threshold.
func TestPhase2FreezeFires_AllThree(t *testing.T) {
	got := phase2FreezeFires(confidenceWithSourceCount{
		Confidence:  0.05, // < 0.10
		ZScore:      8.0,  // > 5.0
		SourceCount: 1,    // <= 1
	})
	if !got {
		t.Error("phase2FreezeFires returned false on a clean 3-signal hit")
	}
}

// TestPhase2FreezeFires_MissingOneSignal — any single signal
// failing the threshold suppresses the freeze. Walks each of the
// three signals being just-clean while the other two are anomalous.
func TestPhase2FreezeFires_MissingOneSignal(t *testing.T) {
	// Multi-source: even with very low confidence + huge z, having
	// 2 sources keeps the freeze off.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 2,
	}) {
		t.Error("multi-source bucket should NOT freeze")
	}
	// Sub-threshold z: deviation isn't large enough for a freeze.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 4.5, SourceCount: 1,
	}) {
		t.Error("z=4.5 bucket should NOT freeze (below 5.0 threshold)")
	}
	// Healthy confidence: every other signal looks bad but the
	// confidence-score combiner says "trustworthy"; don't freeze.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.50, ZScore: 8.0, SourceCount: 1,
	}) {
		t.Error("confidence=0.50 bucket should NOT freeze")
	}
}

// TestPhase2FreezeFires_BoundaryStrictness — the conditions are
// strictly > / < / <=. Boundary values don't fire.
func TestPhase2FreezeFires_BoundaryStrictness(t *testing.T) {
	// confidence == 0.10 — strictly less-than required.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.10, ZScore: 8.0, SourceCount: 1,
	}) {
		t.Error("confidence==0.10 boundary should NOT freeze (strictly <)")
	}
	// z == 5.0 — strictly greater-than required.
	if phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 5.0, SourceCount: 1,
	}) {
		t.Error("z==5.0 boundary should NOT freeze (strictly >)")
	}
	// source_count == 1 — the boundary IS included (≤).
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 1,
	}) {
		t.Error("source_count==1 boundary SHOULD freeze (<=)")
	}
}

// TestPhase2FreezeFires_ZeroSources — no contributing sources is
// the most-pathological case and freezes if the other two signals
// agree.
func TestPhase2FreezeFires_ZeroSources(t *testing.T) {
	if !phase2FreezeFires(confidenceWithSourceCount{
		Confidence: 0.05, ZScore: 8.0, SourceCount: 0,
	}) {
		t.Error("zero-source bucket with other signals firing SHOULD freeze")
	}
}
