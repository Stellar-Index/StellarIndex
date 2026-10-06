// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package redispub

import (
	"testing"
	"time"
)

// TestValidateEvent_Coverage: a truncated event must carry a covered_from
// inside its own window, and covered_from never rides without truncated.
func TestValidateEvent_Coverage(t *testing.T) {
	now := time.Now().UTC()
	observedAt := now.Truncate(time.Minute)
	yes, no := true, false
	inside := observedAt.Add(-10 * time.Minute)
	before := observedAt.Add(-2 * time.Hour)
	for _, tc := range []struct {
		name      string
		truncated *bool
		from      *time.Time
		ok        bool
	}{
		{"unknown", nil, nil, true},
		{"complete", &no, nil, true},
		{"truncated inside the window", &yes, &inside, true},
		{"truncated without covered_from", &yes, nil, false},
		{"covered_from without truncated", nil, &inside, false},
		{"covered_from on a complete window", &no, &inside, false},
		{"covered_from before the window", &yes, &before, false},
		{"covered_from at the bucket end", &yes, &observedAt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := ClosedBucketEvent{
				Asset: "native", Quote: "fiat:USD", WindowSeconds: 3600,
				ValueDecimal: "0.123456789012", ObservedAt: observedAt,
				Truncated: tc.truncated, CoveredFrom: tc.from,
			}
			if err := validateEvent(&ev, now); (err == nil) != tc.ok {
				t.Errorf("validateEvent = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
