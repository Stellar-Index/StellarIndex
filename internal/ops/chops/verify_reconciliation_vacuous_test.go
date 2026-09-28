// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import "testing"

// TestReconciliationIsVacuous pins #1093: verify-reconciliation printed
// "OK — expected=0 actual=0" and exited 0 for a target with no data on
// either side — indistinguishable from a source redeployed behind a new
// contract id whose config was never updated (the projector writes
// nothing, the re-derive expects nothing over two empty maps).
// verify-recognition already refuses this shape; verify-reconciliation
// must too.
func TestReconciliationIsVacuous(t *testing.T) {
	cases := []struct {
		name               string
		expTotal, actTotal int
		want               bool
	}{
		{"both empty is vacuous", 0, 0, true},
		{"genuinely reconciled is not vacuous", 5, 5, false},
		{"expected only is a real mismatch, not vacuous", 5, 0, false},
		{"actual only is a real mismatch, not vacuous", 0, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reconciliationIsVacuous(tc.expTotal, tc.actTotal); got != tc.want {
				t.Errorf("reconciliationIsVacuous(%d, %d) = %v, want %v", tc.expTotal, tc.actTotal, got, tc.want)
			}
		})
	}
}
