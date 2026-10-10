package timescale

import (
	"context"
	"strings"
	"testing"
)

// These tests cover the Upsert/Read defensive guards — the real
// round-trip against Postgres needs testcontainers-go and lives in
// test/integration/ (per CONTRIBUTING.md §Testing).

func TestUpsertSACBalanceSeedProvenance_RejectsInvalidRows(t *testing.T) {
	const (
		contract = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset    = "PHO:GAX5..."
	)
	cases := []struct {
		name       string
		in         SACBalanceSeedProvenance
		wantSubstr string
	}{
		{"empty contract", SACBalanceSeedProvenance{AssetKey: asset, Source: SACBalanceSeedSourceFullHistory}, "ContractID"},
		{"empty asset", SACBalanceSeedProvenance{ContractID: contract, Source: SACBalanceSeedSourceFullHistory}, "AssetKey"},
		{"invalid source", SACBalanceSeedProvenance{ContractID: contract, AssetKey: asset, Source: "made_up_source"}, "invalid source"},
		{"negative holders", SACBalanceSeedProvenance{
			ContractID: contract, AssetKey: asset, Source: SACBalanceSeedSourceCurrentState, HoldersSeeded: -1,
		}, "negative"},
		{"full_history without LakeVerifiedThrough", SACBalanceSeedProvenance{
			ContractID: contract, AssetKey: asset, Source: SACBalanceSeedSourceFullHistory,
		}, "LakeVerifiedThrough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Store{}).UpsertSACBalanceSeedProvenance(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err=%v should mention %s", err, tc.wantSubstr)
			}
		})
	}
}

func TestSACBalanceSeedProvenanceFor_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	_, _, err := s.SACBalanceSeedProvenanceFor(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "contractID") {
		t.Errorf("err=%v should mention contractID", err)
	}
}
