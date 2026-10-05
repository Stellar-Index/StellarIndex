package rozo

import (
	"slices"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// RelayerDirection is a relayer account's side of a classic movement.
type RelayerDirection string

// Relayer directions: inbound = user deposit, outbound = bridge payout.
const (
	RelayerInbound  RelayerDirection = "inbound"
	RelayerOutbound RelayerDirection = "outbound"
)

// Bridge assets the relayers move. XLM through these accounts is
// dust-spam, so only these two (code, issuer) pairs count.
var relayerAssets = []canonical.Asset{
	{Type: canonical.AssetClassic, Code: "USDC", Issuer: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
	{Type: canonical.AssetClassic, Code: "EURC", Issuer: "GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"},
}

// RelayerAssetIDs returns the bridge assets as canonical "CODE-ISSUER" ids,
// the form stellar.account_movements.asset stores.
func RelayerAssetIDs() []string {
	ids := make([]string, len(relayerAssets))
	for i, a := range relayerAssets {
		ids[i] = a.String()
	}
	return ids
}

// ClassifyRelayerMovement classifies one stellar.account_movements row
// (address, direction, asset id) as relayer bridge flow. It gates on the
// relayer account identity and the (code, issuer) asset, never on shape
// alone; self-payments and other assets report false.
func ClassifyRelayerMovement(address, direction, assetID string) (RelayerDirection, bool) {
	if !slices.Contains(MainnetRelayerAccounts, address) || !slices.Contains(RelayerAssetIDs(), assetID) {
		return "", false
	}
	switch direction {
	case "received":
		return RelayerInbound, true
	case "sent":
		return RelayerOutbound, true
	}
	return "", false
}
