// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRollupUnitsCanWriteTheirLock pins the unit side of acquireRollupLock:
// a rollup whose unit runs under ProtectSystem=strict cannot create its lock
// file unless the lock's directory is in ReadWritePaths, and it then exits 1
// before doing any work.
func TestRollupUnitsCanWriteTheirLock(t *testing.T) {
	for _, tc := range []struct{ unit, lock string }{
		{"holders-rollup.service.j2", holdersRollupLockPath},
		{"creators-rollup.service.j2", creatorsRollupLockPath},
		{"sponsors-rollup.service.j2", sponsorsRollupLockPath},
		{"cohort-rollup.service.j2", cohortRollupLockPath},
	} {
		t.Run(tc.unit, func(t *testing.T) {
			unit := readRepoFile(t, "configs/ansible/roles/archival-node/templates/systemd/"+tc.unit)
			if !strings.Contains(unit, "\nProtectSystem=strict\n") {
				t.Skip("unit is not ProtectSystem=strict; the filesystem is writable")
			}
			dir := filepath.Dir(tc.lock)
			for _, line := range strings.Split(unit, "\n") {
				paths, ok := strings.CutPrefix(line, "ReadWritePaths=")
				if !ok {
					continue
				}
				for _, p := range strings.Fields(paths) {
					if p == dir {
						return
					}
				}
			}
			t.Errorf("%s runs under ProtectSystem=strict without ReadWritePaths=%s: the rollup cannot create %s and fails before it starts", tc.unit, dir, tc.lock)
		})
	}
}
