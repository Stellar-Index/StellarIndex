package timescale

import (
	"os"
	"regexp"
	"testing"
)

// TestAquariusRewardsKind_ParityWithMigrationCheck pins IsValid() to migration 0099's event_kind CHECK set.
// A kind in the CHECK but missing from IsValid() is rejected client-side and its events silently dropped.
func TestAquariusRewardsKind_ParityWithMigrationCheck(t *testing.T) {
	raw, err := os.ReadFile("../../../migrations/0099_create_aquarius_rewards_events.up.sql")
	if err != nil {
		t.Fatalf("read migration 0099: %v", err)
	}

	// Extract the event_kind CHECK (event_kind IN ('a', 'b', ...)) list.
	block := regexp.MustCompile(`(?s)event_kind\s+text\s+NOT NULL CHECK \(event_kind IN \((.*?)\)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("could not locate the event_kind CHECK constraint in migration 0099")
	}
	kinds := regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1)
	if len(kinds) == 0 {
		t.Fatal("no kinds parsed out of the CHECK constraint")
	}

	for _, m := range kinds {
		k := AquariusRewardsKind(m[1])
		if !k.IsValid() {
			t.Errorf("kind %q is in migration 0099's CHECK but IsValid() rejects it", k)
		}
	}

	// Count parity both ways: every Go constant must also be in the
	// CHECK. IsValid is a switch, so enumerate via the known constants.
	goKinds := []AquariusRewardsKind{
		AquariusRewardsPoolState, AquariusRewardsClaimReward, AquariusRewardsSetRewardsConfig,
		AquariusRewardsPositionUpdate, AquariusRewardsDeposit, AquariusRewardsClaimFees,
		AquariusRewardsGaugeClaim, AquariusRewardsClaim, AquariusRewardsGaugeScheduleReward,
		AquariusRewardsSetRewardsState, AquariusRewardsGaugeAdd, AquariusRewardsConfigRewards,
	}
	if len(goKinds) != len(kinds) {
		t.Errorf("kind-count mismatch: %d Go constants vs %d in migration 0099's CHECK", len(goKinds), len(kinds))
	}
	inCheck := map[string]bool{}
	for _, m := range kinds {
		inCheck[string(m[1])] = true
	}
	for _, k := range goKinds {
		if !inCheck[string(k)] {
			t.Errorf("Go constant %q is not in migration 0099's CHECK", k)
		}
	}
}
