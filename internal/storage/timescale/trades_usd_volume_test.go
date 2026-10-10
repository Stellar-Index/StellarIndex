package timescale

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

const (
	circleIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	circleKey    = "USDC-" + circleIssuer
	usdcSACID    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	aquaID       = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	sep41AID     = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	sep41BID     = "CA6GAFOJCW4MGQQBUCQUSA3CLIH25G4SNKB2JHYKZCVWZTNW5VXMSC4O"
)

// volTrade builds a trade with the given raw base and quote amounts.
func volTrade(t *testing.T, source string, base, quote canonical.Asset, baseAmt, quoteAmt int64) canonical.Trade {
	t.Helper()
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source:      source,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
	}
}

// mkClassicDEXTrade is a trade with a fixed 1.0 (10^7 raw) base leg.
func mkClassicDEXTrade(t *testing.T, source string, base, quote canonical.Asset, quoteAmt int64) canonical.Trade {
	t.Helper()
	return volTrade(t, source, base, quote, 10_000_000, quoteAmt)
}

// circleSpec is a spec pegging Circle's classic USDC, plus the given SAC wrappers.
func circleSpec(t *testing.T, sacWrappers map[string]string) *USDVolumeQuoteSpec {
	t.Helper()
	spec, err := NewUSDVolumeQuoteSpec([]string{circleKey}, sacWrappers)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	return spec
}

// stubFXResolver implements USDVolumeFXResolver from a map keyed by asset string.
type stubFXResolver struct {
	prices map[string]string
	err    error
}

func (s stubFXResolver) USDPriceAt(_ context.Context, asset canonical.Asset, _ time.Time) (string, bool, error) {
	if s.err != nil {
		return "", false, s.err
	}
	p, ok := s.prices[asset.String()]
	if !ok {
		return "", false, nil
	}
	return p, true, nil
}

// panicFXResolver proves an earlier tier short-circuits before the resolver runs.
type panicFXResolver struct{}

func (panicFXResolver) USDPriceAt(_ context.Context, _ canonical.Asset, _ time.Time) (string, bool, error) {
	panic("FX resolver should not be consulted when an earlier tier matches")
}

func mustAsset(t *testing.T, s string) canonical.Asset {
	t.Helper()
	a, err := canonical.ParseAsset(s)
	if err != nil {
		t.Fatalf("ParseAsset(%q): %v", s, err)
	}
	return a
}

func TestQuoteIsUSDOrUSDPegged(t *testing.T) {
	cases := []struct {
		asset string
		want  bool
	}{
		{"fiat:USD", true},
		{"crypto:USDC", true},
		{"crypto:USDT", true},
		{"fiat:EUR", false},
		{"crypto:EURC", false},
		{"crypto:XLM", false},
	}
	for _, tc := range cases {
		t.Run(tc.asset, func(t *testing.T) {
			if got := quoteIsUSDOrUSDPegged(mustAsset(t, tc.asset)); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume walks the usd_volume waterfall. want "" means the column
// stays NULL: no tier may fabricate a figure.
func TestTradeUSDVolume(t *testing.T) {
	t.Parallel()
	var (
		usd       = mustAsset(t, "fiat:USD")
		eur       = mustAsset(t, "fiat:EUR")
		cexXLM    = mustAsset(t, "crypto:XLM")
		cexUSDC   = mustAsset(t, "crypto:USDC")
		xlm       = canonical.NativeAsset()
		xlmSAC    = mustAsset(t, canonical.XLMSacContractID)
		circle    = mustAsset(t, circleKey)
		otherUSDC = mustAsset(t, "USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
		usdcSAC   = mustAsset(t, usdcSACID)
		aqua      = mustAsset(t, aquaID)
		yxt       = mustAsset(t, "YxT-GDQJXPESRTKBHMTLPHKZNVFGQA77HPCGYW2NPUORVMDMPJBCFGGBYNS2")
		sixT      = mustAsset(t, "6T-GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW")
		f8        = mustAsset(t, "F8-GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW")
		sep41A    = mustAsset(t, sep41AID)
		sep41B    = mustAsset(t, sep41BID)

		plain    = circleSpec(t, nil)
		withSAC  = circleSpec(t, map[string]string{usdcSACID: "USDC:" + circleIssuer})
		xlmAt12  = stubFXResolver{prices: map[string]string{xlm.String(): "0.12"}}
		aquaOnly = stubFXResolver{prices: map[string]string{aqua.String(): "0.001"}}
	)
	tr := func(source string, base, quote canonical.Asset, baseAmt, quoteAmt int64) canonical.Trade {
		if base == quote { // NewPair rejects a degenerate pair; the probe needs one
			x := volTrade(t, source, base, aqua, baseAmt, quoteAmt)
			x.Pair.Quote = quote
			return x
		}
		return volTrade(t, source, base, quote, baseAmt, quoteAmt)
	}

	cases := []struct {
		name string
		tr   canonical.Trade
		spec *USDVolumeQuoteSpec
		fx   USDVolumeFXResolver
		want string
	}{
		// Off-chain: uniform 10^8 scale for CEX, 10^6 for FX pollers.
		{"binance + fiat:USD at 1e8", tr("binance", cexXLM, usd, 100_000_000, 100_000_000), nil, nil, "1.00000000"},
		// A hard-coded 8 would value this 100x low.
		{"exchangeratesapi + fiat:USD at 1e6 is $0.50", tr("exchangeratesapi", cexXLM, usd, 100_000_000, 500_000), nil, nil, "0.50000000"},
		{"exchangeratesapi + fiat:USD at 1e6 is $1", tr("exchangeratesapi", cexXLM, usd, 100_000_000, 1_000_000), nil, nil, "1.00000000"},
		{"kraken + crypto:USDC peg", tr("kraken", cexXLM, cexUSDC, 100_000_000, 4_250_000_000), nil, nil, "42.50000000"},

		// On-chain with a declared USD-pegged quote: 10^7 scale.
		{"sdex XLM/classic USDC", tr("sdex", xlm, circle, 10_000_000, 10_000_000), withSAC, nil, "1.00000000"},
		{"soroswap XLM/USDC SAC", tr("soroswap", xlm, usdcSAC, 10_000_000, 4_200_000), withSAC, nil, "0.42000000"},
		{"phoenix XLM/USDC SAC large", tr("phoenix", xlm, usdcSAC, 10_000_000, 12_345_600_000), withSAC, nil, "1234.56000000"},

		// Out of scope: the column stays NULL rather than misleadingly precise.
		{"on-chain DEX with no spec", tr("soroswap", xlm, circle, 100_000_000, 1_000_000_000), nil, nil, ""},
		{"classic USDC not in the allow-list", tr("soroswap", xlm, otherUSDC, 100_000_000, 1_000_000_000), withSAC, nil, ""},
		{"pure SEP-41 with no classic counterpart", tr("soroswap", xlm, sep41A, 100_000_000, 1_000_000_000), withSAC, nil, ""},
		{"binance + EUR quote", tr("binance", cexXLM, eur, 100_000_000, 1_000_000_000), nil, nil, ""},
		{"unknown source fails closed", tr("unregistered-venue", cexXLM, usd, 100_000_000, 1_000_000_000), nil, nil, ""},
		{"oracle-class source", tr("reflector-cex", cexXLM, usd, 100_000_000, 1_000_000_000), nil, nil, ""},
		{"aggregator-class source", tr("coingecko", cexXLM, usd, 100_000_000, 1_000_000_000), nil, nil, ""},
		{"off-chain zero quote_amount", tr("binance", cexXLM, usd, 100_000_000, 0), nil, nil, ""},
		{"on-chain zero quote_amount with spec", tr("soroswap", xlm, circle, 100_000_000, 0), withSAC, nil, ""},

		// FX fallback for a non-pegged quote. 5,000 AQUA x $0.001.
		{"FX fallback prices the quote", mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000), nil, aquaOnly, "5.00000000"},
		// A resolver call on every trade would double the insert hot-path cost.
		{"pegged quote wins before the resolver", mkClassicDEXTrade(t, "soroswap", xlm, circle, 70_000_000), plain, panicFXResolver{}, "7.00000000"},
		{"no resolver keeps NULL", mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000), nil, nil, ""},
		{"resolver miss does not fabricate", mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000), nil, stubFXResolver{prices: map[string]string{}}, ""},
		{"resolver error does not fabricate", mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000), nil, stubFXResolver{err: errors.New("postgres unreachable")}, ""},

		// XLM base anchor: 250 XLM at $0.12, quote a token with no USD market.
		{"XLM base anchor", tr("soroswap", xlm, sep41A, 2_500_000_000, 1), nil, xlmAt12, "30.00000000"},
		{"XLM SAC base anchor", tr("soroswap", xlmSAC, sep41A, 2_500_000_000, 1), nil, xlmAt12, "30.00000000"},
		// The two legs disagree 42x; a token rate is bridged through a
		// counterparty-writable continuous aggregate, so the XLM leg wins
		// ($5.00 would be the quote-side answer).
		{
			"XLM base leg beats a bridged quote price",
			mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000), nil,
			stubFXResolver{prices: map[string]string{aqua.String(): "0.001", xlm.String(): "0.12"}},
			"0.12000000",
		},
		// Legs are coherent (5M AQUA at $0.001 ~ 5,000 USDC) so the leg
		// cross-check does not pick the smaller one.
		{
			"non-XLM base still uses quote resolution",
			tr("soroswap", aqua, circle, 50_000_000_000_000, 50_000_000_000), nil,
			stubFXResolver{prices: map[string]string{circle.String(): "1.00", aqua.String(): "0.001"}},
			"5000.00000000",
		},
		{"neither leg XLM", tr("soroswap", sep41A, sep41B, 2_500_000_000, 1), nil, xlmAt12, ""},
		{"off-chain XLM base has no orientation to fix", tr("binance", cexXLM, sep41A, 2_500_000_000, 1), nil, xlmAt12, ""},

		// Tier 2b: a USD-pegged base leg carries its value in base_amount.
		{"USD-pegged base with no resolver", tr("sdex", circle, yxt, 2_505_000_000, 987_654_321), plain, nil, "250.50000000"},
		// FX would say $5000 off a wrong XLM rate; the exact base says $100.
		{
			"exact base beats the FX estimate", tr("sdex", circle, xlm, 1_000_000_000, 10_000_000_000), plain,
			stubFXResolver{prices: map[string]string{xlm.String(): "5"}},
			"100.00000000",
		},
		// Falling through to the XLM anchor breaks usd_volume = base_amount/10^decimals.
		{
			"zero USD-pegged base stays exact", tr("sdex", circle, xlm, 0, 1), plain,
			stubFXResolver{prices: map[string]string{xlm.String(): "0.16"}},
			"0.00000000",
		},
		// Distinct amounts make the leg used identifiable.
		{"pegged quote is not displaced by tier 2b", tr("sdex", circle, circle, 1_000_000_000, 2_000_000_000), plain, nil, "200.00000000"},

		// Tier 4b: unpriceable quote, priced base. 1000 x 0.005933704130547542.
		{
			"classic base anchor", tr("sdex", sixT, f8, 10_000_000_000, 123_456_789), plain,
			stubFXResolver{prices: map[string]string{sixT.String(): "0.005933704130547542"}},
			"5.93370413",
		},
		// A SEP-41 contract must never read as a USD peg: its decimals are not
		// an invariant, and 18 decimals at 1e7 would overstate by 1e11.
		{"unpriceable SEP-41 pair stays NULL", tr("soroswap", usdcSAC, usdcSAC, 10_000_000_000, 123_456_789), plain, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tradeUSDVolume(context.Background(), tc.tr, tc.spec, tc.fx)
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("got %q, want nil", *got)
			case tc.want != "" && got == nil:
				t.Errorf("got nil, want %q", tc.want)
			case tc.want != "" && *got != tc.want:
				t.Errorf("got %q, want %q", *got, tc.want)
			}
		})
	}
}

// One economic swap of 1 XLM for 5,000 AQUA, stored either way round, gets
// the same usd_volume even when the token leg's rate diverges 240x, and the
// insert path agrees with the xlm-quote re-derive.
func TestTradeUSDVolume_XLMLegIsOrientationSymmetric(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	xlmSAC := mustAsset(t, canonical.XLMSacContractID)
	aqua := mustAsset(t, aquaID)
	resolver := stubFXResolver{prices: map[string]string{xlm.String(): "0.12", aqua.String(): "0.0000001"}}
	const xlmStroops, aquaUnits = 10_000_000, 50_000_000_000
	const want = "0.12000000"

	cases := map[string]canonical.Trade{
		"XLM base":      volTrade(t, "sdex", xlm, aqua, xlmStroops, aquaUnits),
		"XLM quote":     volTrade(t, "sdex", aqua, xlm, aquaUnits, xlmStroops),
		"XLM SAC quote": volTrade(t, "sdex", aqua, xlmSAC, aquaUnits, xlmStroops),
		"XLM SAC base":  volTrade(t, "sdex", xlmSAC, aqua, xlmStroops, aquaUnits),
	}
	for name, tr := range cases {
		got := tradeUSDVolume(context.Background(), tr, nil, resolver)
		if got == nil || *got != want {
			t.Errorf("%s: usd_volume = %v, want %s (the XLM leg, whichever side holds it)", name, usdVolumeOrNil(got), want)
		}
		if !isXLMAsset(tr.Pair.Quote) {
			continue
		}
		restamp, err := tradeUSDVolumeViaXLMQuoteAnchorFor(context.Background(), tr, resolver)
		if err != nil {
			t.Fatalf("%s: xlm-quote re-derive: %v", name, err)
		}
		if restamp == nil || got == nil || *restamp != *got {
			t.Errorf("%s: insert wrote %v but the xlm-quote re-derive writes %v", name, usdVolumeOrNil(got), usdVolumeOrNil(restamp))
		}
	}
}

func usdVolumeOrNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestIsXLMAsset(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		asset canonical.Asset
		want  bool
	}{
		{"native", canonical.NativeAsset(), true},
		{"SAC wrapper of native", mustAsset(t, canonical.XLMSacContractID), true},
		{"unrelated Soroban contract", mustAsset(t, sep41AID), false},
		{"classic credit", mustAsset(t, circleKey), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isXLMAsset(tc.asset); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Operator-config typos must fail at startup rather than silently match no trade.
func TestNewUSDVolumeQuoteSpec_RejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		classicUSDPegs []string
		sacWrappers    map[string]string
	}{
		{"fiat in classic peg list", []string{"fiat:USD"}, nil},
		{"native in classic peg list", []string{"native"}, nil},
		{"soroban in classic peg list", []string{usdcSACID}, nil},
		{"unparseable string", []string{"definitely-not-an-asset"}, nil},
		{"sac_wrapper points at non-classic", nil, map[string]string{usdcSACID: "fiat:USD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewUSDVolumeQuoteSpec(tc.classicUSDPegs, tc.sacWrappers); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestUSDVolumeQuoteSpec_QuoteUSDPegInfo(t *testing.T) {
	t.Parallel()
	spec := circleSpec(t, map[string]string{usdcSACID: "USDC:" + circleIssuer})
	circle := mustAsset(t, circleKey)

	cases := []struct {
		name        string
		asset       canonical.Asset
		wantOK      bool
		wantDecimal int
	}{
		{"classic Circle USDC", circle, true, 7},
		{"SAC of Circle USDC (transitive)", mustAsset(t, usdcSACID), true, 7},
		{"different USDC issuer not in allow-list", mustAsset(t, "USDC-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"), false, 0},
		{"pure SEP-41 contract not in sac_wrappers", mustAsset(t, sep41AID), false, 0},
		{"native XLM is not USD-pegged", canonical.NativeAsset(), false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := spec.QuoteUSDPegInfo(tc.asset)
			if ok != tc.wantOK || d != tc.wantDecimal {
				t.Errorf("got (%d, %v), want (%d, %v)", d, ok, tc.wantDecimal, tc.wantOK)
			}
		})
	}

	// Without the wrapper the same contract is no peg: its decimals are not an invariant.
	if _, ok := circleSpec(t, nil).QuoteUSDPegInfo(mustAsset(t, usdcSACID)); ok {
		t.Error("a pure SEP-41 contract was accepted as a USD peg")
	}
	if _, ok := (*USDVolumeQuoteSpec)(nil).QuoteUSDPegInfo(circle); ok {
		t.Errorf("nil spec should always return ok=false")
	}
}

// WouldPopulateUSDVolume is the predicate the pipeline sink uses to label the
// trade-inserts coverage metric.
func TestStore_WouldPopulateUSDVolume(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	circle := mustAsset(t, circleKey)
	withSpec := &Store{}
	withSpec.SetUSDVolumeQuoteSpec(circleSpec(t, nil))

	cases := []struct {
		name  string
		store *Store
		trade canonical.Trade
		want  bool
	}{
		{"off-chain CEX + USD-pegged", &Store{}, volTrade(t, "kraken", mustAsset(t, "crypto:XLM"), mustAsset(t, "crypto:USDC"), 100_000_000, 4_250_000_000), true},
		{"on-chain DEX with no spec", &Store{}, volTrade(t, "soroswap", xlm, circle, 100_000_000, 1_000_000_000), false},
		{"on-chain DEX with spec recognising the quote", withSpec, volTrade(t, "soroswap", xlm, circle, 100_000_000, 10_000_000), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.store.WouldPopulateUSDVolume(context.Background(), tc.trade); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// baseAnchorEligible admits every on-chain form, including pure SEP-41
// (its decimals cancel in the raw-VWAP product); off-chain shapes are declined.
func TestBaseAnchorEligible(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		asset canonical.Asset
		want  bool
	}{
		"native XLM":  {canonical.NativeAsset(), true},
		"XLM SAC":     {canonical.Asset{Type: canonical.AssetSoroban, ContractID: canonical.XLMSacContractID}, true},
		"classic":     {mustAsset(t, "6T-GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW"), true},
		"pure SEP-41": {canonical.Asset{Type: canonical.AssetSoroban, ContractID: usdcSACID}, true},
		"fiat":        {canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}, false},
	}
	for name, tc := range cases {
		if got := baseAnchorEligible(tc.asset); got != tc.want {
			t.Errorf("baseAnchorEligible(%s) = %t, want %t", name, got, tc.want)
		}
	}
}

// The VWAP-anchored tiers are decimals-independent: prices_1m stores vwap as
// a RAW ratio X/A for A raw token units, so usd = (A/1e7) x (X/A) x usdPerXLM
// = (X/1e7) x usdPerXLM and the token's scale cancels. Pricing one economic
// trade through tokens of 6, 7, 9 and 18 decimals must give an identical
// usd_volume, which guards against replacing the 1e7 divisor with a
// per-token decimals lookup.
func TestUSDVolumeIsIndependentOfTokenDecimals(t *testing.T) {
	t.Parallel()
	spec := circleSpec(t, nil)
	const xlmUSD = "0.18437992688007971271"
	xlmRaw := big.NewInt(250 * 10_000_000)
	token := canonical.Asset{Type: canonical.AssetSoroban, ContractID: "CCT4ZYIYZ3TUO2AWQFEOFGBZ6HQP3GW5TA37CK7CRZVFRDXYTHTYX7KP"}
	other := canonical.Asset{Type: canonical.AssetSoroban, ContractID: "CAUP7NFABXE5TJRL3FKTPMWRLC7IAXYDCTHQRFSCLR5TMGKHOOQO772J"}

	var want string
	for _, decimals := range []int{6, 7, 9, 18} {
		tokenRaw := new(big.Int).Mul(big.NewInt(1000), scaleDenominator(decimals))
		rate, ok := bridgeRate(new(big.Rat).SetFrac(xlmRaw, tokenRaw), xlmUSD)
		if !ok {
			t.Fatalf("decimals=%d: bridgeRate declined", decimals)
		}
		tr := volTrade(t, "aquarius", token, other, 0, 123_456_789)
		tr.BaseAmount = canonical.NewAmount(tokenRaw)
		// Only the base token is priceable, which forces the base anchor.
		fx := stubFXResolver{prices: map[string]string{token.String(): rate}}

		got := tradeUSDVolume(context.Background(), tr, spec, fx)
		if got == nil {
			t.Fatalf("decimals=%d: pure SEP-41 base left unpriced", decimals)
		}
		if want == "" {
			want = *got
			continue
		}
		if *got != want {
			t.Errorf("decimals=%d gave usd_volume %s, but decimals=6 gave %s; the token's scale must cancel", decimals, *got, want)
		}
	}
	const trueValue = "46.09498172"
	if want != trueValue {
		t.Errorf("usd_volume = %s, want %s (250 XLM x %s)", want, trueValue, xlmUSD)
	}
}

// Fake-XMR regression: an attacker planted an INDUSX/XLM bridge rate for the
// cost of the dust floor, and two trades with no XLM leg were stamped ~$91M
// each. When both legs resolve and disagree beyond usdLegAgreementFactor the
// FX tier stores the smaller leg's value; within tolerance the quote-side
// value stays authoritative.
func TestTradeUSDVolumeViaFX_LegCrossCheck(t *testing.T) {
	base := mustAsset(t, "XMR-GDGE5SNCNHIP7HG3EHZWBC6NU3TWYODNXDMGC5CCKNJW555VCEAHPORT")
	quote := mustAsset(t, "INDUSX-GCBSLNSO4NWX3BUQ2K5HZYW4JV2CWJIBZ3KOY6L3WNNGEX6RE7PINDUS")
	tr := volTrade(t, "sdex", base, quote, 13_012_363_332_323, 2_922_474_116_360) // 1,301,236.3 and 292,247.4 units at 7dp
	md := external.Lookup("sdex")

	// Poisoned quote rate ($549.43) vs honest dust base rate: divergence ~10^10, base leg wins.
	poisoned := stubFXResolver{prices: map[string]string{quote.String(): "549.43", base.String(): "0.00000001"}}
	got := tradeUSDVolumeViaFX(context.Background(), tr, md, poisoned)
	if got == nil {
		t.Fatal("expected a value (the conservative leg), got nil")
	}
	v, _ := new(big.Rat).SetString(*got)
	if v.Cmp(big.NewRat(1, 1)) > 0 { // < $1, not $160M
		t.Fatalf("cross-check failed: stored %s, want the dust base-leg value (<$1)", *got)
	}

	// Agreeing legs ($156k vs $146k, within 10x): quote-side value stays authoritative.
	agreeing := stubFXResolver{prices: map[string]string{quote.String(): "0.50", base.String(): "0.12"}}
	got2 := tradeUSDVolumeViaFX(context.Background(), tr, md, agreeing)
	if got2 == nil {
		t.Fatal("agreeing legs: expected quote-side value, got nil")
	}
	want := new(big.Rat).Mul(big.NewRat(2_922_474_116_360, 10_000_000), big.NewRat(1, 2))
	v2, _ := new(big.Rat).SetString(*got2)
	if v2.Cmp(want) != 0 {
		t.Fatalf("agreeing legs: stored %s, want quote-side %s", *got2, want.FloatString(8))
	}

	// Base unresolvable: the cross-check cannot fire, so the single-leg print
	// is bounded. 292,247.4 x $549.43 ~ $160.5M is above singleLegMaxUSDVolume.
	single := stubFXResolver{prices: map[string]string{quote.String(): "549.43"}}
	if got3 := tradeUSDVolumeViaFX(context.Background(), tr, md, single); got3 != nil {
		t.Fatalf("single-leg above ceiling: want NULL (refused), got %q", *got3)
	}
}

// With the base leg unresolvable the cross-check cannot fire, so an inflated
// bridge rate on the quote token could stamp an arbitrary usd_volume. The
// single-leg print is bounded at singleLegMaxUSDVolume; a plausible print
// below the ceiling is still valued.
func TestTradeUSDVolumeViaFX_SingleLegBaseUnresolvableBound(t *testing.T) {
	base := mustAsset(t, sep41AID) // fresh token, never priced
	quote := mustAsset(t, usdcSACID)
	md := external.Lookup("soroswap")
	// 20,000 quote units at 1e7.
	tr := volTrade(t, "soroswap", base, quote, 1_000_000_000, 200_000_000_000)

	poisoned := stubFXResolver{prices: map[string]string{quote.String(): "50000"}} // $1,000,000,000
	if got := tradeUSDVolumeViaFX(context.Background(), tr, md, poisoned); got != nil {
		t.Fatalf("poisoned single-leg print above ceiling: want NULL (refused), got %q", *got)
	}

	honest := stubFXResolver{prices: map[string]string{quote.String(): "2.50"}} // $50,000
	got := tradeUSDVolumeViaFX(context.Background(), tr, md, honest)
	if got == nil {
		t.Fatal("legitimate below-ceiling single-leg print: want a value, got NULL")
	}
	if want := "50000.00000000"; *got != want {
		t.Fatalf("legitimate single-leg print: got %q, want %q", *got, want)
	}
}

// boundIssuer issues both test tokens. They are classic so
// [baseAnchorEligible] admits the base at the 1e7 scale.
const boundIssuer = "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW"

// boundAnchorTrade is base/TOKB on sdex. TOKB never has a rate in any
// resolver below unless a case adds one, which is what makes the quote
// tier decline and the anchor fire.
func boundAnchorTrade(t *testing.T, base canonical.Asset, baseStroops *big.Int) canonical.Trade {
	t.Helper()
	quote, err := canonical.NewClassicAsset("TOKB", boundIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset TOKB: %v", err)
	}
	return canonical.Trade{
		Source:      "sdex",
		Ledger:      63_890_200,
		TxHash:      "f044",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: base, Quote: quote},
		BaseAmount:  canonical.NewAmount(baseStroops),
		QuoteAmount: canonical.NewAmount(big.NewInt(1_000_000_000)),
	}
}

// boundTokenA is the bridge-writable base.
func boundTokenA(t *testing.T) canonical.Asset {
	t.Helper()
	a, err := canonical.NewClassicAsset("TOKA", boundIssuer)
	if err != nil {
		t.Fatalf("NewClassicAsset TOKA: %v", err)
	}
	return a
}

func boundQuoteSpec(t *testing.T) *USDVolumeQuoteSpec {
	t.Helper()
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	return spec
}

// TestTradeUSDVolume_BaseAnchorRefusesAnUncrossCheckablePrintAboveTheCeiling
// is the attack: 20,000 TOKEN_A (2e11 stroops) x a planted $50,000 rate
// is $1,000,000,000 resting on one attacker-authored leg. It must be
// refused (usd_volume NULL), exactly as the quote side refuses it.
func TestTradeUSDVolume_BaseAnchorRefusesAnUncrossCheckablePrintAboveTheCeiling(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	poisoned := stubFXResolver{prices: map[string]string{tokenA.String(): "50000"}}

	if got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), poisoned); got != nil {
		t.Fatalf("single-leg base-anchored print above singleLegMaxUSDVolume: want NULL (refused), got %q", *got)
	}
}

// TestTradeUSDVolume_BaseAnchorBoundLeavesPlausiblePrintsByteIdentical
// is the other half of a lossy guard: the refusal must fire ONLY above
// the ceiling. The token/token class this tier exists for (99.2% of the
// remaining unpriced trades) keeps its value to the digit,
// and the boundary itself is inclusive, as it is on the quote side
// (`> ceiling` refuses; `== ceiling` serves).
func TestTradeUSDVolume_BaseAnchorBoundLeavesPlausiblePrintsByteIdentical(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	cases := []struct {
		name string
		rate string
		want string
	}{
		{"ordinary", "2.50", "50000.00000000"},
		{"exactly at the ceiling", "5000", "100000000.00000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
			fx := stubFXResolver{prices: map[string]string{tokenA.String(): tc.rate}}
			got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx)
			if got == nil {
				t.Fatalf("plausible base-anchored print: want %s, got NULL", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("usd_volume = %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume_BaseAnchorRefusesJustAboveTheCeiling pins the edge
// from the other side: 20,000 x $5000.00000001 = $100,000,000.0002.
func TestTradeUSDVolume_BaseAnchorRefusesJustAboveTheCeiling(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	fx := stubFXResolver{prices: map[string]string{tokenA.String(): "5000.00000001"}}
	if got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx); got != nil {
		t.Fatalf("just above the ceiling: want NULL, got %q", *got)
	}
}

// TestTradeUSDVolume_XLMAnchorIsExemptFromTheBound arms the trigger on
// the one anchor that must NOT be bounded. XLM is the bridge's own
// anchor: its rate is a direct XLM/USD market nobody can author, and
// base_amount is XLM that actually moved, so the value is exact rather
// than estimated. A ceiling would NULL a real (if enormous) trade, and a
// cross-check would let a planted token rate drag an exact value DOWN —
// see [tradeUSDVolumeViaXLMQuoteAnchorFor]. 1e9 XLM x $0.50 = $500M.
func TestTradeUSDVolume_XLMAnchorIsExemptFromTheBound(t *testing.T) {
	t.Parallel()
	stroops, ok := new(big.Int).SetString("10000000000000000", 10) // 1e9 XLM
	if !ok {
		t.Fatal("bad stroop literal")
	}
	tr := boundAnchorTrade(t, canonical.NativeAsset(), stroops)
	// The quote token carries a planted rate that values the trade at
	// one cent — far more than 10x below the XLM leg. It must not win.
	fx := stubFXResolver{prices: map[string]string{
		canonical.NativeAsset().String(): "0.5",
		tr.Pair.Quote.String():           "0.0001",
	}}
	got := tradeUSDVolume(context.Background(), tr, boundQuoteSpec(t), fx)
	if got == nil {
		t.Fatal("XLM-anchored trade above the single-leg ceiling: want its exact value, got NULL")
	}
	if want := "500000000.00000000"; *got != want {
		t.Fatalf("usd_volume = %q, want %q", *got, want)
	}
}

// TestRestampAnchorInheritsTheBound pins the shared primitive. The
// restamp tiers reach the anchor through
// [tradeUSDVolumeViaXLMBaseAnchorFor] rather than the waterfall, so a
// bound applied at the waterfall's call sites would have left a backfill
// free to re-write the very rows the live path now refuses.
func TestRestampAnchorInheritsTheBound(t *testing.T) {
	t.Parallel()
	tokenA := boundTokenA(t)
	tr := boundAnchorTrade(t, tokenA, big.NewInt(200_000_000_000))
	poisoned := stubFXResolver{prices: map[string]string{tokenA.String(): "50000"}}

	got, err := tradeUSDVolumeViaXLMBaseAnchorFor(context.Background(), tr, poisoned)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("restamp entry point, above-ceiling non-XLM anchor: want NULL, got %q", *got)
	}
}
