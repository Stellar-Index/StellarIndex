package wiring

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// The adapters are wired into v1.Options by interface; these assertions pin
// the contracts main.go relies on, so a signature drift fails here, not at boot.
var (
	_ v1.AssetReader         = StoreAssetReader{}
	_ v1.AssetReader         = CachedAssetReader{}
	_ v1.MarketsReader       = StoreMarketsReader{}
	_ v1.MarketsReader       = CachedMarketsReader{}
	_ v1.OracleReader        = StoreOracleReader{}
	_ v1.OracleReader        = CachedOracleReader{}
	_ v1.HistoryReader       = StoreHistoryReader{}
	_ v1.CoverageFloorReader = StoreCoverageFloorReader{}
	_ v1.ReadyChecker        = StoreChecker{}
	_ v1.ReadyChecker        = RedisChecker{}
	_ v1.SchemaVersionReader = SchemaChecker{}
)

func TestReadyCheckerIdentity(t *testing.T) {
	cases := []struct {
		c        v1.ReadyChecker
		name     string
		critical bool
	}{
		{StoreChecker{}, "postgres", true},
		{RedisChecker{}, "redis", false},
	}
	for _, tc := range cases {
		if tc.c.Name() != tc.name || tc.c.Critical() != tc.critical {
			t.Errorf("%T = (%q, %v), want (%q, %v)", tc.c, tc.c.Name(), tc.c.Critical(), tc.name, tc.critical)
		}
	}
}
