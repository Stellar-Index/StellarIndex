// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"
)

// TestRWACuratedSummarise_DegradedContractReadIsUnavailableNotServedZero.
//
// A curated snapshot that answered (snap.available) says nothing about
// whether the SEPARATE contract-catalogue read behind the per-asset rows
// also answered. Before this fix a failed catalogue read (rows == nil,
// degraded == true) fell through the switch's default branch exactly
// like a curator with a genuinely empty recognised set: "served" with
// Assets: 0. Those two states are not the same thing and must not
// render identically (RWC-524).
func TestRWACuratedSummarise_DegradedContractReadIsUnavailableNotServedZero(t *testing.T) {
	snap := rwaCurated{available: true, wired: true}
	now := time.Now().UTC()

	degraded := rwaCuratedSummarise(snap, nil, true, nil, now)
	if degraded.Status != "unavailable" {
		t.Errorf("degraded read: Status = %q, want %q", degraded.Status, "unavailable")
	}

	notDegraded := rwaCuratedSummarise(snap, nil, false, nil, now)
	if notDegraded.Status != "served" {
		t.Errorf("genuinely empty set: Status = %q, want %q (must stay served)", notDegraded.Status, "served")
	}
	if notDegraded.Assets != 0 {
		t.Errorf("genuinely empty set: Assets = %d, want 0", notDegraded.Assets)
	}
}
