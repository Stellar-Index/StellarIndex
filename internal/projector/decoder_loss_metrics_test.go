package projector

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeLossyDecoder implements dispatcher.Decoder plus the two duck-typed
// loss-counter methods (EvictedOrphans / UnknownContractDrops) soroswap's
// real decoder exposes — mimicking the shape emitDecoderLossDeltas reads,
// without pulling in the real soroswap package.
type fakeLossyDecoder struct {
	orphans, drops int
}

func (*fakeLossyDecoder) Name() string              { return "fake-lossy" }
func (*fakeLossyDecoder) Matches(events.Event) bool { return false }
func (*fakeLossyDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return nil, nil
}
func (f *fakeLossyDecoder) EvictedOrphans() int       { return f.orphans }
func (f *fakeLossyDecoder) UnknownContractDrops() int { return f.drops }

// TestEmitDecoderLossDeltas pins Q037: the projector builds its OWN decoder
// instance per source (a separate instance from the live indexer's
// dispatcher), and before this fix nothing ever read that instance's
// EvictedOrphans / UnknownContractDrops counters — the projector's half of
// the loss signal had zero observability. The metric must move by the
// decoder's OWN accumulated count, not stay at zero forever.
func TestEmitDecoderLossDeltas(t *testing.T) {
	dec := &fakeLossyDecoder{}
	src := Source{Name: "q037-fake-source", Decoder: dec}
	p := &Projector{}

	before := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "orphan_evicted"))
	beforeDrops := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "unknown_contract_drop"))

	dec.orphans = 3
	dec.drops = 2
	p.emitDecoderLossDeltas(src)

	gotOrphans := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "orphan_evicted")) - before
	if gotOrphans != 3 {
		t.Fatalf("orphan_evicted delta = %v, want 3", gotOrphans)
	}
	gotDrops := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "unknown_contract_drop")) - beforeDrops
	if gotDrops != 2 {
		t.Fatalf("unknown_contract_drop delta = %v, want 2", gotDrops)
	}

	// A second call with unchanged decoder counters must add nothing more
	// (delta, not a re-emit of the cumulative value).
	p.emitDecoderLossDeltas(src)
	if got := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "orphan_evicted")) - before; got != 3 {
		t.Fatalf("orphan_evicted after unchanged cycle = %v, want 3 (no double count)", got)
	}

	// A further real eviction on the SAME decoder instance must show up as
	// an additional delta.
	dec.orphans = 5
	p.emitDecoderLossDeltas(src)
	if got := testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(src.Name, "orphan_evicted")) - before; got != 5 {
		t.Fatalf("orphan_evicted after second eviction = %v, want 5", got)
	}
}
