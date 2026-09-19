package external

import (
	"reflect"
	"testing"
)

// ReplayBackfillSafe is BackfillSafe over the PROJECTOR source namespace
// (finding F050). The cases pin each of its three branches and, above
// all, that the fall-through is BackfillSafe's own fail-closed answer.
func TestReplayBackfillSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		source string
		want   bool
		why    string
	}{
		{"aquarius", true, "registered and attested"},
		{"sushiswap_v3", false, "registered, audit pending"},
		{"upshift", false, "registered, audit pending"},
		{"blend_backstop", true, "no row of its own; covered by blend's attestation"},
		{"sep41_supply", true, "standard-schema source"},
		{"sep41_transfers", true, "standard-schema source"},
		{"blend-backstop", false, "hyphenated gap-detector name is not a source"},
		{"", false, "empty name"},
		{"no_such_source", false, "unregistered names fail closed"},
	}
	for _, tc := range cases {
		if got := ReplayBackfillSafe(tc.source); got != tc.want {
			t.Errorf("ReplayBackfillSafe(%q) = %v, want %v (%s)", tc.source, got, tc.want, tc.why)
		}
	}
	// For every REGISTERED name the two questions must agree: the replay
	// namespace only ever adds names, it never overrides a registry row.
	for name := range Registry {
		if ReplayBackfillSafe(name) != BackfillSafe(name) {
			t.Errorf("%s: ReplayBackfillSafe disagrees with its own registry row", name)
		}
	}
}

// The namespace maps must never shadow a registry row, and an alias must
// point at one — otherwise the alias silently resolves to the unknown-
// name fallback.
func TestReplayNamespaceMapsAreConsistentWithRegistry(t *testing.T) {
	t.Parallel()
	for name, coveredBy := range replayAuditCoveredBy {
		if _, ok := Registry[name]; ok {
			t.Errorf("%s has a registry row AND an audit alias — the alias would override the row", name)
		}
		if _, ok := Registry[coveredBy]; !ok {
			t.Errorf("%s is covered by %q, which is not in the registry", name, coveredBy)
		}
	}
	for name := range replayStandardSchemaSources {
		if _, ok := Registry[name]; ok {
			t.Errorf("%s has a registry row AND a standard-schema exemption — the exemption would override the row", name)
		}
	}
}

func TestUnsafeReplaySources(t *testing.T) {
	t.Parallel()
	got := UnsafeReplaySources([]string{"aquarius", "upshift", "blend_backstop", "typo", "sushiswap_v3"})
	want := []string{"upshift", "typo", "sushiswap_v3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UnsafeReplaySources = %v, want %v", got, want)
	}
	if got := UnsafeReplaySources(nil); got != nil {
		t.Errorf("UnsafeReplaySources(nil) = %v, want nil", got)
	}
}
