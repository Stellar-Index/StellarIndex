package timescale

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
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

// TestFXSnapFromRows_DirectAndInverse — the two USD-anchored
// orientations, with EXACT rational arithmetic asserted (ADR-0003:
// no float on the money path; 1.085 must be exactly 217/200, and the
// inverse exactly 200/217 — not a float reciprocal).
func TestFXSnapFromRows_DirectAndInverse(t *testing.T) {
	bucket := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	rows := map[string]fxSnapRow{
		"EUR": {Bucket: bucket, RateUSD: "1.085", Source: "massive"},
	}

	t.Run("EUR/USD = rate_usd(EUR) exactly", func(t *testing.T) {
		price, obs, src, err := fxSnapFromRows(mkFiatPair(t, "EUR", "USD"), rows)
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

	t.Run("USD/EUR = exact Rat inverse, not float inverse_usd", func(t *testing.T) {
		price, _, _, err := fxSnapFromRows(mkFiatPair(t, "USD", "EUR"), rows)
		if err != nil {
			t.Fatalf("fxSnapFromRows: %v", err)
		}
		want := big.NewRat(200, 217) // 1/1.085 exact
		if price.Cmp(want) != 0 {
			t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
		}
	})
}

// TestFXSnapFromRows_Cross — non-USD cross leg chains through the USD
// anchor: price(EUR/GBP) = rate_usd(EUR)/rate_usd(GBP), exact.
// observedAt is the OLDER of the two buckets (the staler input governs
// freshness); the source label is the sorted "+"-join of distinct
// providers.
func TestFXSnapFromRows_Cross(t *testing.T) {
	newer := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	older := newer.Add(-24 * time.Hour)
	rows := map[string]fxSnapRow{
		"EUR": {Bucket: newer, RateUSD: "1.085", Source: "massive"},
		"GBP": {Bucket: older, RateUSD: "1.25", Source: "polygon-forex"},
	}
	price, obs, src, err := fxSnapFromRows(mkFiatPair(t, "EUR", "GBP"), rows)
	if err != nil {
		t.Fatalf("fxSnapFromRows: %v", err)
	}
	want := big.NewRat(217, 250) // 1.085/1.25 = 0.868 exact
	if price.Cmp(want) != 0 {
		t.Errorf("price = %s, want %s", price.RatString(), want.RatString())
	}
	if !obs.Equal(older) {
		t.Errorf("observedAt = %v, want the older bucket %v", obs, older)
	}
	if src != "massive+polygon-forex" {
		t.Errorf("source = %q, want massive+polygon-forex", src)
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

// TestFXSnapFromRows_InvalidRate — a malformed or non-positive
// rate_usd is a hard error (NOT ErrNoFXQuote): silently falling back
// would mask a corrupt row, and the caller must skip publish rather
// than trust any chained-fiat output this tick. The CHECK constraint
// makes this unreachable in practice; the guard is defensive.
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
