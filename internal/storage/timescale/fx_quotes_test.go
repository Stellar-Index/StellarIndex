package timescale

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// mkFiatPair builds a fiat/fiat pair or fails the test.
func mkFiatPair(t *testing.T, base, quote string) canonical.Pair {
	t.Helper()
	b, err := canonical.NewFiatAsset(base)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", base, err)
	}
	q, err := canonical.NewFiatAsset(quote)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", quote, err)
	}
	p, err := canonical.NewPair(b, q)
	if err != nil {
		t.Fatalf("NewPair(%s/%s): %v", base, quote, err)
	}
	return p
}

// TestFXSnapFromRows_RateUSDOrientation pins the orientation of rate_usd, which the whole
// fiat-quoted read path hinges on: rate_usd(T) is UNITS-OF-T-PER-1-USD (usd→eur ≈ 0.92 means
// 1 USD buys 0.92 EUR), NOT "USD per 1 T".
//
// fxSnapFromRows must return quote-units-per-base-unit (Q per B), the same orientation as the
// trades fallback (quote_amount/base_amount):
//
//	price(B/Q) = (Q per USD) / (B per USD) = rate_usd(Q) / rate_usd(B)
//
// With rate_usd(EUR) = 0.8 (1 USD = 0.8 EUR):
//
//	price(USD/EUR) = EUR per USD = 0.8/1 = 4/5
//	price(EUR/USD) = USD per EUR = 1/0.8 = 5/4
//
// Returning rate_usd(B)/rate_usd(Q) inverts every leg; for JPY (rate_usd about 150) that is a
// ~150^2 error on every served XLM/JPY-style pair.
func TestFXSnapFromRows_RateUSDOrientation(t *testing.T) {
	bucket := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	rows := map[string]fxSnapRow{
		// 0.8 EUR per 1 USD (ground-truth orientation).
		"EUR": {Bucket: bucket, RateUSD: "0.8", Source: "massive"},
	}

	t.Run("USD/EUR = EUR per USD = rate_usd(EUR)", func(t *testing.T) {
		price, _, _, err := fxSnapFromRows(mkFiatPair(t, "USD", "EUR"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		want := big.NewRat(4, 5) // 0.8 exact
		if price.Cmp(want) != 0 {
			t.Errorf("price(USD/EUR) = %s, want %s (buggy inverted value is 5/4)",
				price.RatString(), want.RatString())
		}
	})

	t.Run("EUR/USD = USD per EUR = 1/rate_usd(EUR)", func(t *testing.T) {
		price, _, _, err := fxSnapFromRows(mkFiatPair(t, "EUR", "USD"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		want := big.NewRat(5, 4) // 1/0.8 exact
		if price.Cmp(want) != 0 {
			t.Errorf("price(EUR/USD) = %s, want %s (buggy inverted value is 4/5)",
				price.RatString(), want.RatString())
		}
	})

	t.Run("triangulated XLM/EUR uses the FX leg the right way up", func(t *testing.T) {
		// FX leg USD/EUR × XLM/USD, all in exact Rat.
		usdEUR, _, _, err := fxSnapFromRows(mkFiatPair(t, "USD", "EUR"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		xlmUSD := big.NewRat(12, 100) // 0.12 USD per XLM
		xlmEUR := new(big.Rat).Mul(xlmUSD, usdEUR)
		want := big.NewRat(96, 1000) // 0.096 EUR per XLM
		if xlmEUR.Cmp(want) != 0 {
			t.Errorf("XLM/EUR = %s, want %s (buggy path yields 0.15)",
				xlmEUR.RatString(), want.RatString())
		}
	})
}

// TestFXSnapTickers — which fx_quotes tickers a pair needs. USD is the
// rate_usd anchor (exact 1) so it never needs a row; non-fiat pairs
// can't be priced from fx_quotes at all.
func TestFXSnapTickers(t *testing.T) {
	xlm, err := canonical.NewCryptoAsset("XLM")
	if err != nil {
		t.Fatalf("NewCryptoAsset: %v", err)
	}
	usd, _ := canonical.NewFiatAsset("USD")
	cryptoPair, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair(XLM/USD): %v", err)
	}

	cases := []struct {
		name string
		pair canonical.Pair
		want []string
	}{
		{"EUR/USD needs EUR only", mkFiatPair(t, "EUR", "USD"), []string{"EUR"}},
		{"USD/EUR needs EUR only", mkFiatPair(t, "USD", "EUR"), []string{"EUR"}},
		{"EUR/GBP needs both", mkFiatPair(t, "EUR", "GBP"), []string{"EUR", "GBP"}},
		{"crypto leg is not an fx_quotes pair", cryptoPair, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fxSnapTickers(tc.pair)
			if len(got) != len(tc.want) {
				t.Fatalf("fxSnapTickers = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("fxSnapTickers[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestFXSnapFromRows_DirectAndInverse covers the two USD-anchored orientations with EXACT
// rational arithmetic (no float on the money path: an exact Rat inverse, not a float
// reciprocal). rate_usd(EUR)=1.085 means 1.085 EUR per USD, so:
//
//	price(USD/EUR) = EUR per USD = rate_usd(EUR)         = 1.085 = 217/200
//	price(EUR/USD) = USD per EUR = 1/rate_usd(EUR)       = 200/217
func TestFXSnapFromRows_DirectAndInverse(t *testing.T) {
	bucket := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	rows := map[string]fxSnapRow{
		"EUR": {Bucket: bucket, RateUSD: "1.085", Source: "massive"},
	}

	t.Run("USD/EUR = rate_usd(EUR) exactly", func(t *testing.T) {
		price, obs, src, err := fxSnapFromRows(mkFiatPair(t, "USD", "EUR"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		want := big.NewRat(217, 200) // 1.085 exact
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
		}
		if !obs.Equal(bucket) {
			t.Errorf("observedAt = %v, want %v", obs, bucket)
		}
		if src != "massive" {
			t.Errorf("source = %q, want massive", src)
		}
	})

	t.Run("EUR/USD = exact Rat inverse, not float inverse_usd", func(t *testing.T) {
		price, _, _, err := fxSnapFromRows(mkFiatPair(t, "EUR", "USD"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		want := big.NewRat(200, 217) // 1/1.085 exact
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
		}
	})
}

// TestFXSnapFromRows_Cross: a non-USD cross chains through the USD anchor, exactly:
// price(EUR/GBP) = GBP per EUR = rate_usd(GBP)/rate_usd(EUR). observedAt is the OLDER of the
// two buckets (the staler input governs freshness); the source label is the sorted "+"-join
// of distinct providers.
func TestFXSnapFromRows_Cross(t *testing.T) {
	newer := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	older := newer.Add(-24 * time.Hour)
	rows := map[string]fxSnapRow{
		"EUR": {Bucket: newer, RateUSD: "1.085", Source: "massive"},
		"GBP": {Bucket: older, RateUSD: "1.25", Source: "exchangeratesapi"},
	}
	price, obs, src, err := fxSnapFromRows(mkFiatPair(t, "EUR", "GBP"), rows)
	if err != nil {
		t.Fatalf("fxSnapFromRows: %v", err)
	}
	want := big.NewRat(250, 217) // 1.25/1.085 = 250/217 ≈ 1.152 exact
	if price.Cmp(want) != 0 {
		t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
	}
	if !obs.Equal(older) {
		t.Errorf("observedAt = %v, want the older bucket %v", obs, older)
	}
	// Sorted join: exchangeratesapi < massive alphabetically.
	if src != "exchangeratesapi+massive" {
		t.Errorf("source = %q, want exchangeratesapi+massive (sorted join)", src)
	}
}

// TestFXSnapFromRows_MissRoutesToFallback — a needed ticker with no
// row must return ErrNoFXQuote (the sentinel FXQuoteAtOrBefore uses to
// fire the legacy trades fallback), for both the missing-row and the
// not-an-fx-pair shapes.
func TestFXSnapFromRows_MissRoutesToFallback(t *testing.T) {
	t.Run("needed ticker absent", func(t *testing.T) {
		_, _, _, err := fxSnapFromRows(mkFiatPair(t, "USD", "MXN"), map[string]fxSnapRow{})
		if !errors.Is(err, ErrNoFXQuote) {
			t.Fatalf("err = %v, want ErrNoFXQuote", err)
		}
	})
	t.Run("one leg of a cross absent", func(t *testing.T) {
		rows := map[string]fxSnapRow{
			"EUR": {Bucket: time.Now().UTC(), RateUSD: "1.085", Source: "massive"},
		}
		_, _, _, err := fxSnapFromRows(mkFiatPair(t, "EUR", "GBP"), rows)
		if !errors.Is(err, ErrNoFXQuote) {
			t.Fatalf("err = %v, want ErrNoFXQuote", err)
		}
	})
}

// TestFXSnapFromRows_InvalidRate: a malformed or non-positive rate_usd is a hard error (NOT
// ErrNoFXQuote). Falling back silently would mask a corrupt row; the caller must skip publish
// rather than trust chained-fiat output. The CHECK constraint makes it unreachable in practice.
func TestFXSnapFromRows_InvalidRate(t *testing.T) {
	for _, bad := range []string{"", "not-a-number", "0", "-1.05"} {
		rows := map[string]fxSnapRow{
			"EUR": {Bucket: time.Now().UTC(), RateUSD: bad, Source: "massive"},
		}
		_, _, _, err := fxSnapFromRows(mkFiatPair(t, "EUR", "USD"), rows)
		if err == nil {
			t.Errorf("rate_usd=%q: expected error, got nil", bad)
			continue
		}
		if errors.Is(err, ErrNoFXQuote) {
			t.Errorf("rate_usd=%q: got ErrNoFXQuote, want a hard error", bad)
		}
	}
}

// TestFXSnapFromRows_LegacyNullSource — migration 0028 allows NULL
// source only for pre-attribution backfill rows; they surface under
// the fx_quotes feed's canonical label rather than an empty string.
func TestFXSnapFromRows_LegacyNullSource(t *testing.T) {
	rows := map[string]fxSnapRow{
		"EUR": {Bucket: time.Now().UTC(), RateUSD: "1.09", Source: ""},
	}
	_, _, src, err := fxSnapFromRows(mkFiatPair(t, "EUR", "USD"), rows)
	if err != nil {
		t.Fatalf("fxSnapFromRows: %v", err)
	}
	if src != fxQuotesSourceLabel {
		t.Errorf("source = %q, want %q", src, fxQuotesSourceLabel)
	}
}
