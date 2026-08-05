package pricingguard

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func mustAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

func testPolicy() SubstancePolicy {
	return SubstancePolicy{
		MinVolumeUSD: new(big.Rat).SetInt64(1000),
		MinBuckets:   20,
		MinSpan:      6 * time.Hour,
		Window:       24 * time.Hour,
	}
}

func TestSubstanceOK_Floors(t *testing.T) {
	pol := testPolicy()
	ok := func(vol int64, buckets, span int64) bool {
		return SubstanceOK(new(big.Rat).SetInt64(vol), buckets, span, pol)
	}
	if !ok(1000, 20, 6*3600) {
		t.Error("exactly-at-floor must pass")
	}
	if ok(999, 20, 6*3600) {
		t.Error("below volume floor must fail")
	}
	if ok(1000, 19, 6*3600) {
		t.Error("below bucket floor must fail")
	}
	if ok(1000, 20, 6*3600-1) {
		t.Error("below span floor must fail")
	}
	// The 2026-08-04 incident shape: one $8.57 seed bucket + one dump
	// bucket. Massively below every floor.
	if ok(9, 2, 21*60) {
		t.Error("incident-shaped market must fail")
	}
	if SubstanceOK(nil, 100, 24*3600, pol) {
		t.Error("nil volume must count as zero (fail-closed)")
	}
}

func TestSubstanceGated_Applicability(t *testing.T) {
	classic := mustAsset(t, "USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	fiatUSD := mustAsset(t, "fiat:USD")
	fiatEUR := mustAsset(t, "fiat:EUR")
	cryptoBTC := mustAsset(t, "crypto:BTC")

	cases := []struct {
		name        string
		base, quote canonical.Asset
		want        bool
	}{
		{"classic/native (the attack class)", classic, native, true},
		{"native/fiat:USD (one on-chain leg)", native, fiatUSD, true},
		{"fiat/fiat cross", fiatEUR, fiatUSD, false},
		{"CEX crypto ticker vs fiat", cryptoBTC, fiatUSD, false},
		{"crypto vs crypto", cryptoBTC, mustAsset(t, "crypto:ETH"), false},
	}
	for _, tc := range cases {
		if got := SubstanceGated(tc.base, tc.quote); got != tc.want {
			t.Errorf("%s: SubstanceGated = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeSubstanceReader returns canned substance per pair key and counts
// calls.
type fakeSubstanceReader struct {
	byPair map[string]timescale.MarketSubstance
	err    error
	calls  int
}

func (f *fakeSubstanceReader) PairMarketSubstance(_ context.Context, p canonical.Pair, _ time.Duration) (timescale.MarketSubstance, error) {
	f.calls++
	if f.err != nil {
		return timescale.MarketSubstance{}, f.err
	}
	if sub, ok := f.byPair[p.String()]; ok {
		return sub, nil
	}
	return timescale.MarketSubstance{VolumeUSD: "0"}, nil
}

func TestSubstanceGate_WithholdsThinPair(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("empty-substance on-chain pair must be withheld")
	}
}

func TestSubstanceGate_AllowsHealthyPair(t *testing.T) {
	classic := mustAsset(t, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	native := mustAsset(t, "native")
	pair, _ := canonical.NewPair(classic, native)
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{
		pair.String(): {VolumeUSD: "250000.5", Buckets: 900, SpanSeconds: 23 * 3600},
	}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("healthy pair must be allowed")
	}
}

func TestSubstanceGate_AliasUnion(t *testing.T) {
	// The pair as requested: native/fiat:USD — zero rows by
	// construction. The alias family (crypto:XLM/fiat:USD) carries the
	// CEX volume. The gate must sum across the union and allow.
	native := mustAsset(t, "native")
	fiatUSD := mustAsset(t, "fiat:USD")
	cexPair, _ := canonical.NewPair(mustAsset(t, "crypto:XLM"), fiatUSD)
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{
		cexPair.String(): {VolumeUSD: "9000000", Buckets: 1400, SpanSeconds: 24 * 3600},
	}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), native, fiatUSD, "test") {
		t.Fatal("native/fiat:USD must be allowed via the crypto:XLM alias union")
	}
}

func TestSubstanceGate_CachesVerdict(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	gate.Allowed(context.Background(), classic, native, "test")
	after := reader.calls
	if after == 0 {
		t.Fatal("first verdict must measure")
	}
	gate.Allowed(context.Background(), classic, native, "test")
	// Direction-insensitive cache: the flipped orientation shares it.
	gate.Allowed(context.Background(), native, classic, "test")
	if reader.calls != after {
		t.Errorf("cached verdict re-measured: %d calls after, %d now", after, reader.calls)
	}
}

func TestSubstanceGate_FailsOpenOnStoreError(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{err: errors.New("db down")}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})

	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("store error must fail open (a DB blip must not 404 the price surface)")
	}
	// And the error verdict must NOT be cached — recovery is immediate.
	reader.err = nil
	before := reader.calls
	gate.Allowed(context.Background(), classic, native, "test")
	if reader.calls == before {
		t.Error("error verdict was cached; next request must re-measure")
	}
}

func TestSubstanceGate_NilGateAllows(t *testing.T) {
	var gate *SubstanceGate
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	if !gate.Allowed(context.Background(), classic, mustAsset(t, "native"), "test") {
		t.Fatal("nil gate must allow everything")
	}
}

func TestSubstanceGate_OffChainPairSkipsMeasurement(t *testing.T) {
	reader := &fakeSubstanceReader{}
	gate := NewSubstanceGate(reader, SubstanceGateOptions{Policy: testPolicy()})
	if !gate.Allowed(context.Background(), mustAsset(t, "fiat:EUR"), mustAsset(t, "fiat:USD"), "test") {
		t.Fatal("fiat cross must be exempt")
	}
	if reader.calls != 0 {
		t.Errorf("exempt pair must not hit the store, got %d calls", reader.calls)
	}
}

func TestSubstancePolicyFromValues(t *testing.T) {
	pol := SubstancePolicyFromValues(2500, 30, 120, 48)
	if pol.MinVolumeUSD.Cmp(new(big.Rat).SetInt64(2500)) != 0 {
		t.Errorf("MinVolumeUSD = %v", pol.MinVolumeUSD)
	}
	if pol.MinBuckets != 30 || pol.MinSpan != 2*time.Hour || pol.Window != 48*time.Hour {
		t.Errorf("policy = %+v", pol)
	}
	// Zeros defer to package defaults at construction time.
	gate := NewSubstanceGate(&fakeSubstanceReader{}, SubstanceGateOptions{Policy: SubstancePolicyFromValues(0, 0, 0, 0)})
	if gate.policy.MinVolumeUSD.Cmp(new(big.Rat).SetInt64(DefaultSubstanceMinVolumeUSD)) != 0 {
		t.Errorf("default MinVolumeUSD = %v", gate.policy.MinVolumeUSD)
	}
	if gate.policy.MinBuckets != DefaultSubstanceMinBuckets || gate.policy.MinSpan != DefaultSubstanceMinSpan || gate.policy.Window != DefaultSubstanceWindow {
		t.Errorf("defaults = %+v", gate.policy)
	}
}

// countingHandler counts slog records at Warn+ so the transition-only
// logging contract is testable.
type countingHandler struct{ warns *int }

func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		*h.warns++
	}
	return nil
}
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h countingHandler) WithGroup(string) slog.Handler      { return h }

// TestSubstanceGate_LogsOnTransitionsOnly — the steady state
// (hundreds of thin pairs re-measured every TTL expiry) produced
// 6,000 WARNs/hour on r1 (2026-08-05). The metric carries the volume;
// the log carries only verdict CHANGES.
func TestSubstanceGate_LogsOnTransitionsOnly(t *testing.T) {
	classic := mustAsset(t, "SCAM-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V")
	native := mustAsset(t, "native")
	reader := &fakeSubstanceReader{byPair: map[string]timescale.MarketSubstance{}}
	warns := 0
	gate := NewSubstanceGate(reader, SubstanceGateOptions{
		Policy: testPolicy(),
		Logger: slog.New(countingHandler{warns: &warns}),
	})
	now := time.Unix(1_700_000_000, 0)
	gate.now = func() time.Time { return now }

	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 1 {
		t.Fatalf("first withheld observation must WARN once, got %d", warns)
	}
	// TTL expiry → re-measure, still withheld → NO new warn.
	now = now.Add(substanceCacheTTL + time.Second)
	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 1 {
		t.Errorf("repeat withheld verdict must not re-WARN, got %d", warns)
	}
	// Pair recovers → next re-measure logs no warn either (Info only).
	pair, _ := canonical.NewPair(classic, native)
	reader.byPair[pair.String()] = timescale.MarketSubstance{
		VolumeUSD: "50000", Buckets: 500, SpanSeconds: 23 * 3600,
	}
	now = now.Add(substanceCacheTTL + time.Second)
	if !gate.Allowed(context.Background(), classic, native, "test") {
		t.Fatal("recovered pair must be allowed")
	}
	if warns != 1 {
		t.Errorf("recovery must not WARN, got %d", warns)
	}
	// And a flip BACK to withheld warns again.
	delete(reader.byPair, pair.String())
	now = now.Add(substanceCacheTTL + time.Second)
	gate.Allowed(context.Background(), classic, native, "test")
	if warns != 2 {
		t.Errorf("allowed→withheld flip must WARN, got %d", warns)
	}
}
