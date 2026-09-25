// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"strings"
	"testing"
)

// TestChReprojectVerdict_DivergenceExitsNonZero pins the -h contract
// ("exits non-zero on any divergence"): a caller scripting $? must not read
// a divergent range as a match.
func TestChReprojectVerdict_DivergenceExitsNonZero(t *testing.T) {
	err := chReprojectVerdict([]string{"aquarius/aquarius_swaps", "sdex (undecodable)"})
	if !errors.Is(err, errChReprojectDiverged) {
		t.Fatalf("divergence must be errChReprojectDiverged, got %v", err)
	}
	for _, want := range []string{"2 target(s)", "aquarius/aquarius_swaps", "sdex (undecodable)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if err := chReprojectVerdict(nil); err != nil {
		t.Fatalf("an exact match must exit 0, got %v", err)
	}
}
