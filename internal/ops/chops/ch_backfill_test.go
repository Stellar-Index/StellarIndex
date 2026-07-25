// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// testBackfillConfig is the r1-shaped storage block: both buckets named,
// seam unset (r1's live value — see configs/ansible/inventory/r1.yml).
func testBackfillConfig(seam uint32) config.Config {
	var cfg config.Config
	cfg.Storage.S3BucketArchive = "galexie-archive"
	cfg.Storage.S3BucketLive = "galexie-live"
	cfg.Ingestion.LiveSeamLedger = seam
	return cfg
}

// TestBackfillBucket_NoSeamKeepsTheLiveDefault pins the DELIBERATE limit of
// the bucket-resolution fix.
//
// With no ingestion.live_seam_ledger (r1 today) the live bucket's floor is
// not knowable from config, and guessing it is what produced this class of
// bug. Silently switching the default to the archive would also break
// scripts/ops/ch-live-catchup.sh, which heals live-era holes and extends the
// tip with NO -bucket: the archive is an hourly mirror of live, so it lags
// the tip and that timer would fail every run until it caught up.
//
// So the wrong-bucket defect is closed by backfillCoverage (a historic range
// against the live bucket streams zero ledgers and now HARD FAILS naming the
// bucket), not by second-guessing the default here.
func TestBackfillBucket_NoSeamKeepsTheLiveDefault(t *testing.T) {
	cfg := testBackfillConfig(0) // r1: no seam configured

	got, err := backfillBucket(cfg, "", 2, 1_000_000)
	if err != nil {
		t.Fatalf("backfillBucket([2,1000000]) errored: %v", err)
	}
	if got != cfg.Storage.S3BucketLive {
		t.Fatalf("backfillBucket = %q, want %q — ch-live-catchup.sh passes no -bucket and must "+
			"keep reading the live tail", got, cfg.Storage.S3BucketLive)
	}
	// ...and the historic range that lands there must not be able to pass.
	if backfillCoverage(2, 1_000_000, 0, got, false) == nil {
		t.Fatal("a historic range resolved to the live bucket streamed zero ledgers and still " +
			"reported success — the wrong-bucket defect is not closed")
	}
}

// TestBackfillBucket_ExplicitOverrideAlwaysWins — every runbook that matters
// passes -bucket (scripts/ops/ch-full-backfill.sh sets BUCKET=galexie-archive);
// resolution must never second-guess it.
func TestBackfillBucket_ExplicitOverrideAlwaysWins(t *testing.T) {
	cfg := testBackfillConfig(60_000_000)
	for _, tc := range []struct{ from, to uint32 }{
		{2, 1_000_000},           // below the seam
		{61_000_000, 62_000_000}, // above it
		{59_000_000, 61_000_000}, // straddling it
	} {
		got, err := backfillBucket(cfg, "some-other-bucket", tc.from, tc.to)
		if err != nil {
			t.Fatalf("[%d,%d]: override errored: %v", tc.from, tc.to, err)
		}
		if got != "some-other-bucket" {
			t.Errorf("[%d,%d]: override = %q, want %q", tc.from, tc.to, got, "some-other-bucket")
		}
	}
}

// TestBackfillBucket_SeamAware — with ingestion.live_seam_ledger set, the
// range decides which bucket holds it, and a range that spans the seam is an
// ERROR rather than a coin-flip: one walk reads one bucket, so either choice
// silently drops the ledgers on the other side.
func TestBackfillBucket_SeamAware(t *testing.T) {
	const seam = uint32(60_000_000)
	cfg := testBackfillConfig(seam)

	if got, err := backfillBucket(cfg, "", 2, seam-1); err != nil || got != "galexie-archive" {
		t.Errorf("below the seam = (%q, %v), want (galexie-archive, nil)", got, err)
	}
	if got, err := backfillBucket(cfg, "", seam, seam+1000); err != nil || got != "galexie-live" {
		t.Errorf("at/above the seam = (%q, %v), want (galexie-live, nil)", got, err)
	}
	got, err := backfillBucket(cfg, "", seam-1, seam+1)
	if err == nil {
		t.Fatalf("a range straddling the seam resolved to %q instead of erroring — "+
			"one of the two sides would be silently skipped", got)
	}
	if !strings.Contains(err.Error(), "straddle") || !strings.Contains(err.Error(), "60000000") {
		t.Errorf("straddle error must name the seam, got: %v", err)
	}
}

// TestBackfillCoverage_ZeroLedgersIsAHardError is the other half of the
// 2026-07-25 defect: ch-backfill exited 0 after streaming ZERO ledgers of a
// nonzero request. That exit code is load-bearing — ch-full-backfill.sh
// appends the window to its resume state only on exit 0 — so a vacuous
// success removes the window from the backfill permanently.
func TestBackfillCoverage_ZeroLedgersIsAHardError(t *testing.T) {
	err := backfillCoverage(2, 1_000_000, 0, "galexie-live", false)
	if err == nil {
		t.Fatal("backfillCoverage(streamed=0) returned nil — ch-backfill would exit 0 having " +
			"written nothing, and ch-full-backfill.sh would bank [2,1000000] as DONE")
	}
	for _, want := range []string{"ZERO", "1000000", "galexie-live"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q so the operator sees which bucket was read; got: %v", want, err)
		}
	}
}

// TestBackfillCoverage_PartialWalkIsAHardError — the command's contract is
// "[from,to] is now in ClickHouse". A short count means the bucket lacks the
// objects or per-ledger extraction failed (chBackfillChunk logs and skips
// those); either way the range is not in the lake and the window must not be
// banked.
func TestBackfillCoverage_PartialWalkIsAHardError(t *testing.T) {
	err := backfillCoverage(1_000_000, 1_000_999, 900, "galexie-archive", false)
	if err == nil {
		t.Fatal("backfillCoverage(streamed=900, want=1000) returned nil — a 100-ledger hole " +
			"would be recorded as a completed window")
	}
	if !strings.Contains(err.Error(), "900 of 1000") || !strings.Contains(err.Error(), "100 ledgers are missing") {
		t.Errorf("error must state the shortfall exactly, got: %v", err)
	}
}

// TestBackfillCoverage_InterruptedRunIsNotADoneWindow — a SIGINT'd walk
// currently unwinds through `errors.Is(walkErr, context.Canceled)` and
// returns nil. A partially-walked window is not a done window.
func TestBackfillCoverage_InterruptedRunIsNotADoneWindow(t *testing.T) {
	err := backfillCoverage(1_000_000, 1_000_999, 1000, "galexie-archive", true)
	if err == nil {
		t.Fatal("an interrupted run reported success — Ctrl-C would bank the window")
	}
	if !strings.Contains(err.Error(), "INTERRUPTED") {
		t.Errorf("error must say it was interrupted, got: %v", err)
	}
}

// TestBackfillCoverage_FullWalkPasses — the fix must not turn a genuinely
// complete window into a failure.
func TestBackfillCoverage_FullWalkPasses(t *testing.T) {
	if err := backfillCoverage(1_000_000, 1_000_999, 1000, "galexie-archive", false); err != nil {
		t.Fatalf("a complete walk must pass, got: %v", err)
	}
	if err := backfillCoverage(5, 5, 1, "galexie-archive", false); err != nil {
		t.Fatalf("a single-ledger walk must pass, got: %v", err)
	}
}
