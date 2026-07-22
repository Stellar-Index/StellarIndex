package timescale

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestQuoteIsUSDOrUSDPegged covers the predicate's three branches:
// fiat:USD literal, USD-pegged stablecoin via FiatProxy, and the
// fall-through.
func TestQuoteIsUSDOrUSDPegged(t *testing.T) {
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	usdc, _ := canonical.NewCryptoAsset("USDC")
	usdt, _ := canonical.NewCryptoAsset("USDT")
	eurc, _ := canonical.NewCryptoAsset("EURC")
	xlm, _ := canonical.NewCryptoAsset("XLM")

	cases := []struct {
		name string
		a    canonical.Asset
		want bool
	}{
		{"fiat:USD literal", usd, true},
		{"crypto:USDC pegged → USD", usdc, true},
		{"crypto:USDT pegged → USD", usdt, true},
		{"fiat:EUR not USD-pegged", eur, false},
		{"crypto:EURC pegs to EUR not USD", eurc, false},
		{"crypto:XLM no peg", xlm, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteIsUSDOrUSDPegged(tc.a); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume_PopulatedForExternalUSDPaths exercises the
// off-chain happy paths: CEX/FX source + USD-or-pegged quote
// → numeric string at the uniform 10^8 external scale.
func TestTradeUSDVolume_PopulatedForExternalUSDPaths(t *testing.T) {
	usd, _ := canonical.NewFiatAsset("USD")
	usdc, _ := canonical.NewCryptoAsset("USDC")
	xlm, _ := canonical.NewCryptoAsset("XLM")

	mkTrade := func(source string, quote canonical.Asset, quoteAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(xlm, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
		}
	}

	cases := []struct {
		name   string
		source string
		quote  canonical.Asset
		amt    int64
		want   string // expected NUMERIC string
	}{
		{
			name:   "binance + fiat:USD → 1e8 → $1.00",
			source: "binance",
			quote:  usd,
			amt:    100_000_000,
			want:   "1.00000000",
		},
		{
			name:   "polygon-forex + fiat:USD → $0.005",
			source: "polygon-forex",
			quote:  usd,
			amt:    500_000,
			want:   "0.00500000",
		},
		{
			name:   "kraken + crypto:USDC peg → $42.50",
			source: "kraken",
			quote:  usdc,
			amt:    4_250_000_000,
			want:   "42.50000000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Off-chain path doesn't consult the spec — pass nil.
			got := tradeUSDVolume(context.Background(), mkTrade(tc.source, tc.quote, tc.amt), nil, nil)
			if got == nil {
				t.Fatalf("got nil, want %q", tc.want)
			}
			if *got != tc.want {
				t.Errorf("got %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume_PopulatedForOnChainDEX exercises the phase-1
// on-chain path: DEX source + operator-declared USD-pegged quote
// → numeric string at the Stellar classic 10^7 scale.
func TestTradeUSDVolume_PopulatedForOnChainDEX(t *testing.T) {
	xlm := canonical.NativeAsset()

	circleUSDC, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	// USDC's SAC contract — an example C-strkey Soroswap would
	// stamp on a Soroswap XLM/USDC trade.
	usdcSAC, err := canonical.NewSorobanAsset("CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if err != nil {
		t.Fatalf("NewSorobanAsset USDC SAC: %v", err)
	}

	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		map[string]string{
			"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		},
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	mkTrade := func(source string, base, quote canonical.Asset, quoteAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000)), // 1.0 XLM at 10^7
			QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
		}
	}

	cases := []struct {
		name   string
		source string
		base   canonical.Asset
		quote  canonical.Asset
		amt    int64
		want   string
	}{
		{
			name:   "sdex XLM/classic-USDC at 10^7 → $1.00",
			source: "sdex",
			base:   xlm,
			quote:  circleUSDC,
			amt:    10_000_000,
			want:   "1.00000000",
		},
		{
			name:   "soroswap XLM/USDC-SAC at 10^7 → $0.42",
			source: "soroswap",
			base:   xlm,
			quote:  usdcSAC,
			amt:    4_200_000,
			want:   "0.42000000",
		},
		{
			name:   "phoenix XLM/USDC-SAC big number → $1234.56",
			source: "phoenix",
			base:   xlm,
			quote:  usdcSAC,
			amt:    12_345_600_000,
			want:   "1234.56000000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tradeUSDVolume(context.Background(), mkTrade(tc.source, tc.base, tc.quote, tc.amt), spec, nil)
			if got == nil {
				t.Fatalf("got nil, want %q", tc.want)
			}
			if *got != tc.want {
				t.Errorf("got %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestTradeUSDVolume_NilForOutOfScope nails down what does NOT
// produce a usd_volume — the column stays NULL for these so callers
// don't get a misleadingly-precise figure.
func TestTradeUSDVolume_NilForOutOfScope(t *testing.T) {
	usd, _ := canonical.NewFiatAsset("USD")
	eur, _ := canonical.NewFiatAsset("EUR")
	xlm, _ := canonical.NewCryptoAsset("XLM")
	xlmNative := canonical.NativeAsset()
	circleUSDC, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	// A different (operator-untrusted) issuer of USDC. Real
	// validated G-strkey, just not Circle's.
	unknownClassicUSDC, err := canonical.NewClassicAsset("USDC", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset unknown USDC: %v", err)
	}
	pureSEP41, _ := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")

	specWithCircle, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		map[string]string{},
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	mkTrade := func(source string, base, quote canonical.Asset, quoteAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
		}
	}

	cases := []struct {
		name   string
		source string
		base   canonical.Asset
		quote  canonical.Asset
		amt    int64
		spec   *USDVolumeQuoteSpec
	}{
		{"on-chain DEX with no spec installed (default)", "soroswap", xlmNative, circleUSDC, 1_000_000_000, nil},
		{"on-chain DEX + classic-USDC NOT in operator allow-list", "soroswap", xlmNative, unknownClassicUSDC, 1_000_000_000, specWithCircle},
		{"on-chain DEX + pure SEP-41 (no classic counterpart)", "soroswap", xlmNative, pureSEP41, 1_000_000_000, specWithCircle},
		{"binance + EUR quote — not USD-pegged", "binance", xlm, eur, 1_000_000_000, nil},
		{"unknown source — fail-closed", "unregistered-venue", xlm, usd, 1_000_000_000, nil},
		{"oracle-class source (reflector)", "reflector-cex", xlm, usd, 1_000_000_000, nil},
		{"aggregator-class source (coingecko)", "coingecko", xlm, usd, 1_000_000_000, nil},
		{"zero quote_amount", "binance", xlm, usd, 0, nil},
		{"on-chain zero quote_amount even with spec", "soroswap", xlmNative, circleUSDC, 0, specWithCircle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tradeUSDVolume(context.Background(), mkTrade(tc.source, tc.base, tc.quote, tc.amt), tc.spec, nil)
			if got != nil {
				t.Errorf("got %q, want nil", *got)
			}
		})
	}
}

// mkClassicDEXTrade is a tiny helper for the Phase 2 tests that
// don't already have a closure-scoped mkTrade. Returns a XLM/quote
// trade at the soroswap source with the supplied quote-amount.
func mkClassicDEXTrade(t *testing.T, source string, base, quote canonical.Asset, quoteAmt int64) canonical.Trade {
	t.Helper()
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source:      source,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
	}
}

// stubFXResolver implements USDVolumeFXResolver for tests. The
// price map is keyed by canonical asset string and consulted on
// every USDPriceAt call; stale time-checks are the resolver's
// concern, not the test's, so we always return ok=true when the
// asset is present.
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

// TestTradeUSDVolume_Phase2FXFallback exercises the L2.2 Phase 2
// path: on-chain DEX trade quoted in a non-USD-pegged asset, FX
// resolver returns a price, tradeUSDVolume multiplies through.
//
// Pinned at scale 7 (Stellar classic) regardless of the quote
// asset's actual decimals — the L2.2 design treats every classic +
// SAC quote as a 7-decimal Stellar invariant; pure SEP-41 with
// non-7 decimals is L7 future-scope.
func TestTradeUSDVolume_Phase2FXFallback(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset AQUA: %v", err)
	}

	// XLM/AQUA trade: 100 XLM (1_000_000_000 stroops base) for
	// 5_000 AQUA (50_000_000_000 stroops quote). Operator's FX
	// resolver knows AQUA = $0.001 → $5.00 USD volume.
	resolver := stubFXResolver{prices: map[string]string{
		aqua.String(): "0.001",
	}}

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000),
		nil, // no Phase 1 spec
		resolver,
	)
	if got == nil {
		t.Fatal("Phase 2 fallback returned nil; want non-nil")
	}
	// 50_000_000_000 stroops / 10^7 = 5_000 AQUA × $0.001 = $5.00.
	want := "5.00000000"
	if *got != want {
		t.Errorf("got %q, want %q", *got, want)
	}
}

// TestTradeUSDVolume_Phase1WinsBeforePhase2 — when Phase 1 matches
// (USD-pegged classic in spec), the FX resolver MUST NOT be
// consulted. Pinned because a regression that calls the resolver
// every trade would double the trade-insert hot-path cost.
func TestTradeUSDVolume_Phase1WinsBeforePhase2(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	spec, err := NewUSDVolumeQuoteSpec([]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"}, nil)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	// Resolver is set up to PANIC if called — proves Phase 1
	// short-circuits before the resolver runs.
	resolver := panicFXResolver{}

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, usdc, 70_000_000),
		spec,
		resolver,
	)
	if got == nil {
		t.Fatal("Phase 1 returned nil; expected USD-pegged classic to populate")
	}
	// 70_000_000 stroops / 10^7 = $7.00.
	if *got != "7.00000000" {
		t.Errorf("got %q, want 7.00000000", *got)
	}
}

// TestTradeUSDVolume_Phase2_NoResolver — when fxResolver is nil
// (the default for tests + ops binary + every deployment that
// hasn't enabled Phase 2), the Phase 1 fall-through stays NULL.
// This is the L2.2 "preserves existing behaviour exactly" guarantee.
func TestTradeUSDVolume_Phase2_NoResolver(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, _ := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000),
		nil, // no Phase 1
		nil, // no Phase 2
	)
	if got != nil {
		t.Errorf("got %q, want nil (no resolver should preserve Phase 1 NULL fall-through)", *got)
	}
}

// TestTradeUSDVolume_Phase2_ResolverNoHit — resolver wired but
// doesn't have a rate for this asset (asset isn't on the
// operator's covered set, or its cache hasn't warmed up). Stays
// NULL — never silently fabricates a rate.
func TestTradeUSDVolume_Phase2_ResolverNoHit(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, _ := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")

	resolver := stubFXResolver{prices: map[string]string{}} // empty

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000),
		nil, resolver,
	)
	if got != nil {
		t.Errorf("got %q, want nil (resolver miss must not fabricate USD volume)", *got)
	}
}

// TestTradeUSDVolume_Phase2_ResolverError — resolver errors fall
// through to NULL silently. Best-effort posture matches Phase 1's
// "trade still inserts; just no USD column".
func TestTradeUSDVolume_Phase2_ResolverError(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, _ := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")

	resolver := stubFXResolver{err: errors.New("postgres unreachable")}

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000),
		nil, resolver,
	)
	if got != nil {
		t.Errorf("got %q, want nil (resolver error must not fabricate USD volume)", *got)
	}
}

// TestTradeUSDVolume_L76XLMBaseAnchor covers ROADMAP #37 / L7.6:
// a pure-Soroban SEP-41 token traded against XLM where the pool
// stores base=XLM, quote=TOKEN — the orientation tier 3 (quote-side
// FX resolution) can't cover, since TOKEN has no direct USD-pegged
// market. Tier 4 anchors off the XLM base leg instead.
func TestTradeUSDVolume_L76XLMBaseAnchor(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	xlmSAC, err := canonical.NewSorobanAsset(nativeXLMSAC)
	if err != nil {
		t.Fatalf("NewSorobanAsset(nativeXLMSAC): %v", err)
	}
	pureSEP41, err := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("NewSorobanAsset pureSEP41: %v", err)
	}

	// XLM/USD anchor: $0.12 per XLM (matches the resolver's native
	// asset key — same "native" identifier tier 3 uses when XLM
	// is the trade's quote).
	resolver := stubFXResolver{prices: map[string]string{
		xlm.String(): "0.12",
	}}

	mkTrade := func(source string, base, quote canonical.Asset, baseAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
			QuoteAmount: canonical.NewAmount(big.NewInt(1)), // pure SEP-41 amount; irrelevant to the anchor math
		}
	}

	// 250 XLM (2_500_000_000 stroops) at $0.12/XLM = $30.00.
	got := tradeUSDVolume(
		context.Background(),
		mkTrade("soroswap", xlm, pureSEP41, 2_500_000_000),
		nil, // no Phase 1 spec
		resolver,
	)
	if got == nil {
		t.Fatal("L7.6 XLM-base anchor returned nil; want non-nil")
	}
	want := "30.00000000"
	if *got != want {
		t.Errorf("got %q, want %q", *got, want)
	}

	// Same math when the pool holds XLM via its SAC wrapper instead
	// of the plain `native` form.
	gotSAC := tradeUSDVolume(
		context.Background(),
		mkTrade("soroswap", xlmSAC, pureSEP41, 2_500_000_000),
		nil,
		resolver,
	)
	if gotSAC == nil {
		t.Fatal("L7.6 XLM-base anchor (SAC form) returned nil; want non-nil")
	}
	if *gotSAC != want {
		t.Errorf("got %q, want %q", *gotSAC, want)
	}
}

// TestTradeUSDVolume_L76DoesNotFireWhenQuoteAlreadyResolved proves
// tier 4 is a fallback, not a double-count: when the quote asset
// itself resolves (tier 3), the XLM-base anchor is never reached —
// pinned with a panicking resolver call-count guard would be
// redundant with TestTradeUSDVolume_Phase1WinsBeforePhase2's style,
// so instead this asserts the tier-3 numeric result wins even
// though the trade's base asset would also satisfy tier 4's
// isXLMAsset check.
func TestTradeUSDVolume_L76DoesNotFireWhenQuoteAlreadyResolved(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("NewClassicAsset AQUA: %v", err)
	}

	// Resolver knows AQUA's USD rate (tier 3 hit) AND XLM's (tier 4
	// would also hit if reached) — if tier 4 fired instead of tier 3
	// the base-anchored math (below) would produce a different
	// number, so a mismatch proves the wrong tier ran.
	resolver := stubFXResolver{prices: map[string]string{
		aqua.String(): "0.001",
		xlm.String():  "0.12",
	}}

	got := tradeUSDVolume(
		context.Background(),
		mkClassicDEXTrade(t, "soroswap", xlm, aqua, 50_000_000_000),
		nil,
		resolver,
	)
	if got == nil {
		t.Fatal("expected tier 3 to populate usd_volume")
	}
	// Tier 3 (quote=AQUA): 50_000_000_000 stroops / 1e7 = 5_000 AQUA
	// × $0.001 = $5.00. Tier 4 (base=XLM) would instead compute
	// 10_000_000 stroops (mkClassicDEXTrade's fixed base) / 1e7 =
	// 1.0 XLM × $0.12 = $0.12 — a different, wrong number.
	want := "5.00000000"
	if *got != want {
		t.Errorf("got %q, want %q (tier 3 must win over tier 4)", *got, want)
	}
}

// TestTradeUSDVolume_L76OutOfScope pins the cases tier 4 must NOT
// cover.
func TestTradeUSDVolume_L76OutOfScope(t *testing.T) {
	t.Parallel()
	xlm := canonical.NativeAsset()
	pureSEP41A, _ := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	pureSEP41B, _ := canonical.NewSorobanAsset("CA6GAFOJCW4MGQQBUCQUSA3CLIH25G4SNKB2JHYKZCVWZTNW5VXMSC4O")
	binanceXLM, _ := canonical.NewCryptoAsset("XLM")

	resolver := stubFXResolver{prices: map[string]string{
		xlm.String(): "0.12",
	}}

	mkTrade := func(source string, base, quote canonical.Asset, baseAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
			QuoteAmount: canonical.NewAmount(big.NewInt(1)),
		}
	}

	cases := []struct {
		name   string
		source string
		base   canonical.Asset
		quote  canonical.Asset
	}{
		{"pure SEP-41/SEP-41 — neither leg is XLM", "soroswap", pureSEP41A, pureSEP41B},
		{"off-chain source with XLM base — no orientation problem to fix", "binance", binanceXLM, pureSEP41A},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tradeUSDVolume(context.Background(), mkTrade(tc.source, tc.base, tc.quote, 2_500_000_000), nil, resolver)
			if got != nil {
				t.Errorf("got %q, want nil", *got)
			}
		})
	}
}

// TestIsXLMAsset pins the two recognised on-chain wire forms of XLM
// plus a representative negative.
func TestIsXLMAsset(t *testing.T) {
	t.Parallel()
	xlmSAC, err := canonical.NewSorobanAsset(nativeXLMSAC)
	if err != nil {
		t.Fatalf("NewSorobanAsset(nativeXLMSAC): %v", err)
	}
	otherSAC, err := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	if err != nil {
		t.Fatalf("NewSorobanAsset otherSAC: %v", err)
	}
	classicUSDC, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	cases := []struct {
		name string
		a    canonical.Asset
		want bool
	}{
		{"native", canonical.NativeAsset(), true},
		{"SAC wrapper of native", xlmSAC, true},
		{"unrelated Soroban contract", otherSAC, false},
		{"classic credit", classicUSDC, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isXLMAsset(tc.a); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// panicFXResolver fails the test loudly if its USDPriceAt is
// called. Proves Phase 1 short-circuits before Phase 2 runs.
type panicFXResolver struct{}

func (panicFXResolver) USDPriceAt(_ context.Context, _ canonical.Asset, _ time.Time) (string, bool, error) {
	panic("Phase 2 resolver should not be consulted when Phase 1 matches")
}

// TestNewUSDVolumeQuoteSpec_RejectsBadInput — typos in operator
// config fail at startup rather than silently match no trade.
func TestNewUSDVolumeQuoteSpec_RejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		classicUSDPegs []string
		sacWrappers    map[string]string
	}{
		{"non-classic in classic peg list (fiat)", []string{"fiat:USD"}, nil},
		{"non-classic in classic peg list (native)", []string{"native"}, nil},
		{"non-classic in classic peg list (soroban)", []string{"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"}, nil},
		{"unparseable string", []string{"definitely-not-an-asset"}, nil},
		{"sac_wrapper points at non-classic", nil, map[string]string{"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "fiat:USD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewUSDVolumeQuoteSpec(tc.classicUSDPegs, tc.sacWrappers)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// TestUSDVolumeQuoteSpec_QuoteUSDPegInfo — direct unit coverage of
// the lookup primitive, both happy and not-found paths.
func TestUSDVolumeQuoteSpec_QuoteUSDPegInfo(t *testing.T) {
	t.Parallel()
	circleUSDCKey := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	circleUSDCSAC := "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

	spec, err := NewUSDVolumeQuoteSpec(
		[]string{circleUSDCKey},
		map[string]string{circleUSDCSAC: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	circleUSDC, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	sacAsset, _ := canonical.NewSorobanAsset(circleUSDCSAC)
	otherClassic, _ := canonical.NewClassicAsset("USDC", "GBADOTHERISSUER000000000000000000000000000000000000000000")
	unwrappedSEP41, _ := canonical.NewSorobanAsset("CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7")
	xlm := canonical.NativeAsset()

	cases := []struct {
		name        string
		asset       canonical.Asset
		wantOK      bool
		wantDecimal int
	}{
		{"classic Circle USDC → 7 decimals", circleUSDC, true, 7},
		{"SAC of Circle USDC (transitive) → 7 decimals", sacAsset, true, 7},
		{"different USDC issuer not in allow-list", otherClassic, false, 0},
		{"pure SEP-41 contract not in sac_wrappers", unwrappedSEP41, false, 0},
		{"native XLM is not USD-pegged", xlm, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := spec.QuoteUSDPegInfo(tc.asset)
			if ok != tc.wantOK || d != tc.wantDecimal {
				t.Errorf("got (%d, %v), want (%d, %v)", d, ok, tc.wantDecimal, tc.wantOK)
			}
		})
	}

	// nil receiver behaves as "no spec configured".
	if _, ok := (*USDVolumeQuoteSpec)(nil).QuoteUSDPegInfo(circleUSDC); ok {
		t.Errorf("nil spec should always return ok=false")
	}
}

// TestStore_WouldPopulateUSDVolume — predicate the pipeline sink
// uses to label the trade-inserts coverage metric. Wraps the same
// decision as tradeUSDVolume but exposed publicly so the sink
// doesn't need the unexported helper.
func TestStore_WouldPopulateUSDVolume(t *testing.T) {
	t.Parallel()
	usdc, _ := canonical.NewCryptoAsset("USDC")
	xlm, _ := canonical.NewCryptoAsset("XLM")
	circleUSDC, _ := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")

	mkTrade := func(source string, base, quote canonical.Asset, quoteAmt int64) canonical.Trade {
		pair, err := canonical.NewPair(base, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return canonical.Trade{
			Source:      source,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(100_000_000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
		}
	}

	cases := []struct {
		name     string
		store    *Store
		trade    canonical.Trade
		wantTrue bool
	}{
		{
			name:     "off-chain CEX + USD-pegged → populated",
			store:    &Store{},
			trade:    mkTrade("kraken", xlm, usdc, 4_250_000_000),
			wantTrue: true,
		},
		{
			name:     "on-chain DEX with no spec → not populated",
			store:    &Store{},
			trade:    mkTrade("soroswap", canonical.NativeAsset(), circleUSDC, 1_000_000_000),
			wantTrue: false,
		},
		{
			name: "on-chain DEX with spec that recognises the quote → populated",
			store: func() *Store {
				s := &Store{}
				spec, _ := NewUSDVolumeQuoteSpec(
					[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
					nil,
				)
				s.SetUSDVolumeQuoteSpec(spec)
				return s
			}(),
			trade:    mkTrade("soroswap", canonical.NativeAsset(), circleUSDC, 10_000_000),
			wantTrue: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.store.WouldPopulateUSDVolume(context.Background(), tc.trade)
			if got != tc.wantTrue {
				t.Errorf("got %v, want %v", got, tc.wantTrue)
			}
		})
	}
}

// ─── tier 2b: USD-pegged BASE leg (2026-07-22) ───────────────────────

// TestTradeUSDVolume_Tier2bUSDBase — a `USDC/TOKEN`-oriented on-chain
// market carries its USD value in base_amount. The waterfall used to
// inspect only the quote leg, so these fell through every tier and
// inserted NULL (43,277 on-chain trades on 2026-07-17 alone).
func TestTradeUSDVolume_Tier2bUSDBase(t *testing.T) {
	t.Parallel()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	yxt, err := canonical.NewClassicAsset("YxT", "GDQJXPESRTKBHMTLPHKZNVFGQA77HPCGYW2NPUORVMDMPJBCFGGBYNS2")
	if err != nil {
		t.Fatalf("NewClassicAsset YxT: %v", err)
	}
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	// 250.5 USDC (2_505_000_000 stroops) base for some amount of YxT.
	tr := canonical.Trade{
		Source:      "sdex",
		Ledger:      63514245,
		TxHash:      "abc",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: usdc, Quote: yxt},
		BaseAmount:  canonical.NewAmount(big.NewInt(2_505_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(987_654_321)),
	}

	// No FX resolver at all: tier 2b must stand on its own, since the
	// quote leg (YxT) has no USD market to resolve through.
	got := tradeUSDVolume(context.Background(), tr, spec, nil)
	if got == nil {
		t.Fatal("tier 2b did not fire — usd_volume would insert NULL")
	}
	if *got != "250.50000000" {
		t.Errorf("usd_volume = %q, want 250.50000000 (base_amount/1e7)", *got)
	}
}

// TestTradeUSDVolume_Tier2bPrefersExactBaseOverFXEstimate — when BOTH
// a USD-pegged base and a quote-side FX rate are available, the exact
// dollar amount wins. The FX route is an estimate through a VWAP; on
// 2026-07-17 the two agreed to 0.69% on average across 111,617 trades
// but diverged up to 134.92%, and the error is the VWAP's.
func TestTradeUSDVolume_Tier2bPrefersExactBaseOverFXEstimate(t *testing.T) {
	t.Parallel()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	xlm := canonical.NativeAsset()
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}

	// 100 USDC base; quote 1000 XLM. A deliberately wrong XLM rate
	// ($5) so the two routes cannot be confused: FX would say $5000,
	// the exact base says $100.
	tr := canonical.Trade{
		Source:      "sdex",
		Ledger:      63514245,
		TxHash:      "def",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: usdc, Quote: xlm},
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(10_000_000_000)),
	}
	fx := stubFXResolver{prices: map[string]string{xlm.String(): "5"}}

	got := tradeUSDVolume(context.Background(), tr, spec, fx)
	if got == nil {
		t.Fatal("expected a usd_volume")
	}
	if *got != "100.00000000" {
		t.Errorf("usd_volume = %q, want 100.00000000 (exact base), not the FX estimate", *got)
	}
}

// TestTradeUSDVolume_Tier2bDoesNotDisplaceQuoteSidePeg — when the
// QUOTE leg is pegged, tier 1/2 still owns the answer. Guards the
// ordering: tier 2b must not intercept trades the quote side handles.
func TestTradeUSDVolume_Tier2bDoesNotDisplaceQuoteSidePeg(t *testing.T) {
	t.Parallel()
	usdc, err := canonical.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset USDC: %v", err)
	}
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	// Both legs USDC (degenerate but the clearest ordering probe):
	// distinct amounts, so whichever leg was used is identifiable.
	tr := canonical.Trade{
		Source:      "sdex",
		Ledger:      63514245,
		TxHash:      "ghi",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: usdc, Quote: usdc},
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000_000)), // 100
		QuoteAmount: canonical.NewAmount(big.NewInt(2_000_000_000)), // 200
	}
	got := tradeUSDVolume(context.Background(), tr, spec, nil)
	if got == nil {
		t.Fatal("expected a usd_volume")
	}
	if *got != "200.00000000" {
		t.Errorf("usd_volume = %q, want 200.00000000 (quote leg) — tier 2b displaced tier 2", *got)
	}
}

// ─── tier 4b: widened base anchor (2026-07-22) ───────────────────────

// TestBaseAnchorEligible — the 1e7 divisor in the base anchor is only
// valid for assets known to sit at the Stellar classic scale. A pure
// SEP-41 token's decimals() is per-contract and is NOT plumbed through
// the trade-insert path, so admitting one here would silently
// mis-scale its value — a money bug, strictly worse than a NULL.
func TestBaseAnchorEligible(t *testing.T) {
	t.Parallel()
	classic, err := canonical.NewClassicAsset("6T", "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	cases := map[string]struct {
		asset canonical.Asset
		want  bool
	}{
		"native XLM":  {canonical.NativeAsset(), true},
		"XLM SAC":     {canonical.Asset{Type: canonical.AssetSoroban, ContractID: nativeXLMSAC}, true},
		"classic":     {classic, true},
		"pure SEP-41": {canonical.Asset{Type: canonical.AssetSoroban, ContractID: "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"}, false},
		"fiat":        {canonical.Asset{Type: canonical.AssetFiat, Code: "EUR"}, false},
	}
	for name, tc := range cases {
		if got := baseAnchorEligible(tc.asset); got != tc.want {
			t.Errorf("baseAnchorEligible(%s) = %t, want %t", name, got, tc.want)
		}
	}
}

// TestTradeUSDVolume_Tier4bClassicBaseAnchor — a token/token trade
// whose QUOTE leg has no resolvable price is valued off its BASE leg.
// This is the path that takes on-chain coverage from 87.5% to 99.2%
// of the remaining unpriced trades (measured 2026-07-22): for a 6T/F8
// trade where F8 has no usable market, 6T does.
func TestTradeUSDVolume_Tier4bClassicBaseAnchor(t *testing.T) {
	t.Parallel()
	sixT, err := canonical.NewClassicAsset("6T", "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW")
	if err != nil {
		t.Fatalf("NewClassicAsset 6T: %v", err)
	}
	f8, err := canonical.NewClassicAsset("F8", "GBGRBCUB6L7LH4JQ6EPDP7REH2DDACMCUQI76M3P6DM52QWU2Z5LIEVW")
	if err != nil {
		t.Fatalf("NewClassicAsset F8: %v", err)
	}
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	// 1000 6T base (1e10 stroops at 1e7). Resolver knows 6T (the real
	// bridged rate) but NOT F8 — the quote leg must decline and the
	// base leg must carry it.
	tr := canonical.Trade{
		Source:      "sdex",
		Ledger:      63514245,
		TxHash:      "jkl",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: sixT, Quote: f8},
		BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(123_456_789)),
	}
	fx := stubFXResolver{prices: map[string]string{sixT.String(): "0.005933704130547542"}}

	got := tradeUSDVolume(context.Background(), tr, spec, fx)
	if got == nil {
		t.Fatal("tier 4b did not fire — token/token trade would insert NULL")
	}
	// 1000 x 0.005933704130547542 = 5.933704130547542, to 8dp.
	if *got != "5.93370413" {
		t.Errorf("usd_volume = %q, want 5.93370413", *got)
	}
}

// TestTradeUSDVolume_Tier4bRejectsPureSEP41Base — the decimals
// boundary holds: a pure SEP-41 base is left NULL rather than valued
// at an assumed 1e7.
func TestTradeUSDVolume_Tier4bRejectsPureSEP41Base(t *testing.T) {
	t.Parallel()
	sep41 := canonical.Asset{Type: canonical.AssetSoroban, ContractID: "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"}
	other := canonical.Asset{Type: canonical.AssetSoroban, ContractID: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMB"}
	spec, err := NewUSDVolumeQuoteSpec(
		[]string{"USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		nil,
	)
	if err != nil {
		t.Fatalf("NewUSDVolumeQuoteSpec: %v", err)
	}
	tr := canonical.Trade{
		Source:      "soroswap",
		Ledger:      63514245,
		TxHash:      "mno",
		OpIndex:     0,
		Timestamp:   time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
		Pair:        canonical.Pair{Base: sep41, Quote: other},
		BaseAmount:  canonical.NewAmount(big.NewInt(10_000_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(123_456_789)),
	}
	// Even with a price available for the base, the scale is unknown.
	fx := stubFXResolver{prices: map[string]string{sep41.String(): "1.23"}}
	if got := tradeUSDVolume(context.Background(), tr, spec, fx); got != nil {
		t.Errorf("pure SEP-41 base was valued at %q; want NULL (decimals unknown)", *got)
	}
}
