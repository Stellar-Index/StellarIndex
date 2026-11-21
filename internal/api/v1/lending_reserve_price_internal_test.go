package v1

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

type nativeRowErrReader struct{ AssetsReader }

func (nativeRowErrReader) GetNativeAssetRow(context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{}, errors.New("db down")
}

// A backstop tier whose price read failed is a read failure that degrades the response, never "unpriced".
func TestBackstopValue_FailedPriceReadIsAReadFailureNotUnpriced(t *testing.T) {
	xlmSAC, ok := xdrjson.SACContractID("native", pubnetPassphrase)
	if !ok {
		t.Fatal("native SAC")
	}
	s := New(Options{AssetsReader: nativeRowErrReader{}})
	view := LendingBackstopView{Protocol: strRef(blnt.SourceName), Tiers: make([]LendingBackstopTierView, 1)}
	st := clickhouse.LendingBackstopState{Tiers: []clickhouse.BackstopTierState{
		{Tier: "FirstLoss", Asset: "Xlm", Token: xlmSAC, Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(1), Tokens: big.NewInt(1e7), Q4W: big.NewInt(0)}},
	}}
	_, degraded := s.valueBackstopView(context.Background(), &view, st)
	if tv := view.Tiers[0]; tv.TokensUSD != nil || strPtrOr(tv.USDReason) != exclusionPriceReadFailed {
		t.Errorf("tier tokens_usd=%s usd_reason=%s, want null with %s", strPtrOr(tv.TokensUSD), strPtrOr(tv.USDReason), exclusionPriceReadFailed)
	}
	if !degraded {
		t.Error("a failed price read did not degrade the backstop response")
	}
}

// A backstop tier whose price a gate refused is withheld, never "unpriced", and carries no value.
func TestBackstopValue_WithheldPriceIsWithheldNotUnpriced(t *testing.T) {
	xlmSAC, ok := xdrjson.SACContractID("native", pubnetPassphrase)
	if !ok {
		t.Fatal("native SAC")
	}
	s := New(Options{Prices: withheldUSDReader{"native/" + defaultPriceQuote.String(): {err: ErrPriceWithheld}}})
	view := LendingBackstopView{Protocol: strRef(blnt.SourceName), Tiers: make([]LendingBackstopTierView, 1)}
	st := clickhouse.LendingBackstopState{Tiers: []clickhouse.BackstopTierState{
		{Tier: "FirstLoss", Asset: "Xlm", Token: xlmSAC, Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(1), Tokens: big.NewInt(1e7), Q4W: big.NewInt(0)}},
	}}
	_, degraded := s.valueBackstopView(context.Background(), &view, st)
	if tv := view.Tiers[0]; tv.TokensUSD != nil || tv.PriceUSD != nil || strPtrOr(tv.USDReason) != exclusionPriceWithheld {
		t.Errorf("tier tokens_usd=%s price_usd=%s usd_reason=%s, want null with %s",
			strPtrOr(tv.TokensUSD), strPtrOr(tv.PriceUSD), strPtrOr(tv.USDReason), exclusionPriceWithheld)
	}
	if view.TotalUSD != nil || degraded {
		t.Errorf("total_usd=%s degraded=%v, want null and not degraded", strPtrOr(view.TotalUSD), degraded)
	}
}

// reservePriceToken is a Soroban token with no canonical USD feed: only its catalogue row prices it.
const reservePriceToken = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"

func catalogueOnlyReserve() clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Asset: reservePriceToken, Decimals: 7, DecimalsFound: true,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(100_000_000), BorrowedUnderlying: big.NewInt(40_000_000)},
	}
}

func catalogueRowReader(price string) *truthAssetsReader {
	return &truthAssetsReader{byID: map[string]timescale.AssetRow{
		reservePriceToken: {AssetID: reservePriceToken, PriceUSD: &price},
	}}
}

// A reserve whose token /v1/assets prices from its catalogue row is valued at that price, not reported as having no USD value.
func TestReserveValue_UsesTheAssetPagesCatalogueRowPrice(t *testing.T) {
	s := New(Options{AssetsReader: catalogueRowReader("2.5")})
	rv, supplied, _ := s.buildReserveView(context.Background(), catalogueOnlyReserve())
	if strPtrOr(rv.SuppliedUSD) != "25.00" || strPtrOr(rv.BorrowedUSD) != "10.00" || strPtrOr(rv.PriceUSD) != "2.5" {
		t.Fatalf("supplied_usd=%s borrowed_usd=%s price_usd=%s, want 25.00 / 10.00 / 2.5",
			strPtrOr(rv.SuppliedUSD), strPtrOr(rv.BorrowedUSD), strPtrOr(rv.PriceUSD))
	}
	if supplied == nil || supplied.Cmp(big.NewRat(25, 1)) != 0 {
		t.Errorf("TVL contribution = %v, want exactly 25", supplied)
	}
	if rv.usdExclusion != "" {
		t.Errorf("valued reserve carries exclusion %q", rv.usdExclusion)
	}
	if got := reserveWirePriceBasis(t, rv); got != "" {
		t.Errorf("price_basis = %s on a catalogue-row price, want it omitted", got)
	}
}

// reserveWirePriceBasis is the reserve's serialised price_basis, "" when omitted.
func reserveWirePriceBasis(t *testing.T, rv ReserveView) string {
	t.Helper()
	b, err := json.Marshal(rv)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	return string(wire["price_basis"])
}

// A reserve token with no feed and no catalogue row is valued at its transitive price when both legs clear the substance gate,
// and not when either leg is refused.
func TestReserveValue_TransitiveOnlyPriceUnderBothLegGates(t *testing.T) {
	const hop = "CBIJBDNZNF4X35BJ4FFZWCDBSCKOP5NB4PLG4SNENRMLAPYG4P5FM6VN"
	near, far := reservePriceToken+"|"+hop, hop+"|native"
	for _, tc := range []struct {
		name  string
		allow map[string]bool
		want  string
	}{
		{"both legs clear", map[string]bool{near: true, far: true}, "25.00"},
		{"near leg refused", map[string]bool{far: true}, ""},
		{"far leg refused", map[string]bool{near: true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{
				AssetsReader:     &truthAssetsReader{byID: map[string]timescale.AssetRow{}},
				TransitivePricer: &stubPricer{ok: true, tp: timescale.TransitivePrice{PriceUSD: "2.5", Hop: hop, HopVolume24hUSD: "5000", HopQuote: "native"}},
				Substance:        &stubListingGate{allow: tc.allow},
			})
			rv, supplied, _ := s.buildReserveView(context.Background(), catalogueOnlyReserve())
			if got := strPtrOr(rv.SuppliedUSD); got != tc.want && (tc.want != "" || rv.SuppliedUSD != nil) {
				t.Fatalf("supplied_usd = %s, want %q", got, tc.want)
			}
			if tc.want == "" {
				if supplied != nil || rv.PriceUSD != nil {
					t.Errorf("refused leg valued: price_usd = %s", strPtrOr(rv.PriceUSD))
				}
				return
			}
			if strPtrOr(rv.BorrowedUSD) != "10.00" || supplied == nil || supplied.Cmp(big.NewRat(25, 1)) != 0 {
				t.Errorf("borrowed_usd = %s TVL contribution = %v, want 10.00 and exactly 25", strPtrOr(rv.BorrowedUSD), supplied)
			}
			if got := reserveWirePriceBasis(t, rv); got != `"transitive"` {
				t.Errorf("price_basis = %s, want \"transitive\" beside a transitive price_usd", got)
			}
		})
	}
}

// A catalogue row price the substance gate refuses (a thin market) stays withheld for a reserve, under its own reason.
func TestReserveValue_ThinCatalogueRowPriceIsWithheldNotValued(t *testing.T) {
	s := New(Options{AssetsReader: catalogueRowReader("2.5"), Substance: &stubListingGate{allow: map[string]bool{}}})
	rv, supplied, _ := s.buildReserveView(context.Background(), catalogueOnlyReserve())
	if rv.SuppliedUSD != nil || rv.BorrowedUSD != nil || rv.PriceUSD != nil || supplied != nil {
		t.Fatalf("thin price valued: supplied_usd=%s price_usd=%s", strPtrOr(rv.SuppliedUSD), strPtrOr(rv.PriceUSD))
	}
	if rv.usdExclusion != exclusionPriceWithheld {
		t.Errorf("exclusion = %q, want %q", rv.usdExclusion, exclusionPriceWithheld)
	}
}

// A failed read on the price chain is a read failure, never "no USD value", and never a value from a later arm.
func TestReserveValue_FailedPriceReadIsAReadFailureNotNoValue(t *testing.T) {
	for name, opts := range map[string]Options{
		"catalogue row read failed":   {AssetsReader: &truthAssetsReader{byIDErr: errors.New("db down")}},
		"substance gate did not rule": {AssetsReader: catalogueRowReader("2.5"), Substance: &unmeasuredGate{}},
	} {
		t.Run(name, func(t *testing.T) {
			rv, supplied, _ := New(opts).buildReserveView(context.Background(), catalogueOnlyReserve())
			if rv.SuppliedUSD != nil || rv.PriceUSD != nil || supplied != nil {
				t.Fatalf("unread price valued: supplied_usd=%s price_usd=%s", strPtrOr(rv.SuppliedUSD), strPtrOr(rv.PriceUSD))
			}
			if rv.usdExclusion != exclusionPriceReadFailed {
				t.Errorf("exclusion = %q, want %q", rv.usdExclusion, exclusionPriceReadFailed)
			}
		})
	}
}

func strPtrOr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
