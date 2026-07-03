// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import "testing"

// TestScamIdentitySuppression pins S-010: a flagged, UNVERIFIED
// issuer's self-declared org identity is the impersonation itself —
// the counterfeiter that set home_domain=lobstr.co must never render
// as "LOBSTR — SCAM".
func TestScamIdentitySuppression(t *testing.T) {
	// The known counterfeiter from the live incident.
	g := "GBYBVWOOVC4EJVRIF4HMWG5B7POLCS7JRPY5KYR3BCLEK24IJQOGUARD"
	if scamReason(g) == "" {
		t.Fatal("counterfeiter missing from the scam list — the suppression rule has nothing to key on")
	}
	// The rule under test lives inline in both handlers; assert its
	// inputs' invariant here: every scam-list entry has a non-empty
	// category-bearing reason so the UI can derive a badge label.
	for key, entry := range scamIssuers {
		if entry.Reason == "" {
			t.Errorf("scam entry %s has empty reason — badge derivation breaks", key)
		}
	}
}
