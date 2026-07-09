package divergence_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/cachekeys"
	"github.com/StellarIndex/stellar-index/internal/divergence"
)

// TestCountAgreeing pins the cross-oracle agreement math (ADR-0019
// Phase 3): a reference agrees when its per-reference delta is at or
// below the threshold, mirroring the complement of the observation
// sink's strict `>` firing test.
func TestCountAgreeing(t *testing.T) {
	cases := []struct {
		name      string
		ourPrice  float64
		sources   map[string]float64
		threshold float64
		want      int
	}{
		{
			name:      "all agree exactly",
			ourPrice:  1.00,
			sources:   map[string]float64{"a": 1.00, "b": 1.00, "c": 1.00},
			threshold: 5.0,
			want:      3,
		},
		{
			name:     "one outlier excluded",
			ourPrice: 1.00,
			// |1.00-1.30|/1.30*100 ≈ 23.1% > 5%.
			sources:   map[string]float64{"a": 1.001, "b": 0.999, "outlier": 1.30},
			threshold: 5.0,
			want:      2,
		},
		{
			// Delta exactly AT threshold counts as agreement (<=),
			// mirroring flushObservations' strict > firing test.
			// Binary-exact values so the boundary is truly exact:
			// ref=1.0, our=1.5 → |0.5|/1.0*100 = 50.0 == threshold.
			name:      "delta exactly at threshold agrees",
			ourPrice:  1.5,
			sources:   map[string]float64{"a": 1.0},
			threshold: 50.0,
			want:      1,
		},
		{
			// Just past the threshold does not agree.
			name:      "delta just above threshold disagrees",
			ourPrice:  1.5078125, // binary-exact; delta = 50.78125%
			sources:   map[string]float64{"a": 1.0},
			threshold: 50.0,
			want:      0,
		},
		{
			// CS-087: no responders means UNCHECKED — zero here must
			// pair with SuccessCount=0 at the consumer, never read as
			// "everyone disagrees".
			name:      "empty sources",
			ourPrice:  1.00,
			sources:   map[string]float64{},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "nil sources",
			ourPrice:  1.00,
			sources:   nil,
			threshold: 5.0,
			want:      0,
		},
		{
			// Defensive: zero / negative / non-finite reference prices
			// are skipped (no meaningful delta), matching the
			// observation sink's zero-guard.
			name:      "bad reference prices skipped",
			ourPrice:  1.00,
			sources:   map[string]float64{"zero": 0, "neg": -1, "nan": math.NaN(), "inf": math.Inf(1), "good": 1.00},
			threshold: 5.0,
			want:      1,
		},
		{
			name:      "non-positive our price counts nothing",
			ourPrice:  0,
			sources:   map[string]float64{"a": 1.00},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "NaN our price counts nothing",
			ourPrice:  math.NaN(),
			sources:   map[string]float64{"a": 1.00},
			threshold: 5.0,
			want:      0,
		},
		{
			name:      "negative threshold counts nothing",
			ourPrice:  1.00,
			sources:   map[string]float64{"a": 1.00},
			threshold: -1,
			want:      0,
		},
		{
			// Zero threshold: only exact matches agree.
			name:      "zero threshold exact match only",
			ourPrice:  1.00,
			sources:   map[string]float64{"exact": 1.00, "near": 1.0001},
			threshold: 0,
			want:      1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := divergence.CountAgreeing(tc.ourPrice, tc.sources, tc.threshold); got != tc.want {
				t.Errorf("CountAgreeing(%v, %v, %v) = %d, want %d",
					tc.ourPrice, tc.sources, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestRefreshPair_AgreementCountPersisted — RefreshPair computes the
// agreement count against the worker's threshold and persists it in
// the cached JSON, distinct from SuccessCount ("responded" vs
// "corroborates").
func TestRefreshPair_AgreementCountPersisted(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", price: 1.00},
		&stubReference{name: "b", price: 1.001},
		&stubReference{name: "dissenter", price: 1.30}, // ~23% off — responds but does not agree
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	if err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now()); err != nil {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cached.SuccessCount != 3 {
		t.Errorf("SuccessCount = %d, want 3", cached.SuccessCount)
	}
	if cached.AgreementCount != 2 {
		t.Errorf("AgreementCount = %d, want 2 (dissenter responded but must not corroborate)", cached.AgreementCount)
	}
}

// TestRefreshPair_AllReferencesDark_AgreementZeroMeansUnchecked —
// CS-087: when every reference fails, the cached result reads
// SuccessCount=0 + AgreementCount=0 and RefreshPair returns
// ErrNoReferenceResponded. Consumers must interpret that as
// "unchecked", never as "zero references agree with us".
func TestRefreshPair_AllReferencesDark_AgreementZeroMeansUnchecked(t *testing.T) {
	refs := []divergence.Reference{
		&stubReference{name: "a", err: divergence.ErrPriceUnavailable},
		&stubReference{name: "b", err: divergence.ErrAssetUnsupported},
	}
	svc, rdb, _ := newTestService(t, refs, divergence.ServiceOptions{Threshold: 5.0})

	err := svc.RefreshPair(context.Background(), xlmUSD(t), 1.00, time.Now())
	if !errors.Is(err, divergence.ErrNoReferenceResponded) {
		t.Fatalf("RefreshPair err = %v, want ErrNoReferenceResponded", err)
	}

	body, gerr := rdb.Get(context.Background(), cachekeys.Divergence(xlmUSD(t)).String()).Bytes()
	if gerr != nil {
		t.Fatalf("redis get: %v", gerr)
	}
	var cached divergence.CachedResult
	if uerr := json.Unmarshal(body, &cached); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if cached.SuccessCount != 0 {
		t.Errorf("SuccessCount = %d, want 0", cached.SuccessCount)
	}
	if cached.AgreementCount != 0 {
		t.Errorf("AgreementCount = %d, want 0 on a dark run", cached.AgreementCount)
	}
}
