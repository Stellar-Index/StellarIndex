package v1

import (
	"testing"
)

// TestRWARowsCarryTheVolumeFloor pins both RWA projections (classic and contract) to /v1/assets' volume_lower_bound: a floor
// volume served without its flag reads as an exact figure on /rwa while /assets calls the same number a lower bound.
func TestRWARowsCarryTheVolumeFloor(t *testing.T) {
	const contractID = "CBIELTK6YBZJU5UP2WWQEUCYKLPU6AUNZ2BQ4WWFEIE3USCIHMXQDAMA"
	vol := "12000.00"
	for _, floor := range []bool{true, false} {
		s, _ := lakeSupplyServer(t, "")
		d := cetesRow()[0]
		d.VolumeUSD24h, d.VolumeLowerBound = &vol, floor
		if got := rwaRowFor(t, s, d); got.Volume24hUSD == nil || *got.Volume24hUSD != vol || got.VolumeLowerBound != floor {
			t.Errorf("classic row: volume=%v lower_bound=%v, want %s lower_bound=%v", got.Volume24hUSD, got.VolumeLowerBound, vol, floor)
		}

		c := AssetDetail{AssetID: contractID, Kind: "soroban_token", Decimals: 7, VolumeUSD24h: &vol, VolumeLowerBound: floor}
		out := rwaContractAssetRows([]rwaContractMember{{contractID: contractID, symbol: "XYZ"}}, map[string]AssetDetail{contractID: c})
		if len(out) != 1 {
			t.Fatalf("contract rows = %d, want 1", len(out))
		}
		if out[0].VolumeLowerBound != floor {
			t.Errorf("contract row: lower_bound=%v, want %v", out[0].VolumeLowerBound, floor)
		}
	}
}
