package timescale

import (
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	testAquaSAC = "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK"
	testUsdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

func claimVolume(rewardSAC string, events, amount int64) AquariusClaimRewardTokenVolume {
	return AquariusClaimRewardTokenVolume{RewardToken: rewardSAC, Events: events, Amount: canonical.NewAmount(big.NewInt(amount))}
}

// Base units of two different reward tokens must never be added into one
// "Reward volume (30d)" figure; the per-token table carries each sum.
func TestAquariusClaimKPIs_MultiTokenPublishesNoScalarVolume(t *testing.T) {
	claims := &AquariusClaimRewardWindow{
		Events: 3, DistinctClaimants: 2,
		ByToken: []AquariusClaimRewardTokenVolume{claimVolume(testAquaSAC, 2, 10_000_000), claimVolume(testUsdcSAC, 1, 5)},
	}
	kpis := aquariusClaimKPIs(claims)
	var labels []string
	for _, k := range kpis {
		labels = append(labels, k.Label)
	}
	if want := []string{"Reward claims (30d)", "Distinct claimants (30d)"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("KPI labels = %v, want %v (no cross-token volume)", labels, want)
	}

	tbl := aquariusClaimVolumeTable(claims)
	want := [][]string{{testAquaSAC, "2", "10000000"}, {testUsdcSAC, "1", "5"}}
	if !reflect.DeepEqual(tbl.Rows, want) {
		t.Fatalf("volume table rows = %v, want %v", tbl.Rows, want)
	}
}

func TestAquariusClaimKPIs_SingleTokenNamesItsToken(t *testing.T) {
	claims := &AquariusClaimRewardWindow{Events: 1, DistinctClaimants: 1, ByToken: []AquariusClaimRewardTokenVolume{claimVolume(testAquaSAC, 1, 42_000)}}
	var vol *BespokeKPI
	kpis := aquariusClaimKPIs(claims)
	for i := range kpis {
		if kpis[i].Label == "Reward volume (30d)" {
			vol = &kpis[i]
		}
	}
	if vol == nil {
		t.Fatalf("single-token window lost its Reward volume (30d) KPI: %+v", kpis)
	}
	if vol.Value != "42000" || vol.Unit != "token-units" || !strings.Contains(vol.Hint, testAquaSAC) {
		t.Errorf("Reward volume (30d) = %+v, want 42000 token-units naming %s", *vol, testAquaSAC)
	}
}
