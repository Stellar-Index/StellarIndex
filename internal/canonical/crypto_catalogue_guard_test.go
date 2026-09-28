// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// cryptoTickersAbsentFromCatalogue lists every allow-listed crypto
// ticker that deliberately has no verified-catalogue entry, with why.
// A ticker that is a SPELLING of a catalogue ticker does not belong
// here: it belongs in an alias family (see eurcAliasFamily).
var cryptoTickersAbsentFromCatalogue = map[string]string{
	"ATOM":  "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"BCH":   "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"DASH":  "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"LTC":   "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"NEAR":  "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"SHIB":  "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"TON":   "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"TRX":   "Reflector CEX reference ticker; no Stellar issuance and no reference_only entry",
	"MATIC": "pre-migration Polygon token; the catalogue's POL is a 1:1 token swap, not a rename of the same token",
	"DAI":   "RedStone stablecoin feed; not in the catalogue",
	"USDP":  "RedStone stablecoin feed; not in the catalogue",
	"EUROB": "distinct euro-pegged token, not Circle's EURC; not in the catalogue",
	"MXNe":  "Bitso MXNe stablecoin; the catalogue's MXN is the fiat currency, not this token",
	"USDe":  "Ethena synthetic dollar (RedStone feed); not in the catalogue",
	"sUSDe": "staked USDe (RedStone feed); not in the catalogue",
	// NAV / fundamental feeds are quantities about an asset, not assets
	// a catalogue entry could describe.
	"SolvBTC":                     "RedStone tokenized-BTC market feed; not in the catalogue",
	"SolvBTC_FUNDAMENTAL":         "RedStone NAV-ratio feed, not an asset",
	"SolvBTC.BBN_FUNDAMENTAL":     "RedStone NAV-ratio feed, not an asset",
	"SolvBTC_FUNDAMENTAL_USD":     "RedStone USD NAV feed, not an asset",
	"SolvBTC.BBN_FUNDAMENTAL_USD": "RedStone USD NAV feed, not an asset",
	"savUSD_FUNDAMENTAL":          "RedStone NAV feed, not an asset",
}

// TestKnownCryptoCodes_CatalogueOrDeliberatelyAbsent is the
// catalogue guard: every allow-listed crypto ticker is in the verified catalogue,
// or folds through an alias family onto a ticker that is, or is listed
// as deliberately absent with a reason. A renamed ticker added to the
// allow-list without an alias (EUROC before eurcAliasFamily) fails here.
func TestKnownCryptoCodes_CatalogueOrDeliberatelyAbsent(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	inCatalogue := make(map[string]bool)
	for _, vc := range cat.All() {
		inCatalogue[vc.Ticker] = true
	}

	var undecided []string
	for code := range knownCryptoCodes {
		if inCatalogue[code] {
			continue
		}
		canon := defaultAliasRegistry.Canonical(Asset{Type: AssetCrypto, Code: code})
		if canon.Type == AssetCrypto && canon.Code != code && inCatalogue[canon.Code] {
			continue
		}
		if reason := cryptoTickersAbsentFromCatalogue[code]; reason == "" {
			undecided = append(undecided, code)
		}
	}
	sort.Strings(undecided)
	if len(undecided) > 0 {
		t.Errorf("allow-listed crypto ticker(s) with no catalogue decision: %s — add a catalogue"+
			" entry, an alias family onto a catalogue ticker, or a reasoned absence entry",
			strings.Join(undecided, ", "))
	}

	for code := range cryptoTickersAbsentFromCatalogue {
		if _, ok := knownCryptoCodes[code]; !ok {
			t.Errorf("absence entry %q is not an allow-listed crypto ticker; remove it", code)
		}
		if inCatalogue[code] {
			t.Errorf("absence entry %q is in the catalogue; remove it", code)
		}
	}
}
