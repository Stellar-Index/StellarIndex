// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"path/filepath"
	"testing"
)

func TestLockChownTarget(t *testing.T) {
	cases := []struct {
		name     string
		euid     int
		fileUID  uint32
		wantNeed bool
	}{
		{"root run leaves a root-owned lock in the service dir", 0, 0, true},
		{"root run over a lock the service already owns", 0, 990, false},
		{"service user run", 990, 990, false},
		{"other non-root run cannot chown", 1000, 1000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, gid, need := lockChownTarget(c.euid, 990, 991, c.fileUID)
			if need != c.wantNeed {
				t.Fatalf("need = %v, want %v", need, c.wantNeed)
			}
			if need && (uid != 990 || gid != 991) {
				t.Fatalf("target = %d:%d, want 990:991", uid, gid)
			}
		})
	}
}

func TestAcquireRollupLockFailsClosedOnContention(t *testing.T) {
	p := filepath.Join(t.TempDir(), "job.lock")
	unlock, err := acquireRollupLock("job", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireRollupLock("job", p); err == nil {
		t.Fatal("second acquire of a held lock succeeded")
	}
	unlock()
	unlock2, err := acquireRollupLock("job", p)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	unlock2()
}
