// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"strings"
	"testing"
)

// ch-gate walks a BACKFILLED range, so its bucket default is pinned at the
// source: the trimmed live bucket walked 0 ledgers and passed the gate.
func TestChGate_DefaultsToArchiveBucket(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("ch_gate.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func chGate(")
	if start < 0 {
		t.Fatal("chGate not found")
	}
	body = body[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "opsutil.HistoricReadBucket(cfg, *bucket)") {
		t.Error("chGate must resolve its bucket via opsutil.HistoricReadBucket (archive, then live)")
	}
	if strings.Contains(body, "cfg.Storage.S3BucketLive") {
		t.Error("chGate still names the TRIMMED live bucket as its default; a backfilled range walks 0 ledgers")
	}
}
