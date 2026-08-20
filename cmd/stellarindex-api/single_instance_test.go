package main

import (
	"strings"
	"testing"
)

// TestAssertRedisOrSingleInstance pins the fail-closed startup gate
// (W1-auth-passkey-3 / auth-lt-1): a Redis-less deployment must not boot
// unless the operator has explicitly asserted single-instance, because the
// per-process auth fallbacks are unsafe across multiple instances.
func TestAssertRedisOrSingleInstance(t *testing.T) {
	cases := []struct {
		name           string
		redisEnabled   bool
		singleInstance bool
		wantErr        bool
	}{
		{"redis present, multi-instance assumed — OK (fleet-safe backends)", true, false, false},
		{"redis present, single-instance asserted — OK", true, true, false},
		{"redis absent, single-instance asserted — OK (per-process is correct)", false, true, false},
		{"redis absent, NOT asserted — REFUSE (the unsafe case)", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := assertRedisOrSingleInstance(tc.redisEnabled, tc.singleInstance)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected refuse-to-start error, got nil")
				}
				// The message must name the escape hatches so an operator can act.
				if !strings.Contains(err.Error(), "single_instance") || !strings.Contains(err.Error(), "Redis") {
					t.Errorf("error must name Redis + single_instance remedies, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected OK, got error: %v", err)
			}
		})
	}
}
