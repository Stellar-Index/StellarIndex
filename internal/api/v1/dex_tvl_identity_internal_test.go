// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// tvlTestSelfListed is a well-formed Soroban token that no catalogue
// entry, SAC derivation or declared peg vouches for — the shape of a
// token anyone can deploy and pair on the Soroswap factory.
const tvlTestSelfListed = tvlTestPhxBadPool // a well-formed contract id reused as an opaque token

// tvlTestAquaSAC derives the AQUA SAC on the configured network from
// the catalogue's own classic entry, so the test cannot drift from the
// identity the valuer derives.
func tvlTestAquaSAC(t *testing.T) (sac string, classic canonical.Asset) {
	t.Helper()
	aqua, err := canonical.NewClassicAsset("AQUA", "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA")
	if err != nil {
		t.Fatalf("aqua asset: %v", err)
	}
	sac, err = aqua.SacContractID()
	if err != nil {
		t.Fatalf("aqua sac: %v", err)
	}
	return sac, aqua
}

// tvlTestCatalogue builds a verified-currency catalogue vouching for the
// given Soroban token contract ids, so a fixture that exercises a price
// tier with an arbitrary token first clears the identity screen.
func tvlTestCatalogue(contracts ...string) *currency.Catalogue {
	var b strings.Builder
	b.WriteString("verified_currencies:\n")
	for i, c := range contracts {
		fmt.Fprintf(&b, "  - ticker: TST%d\n    slug: tst%d\n    name: Test token %d\n    class: crypto\n"+
			"    networks:\n      - network: stellar\n        asset_id: %s\n", i, i, i, c)
	}
	cat, err := currency.LoadFromBytes([]byte(b.String()))
	if err != nil {
		panic(fmt.Sprintf("tvlTestCatalogue: %v", err))
	}
	return cat
}
