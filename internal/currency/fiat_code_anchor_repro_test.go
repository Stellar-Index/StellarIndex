package currency

import "testing"

// K033 — a SEP anchor's fiat-coded token is reported as an impersonation.
//
// # The reproduction, which runs below when this test is un-skipped
//
// The catalogue's twenty sovereign-currency entries carry `networks: []`
// and an M2 circulating supply: they are units of account, not issued
// tokens. Having no Stellar issuance, they never reach
// indexStellarEntries' issuance loop, so indexTickerOnlyEntry files each
// ticker into byStellarCode — the impersonation index. StellarCollision
// then finds the code, finds no Stellar issuance whose issuer matches,
// and reports a collision for EVERY classic asset coded USD, EUR, GBP,
// JPY, … whoever issued it.
//
// The reasoning that put reference-only tickers there is sound for USDT,
// XRP or BTC: those name a specific token issued somewhere else, so a
// classic `USDT-G…` is by construction claiming to be something it is
// not. A fiat ticker names a DENOMINATION that nobody issues, and on
// Stellar `USD` is exactly how SEP-1 tells an anchor to code a
// dollar-denominated deposit (`anchor_asset_type = "fiat"`,
// `anchor_asset = "USD"`). A regulated anchor issuing `USD-G…` is
// denominating in dollars, not impersonating the dollar.
//
// What the flag costs such an anchor is not cosmetic: it withholds the
// market cap (internal/api/v1/assets_f2.go returns before the cap fill,
// and the listing's fillRowMarketCap has the same guard), refuses the
// listing valuation (asset_listing_valuation.go), and attaches a warning
// saying the asset "matches a well-known asset that has NO verified
// issuance on Stellar" — said of a dollar token, about the dollar.
//
// # Why this is skipped rather than fixed here
//
// The correct fix is a fiat-class branch that keeps a SIGNAL but stops
// the money suppression and rewords the warning, and it cannot be made
// from this unit's file set:
//
//   - internal/api/v1/assets_f2.go holds populateMarketCap, the DETAIL
//     path's suppression. Fixing only the listing path would leave the
//     defect live on the surface a client lands on.
//   - internal/currency/verified_test.go holds
//     TestStellarCollision_CoversEveryCatalogueTicker, which deliberately
//     requires every catalogue ticker — fiat included — to be
//     collision-flaggable. That expectation is the opposite of this one
//     and has to be revised as a decision, not flipped in passing.
//   - internal/currency/data/seed.yaml would carry the operator-curated
//     known-anchor issuer set the original finding asks the collision
//     check to consult.
//
// Un-skip this test with that change; it is the acceptance check.
func TestFiatCodedAnchorIsNotAnImpersonator(t *testing.T) {
	t.Skip("K033: needs internal/api/v1/assets_f2.go, internal/currency/verified_test.go and " +
		"internal/currency/data/seed.yaml, which are outside this unit's file set")

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
				t.Fatalf("catalogue has no %s entry; this reproduction is out of date", ticker)
			}
			if entry.Class != ClassFiat {
				t.Fatalf("%s is class %q, want fiat; this reproduction is out of date", ticker, entry.Class)
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
		})
	}
}
