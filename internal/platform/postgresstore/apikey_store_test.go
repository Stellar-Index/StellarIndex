package postgresstore

import (
	"reflect"
	"testing"
)

// TestNonNilStringArray_NilBecomesEmpty — a nil input must
// surface to the lib/pq driver as a non-nil zero-length
// pq.StringArray, which serialises to the SQL `'{}'` array
// literal. F-1262 (codex audit-2026-05-13): pre-fix the
// bare `pq.StringArray(nil)` cast emitted SQL NULL, which
// violated the migration-0027 NOT NULL constraint on
// `api_keys.referer_allowlist` and surfaced as a 500 on the
// default dashboard create-key request shape.
func TestNonNilStringArray_NilBecomesEmpty(t *testing.T) {
	got := nonNilStringArray(nil)
	if got == nil {
		t.Fatal("nonNilStringArray(nil) returned nil; want non-nil zero-length slice")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestNonNilStringArray_NonNilPassthrough — a non-nil input
// round-trips through unchanged. The wrapper is purely a
// nil-defence boundary; it must NOT mutate or copy values
// when the slice is already non-nil.
func TestNonNilStringArray_NonNilPassthrough(t *testing.T) {
	in := []string{"app.example.com", "console.example.com"}
	got := nonNilStringArray(in)
	if len(got) != len(in) {
		t.Errorf("len = %d, want %d", len(got), len(in))
	}
	if !reflect.DeepEqual([]string(got), in) {
		t.Errorf("got %v, want %v", got, in)
	}
}

// TestNonNilStringArray_EmptyNonNilPassthrough — an explicit
// empty (non-nil) input is preserved as zero-length. This is
// the case the audit cared about least (it's already a safe
// shape) but the helper must not accidentally allocate a new
// array literal from it.
func TestNonNilStringArray_EmptyNonNilPassthrough(t *testing.T) {
	got := nonNilStringArray([]string{})
	if got == nil {
		t.Fatal("nonNilStringArray([]string{}) returned nil; want non-nil zero-length")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
