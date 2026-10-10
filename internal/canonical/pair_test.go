package canonical_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestNewPair_validAndDirectional(t *testing.T) {
	xlm := c.NativeAsset()
	usdc := mustClassic("USDC", usdcIssuer)

	p, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Base.Equal(xlm) || !p.Quote.Equal(usdc) {
		t.Fatalf("got %+v", p)
	}
	if p.String() != "native/USDC-"+usdcIssuer {
		t.Fatalf("String() = %q", p.String())
	}

	flipped := p.Flip()
	if !flipped.Base.Equal(usdc) || !flipped.Quote.Equal(xlm) {
		t.Fatalf("Flip wrong: %+v", flipped)
	}
	if p.Equal(flipped) {
		t.Fatal("directional pairs should not be equal to their flip")
	}
	if !p.EqualEitherWay(flipped) {
		t.Fatal("EqualEitherWay should accept flipped pair")
	}
}

func TestEqualEitherWay_rejectsUnrelated(t *testing.T) {
	// Same-asset-on-both-sides pairs are invalid to construct, so the
	// degenerate "p ≟ p.Flip()" case never reaches EqualEitherWay at
	// runtime. What matters is that a pair with one common asset +
	// one different asset is NOT treated as equal-either-way, or
	// cross-venue normalization in the aggregator would silently
	// combine unrelated markets.
	xlm := c.NativeAsset()
	usdc := mustClassic("USDC", usdcIssuer)
	eurc := mustClassic("EURC", usdcIssuer)

	xlmUsdc := mustPair(xlm, usdc)
	xlmEurc := mustPair(xlm, eurc)
	if xlmUsdc.EqualEitherWay(xlmEurc) {
		t.Fatal("XLM/USDC and XLM/EURC share only the base — must not be equal-either-way")
	}
	if xlmUsdc.EqualEitherWay(xlmEurc.Flip()) {
		t.Fatal("XLM/USDC and EURC/XLM share no directed relationship")
	}
}

func TestNewPair_sameAsset(t *testing.T) {
	xlm := c.NativeAsset()
	_, err := c.NewPair(xlm, xlm)
	if err == nil {
		t.Fatal("expected error for base == quote")
	}
}

func TestParsePair_roundTrip(t *testing.T) {
	xlm := c.NativeAsset()
	usdc := mustClassic("USDC", usdcIssuer)
	xlmSACasset := mustSoroban(xlmSAC)

	cases := []c.Pair{
		mustPair(xlm, usdc),
		mustPair(usdc, xlm),
		mustPair(xlmSACasset, usdc),
	}
	for _, p := range cases {
		t.Run(p.String(), func(t *testing.T) {
			got, err := c.ParsePair(p.String())
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(p) {
				t.Fatalf("round-trip: got %+v, want %+v", got, p)
			}
		})
	}
}

func TestParsePair_bad(t *testing.T) {
	cases := []string{
		"",
		"/",
		"native/",
		"/native",
		"XLM/USD",       // neither side parses as an asset
		"native/native", // same-asset
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			_, err := c.ParsePair(s)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// A raw oracle symbol is the one asset form the grammar lets carry
// "/" (real RedStone feed_ids like "SolvBTC.BBN_FUNDAMENTAL/USD" do).
// It must still never be usable as a pair leg — Pair.Validate keys
// on IsMapped, not on the separator character — and ParsePair's
// right-to-left split must not mistake the embedded "/" for the pair
// separator and hand back a bogus split.
func TestParsePair_rawSymbolSlashNeverALeg(t *testing.T) {
	raw, err := c.NewOracleRawAsset("SolvBTC.BBN_FUNDAMENTAL/USD")
	if err != nil {
		t.Fatalf("NewOracleRawAsset: %v", err)
	}
	xlm := c.NativeAsset()

	if _, err := c.NewPair(raw, xlm); err == nil {
		t.Fatal("expected error: raw oracle symbol must never be a pair leg")
	}

	s := raw.String() + "/native" // "raw:SolvBTC.BBN_FUNDAMENTAL/USD/native"
	if _, err := c.ParsePair(s); err == nil {
		t.Fatalf("ParsePair(%q): expected error, raw symbol is record-layer only", s)
	}
}

func TestPair_JSON(t *testing.T) {
	p := mustPair(c.NativeAsset(), mustClassic("USDC", usdcIssuer))
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}

	// Object form on the wire: {"base":"native","quote":"USDC-G..."}
	var got c.Pair
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(p) {
		t.Fatalf("round-trip: got %+v, want %+v (json %s)", got, p, b)
	}

	// String form on unmarshal also accepted
	str := []byte(`"native/USDC-` + usdcIssuer + `"`)
	var fromStr c.Pair
	if err := json.Unmarshal(str, &fromStr); err != nil {
		t.Fatalf("string-form unmarshal: %v", err)
	}
	if !fromStr.Equal(p) {
		t.Fatalf("string-form: got %+v, want %+v", fromStr, p)
	}
}

func TestPair_JSON_rejectsInvalid(t *testing.T) {
	// Object form passes through each Asset.UnmarshalJSON (which
	// validates), then the outer Pair.Validate() catches same-asset
	// and zero-value cases. String form routes through ParsePair
	// → NewPair, which also validates.
	cases := map[string]string{
		"object same-asset": `{"base":"native","quote":"native"}`,
		"object zero base":  `{"base":"","quote":"native"}`,
		"object zero quote": `{"base":"native","quote":""}`,
		"object bad base":   `{"base":"not-an-asset","quote":"native"}`,
		"string same-asset": `"native/native"`,
		"string empty":      `""`,
		"string malformed":  `"native-only"`,
		"non-string/object": `42`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var p c.Pair
			if err := json.Unmarshal([]byte(body), &p); err == nil {
				t.Errorf("expected error for %s input %q, got %+v", name, body, p)
			}
		})
	}
}

func mustPair(base, quote c.Asset) c.Pair {
	p, err := c.NewPair(base, quote)
	if err != nil {
		panic(err)
	}
	return p
}

// Pair.Validate has three branches (base error, quote error,
// identity collision) that the existing pair_test.go suite only
// hits transitively via NewPair. Direct tests pin each branch so
// a refactor that drops any of them surfaces here, not in a
// downstream caller's confusing stack trace.

func TestPair_Validate_zeroValueSurfacesBaseError(t *testing.T) {
	// Zero-value Pair: both Base and Quote are zero-value Asset.
	// Validate hits the Base branch first and fails there.
	var p c.Pair
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "base") {
		t.Errorf("error %q missing \"base\" fragment", err.Error())
	}
}

func TestPair_Validate_validBaseInvalidQuote(t *testing.T) {
	// Base is valid (native XLM); Quote is zero-value → "quote"-
	// tagged error.
	p := c.Pair{
		Base:  c.NativeAsset(),
		Quote: c.Asset{}, // zero-value, fails Validate
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "quote") {
		t.Errorf("error %q missing \"quote\" fragment", err.Error())
	}
}

func TestPair_Validate_identityCollision(t *testing.T) {
	// Both sides are valid native XLM — Validate must catch the
	// identity collision and return ErrPairMismatch.
	p := c.Pair{Base: c.NativeAsset(), Quote: c.NativeAsset()}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected validation error on identity pair, got nil")
	}
	if !errors.Is(err, c.ErrPairMismatch) {
		t.Errorf("error chain missing ErrPairMismatch: %v", err)
	}
}
