// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// TestCHRebuild_ModeFlagsFollowTheSharedWriteGate pins ch-rebuild to the
// shared -write/-dry-run convention. The named-source BackfillSafe refusal
// is armed only in write mode, so it tells the two modes apart before any
// datastore is reached; a dry run falls through to the absent config.
func TestCHRebuild_ModeFlagsFollowTheSharedWriteGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode      []string
		wantWrite bool
	}{
		{nil, false},
		{[]string{"-dry-run"}, false},
		{[]string{"-write"}, true},
		{[]string{"-dry-run", "-write"}, true},
	} {
		args := append(append([]string{}, tc.mode...), "-sources", sushiswap_v3.SourceName)
		err := chRebuild(chRebuildArgs(t, args...))
		if err == nil {
			t.Fatalf("%v: chRebuild succeeded against a config that does not exist", tc.mode)
		}
		if got := strings.Contains(err.Error(), "not BackfillSafe"); got != tc.wantWrite {
			t.Errorf("%v: write-mode refusal = %v, want %v (err: %v)", tc.mode, got, tc.wantWrite, err)
		}
		if !tc.wantWrite && !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("%v: a dry run should reach the config load, got: %v", tc.mode, err)
		}
	}
}
