// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// TestAccountKeysCreate_ChildInheritsExpiry — POST /v1/account/keys from a
// time-boxed key mints a child with the same expiry, not a permanent one.
func TestAccountKeysCreate_ChildInheritsExpiry(t *testing.T) {
	parentExpiry := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)

	sub := mintChildKeySubject(t, auth.Subject{
		Identifier: "staff-rotation",
		Tier:       auth.TierAPIKey,
		KeyID:      "kid_parent",
		ExpiresAt:  parentExpiry,
	})
	if !sub.ExpiresAt.Equal(parentExpiry) {
		t.Fatalf("child Subject.ExpiresAt = %v, want %v (inherited from the time-boxed parent; "+
			"zero means the child never expires)", sub.ExpiresAt, parentExpiry)
	}
}
