package explorer

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
)

func TestAssetRawKey(t *testing.T) {
	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	issuerRaw, _ := strkey.Decode(strkey.VersionByteAccountID, issuer)
	pool := strings.Repeat("ab", 32)
	poolRaw, _ := hex.DecodeString(pool)
	for _, c := range []struct {
		asset string
		want  string
		ok    bool
	}{
		{"USDC-" + issuer, string(issuerRaw), true},
		{"pool:" + pool, string(poolRaw), true},
		{"native", "", false},
		{"USDC-GNOTANACCOUNT", "", false},
		{"pool:abcd", "", false},
	} {
		got, ok := assetRawKey(c.asset)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("assetRawKey(%q) = %x, %v; want %x, %v", c.asset, got, ok, c.want, c.ok)
		}
	}
}
