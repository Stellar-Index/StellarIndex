// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"fmt"
	"testing"
	"time"
)

// The Go predicate and the anchor's SQL must refuse the same minutes.
func TestFreezeTriggerLookback_MatchesTheAnchorSQL(t *testing.T) {
	if got := fmt.Sprintf("%d minutes", int(FreezeTriggerLookback/time.Minute)); got != xlmUSDFreezeTriggerLookback {
		t.Fatalf("FreezeTriggerLookback renders %q, anchor SQL uses %q", got, xlmUSDFreezeTriggerLookback)
	}
}

func TestFreezeWindow_CoversMinute(t *testing.T) {
	frozen := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)
	recovered := frozen.Add(20 * time.Minute)
	closed := FreezeWindow{FrozenAt: frozen, RecoveredAt: &recovered}
	open := FreezeWindow{FrozenAt: frozen}
	cases := []struct {
		name   string
		w      FreezeWindow
		bucket time.Time
		want   bool
	}{
		{"before the trigger lookback", closed, frozen.Add(-6 * time.Minute), false},
		{"inside the trigger lookback", closed, frozen.Add(-5 * time.Minute), true},
		{"inside the freeze", closed, frozen.Add(10 * time.Minute), true},
		{"last minute before recovery", closed, recovered.Add(-time.Minute), true},
		{"the recovery minute", closed, recovered, false},
		{"after recovery", closed, recovered.Add(time.Minute), false},
		{"open freeze, later minute", open, frozen.Add(48 * time.Hour), true},
	}
	for _, tc := range cases {
		if got := tc.w.CoversMinute(tc.bucket); got != tc.want {
			t.Errorf("%s: CoversMinute(%s) = %v, want %v", tc.name, tc.bucket, got, tc.want)
		}
	}
}
