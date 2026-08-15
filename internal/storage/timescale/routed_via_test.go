package timescale

import (
	"strings"
	"testing"
)

// TestTagTradesRoutedViaUpdate_IsCorrective is the TV-2b regression:
// the back-tag statement used a frozen `t.routed_via IS NULL`
// predicate, so once a trade was tagged a re-derive could NEVER
// correct a wrong routed_via — the row was permanently pinned to the
// first (possibly wrong) value. The corrected statement gates on
// `routed_via IS DISTINCT FROM COALESCE(wrapper.name, $1)` so an
// untagged row is tagged, a stale tag is re-tagged on a re-derive,
// and an equal tag is a no-op.
func TestTagTradesRoutedViaUpdate_IsCorrective(t *testing.T) {
	q := tagTradesRoutedViaUpdate

	// The frozen first-wins predicate must be gone.
	if strings.Contains(q, "routed_via IS NULL") {
		t.Error("statement still uses the frozen `routed_via IS NULL` — a wrong tag can never be corrected by a re-derive (TV-2b)")
	}
	// The corrective predicate must be present.
	if !strings.Contains(q, "routed_via IS DISTINCT FROM COALESCE(wrapper.name, $1)") {
		t.Error("statement lost the corrective `routed_via IS DISTINCT FROM COALESCE(wrapper.name, $1)` predicate")
	}
	// The SET value must remain non-NULL-by-construction ($1 is
	// validated non-empty) so a re-derive can never blank a tag.
	if !strings.Contains(q, "SET routed_via = COALESCE(wrapper.name, $1)") {
		t.Error("statement lost the COALESCE(wrapper.name, $1) SET value")
	}
	// Source-scoping must survive so a different source's sweep never
	// contends for the same row.
	if !strings.Contains(q, "t.source  = $2") {
		t.Error("statement lost source-scoping (t.source = $2)")
	}
}
