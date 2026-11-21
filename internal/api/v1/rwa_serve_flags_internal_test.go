package v1

import (
	"testing"
	"time"
)

// A set that was never built (the cold build still running past the request deadline) is an outage, not an empty sector:
// its envelope must be degraded so the answer is served no-store rather than cached by the browser and CDN.
func TestRWAServeFlags_ANeverBuiltSetIsDegraded(t *testing.T) {
	s := &Server{}
	cold := s.stampRWAServeState(rwaMembership{refusals: map[string]int{}})
	if !cold.serveFlags().Degraded {
		t.Error("degraded = false on a never-built membership set")
	}
	if !rwaMembershipUnavailable(cold) {
		t.Fatal("a never-built set should take the unavailable branch this flag guards")
	}
	built := rwaMembership{refusals: map[string]int{}, builtAt: time.Now().UTC()}
	if f := built.serveFlags(); f.Degraded || f.Stale {
		t.Errorf("flags = %+v on a fresh, successfully built set; want clean", f)
	}
}
