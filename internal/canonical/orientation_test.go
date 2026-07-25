package canonical

import "testing"

func TestOrient(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	const aqua = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AB"
	cases := []struct {
		name        string
		a, b        string
		wantBase    string
		wantQuote   string
		wantFlipped bool
	}{
		// XLM vs a stablecoin → stablecoin is the quote (price in USDC).
		{"xlm/usdc already canonical", "native", usdc, "native", usdc, false},
		{"usdc/xlm flips to xlm/usdc", usdc, "native", "native", usdc, true},
		// XLM vs fiat → fiat is the quote.
		{"xlm/usd", "native", "fiat:USD", "native", "fiat:USD", false},
		{"usd/xlm flips", "fiat:USD", "native", "native", "fiat:USD", true},
		// XLM vs a plain token → XLM is the quote (token priced in XLM).
		{"aqua/xlm already canonical", aqua, "native", aqua, "native", false},
		{"xlm/aqua flips to aqua/xlm", "native", aqua, aqua, "native", true},
		// fiat outranks stablecoin.
		{"usdc/usd → usdc base", usdc, "fiat:USD", usdc, "fiat:USD", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, quote, flipped := Orient(tc.a, tc.b)
			if base != tc.wantBase || quote != tc.wantQuote || flipped != tc.wantFlipped {
				t.Errorf("Orient(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.a, tc.b, base, quote, flipped, tc.wantBase, tc.wantQuote, tc.wantFlipped)
			}
		})
	}
}

// TestOrient_Symmetric ensures both input orders collapse to the same
// canonical market — the whole point.
func TestOrient_Symmetric(t *testing.T) {
	pairs := [][2]string{
		{"native", "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		{"native", "fiat:USD"},
		{"AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AB", "native"},
		{"crypto:BTC", "crypto:USDT"},
	}
	for _, p := range pairs {
		b1, q1, _ := Orient(p[0], p[1])
		b2, q2, _ := Orient(p[1], p[0])
		if b1 != b2 || q1 != q2 {
			t.Errorf("Orient not symmetric for %v: (%q,%q) vs (%q,%q)", p, b1, q1, b2, q2)
		}
	}
}

// TestOrient_StablecoinRankIsIssuerAgnostic is the SEC-B12
// characterization test. A classic token whose CODE collides with a
// real stablecoin ticker but whose ISSUER is an unrelated (possibly
// hostile) account still ranks as a quote — orientation deliberately
// does not know about issuers.
//
// This test exists to make that deliberate, not accidental: the
// behaviour is safe only because no path converts a rank-3 asset into
// a fiat value without re-checking the issuer (aggregate.FiatProxy
// refuses every classic asset, including Circle's genuine USDC — see
// TestFiatProxy_NonCryptoAssetsReturnFalse). Anyone changing this
// ranking, or adding a code-keyed USD substitution, has to reckon
// with that invariant first.
func TestOrient_StablecoinRankIsIssuerAgnostic(t *testing.T) {
	const scamUSDC = "USDC-GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF5"

	base, quote, _ := Orient("native", scamUSDC)
	if base != "native" || quote != scamUSDC {
		t.Errorf("Orient(native, %s) = (%s, %s), want (native, %s) — orientation ranks on CODE alone",
			scamUSDC, base, quote, scamUSDC)
	}

	// The genuine issuer orients identically: the two are
	// indistinguishable HERE, which is the whole point of the
	// downstream issuer check.
	const realUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	if b2, q2, _ := Orient("native", realUSDC); b2 != "native" || q2 != realUSDC {
		t.Errorf("Orient(native, %s) = (%s, %s), want (native, %s)", realUSDC, b2, q2, realUSDC)
	}
}
