package currency

import "testing"

// TestFiatCodedAnchorIsNotAnImpersonator: a catalogue fiat ticker (USD, EUR, ...) is a denomination nobody issues, and SEP-1
// anchors code dollar deposits as `USD`, so that is not impersonation. Fiat entries with no Stellar issuance go to byFiatCode
// (answerable via FiatDenomination); one gaining a verified Stellar issuance collides normally.
func TestFiatCodedAnchorIsNotAnImpersonator(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}

	// A real SEP-24 fiat anchor's issuer. Any G-account works for the
	// mechanism; the point is that it is NOT claiming to be the catalogue's
	// USD entry, because that entry has no issuer to claim to be.
	const anchorAccount = "GDUKMGUGDZQK6YHYA5Z6AY2G4XDSZPSZ3SW5UN3ARVMO6QSRDWP5YLEX"

	for _, ticker := range []string{"USD", "EUR", "GBP", "JPY"} {
		t.Run(ticker, func(t *testing.T) {
			entry, ok := cat.LookupByTicker(ticker)
			if !ok {
				t.Fatalf("catalogue has no %s entry; this check is out of date", ticker)
			}
			if entry.Class != ClassFiat {
				t.Fatalf("%s is class %q, want fiat; this check is out of date", ticker, entry.Class)
			}
			if entry.StellarEntry() != nil {
				t.Skipf("%s now has a verified Stellar issuance — the collision report is correct for it", ticker)
			}
			if _, collision := cat.StellarCollision(ticker, anchorAccount); collision {
				t.Errorf("StellarCollision(%q, anchor) reports an impersonation. A fiat ticker is a "+
					"denomination with no issuer, and SEP-1 codes a fiat anchor's deposit token with "+
					"exactly this code — so the anchor loses its market cap and is warned about for "+
					"following the spec", ticker)
			}
			// Silent is not the same as forgotten: the catalogue must
			// still be able to say what the code denominates, or a
			// future entry could drop out of both indexes unnoticed.
			denom, ok := cat.FiatDenomination(ticker)
			if !ok {
				t.Fatalf("FiatDenomination(%q) found nothing — the code is in neither index", ticker)
			}
			if denom != entry {
				t.Errorf("FiatDenomination(%q) = %q, want the catalogue's own %q entry",
					ticker, denom.Ticker, entry.Ticker)
			}
		})
	}

	// The control: the carve-out is by class, so reference-only tickers (USDT, XRP, BTC) must still report an impersonator, else the
	// whole check would be disarmed.
	for _, ticker := range []string{"USDT", "XRP", "BTC"} {
		t.Run("control/"+ticker, func(t *testing.T) {
			if _, ok := cat.LookupByTicker(ticker); !ok {
				t.Skipf("catalogue no longer holds %s", ticker)
			}
			if _, collision := cat.StellarCollision(ticker, anchorAccount); !collision {
				t.Errorf("StellarCollision(%q, third party) reports no impersonation; %s names a "+
					"token issued elsewhere, so every classic %s-G… is one", ticker, ticker, ticker)
			}
			if _, fiat := cat.FiatDenomination(ticker); fiat {
				t.Errorf("%s resolved as a fiat denomination; it is an issued asset", ticker)
			}
		})
	}
}
