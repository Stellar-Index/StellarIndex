package rozo

import "testing"

const (
	testUSDC = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	testEURC = "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2"
)

func TestRelayerAssetIDs_KeyedOnCodeAndIssuer(t *testing.T) {
	got := RelayerAssetIDs()
	if len(got) != 2 || got[0] != testUSDC || got[1] != testEURC {
		t.Fatalf("RelayerAssetIDs() = %v", got)
	}
}

func TestClassifyRelayerMovement(t *testing.T) {
	relayer := MainnetRelayerAccounts[0]
	cases := []struct {
		name, addr, direction, asset string
		want                         RelayerDirection
		ok                           bool
	}{
		{"inbound usdc", relayer, "received", testUSDC, RelayerInbound, true},
		{"outbound eurc", relayer, "sent", testEURC, RelayerOutbound, true},
		{"non-relayer account", "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "sent", testUSDC, "", false},
		{"native dust spam", relayer, "received", "native", "", false},
		{"scam USDC with other issuer", relayer, "received", "USDC-GBBD47IF6LWK7P7MDEVSCWR7DPUWV3NY3DTQEVFL4NAT4AQH3ZLLFLA5", "", false},
		{"code alone", relayer, "received", "USDC", "", false},
		{"self payment", relayer, "self", testUSDC, "", false},
	}
	for _, c := range cases {
		got, ok := ClassifyRelayerMovement(c.addr, c.direction, c.asset)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%q,%v), want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
