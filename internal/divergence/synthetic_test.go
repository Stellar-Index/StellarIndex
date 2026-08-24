package divergence

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// pairStub is a pair-aware Reference stub: answers from a fixed
// pair-string → price map, ErrAssetUnsupported for everything else, and
// records the pairs it was asked for (leg-routing assertions).
type pairStub struct {
	name   string
	prices map[string]float64
	err    error // when set, returned for EVERY lookup
	asked  []string
}

func (p *pairStub) Name() string { return p.name }

func (p *pairStub) LookupPrice(_ context.Context, pair canonical.Pair, _ time.Time) (float64, error) {
	p.asked = append(p.asked, pair.String())
	if p.err != nil {
		return 0, p.err
	}
	if v, ok := p.prices[pair.String()]; ok {
		return v, nil
	}
	return 0, ErrAssetUnsupported
}

func synthPair(t *testing.T, quoteCode string) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	quote, err := canonical.NewFiatAsset(quoteCode)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	return canonical.Pair{Base: base, Quote: quote}
}

// TestSyntheticCross_DerivesNonUSDFiatQuote is the reason this
// reference exists (2026-08-24): XLM/EUR gets a SECOND reference by
// crossing XLM/USD (oracle leg) with EUR/USD (reflector-fx leg), so
// SuccessCount can reach the divergence trust floor and the
// corroborated-release gate can auto-release genuine repricings on
// EUR/GBP-quoted pairs instead of paging an operator per freeze.
func TestSyntheticCross_DerivesNonUSDFiatQuote(t *testing.T) {
	usdLeg := &pairStub{name: "reflector-cex", prices: map[string]float64{
		"crypto:XLM/fiat:USD": 0.35,
	}}
	fxLeg := &pairStub{name: "reflector-fx", prices: map[string]float64{
		"fiat:EUR/fiat:USD": 1.09,
	}}
	syn, err := NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{usdLeg},
		FXLegs:  []Reference{fxLeg},
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	got, err := syn.LookupPrice(context.Background(), synthPair(t, "EUR"), time.Now())
	if err != nil {
		t.Fatalf("LookupPrice: %v", err)
	}
	want := 0.35 / 1.09
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("cross = %v, want %v (XLM/USD ÷ EUR/USD)", got, want)
	}
	if len(usdLeg.asked) != 1 || usdLeg.asked[0] != "crypto:XLM/fiat:USD" {
		t.Errorf("USD leg asked %v, want exactly crypto:XLM/fiat:USD", usdLeg.asked)
	}
	if len(fxLeg.asked) != 1 || fxLeg.asked[0] != "fiat:EUR/fiat:USD" {
		t.Errorf("FX leg asked %v, want exactly fiat:EUR/fiat:USD", fxLeg.asked)
	}
}

// TestSyntheticCross_USDAndNonFiatQuotesUnsupported — USD-quoted pairs
// already have the direct oracle references; a synthetic reading there
// would double-count the very feeds it is built from. Non-fiat quotes
// are out of scope entirely.
func TestSyntheticCross_USDAndNonFiatQuotesUnsupported(t *testing.T) {
	usdLeg := &pairStub{name: "u", prices: map[string]float64{"crypto:XLM/fiat:USD": 0.35}}
	fxLeg := &pairStub{name: "f", prices: map[string]float64{"fiat:EUR/fiat:USD": 1.09}}
	syn, _ := NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{usdLeg}, FXLegs: []Reference{fxLeg},
	})

	if _, err := syn.LookupPrice(context.Background(), synthPair(t, "USD"), time.Now()); !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("USD quote: err = %v, want ErrAssetUnsupported", err)
	}
	base, _ := canonical.ParseAsset("crypto:XLM")
	native := canonical.NativeAsset()
	cryptoQuote := canonical.Pair{Base: native, Quote: base}
	if _, err := syn.LookupPrice(context.Background(), cryptoQuote, time.Now()); !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("crypto quote: err = %v, want ErrAssetUnsupported", err)
	}
	if len(usdLeg.asked)+len(fxLeg.asked) != 0 {
		t.Errorf("out-of-scope pairs must not consult the legs (asked %v / %v)",
			usdLeg.asked, fxLeg.asked)
	}
}

// TestSyntheticCross_ErrorSemantics preserves Compare's
// unsupported-vs-degraded bookkeeping: all legs unsupported →
// unsupported (no reference universe for the pair); any transient leg
// failure → unavailable (a reading SHOULD have existed).
func TestSyntheticCross_ErrorSemantics(t *testing.T) {
	fxOK := &pairStub{name: "f", prices: map[string]float64{"fiat:EUR/fiat:USD": 1.09}}

	// All USD legs unsupported → unsupported.
	syn, _ := NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{&pairStub{name: "u1"}, &pairStub{name: "u2"}},
		FXLegs:  []Reference{fxOK},
	})
	if _, err := syn.LookupPrice(context.Background(), synthPair(t, "EUR"), time.Now()); !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("all-unsupported USD legs: err = %v, want ErrAssetUnsupported", err)
	}

	// A transient USD-leg failure → unavailable, even with another leg
	// unsupported.
	syn, _ = NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{
			&pairStub{name: "u1", err: errors.New("rpc timeout")},
			&pairStub{name: "u2"},
		},
		FXLegs: []Reference{fxOK},
	})
	if _, err := syn.LookupPrice(context.Background(), synthPair(t, "EUR"), time.Now()); !errors.Is(err, ErrPriceUnavailable) {
		t.Errorf("transient USD leg: err = %v, want ErrPriceUnavailable", err)
	}

	// FX leg unsupported → unsupported (GBP not listed).
	syn, _ = NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{&pairStub{name: "u", prices: map[string]float64{"crypto:XLM/fiat:USD": 0.35}}},
		FXLegs:  []Reference{&pairStub{name: "f"}},
	})
	if _, err := syn.LookupPrice(context.Background(), synthPair(t, "GBP"), time.Now()); !errors.Is(err, ErrAssetUnsupported) {
		t.Errorf("unsupported FX leg: err = %v, want ErrAssetUnsupported", err)
	}

	// A zero/NaN leg answer is transient, never a division input.
	syn, _ = NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{&pairStub{name: "u", prices: map[string]float64{"crypto:XLM/fiat:USD": 0}}},
		FXLegs:  []Reference{fxOK},
	})
	if _, err := syn.LookupPrice(context.Background(), synthPair(t, "EUR"), time.Now()); !errors.Is(err, ErrPriceUnavailable) {
		t.Errorf("zero-price leg: err = %v, want ErrPriceUnavailable", err)
	}
}

// TestSyntheticCross_LegOrderFirstAnswerWins — candidates are tried in
// order; a later leg is consulted only when earlier ones cannot answer.
func TestSyntheticCross_LegOrderFirstAnswerWins(t *testing.T) {
	first := &pairStub{name: "first", prices: map[string]float64{"crypto:XLM/fiat:USD": 0.35}}
	second := &pairStub{name: "second", prices: map[string]float64{"crypto:XLM/fiat:USD": 999}}
	fxLeg := &pairStub{name: "f", prices: map[string]float64{"fiat:EUR/fiat:USD": 1.0}}
	syn, _ := NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{first, second}, FXLegs: []Reference{fxLeg},
	})
	got, err := syn.LookupPrice(context.Background(), synthPair(t, "EUR"), time.Now())
	if err != nil || got != 0.35 {
		t.Errorf("got %v, %v — want the FIRST leg's 0.35", got, err)
	}
	if len(second.asked) != 0 {
		t.Errorf("second leg consulted despite the first answering")
	}
}

// TestSyntheticCross_ConstructionRequiresBothLegs — a cross with a
// missing leg class can never answer and must not be constructed (it
// would only add a permanent failure row to every result).
func TestSyntheticCross_ConstructionRequiresBothLegs(t *testing.T) {
	if _, err := NewSyntheticCrossReference(SyntheticCrossOptions{
		USDLegs: []Reference{&pairStub{name: "u"}},
	}); err == nil {
		t.Error("constructed with no FX leg")
	}
	if _, err := NewSyntheticCrossReference(SyntheticCrossOptions{
		FXLegs: []Reference{&pairStub{name: "f"}},
	}); err == nil {
		t.Error("constructed with no USD leg")
	}
}
