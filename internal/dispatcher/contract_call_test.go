package dispatcher

import (
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// fakeContractCallDecoder is the parallel of fakeOpDecoder /
// fakeDecoder for ContractCallDecoder. Lets us drive
// dispatchContractCall + RouteContractCall directly.
type fakeContractCallDecoder struct {
	name        string
	matchCID    string
	matchFn     string
	outputs     []consumer.Event
	decodeErr   error
	matchCount  int
	decodeCount int
}

func (d *fakeContractCallDecoder) Name() string { return d.name }

func (d *fakeContractCallDecoder) Matches(contractID, fn string) bool {
	d.matchCount++
	return contractID == d.matchCID && fn == d.matchFn
}

func (d *fakeContractCallDecoder) Decode(_ ContractCallContext) ([]consumer.Event, error) {
	d.decodeCount++
	if d.decodeErr != nil {
		return nil, d.decodeErr
	}
	return d.outputs, nil
}

func TestRouteContractCall_routesToFirstMatch(t *testing.T) {
	band := &fakeContractCallDecoder{
		name:     "band",
		matchCID: "CBAND",
		matchFn:  "relay",
		outputs:  []consumer.Event{fakeEvent{source: "band", kind: "update"}},
	}
	otherCC := &fakeContractCallDecoder{
		name:     "other",
		matchCID: "COTHER",
		matchFn:  "noop",
	}
	d := New()
	d.AddContractCallDecoder(band)
	d.AddContractCallDecoder(otherCC)

	out, err := d.RouteContractCall(ContractCallContext{
		Ledger:       1,
		ClosedAt:     time.Unix(1_770_000_000, 0).UTC(),
		ContractID:   "CBAND",
		FunctionName: "relay",
	})
	if err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	if band.decodeCount != 1 {
		t.Errorf("band Decode count = %d, want 1", band.decodeCount)
	}
	// Other decoder must not have decoded — first-match short-
	// circuit.
	if otherCC.decodeCount != 0 {
		t.Errorf("otherCC Decode count = %d, want 0 (first-match short-circuit)",
			otherCC.decodeCount)
	}
}

func TestRouteContractCall_unmatched_returnsNilNil(t *testing.T) {
	band := &fakeContractCallDecoder{
		name:     "band",
		matchCID: "CBAND",
		matchFn:  "relay",
	}
	d := New()
	d.AddContractCallDecoder(band)

	out, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CDIFFERENT",
		FunctionName: "relay",
	})
	if err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events, want 0 (no decoder matched)", len(out))
	}
	// Stats must NOT increment unmatched-hits — that counter is
	// for events, not contract calls. ContractCallDecoder calls
	// fall through silently when nothing claims them; the
	// dispatcher's metric distinction is intentional.
	if d.Stats().UnmatchedHits != 0 {
		t.Errorf("UnmatchedHits = %d, want 0 (CC misses don't bump event-side counter)",
			d.Stats().UnmatchedHits)
	}
}

func TestRouteContractCall_decodeErrorIsCounted(t *testing.T) {
	boom := errors.New("decoder failed")
	band := &fakeContractCallDecoder{
		name:      "band",
		matchCID:  "CBAND",
		matchFn:   "relay",
		decodeErr: boom,
	}
	d := New()
	d.AddContractCallDecoder(band)

	_, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CBAND",
		FunctionName: "relay",
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want %v wrapped", err, boom)
	}
	if got := d.Stats().DecodeErrors["band"]; got != 1 {
		t.Errorf("DecodeErrors[band] = %d, want 1", got)
	}
}

func TestRouteContractCall_emptyDecoderListNoMatch(t *testing.T) {
	d := New()
	out, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CBAND",
		FunctionName: "relay",
	})
	if err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events, want 0", len(out))
	}
}

// ─── Discovery hook (event-less oracle, §3(b)(2)) ─────────────────

// TestDispatchContractCall_DiscoveryHook_FiresOnOracleCallCandidate
// — the event-less-oracle sniffer (docs/architecture/generic-oracle-sep-onboarding.md
// §3(b)(2)) must push a Hit for a call matching the oracle-suggestive
// function-name allow-list, with ZERO ContractCallDecoders
// registered — this is the whole point: a future Band-alike must be
// sighted even before anyone writes a decoder for it.
func TestDispatchContractCall_DiscoveryHook_FiresOnOracleCallCandidate(t *testing.T) {
	sink := &recordingSink{}
	d := New()
	d.SetDiscoverySink(sink)

	out, err := d.RouteContractCall(ContractCallContext{
		Ledger:       60_000_000,
		ClosedAt:     time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
		ContractID:   "CNEWORACLE",
		FunctionName: "relay",
	})
	if err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events, want 0 (no decoder registered, discovery is sighting-only)", len(out))
	}
	if len(sink.hits) != 1 {
		t.Fatalf("sink received %d hits, want 1", len(sink.hits))
	}
	hit := sink.hits[0]
	if hit.Kind != discovery.KindOracleCall {
		t.Errorf("Kind = %q, want %q", hit.Kind, discovery.KindOracleCall)
	}
	if hit.Symbol != "relay" {
		t.Errorf("Symbol = %q, want %q", hit.Symbol, "relay")
	}
	if hit.ContractID != "CNEWORACLE" {
		t.Errorf("ContractID = %q, want %q", hit.ContractID, "CNEWORACLE")
	}
	if hit.ObservedAtRFC3339 != "2026-07-10T12:00:00Z" {
		t.Errorf("ObservedAtRFC3339 = %q, want %q", hit.ObservedAtRFC3339, "2026-07-10T12:00:00Z")
	}
}

// TestDispatchContractCall_DiscoveryHook_SilentOnNonOracleFunction —
// a call whose function name isn't in the oracle-suggestive
// allow-list must not push, even with a sink installed.
func TestDispatchContractCall_DiscoveryHook_SilentOnNonOracleFunction(t *testing.T) {
	sink := &recordingSink{}
	d := New()
	d.SetDiscoverySink(sink)

	if _, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CSWAPROUTER",
		FunctionName: "swap_exact_tokens_for_tokens",
	}); err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(sink.hits) != 0 {
		t.Errorf("sink received %d hits on non-oracle function, want 0", len(sink.hits))
	}
}

// TestDispatchContractCall_DiscoveryHook_FiresAlongsideRealDecoder —
// the discovery hook and a matching ContractCallDecoder are not
// mutually exclusive: Band's own relay() calls still get sighted
// (same pattern as SEP-41 discovery already sighting well-known
// tokens' events).
func TestDispatchContractCall_DiscoveryHook_FiresAlongsideRealDecoder(t *testing.T) {
	sink := &recordingSink{}
	band := &fakeContractCallDecoder{
		name:     "band",
		matchCID: "CBAND",
		matchFn:  "relay",
		outputs:  []consumer.Event{fakeEvent{source: "band", kind: "update"}},
	}
	d := New()
	d.AddContractCallDecoder(band)
	d.SetDiscoverySink(sink)

	out, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CBAND",
		FunctionName: "relay",
	})
	if err != nil {
		t.Fatalf("RouteContractCall: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d events, want 1 (decoder still runs)", len(out))
	}
	if len(sink.hits) != 1 {
		t.Errorf("sink received %d hits, want 1 (discovery still sights a known/decoded contract)", len(sink.hits))
	}
}

// TestDispatchContractCall_DiscoveryHook_NilSinkIsNoop — without a
// sink installed, dispatchContractCall behaves identically.
func TestDispatchContractCall_DiscoveryHook_NilSinkIsNoop(t *testing.T) {
	d := New()
	if _, err := d.RouteContractCall(ContractCallContext{
		ContractID:   "CNEWORACLE",
		FunctionName: "relay",
	}); err != nil {
		t.Errorf("nil-sink path errored: %v", err)
	}
}

// ─── contractCallPathActive gate ───────────────────────────────────

// TestContractCallPathActive covers the ProcessLedger gate that
// decides whether to walk each op's InvokeContract auth tree at all.
// Before this change the gate was `len(contractCallDecoders) > 0`
// only, which meant the event-less-oracle discovery hook (living
// inside dispatchContractCall) silently depended on Band — or some
// other ContractCallDecoder — happening to be registered in the
// running binary. A discovery sink alone must now also activate the
// walk.
func TestContractCallPathActive(t *testing.T) {
	tests := []struct {
		name       string
		addDecoder bool
		setSink    bool
		want       bool
	}{
		{"neither", false, false, false},
		{"decoder only", true, false, true},
		{"sink only", false, true, true},
		{"both", true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New()
			if tc.addDecoder {
				d.AddContractCallDecoder(&fakeContractCallDecoder{name: "x"})
			}
			if tc.setSink {
				d.SetDiscoverySink(&recordingSink{})
			}
			if got := d.contractCallPathActive(); got != tc.want {
				t.Errorf("contractCallPathActive() = %v, want %v", got, tc.want)
			}
		})
	}
}
