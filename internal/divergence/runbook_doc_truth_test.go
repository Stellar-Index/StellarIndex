// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package divergence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPriceDivergenceRunbookStatesTheRealFlagRule pins the runbook's
// Symptoms section to the rule CachedResult.WarningFired implements: a
// source quorum, median-or-nobody-agrees, and a persistence debounce.
// It used to say a `status = 'firing'` row drives the flag, which
// flushObservations writes with neither gate, so a responder could find
// firing rows with the flag off, or the flag on with no firing rows.
func TestPriceDivergenceRunbookStatesTheRealFlagRule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "operations", "runbooks", "price-divergence.md"))
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	_, after, ok := strings.Cut(string(raw), "## Symptoms")
	if !ok {
		t.Fatal("runbook has no '## Symptoms' section")
	}
	symptoms, _, _ := strings.Cut(after, "\n## ")
	symptoms = strings.Join(strings.Fields(symptoms), " ")

	for _, stale := range []string{
		"driven by a `divergence_observations` row with `status = 'firing'`",
		"`> 10` critical",
	} {
		if strings.Contains(symptoms, stale) {
			t.Errorf("Symptoms still claims %q, a mechanism the worker never had", stale)
		}
	}
	for _, want := range []string{
		"`min_sources_for_warning`",
		"`WarningPersistence`",
		fmt.Sprintf("%d min", int(DefaultWarningPersistence.Minutes())),
		"`agreement_count`",
		"`warning_fired`",
		"`success_count`",
		"`div:<base>/<quote>`",
	} {
		if !strings.Contains(symptoms, want) {
			t.Errorf("Symptoms does not name %q; the flag rule cannot be checked without it", want)
		}
	}
}
