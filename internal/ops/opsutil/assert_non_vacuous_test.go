// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import "testing"

func TestAssertNonVacuous(t *testing.T) {
	cases := []struct {
		name            string
		observed, total int
		wantErr         bool
	}{
		{"nothing in scope", 0, 0, true},
		{"nothing observed", 0, 9, true},
		{"partly observed", 1, 9, false},
		{"all observed", 9, 9, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertNonVacuous(tc.observed, tc.total, "venues")
			if (err != nil) != tc.wantErr {
				t.Fatalf("AssertNonVacuous(%d, %d) = %v; wantErr %v", tc.observed, tc.total, err, tc.wantErr)
			}
		})
	}
}
