// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLockChownTarget(t *testing.T) {
	cases := []struct {
		name     string
		euid     int
		dirUID   uint32
		fileUID  uint32
		wantNeed bool
		wantUID  int
		wantMode os.FileMode
	}{
		{"root run leaves a root-owned lock in a service-owned dir", 0, 990, 0, true, 990, 0o600},
		{"root run leaves a root-owned lock in a root-owned dir", 0, 0, 0, true, 0, 0o660},
		{"root run over a lock the service already owns", 0, 0, 990, false, 0, 0},
		{"service user run", 990, 0, 990, false, 0, 0},
		{"other non-root run cannot chown", 1000, 0, 1000, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, gid, mode, need := lockChownTarget(c.euid, c.dirUID, 991, c.fileUID)
			if need != c.wantNeed {
				t.Fatalf("need = %v, want %v", need, c.wantNeed)
			}
			if need && (uid != c.wantUID || gid != 991 || mode != c.wantMode) {
				t.Fatalf("target = %d:%d %o, want %d:991 %o", uid, gid, mode, c.wantUID, c.wantMode)
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
