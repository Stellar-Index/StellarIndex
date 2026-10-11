// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRollupCLIsSerializeOnTheirLock pins that ch-creators-rollup, ch-sponsors-rollup and
// ch-cohort-rollup refuse to run while another invocation of the SAME job holds its lock, as
// ch-holders-rollup does (TestAcquireHoldersRollupLockSerializesConcurrentRuns).
//
// Unguarded, a manual run would race the 30-minute timer's invocation mid TRUNCATE -> fill ->
// EXCHANGE on the same global staging tables. Each case holds the lock externally and asserts the
// entry point returns the contention error BEFORE any network call, so no ClickHouse or config
// file is needed.
func TestRollupCLIsSerializeOnTheirLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(lockFile string) error
	}{
		{
			name: "ch-creators-rollup",
			run: func(lockFile string) error {
				return chCreatorsRollup([]string{"-write", "-lock-file", lockFile})
			},
		},
		{
			name: "ch-sponsors-rollup",
			run: func(lockFile string) error {
				return chSponsorsRollup([]string{"-write", "-lock-file", lockFile})
			},
		},
		{
			name: "ch-cohort-rollup",
			run: func(lockFile string) error {
				// -config only needs to be non-empty: the lock is taken
				// before the config file is ever opened.
				return chCohortRollup([]string{"-write", "-lock-file", lockFile, "-config", "/nonexistent-config.toml"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lockFile := filepath.Join(t.TempDir(), tc.name+".lock")

			release, err := acquireRollupLock(tc.name, lockFile)
			if err != nil {
				t.Fatalf("pre-acquiring the lock: %v", err)
			}
			defer release()

			err = tc.run(lockFile)
			if err == nil {
				t.Fatalf("%s ran to completion while its own lock was already held — the two invocations are not serialized", tc.name)
			}
			if !strings.Contains(err.Error(), "already locked") {
				t.Errorf("%s: got error %q, want it to report lock contention (does it take the lock at all?)", tc.name, err)
			}
		})
	}
}
