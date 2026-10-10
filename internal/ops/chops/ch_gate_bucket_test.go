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

// TestChGate_GatesOnRequestedCoverage pins the ch-gate call site of the
// coverage rule. REQUESTED there is -to minus -from plus one; DELIVERED
// is `walked`, incremented once per LedgerCloseMeta the census walk
// hands back. The guard has to run before the PASSED banner, and the
// ClickHouse row count has to be measured against the requested span —
// measuring it against `walked` is what let a short walk certify itself.
func TestChGate_GatesOnRequestedCoverage(t *testing.T) {
	t.Parallel()
	body := funcBody(t, "ch_gate.go", "chGate")

	coverage := strings.Index(body, `walkCoverage("ch-gate", uint32(*from), uint32(*to), walked, streamBucket)`)
	passed := strings.Index(body, "completeness gate PASSED")
	switch {
	case coverage < 0:
		t.Fatal("chGate never calls walkCoverage — a short walk still reports a verdict over " +
			"the slice it happened to read (RLT-282)")
	case passed < 0:
		t.Fatal("PASSED banner not found — this test is asserting nothing")
	case coverage > passed:
		t.Error("the coverage guard runs AFTER the PASSED banner — the gate announces success " +
			"on a range it only partly opened")
	}
	if strings.Contains(body, "ch.LedgerRows != uint64(walked)") {
		t.Error("ledger coverage is still measured against `walked`; both sides of that " +
			"comparison shrink together on a short walk, so the gate compares a subset to itself")
	}
	if !strings.Contains(body, "ch.LedgerRows != requested") {
		t.Error("ledger coverage must be measured against the REQUESTED span")
	}
}
