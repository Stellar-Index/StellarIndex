// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"sort"
	"strings"
	"testing"
)

// The summary's `provenances` enum is the set a VERIFIED row can carry
// into the reference total; curated rows are served apart and never
// enter it, so the curator provenance belongs on the per-row enum only.
// Both spec enums are pinned to the Go vocabulary so a new provenance
// cannot reach the wire without the spec (and its generated clients).
func TestRWAReferenceProvenance_SpecEnumsAreTheGoVocabulary(t *testing.T) {
	summary := make([]string, 0, len(rwaReferenceProvenanceProseOrder))
	for _, p := range rwaReferenceProvenanceProseOrder {
		summary = append(summary, p.provenance)
	}
	row := append(append([]string{}, summary...), RWAReferenceCuratorPrice)

	doc := loadSpecDoc(t)
	gotSummary := specEnumAt(t, doc, "components", "schemas", "RWAReferenceSummary", "properties", "provenances", "items")
	gotRow := specEnumAt(t, doc, "components", "schemas", "RWAReference", "properties", "provenance")
	sort.Strings(summary)
	sort.Strings(row)
	if strings.Join(gotSummary, ",") != strings.Join(summary, ",") {
		t.Errorf("openapi RWAReferenceSummary.provenances enum = %v, Go vocabulary = %v; edit both and regenerate", gotSummary, summary)
	}
	if strings.Join(gotRow, ",") != strings.Join(row, ",") {
		t.Errorf("openapi RWAReference.provenance enum = %v, Go vocabulary = %v; edit both and regenerate", gotRow, row)
	}
}
