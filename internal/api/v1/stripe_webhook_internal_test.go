package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestStripeWebhook_RevokedOnlyKeys_DeadLettersInsteadOfClaimingSuccess pins
// that a customer who rotated (revoked) their key and then paid is reported
// as UNPROVISIONED, not as a successful upgrade.
//
// ListKeysForIdentifier filters on identifier only, so it returns revoked
// records. upgradeAllKeys used to lift the budget on them and count them,
// making `upgraded == len(keys)` (suppressing DeadLetterKeyUpgradeFailed)
// while `len(keys) > 0` suppressed DeadLetterNoKeys — so the response was
// 200 {"upgraded":1} with no dead-letter and no usable credential. Money in,
// nothing provisioned, and the detector built for exactly that reported
// clean (cold audit 2026-08-04).
func TestStripeWebhook_RevokedOnlyKeys_DeadLettersInsteadOfClaimingSuccess(t *testing.T) {
	keys := []auth.APIKeyRecord{
		{KeyID: "kid_revoked", Identifier: "signup-abc", RevokedAt: time.Unix(1_770_000_000, 0).UTC()},
	}
	if got := activeKeysOnly(keys); len(got) != 0 {
		t.Fatalf("activeKeysOnly kept a revoked key: %+v", got)
	}

	// A live key alongside it must survive, and the revoked one must not
	// dilute the count.
	keys = append(keys, auth.APIKeyRecord{KeyID: "kid_live", Identifier: "signup-abc"})
	got := activeKeysOnly(keys)
	if len(got) != 1 || got[0].KeyID != "kid_live" {
		t.Fatalf("activeKeysOnly = %+v, want just kid_live", got)
	}
}
