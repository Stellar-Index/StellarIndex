package timescale

import "testing"

// Harvest must be a valid direction end to end (audit 2026-08-04
// finding 4): the DB CHECK was widened by migration 0138, and this
// Go-side whitelist was the second gate that silently rejected it.
func TestDefindexDirection_HarvestIsValid(t *testing.T) {
	if !DefindexHarvest.IsValid() {
		t.Fatal("DefindexHarvest.IsValid() = false — harvest rows would be rejected at insert despite the widened CHECK")
	}
}
