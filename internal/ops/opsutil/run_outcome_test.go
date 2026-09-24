// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package opsutil

import (
	"strings"
	"testing"
)

func TestRunOutcomeErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		o       RunOutcome
		wantErr string
	}{
		{"clean run", RunOutcome{Verb: "v", Noun: "row", Attempted: 3, Written: 3}, ""},
		{"nothing to do", RunOutcome{Verb: "v", Noun: "row"}, ""},
		{"one dropped row", RunOutcome{Verb: "v", Noun: "row", Attempted: 3, Written: 2, Failed: 1}, "v: 1 of 3 row(s) failed to insert"},
		{"every row dropped", RunOutcome{Verb: "v", Noun: "row", Attempted: 2, Failed: 2}, "v: 2 of 2 row(s) failed to insert"},
		{"attempted, none written, none counted failed", RunOutcome{Verb: "v", Noun: "row", Attempted: 2}, "v: 2 row(s) attempted and none written"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.o.Err()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Err() = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
