package diagnostics

import (
	"testing"
)

// verify-decoders and verify-external print how many members were
// silent; the exit status must carry the same finding. 9/9 silent used
// to exit 0.
func TestSilentVerdict(t *testing.T) {
	cases := []struct {
		name          string
		silent, total int
		failOnSilent  bool
		wantErr       bool
	}{
		{"every member silent, flag off", 9, 9, false, true},
		{"every member silent, flag on", 6, 6, true, true},
		{"nothing registered", 0, 0, false, true},
		{"some silent, flag on", 5, 6, true, true},
		{"some silent, flag off", 2, 9, false, false},
		{"none silent, flag on", 0, 6, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := silentVerdict("verify-x", "members", tc.silent, tc.total, tc.failOnSilent)
			if (err != nil) != tc.wantErr {
				t.Fatalf("silentVerdict(silent=%d, total=%d, failOnSilent=%v) = %v; wantErr %v",
					tc.silent, tc.total, tc.failOnSilent, err, tc.wantErr)
			}
		})
	}
}
