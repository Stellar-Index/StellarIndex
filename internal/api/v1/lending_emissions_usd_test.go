package v1_test

import (
	"context"
	"testing"

	"github.com/stellar/go-stellar-sdk/network"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// TestEmissionTokenPrice_GatedOnTheProtocolTokenSAC prices a pool's emission token only when it is the SAC of its own
// protocol's verified token, for Blend (BLND) and BLNT, and withholds a foreign or cross-protocol token.
func TestEmissionTokenPrice_GatedOnTheProtocolTokenSAC(t *testing.T) {
	blndSAC, _ := xdrjson.SACContractID(blndAssetID, network.PublicNetworkPassphrase)
	blntSAC, _ := xdrjson.SACContractID(blntAssetID, network.PublicNetworkPassphrase)
	ctx := context.Background()

	srv := newProtocolsServer(t, protocolsCase{}).srv // BLND 0.05, BLNT unpriced
	if p, why := srv.EmissionTokenPriceForTest(ctx, "blend", blndSAC); why != "emission_token_unverified" || p != "" {
		t.Errorf("no network passphrase: price=%q reason=%q, want withheld unverified", p, why)
	}
	srv.NetworkPassphrase = network.PublicNetworkPassphrase
	for _, tc := range []struct{ protocol, token, price, why string }{
		{"blend", blndSAC, "0.05", ""},
		{"blend", blntSAC, "", "emission_token_unverified"},
		{"blend", mkCStrkey(t, 9), "", "emission_token_unverified"},
		{"blnt", blntSAC, "", "unpriced"},
		{"blend", "", "", "emission_token_unread"},
	} {
		if p, why := srv.EmissionTokenPriceForTest(ctx, tc.protocol, tc.token); p != tc.price || why != tc.why {
			t.Errorf("%s %s: price=%q reason=%q, want %q %q", tc.protocol, tc.token, p, why, tc.price, tc.why)
		}
	}

	clean := newProtocolsServer(t, protocolsCase{clean: true}).srv // BLNT 0.01
	clean.NetworkPassphrase = network.PublicNetworkPassphrase
	if p, why := clean.EmissionTokenPriceForTest(ctx, "blnt", blntSAC); p != "0.01" || why != "" {
		t.Errorf("BLNT priced: price=%q reason=%q, want 0.01", p, why)
	}
}
