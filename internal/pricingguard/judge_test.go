package pricingguard

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// surfaceCount sums every series of vec carrying surface=surface.
func surfaceCount(t *testing.T, vec *prometheus.CounterVec, surface string) float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 64)
	go func() {
		vec.Collect(ch)
		close(ch)
	}()
	var sum float64
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatal(err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "surface" && l.GetValue() == surface {
				sum += pb.GetCounter().GetValue()
			}
		}
	}
	return sum
}

func thinGate(t *testing.T, sub timescale.MarketSubstance) (*SubstanceGate, *fakeSubstanceReader, canonical.Asset, canonical.Asset) {
	t.Helper()
	base, quote := scamPair(t)
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{pair.String(): sub}}
	gate := NewSubstanceGate(r, SubstanceGateOptions{Policy: testPolicy()})
	gate.now = func() time.Time { return gateNow }
	return gate, r, base, quote
}

func TestMeasureEvidenceComesFromTheCache(t *testing.T) {
	gate, r, base, quote := thinGate(t, dustSubstance)
	first := gate.Measure(context.Background(), base, quote)
	if first.Allowed || !first.Measured || first.Floor == FloorNone || first.Evidence == nil {
		t.Fatalf("dust pair: got %+v, want a measured thin verdict with evidence", first)
	}
	ev := *first.Evidence
	if ev.VolumeUSD != "8.57" || ev.Buckets != 1 || ev.Floor != first.Floor ||
		!ev.Base.Equal(base) || !ev.Quote.Equal(quote) || ev.Window != testPolicy().Window ||
		!ev.MeasuredAt.Equal(gateNow) || !ev.WindowEnd.Equal(gateNow) {
		t.Fatalf("evidence = %+v, does not describe the measurement", ev)
	}
	first.Evidence.VolumeUSD = "999999999"
	second := gate.Measure(context.Background(), base, quote)
	if r.calls != 1 {
		t.Errorf("store read %d times, want 1: the second Measure must hit the cache", r.calls)
	}
	if second.Evidence == nil || *second.Evidence != ev {
		t.Errorf("cached evidence = %+v, want %+v (a caller wrote through into the cache)", second.Evidence, ev)
	}
	if allowed, measured, floor := gate.Probe(context.Background(), base, quote); allowed || !measured || floor != first.Floor {
		t.Errorf("Probe = (%v, %v, %q), want the Measure projection", allowed, measured, floor)
	}
}

func TestMeasureUnmeasuredAndUngated(t *testing.T) {
	base, quote := scamPair(t)
	gate := NewSubstanceGate(&fakeSubstanceReader{err: errors.New("db down")}, SubstanceGateOptions{Policy: testPolicy()})
	if v := gate.Measure(context.Background(), base, quote); v.Allowed || v.Measured || v.Evidence != nil {
		t.Errorf("store error: got %+v, want an unmeasured verdict without evidence", v)
	}
	var nilGate *SubstanceGate
	if v := nilGate.Measure(context.Background(), base, quote); !v.Allowed || !v.Measured || v.Evidence != nil {
		t.Errorf("nil gate: got %+v, want allowed and measured without evidence", v)
	}
}

func TestMeasureAtEvidenceWindowEnd(t *testing.T) {
	base, quote := scamPair(t)
	r := &timedSubstanceReader{at: func(time.Time) timescale.MarketSubstance { return dustSubstance }}
	gate := newTimedGate(r)
	at := time.Date(2021, 3, 1, 9, 17, 33, 0, time.UTC)
	v := gate.MeasureAt(context.Background(), base, quote, at)
	if v.Evidence == nil {
		t.Fatalf("MeasureAt = %+v, want evidence", v)
	}
	if want := r.atCalls[0].asOf; !v.Evidence.WindowEnd.Equal(want) {
		t.Errorf("WindowEnd = %v, want the measured instant %v", v.Evidence.WindowEnd, want)
	}
	if got, want := v.Evidence.Policy, hourGrainPolicy(testPolicy()); got.MinBuckets != want.MinBuckets || got.MinSpan != want.MinSpan {
		t.Errorf("Policy = %+v, want the hour-grain policy actually applied %+v", got, want)
	}
	if !v.Evidence.MeasuredAt.Equal(gateNow) {
		t.Errorf("MeasuredAt = %v, want the gate clock %v", v.Evidence.MeasuredAt, gateNow)
	}
}

func TestJudgeAdmitThinReleasesOnlySubstance(t *testing.T) {
	ctx := context.Background()
	flagged := NewScamGate(&fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"unsafe"}}, found: true}, ScamGateOptions{})
	unmeasured := NewSubstanceGate(&fakeSubstanceReader{err: errors.New("db down")}, SubstanceGateOptions{Policy: testPolicy()})
	cases := []struct {
		name         string
		gate         func(*SubstanceGate) Gate
		admit        bool
		want         Withholding
		wantAdmitted bool
		wantEvidence bool
	}{
		{"thin, default", func(s *SubstanceGate) Gate { return Gate{Substance: s} }, false, WithheldThinMarket, false, true},
		{"thin, admitted", func(s *SubstanceGate) Gate { return Gate{Substance: s} }, true, NotWithheld, true, true},
		{"thin and flagged, admitted", func(s *SubstanceGate) Gate { return Gate{Substance: s, Scam: flagged} }, true, WithheldFlaggedIssuer, false, false},
		{"unmeasured, admitted", func(*SubstanceGate) Gate { return Gate{Substance: unmeasured} }, true, NotWithheld, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, _, base, quote := thinGate(t, dustSubstance)
			v := tc.gate(sub).Judge(ctx, base, quote, "judge_test", Query{AdmitThin: tc.admit})
			if v.Withholding != tc.want || v.ThinAdmitted != tc.wantAdmitted || (v.Substance != nil) != tc.wantEvidence {
				t.Errorf("Judge = %+v, want withholding=%q admitted=%v evidence=%v", v, tc.want, tc.wantAdmitted, tc.wantEvidence)
			}
		})
	}
}

func TestJudgeCounters(t *testing.T) {
	const surface = "judge_counters_test"
	sub, _, base, quote := thinGate(t, dustSubstance)
	g := Gate{Substance: sub}
	withheld0 := surfaceCount(t, obs.PriceServeSubstanceWithheldTotal, surface)
	admitted0 := surfaceCount(t, obs.PriceServeThinAdmittedTotal, surface)

	g.Judge(context.Background(), base, quote, surface, Query{AdmitThin: true})
	if d := surfaceCount(t, obs.PriceServeThinAdmittedTotal, surface) - admitted0; d != 1 {
		t.Errorf("admitted counter moved %v, want 1", d)
	}
	if d := surfaceCount(t, obs.PriceServeSubstanceWithheldTotal, surface) - withheld0; d != 0 {
		t.Errorf("withheld counter moved %v on an admitted verdict, want 0", d)
	}

	g.Judge(context.Background(), base, quote, surface, Query{})
	if d := surfaceCount(t, obs.PriceServeSubstanceWithheldTotal, surface) - withheld0; d != 1 {
		t.Errorf("withheld counter moved %v, want 1", d)
	}
	if d := surfaceCount(t, obs.PriceServeThinAdmittedTotal, surface) - admitted0; d != 1 {
		t.Errorf("admitted counter moved %v on a withheld verdict, want it to stay at 1", d)
	}
}

func TestPriceWithholdingUnchangedByJudge(t *testing.T) {
	ctx := context.Background()
	flagged := NewScamGate(&fakeDir{entry: timescale.DirectoryEntry{Tags: []string{"unsafe"}}, found: true}, ScamGateOptions{})
	for _, sub := range []timescale.MarketSubstance{dustSubstance, thickSubstance} {
		for _, scam := range []*ScamGate{nil, flagged} {
			gate, _, base, quote := thinGate(t, sub)
			g := Gate{Substance: gate, Scam: scam}
			want := WithholdingFor(scam.WithheldPair(ctx, base, quote, "x"), gate.Allowed(ctx, base, quote, "x"))
			if got := g.PriceWithholding(ctx, base, quote, "x"); got != want {
				t.Errorf("volume=%s flagged=%v: PriceWithholding = %q, want the two-half fold %q", sub.VolumeUSD, scam != nil, got, want)
			}
		}
	}
}

func TestAssetSubstanceVerdictThinServedCounts(t *testing.T) {
	const surface = "thin_served_test"
	asset := mustAsset(t, "THIN-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	thin := NewSubstanceGate(&fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}},
		SubstanceGateOptions{Policy: testPolicy()})
	withheld0 := surfaceCount(t, obs.PriceServeSubstanceWithheldTotal, surface)
	allowed, measured, floor := AssetSubstanceVerdictThinServed(context.Background(), thin, asset, nil, surface)
	if allowed || !measured || floor == FloorNone {
		t.Fatalf("got (%v, %v, %q), want a measured thin verdict naming its floor", allowed, measured, floor)
	}
	if d := surfaceCount(t, obs.PriceServeSubstanceWithheldTotal, surface) - withheld0; d != 0 {
		t.Errorf("withheld counter moved %v: the caller counts a thin-served row, not the verdict", d)
	}

	down := NewSubstanceGate(&fakeSubstanceReader{err: errors.New("db down")}, SubstanceGateOptions{Policy: testPolicy()})
	unmeasured0 := surfaceCount(t, obs.PriceServeSubstanceUnmeasuredTotal, surface)
	if allowed, measured, _ := AssetSubstanceVerdictThinServed(context.Background(), down, asset, nil, surface); allowed || measured {
		t.Fatalf("store error: got (%v, %v), want unmeasured", allowed, measured)
	}
	if d := surfaceCount(t, obs.PriceServeSubstanceUnmeasuredTotal, surface) - unmeasured0; d != 1 {
		t.Errorf("unmeasured counter moved %v, want 1", d)
	}
}
