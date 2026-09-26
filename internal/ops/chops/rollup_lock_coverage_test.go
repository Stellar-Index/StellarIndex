// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRollupCLIsSerializeOnTheirLock pins GH-1188: ch-creators-rollup,
// ch-sponsors-rollup and ch-cohort-rollup must refuse to run while another
// invocation of the SAME job already holds its lock, exactly as
// ch-holders-rollup does (TestAcquireHoldersRollupLockSerializesConcurrentRuns).
//
// Before this fix these three CLI entry points took no lock at all: a
// manually-invoked `stellarindex-ops ch-creators-rollup` (etc.) proceeded
// straight past flag parsing into ClickHouse work, so a run launched
// while the 30-minute timer's own invocation was mid TRUNCATE -> fill ->
// EXCHANGE on the same global staging tables raced it instead of being
// turned away here. Each case below holds the lock externally first and
// asserts the CLI entry point returns the contention error BEFORE it
// would reach any network call, so the test needs no ClickHouse and no
// config file.
func TestRollupCLIsSerializeOnTheirLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(lockFile string) error
	}{
		{
			name: "ch-creators-rollup",
			run: func(lockFile string) error {
				return chCreatorsRollup([]string{"-lock-file", lockFile})
			},
		},
		{
			name: "ch-sponsors-rollup",
			run: func(lockFile string) error {
				return chSponsorsRollup([]string{"-lock-file", lockFile})
			},
		},
		{
			name: "ch-cohort-rollup",
			run: func(lockFile string) error {
				// -config only needs to be non-empty: the lock is taken
				// before the config file is ever opened.
				return chCohortRollup([]string{"-lock-file", lockFile, "-config", "/nonexistent-config.toml"})
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
