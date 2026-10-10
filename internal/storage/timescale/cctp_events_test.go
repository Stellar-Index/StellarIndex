package timescale

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/cctp"
)

// TestCCTPEventType_MatchesSourceEventNames cross-checks the second
// gating layer (this enum's IsValid) against the FIRST (the decoder's
// Event* constants in internal/sources/cctp) — the pair that was
// ungated: a fixture that re-listed the 26 kinds as string
// literals, so a source-side rename or a new Event* constant with no
// matching CCTPEventType entry compiled and passed silently. Importing
// the source package's actual constants means a rename here fails to
// compile, and a set-membership diff catches an added-but-not-mirrored
// (or removed-but-not-mirrored) event by name.
func TestCCTPEventType_MatchesSourceEventNames(t *testing.T) {
	t.Parallel()
	fromSource := []string{
		cctp.EventDepositForBurn,
		cctp.EventMintAndWithdraw,
		cctp.EventMessageSent,
		cctp.EventMessageReceived,
		cctp.EventMintAndForward,
		cctp.EventOwnershipTransfer,
		cctp.EventOwnershipTransferCompleted,
		cctp.EventAdminChanged,
		cctp.EventRemoteTokenMessengerAdded,
		cctp.EventTokenPairLinked,
		cctp.EventAdminChangeStarted,
		cctp.EventAttesterEnabled,
		cctp.EventAttesterManagerUpdated,
		cctp.EventDenylisted,
		cctp.EventDenylisterChanged,
		cctp.EventFeeRecipientSet,
		cctp.EventMaxMessageBodySizeUpdated,
		cctp.EventMinFeeControllerSet,
		cctp.EventPauserChanged,
		cctp.EventRescuerChanged,
		cctp.EventSetBurnLimitPerMessage,
		cctp.EventSetTokenController,
		cctp.EventSignatureThresholdUpdated,
		cctp.EventSwapMinterConfigSet,
		cctp.EventTokenDecimalConfigAdded,
		cctp.EventUnDenylisted,
	}
	storageKnown := map[CCTPEventType]bool{
		CCTPDepositForBurn:             true,
		CCTPMintAndWithdraw:            true,
		CCTPMessageSent:                true,
		CCTPMessageReceived:            true,
		CCTPMintAndForward:             true,
		CCTPOwnershipTransfer:          true,
		CCTPOwnershipTransferCompleted: true,
		CCTPAdminChanged:               true,
		CCTPRemoteTokenMessengerAdded:  true,
		CCTPTokenPairLinked:            true,
		CCTPAdminChangeStarted:         true,
		CCTPAttesterEnabled:            true,
		CCTPAttesterManagerUpdated:     true,
		CCTPDenylisted:                 true,
		CCTPDenylisterChanged:          true,
		CCTPFeeRecipientSet:            true,
		CCTPMaxMessageBodySizeUpdated:  true,
		CCTPMinFeeControllerSet:        true,
		CCTPPauserChanged:              true,
		CCTPRescuerChanged:             true,
		CCTPSetBurnLimitPerMessage:     true,
		CCTPSetTokenController:         true,
		CCTPSignatureThresholdUpdated:  true,
		CCTPSwapMinterConfigSet:        true,
		CCTPTokenDecimalConfigAdded:    true,
		CCTPUnDenylisted:               true,
	}

	if len(fromSource) != len(storageKnown) {
		t.Fatalf("layer mismatch: internal/sources/cctp has %d Event* constants, "+
			"timescale.CCTPEventType has %d — a new/removed event was added to one "+
			"layer without the other", len(fromSource), len(storageKnown))
	}

	seen := make(map[CCTPEventType]bool, len(fromSource))
	for _, name := range fromSource {
		kind := CCTPEventType(name)
		if !storageKnown[kind] {
			t.Errorf("cctp.Event %q has no matching timescale.CCTPEventType constant", name)
			continue
		}
		if !kind.IsValid() {
			t.Errorf("timescale.CCTPEventType(%q).IsValid() = false, want true", name)
		}
		seen[kind] = true
	}
	for kind := range storageKnown {
		if !seen[kind] {
			t.Errorf("timescale.CCTPEventType %q has no matching internal/sources/cctp Event* constant", kind)
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
