package timescale

import "testing"

// TestCCTPEventType_IsValid_AllTwentySixKinds guards the THIRD gating
// layer the type's godoc warns about (board #31, re-confirmed
// 2026-07-08 / ROADMAP #89b, and again 2026-07-09 / #89c): Classify
// (decoder), IsValid (this file), and the SQL CHECK (migrations
// 0038/0070/0092/0094) must all agree on the same set, or
// InsertCCTPEvent silently rejects a decoded event before it ever
// reaches Postgres.
func TestCCTPEventType_IsValid_AllTwentySixKinds(t *testing.T) {
	t.Parallel()
	known := []CCTPEventType{
		CCTPDepositForBurn,
		CCTPMintAndWithdraw,
		CCTPMessageSent,
		CCTPMessageReceived,
		CCTPMintAndForward,
		CCTPOwnershipTransfer,
		CCTPOwnershipTransferCompleted,
		CCTPAdminChanged,
		CCTPRemoteTokenMessengerAdded,
		CCTPTokenPairLinked,
		CCTPAdminChangeStarted,
		CCTPAttesterEnabled,
		CCTPAttesterManagerUpdated,
		CCTPDenylisted,
		CCTPDenylisterChanged,
		CCTPFeeRecipientSet,
		CCTPMaxMessageBodySizeUpdated,
		CCTPMinFeeControllerSet,
		CCTPPauserChanged,
		CCTPRescuerChanged,
		CCTPSetBurnLimitPerMessage,
		CCTPSetTokenController,
		CCTPSignatureThresholdUpdated,
		CCTPSwapMinterConfigSet,
		CCTPTokenDecimalConfigAdded,
		CCTPUnDenylisted,
	}
	if len(known) != 26 {
		t.Fatalf("test fixture drift: got %d known kinds, want 26", len(known))
	}
	for _, k := range known {
		if !k.IsValid() {
			t.Errorf("IsValid(%q) = false, want true", k)
		}
	}
}

func TestCCTPEventType_IsValid_RejectsUnknown(t *testing.T) {
	t.Parallel()
	for _, bad := range []CCTPEventType{"", "bogus_event", "admin_change_started_v2"} {
		if bad.IsValid() {
			t.Errorf("IsValid(%q) = true, want false", bad)
		}
	}
}
