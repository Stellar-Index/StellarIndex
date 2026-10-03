package v1

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// ThinMarketConfidenceCeiling caps /v1/price's confidence on a
// thin-admitted serve. It is a declared ceiling, not a measurement:
// ADR-0019's original freeze threshold.
const ThinMarketConfidenceCeiling = 0.10

type thinAdmissionKey struct{}

// ThinAdmission is the request-scoped record of the `include_thin`
// opt-in. A handler installs it with [WithThinAdmission] and reads it
// after the price read; only the gate chokepoints and the coalescing
// price reader fetch it from the context, to release a covered thin leg
// and record the substance evidence they measured.
type ThinAdmission struct {
	asset     canonical.Asset
	quote     canonical.Asset
	requested bool

	mu       sync.Mutex
	admitted bool
	admitEv  *pricingguard.SubstanceEvidence
	heldEv   *pricingguard.SubstanceEvidence
}

// WithThinAdmission installs a fresh record for the request pair. With
// requested=false the record only collects evidence for a 404 body.
func WithThinAdmission(ctx context.Context, asset, quote canonical.Asset, requested bool) (context.Context, *ThinAdmission) {
	a := &ThinAdmission{asset: asset, quote: quote, requested: requested}
	return context.WithValue(ctx, thinAdmissionKey{}, a), a
}

// ThinAdmissionFrom returns the record installed on ctx, or nil.
func ThinAdmissionFrom(ctx context.Context) *ThinAdmission {
	a, _ := ctx.Value(thinAdmissionKey{}).(*ThinAdmission)
	return a
}

// Requested reports whether this record releases thin markets.
func (a *ThinAdmission) Requested() bool { return a != nil && a.requested }

// Covers reports whether a gate verdict on (base, quote) is about the
// request's own market: the request BASE is either leg. The quote alone
// never covers, so a cross through another asset's market stays gated.
func (a *ThinAdmission) Covers(base, quote canonical.Asset) bool {
	return a != nil && (sameAsset(a.asset, base) || sameAsset(a.asset, quote))
}

// ThinWithheld is the second-pass rule: the read ended withheld for a
// reason a thin covered leg can produce, and such a leg was measured.
// A flagged issuer is never measured, so it never qualifies.
func (a *ThinAdmission) ThinWithheld(reason PriceWithheldReason) bool {
	switch reason {
	case PriceWithheldSubstance, PriceWithheldUpstreamLeg, PriceWithheldUnattributed:
		return a.Evidence() != nil
	default:
		return false
	}
}

// Record stores a covered chokepoint verdict's evidence.
func (a *ThinAdmission) Record(base, quote canonical.Asset, v pricingguard.Verdict) {
	if a == nil || v.Substance == nil || !a.Covers(base, quote) {
		return
	}
	switch {
	case v.ThinAdmitted:
		a.Merge(true, v.Substance)
	case v.Withholding == pricingguard.WithheldThinMarket:
		a.Merge(false, v.Substance)
	}
}

// Merge folds another record's answer in; callers gate it on Covers.
// The admitted slot keeps the last admitted write (the route served);
// the withheld slot keeps the first non-empty measurement, because a
// peg or orientation with no data measures thin at zero buckets and
// must not displace real evidence.
func (a *ThinAdmission) Merge(admitted bool, ev *pricingguard.SubstanceEvidence) {
	if a == nil || ev == nil {
		return
	}
	oriented := a.orient(*ev)
	a.mu.Lock()
	defer a.mu.Unlock()
	if admitted {
		a.admitted = true
		a.admitEv = &oriented
		return
	}
	if a.heldEv == nil || (a.heldEv.Buckets == 0 && oriented.Buckets > 0) {
		a.heldEv = &oriented
	}
}

// orient names the request base as the evidence's base. The verdict
// cache is direction-insensitive, so a cached entry may hold either.
func (a *ThinAdmission) orient(ev pricingguard.SubstanceEvidence) pricingguard.SubstanceEvidence {
	if !sameAsset(a.asset, ev.Base) && sameAsset(a.asset, ev.Quote) {
		ev.Base, ev.Quote = ev.Quote, ev.Base
	}
	return ev
}

// Admitted reports whether any covered thin leg was released.
func (a *ThinAdmission) Admitted() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admitted
}

// Evidence is the admitted route's evidence, else the withheld one.
func (a *ThinAdmission) Evidence() *pricingguard.SubstanceEvidence {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitEv != nil {
		return a.admitEv
	}
	return a.heldEv
}

// includeThinRequested is the opt-in: only the literal "true" counts.
func includeThinRequested(r *http.Request) bool {
	return r.URL.Query().Get("include_thin") == "true"
}

// SubstanceEvidence is the wire form of a substance measurement: the
// market measured, its figures, and the floor it was held to.
type SubstanceEvidence struct {
	Base          string                      `json:"base"`
	Quote         string                      `json:"quote"`
	WindowSeconds int64                       `json:"window_seconds"`
	MeasuredAt    WireTime                    `json:"measured_at"`
	WindowEnd     *WireTime                   `json:"window_end,omitempty"`
	VolumeUSD     string                      `json:"volume_usd"`
	Buckets       int64                       `json:"buckets"`
	ValuedBuckets int64                       `json:"valued_buckets"`
	SpanSeconds   int64                       `json:"span_seconds"`
	Floor         SubstanceFloorWire          `json:"floor"`
	Failed        pricingguard.SubstanceFloor `json:"failed"`
}

// SubstanceFloorWire is the substance policy a measurement was held to.
type SubstanceFloorWire struct {
	MinVolumeUSD   string `json:"min_volume_usd"`
	MinBuckets     int64  `json:"min_buckets"`
	MinSpanSeconds int64  `json:"min_span_seconds"`
}

// substanceEvidenceWire is the one serializer of a measurement.
func substanceEvidenceWire(ev *pricingguard.SubstanceEvidence) *SubstanceEvidence {
	if ev == nil {
		return nil
	}
	minVol := "0.00"
	if ev.Policy.MinVolumeUSD != nil {
		minVol = ev.Policy.MinVolumeUSD.FloatString(2)
	}
	out := &SubstanceEvidence{
		Base:          ev.Base.String(),
		Quote:         ev.Quote.String(),
		WindowSeconds: int64(ev.Window / time.Second),
		MeasuredAt:    WireTime(ev.MeasuredAt),
		VolumeUSD:     ev.VolumeUSD,
		Buckets:       ev.Buckets,
		ValuedBuckets: ev.ValuedBuckets,
		SpanSeconds:   ev.SpanSeconds,
		Floor: SubstanceFloorWire{
			MinVolumeUSD:   minVol,
			MinBuckets:     ev.Policy.MinBuckets,
			MinSpanSeconds: int64(ev.Policy.MinSpan / time.Second),
		},
		Failed: ev.Floor,
	}
	out.WindowEnd = wireTimeOrNil(ev.WindowEnd)
	return out
}

// thinEvidenceFor is the 404 body's evidence: only for a reason a thin
// leg can produce, since the record is request-wide.
func thinEvidenceFor(adm *ThinAdmission, reason PriceWithheldReason) *SubstanceEvidence {
	switch reason {
	case PriceWithheldSubstance, PriceWithheldUpstreamLeg, PriceWithheldUnattributed:
		return substanceEvidenceWire(adm.Evidence())
	default:
		return nil
	}
}

// capThinConfidence applies [ThinMarketConfidenceCeiling].
func capThinConfidence(snap *PriceSnapshot) {
	if snap.Confidence == nil || *snap.Confidence > ThinMarketConfidenceCeiling {
		c := ThinMarketConfidenceCeiling
		snap.Confidence = &c
	}
}
