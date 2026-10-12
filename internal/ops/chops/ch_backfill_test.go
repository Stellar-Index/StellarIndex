// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
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

// Without ingestion.live_seam_ledger the live bucket floor is unknowable; defaulting to the lagging archive would break
// ch-live-catchup.sh (no -bucket). backfillCoverage closes the wrong-bucket defect by hard-failing a historic range on
// live.
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

// TestBackfillBucket_SeamAware: with ingestion.live_seam_ledger set, the range picks the bucket; a range spanning the
// seam is an error. One walk reads one bucket, so either choice would silently drop the ledgers on the other side.
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

// TestBackfillCoverage_ZeroLedgersIsAHardError: ch-backfill must not exit 0 after streaming
// ZERO ledgers of a nonzero request. ch-full-backfill.sh banks the window in its resume state
// only on exit 0, so a vacuous success removes the window from the backfill permanently.
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

// TestBackfillCoverage_PartialWalkIsAHardError: the contract is "[from,to] is now in
// ClickHouse". A short count means missing objects or failed per-ledger extraction
// (chBackfillChunk logs and skips those), so the window must not be banked.
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

// ch-backfill must not write by default: a run naming neither -write nor -dry-run refuses before loading config.
// Otherwise a write-by-default caller (ch-live-catchup.sh timer) would preview and exit 0 silently.
func TestCHBackfill_RefusesWithoutAStatedMode(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "absent.toml")
	base := []string{"-config", cfgPath, "-from", "2", "-to", "3"}

	if err := chBackfill(base); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("no mode flag: err = %v, want opsutil.ErrWriteModeUnstated", err)
	}
	for _, mode := range []string{"-write", "-dry-run"} {
		err := chBackfill(append(append([]string{}, base...), mode))
		if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) || !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("%s: err = %v, want the mode accepted and the run to reach config load", mode, err)
		}
	}
}

// TestChangesOnlyExtract_KeepsCommitMarkerAndChangesOnly fills every slice
// field of LedgerExtract by reflection, so a table added later is covered:
// -changes-only must write it nowhere.
func TestChangesOnlyExtract_KeepsCommitMarkerAndChangesOnly(t *testing.T) {
	var in clickhouse.LedgerExtract
	v := reflect.ValueOf(&in).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.Slice {
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		}
	}
	in.Ledger.LedgerSeq = 58_762_517
	in.Changes[0].LedgerSeq = 58_762_517

	out := changesOnlyExtract(in)

	if out.Ledger != in.Ledger {
		t.Fatalf("ledgers row changed: got %+v, want %+v — it is the commit marker and must be written", out.Ledger, in.Ledger)
	}
	if !reflect.DeepEqual(out.Changes, in.Changes) {
		t.Fatalf("Changes = %+v, want %+v", out.Changes, in.Changes)
	}
	ov := reflect.ValueOf(out)
	for i := 0; i < ov.NumField(); i++ {
		name := ov.Type().Field(i).Name
		if f := ov.Field(i); f.Kind() == reflect.Slice && name != "Changes" && f.Len() != 0 {
			t.Errorf("%s kept %d row(s); -changes-only must drop every table but ledgers and ledger_entry_changes", name, f.Len())
		}
	}
	if len(in.Txs) != 1 {
		t.Error("changesOnlyExtract mutated its input")
	}
}

// TestCHBackfill_ChangesOnlyFollowsTheModeGate — -changes-only is a filter,
// not a mode: alone it must still refuse, and with -write or -dry-run it must
// parse and reach config load like the default path.
func TestCHBackfill_ChangesOnlyFollowsTheModeGate(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "absent.toml")
	base := []string{"-config", cfgPath, "-from", "2", "-to", "3", "-changes-only"}

	if err := chBackfill(base); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
		t.Fatalf("-changes-only with no mode flag: err = %v, want opsutil.ErrWriteModeUnstated", err)
	}
	for _, mode := range []string{"-write", "-dry-run"} {
		err := chBackfill(append(append([]string{}, base...), mode))
		if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) || !strings.Contains(err.Error(), "absent.toml") {
			t.Errorf("-changes-only %s: err = %v, want the run to reach config load", mode, err)
		}
	}
}

// TestWindowedCHBackfills_RefuseImplicitFullRun: every windowed ClickHouse backfill refuses a bare invocation (no
// -from/-to/-full) rather than starting the whole ledger-2..tip history outside run-heavy-job.sh. The refusal fires
// before any connection, so no live lake is needed.
func TestWindowedCHBackfills_RefuseImplicitFullRun(t *testing.T) {
	cases := []struct {
		name string
		run  func(args []string) error
	}{
		{"ch-contract-ledgers-backfill", chContractLedgersBackfill},
		{"ch-instance-backfill", chInstanceBackfill},
		{"ch-txindex-backfill", func(args []string) error {
			_, err := parseTxIndexBackfillFlags(args)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run([]string{"-write"})
			if err == nil {
				t.Fatalf("%s: bare invocation (no -from/-to/-full) was accepted — it must refuse the implicit full-history backfill", tc.name)
			}
			if !strings.Contains(err.Error(), "full-history") {
				t.Fatalf("%s: refusal error should explain the full-history footgun, got: %v", tc.name, err)
			}
		})
	}
}

// TestChParticipantBackfill_FailureIsNotReportedDone: a failed backfill must not print the "done - wrote N rows" line,
// or the operator would take it for a finished run. A closed ClickHouse port fails it at once; the bound resolves from a
// stub.
func TestChParticipantBackfill_FailureIsNotReportedDone(t *testing.T) {
	stubLakeContiguousThrough(t, 10)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	runErr := chParticipantBackfill([]string{"-ch-addr", "127.0.0.1:1", "-from", "2", "-to", "10"})
	os.Stderr = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)

	if runErr == nil {
		t.Fatal("expected an error against an unreachable ClickHouse")
	}
	if strings.Contains(string(out), "done —") {
		t.Errorf("failed run printed the completion line:\n%s", out)
	}
	if !strings.Contains(string(out), "FAILED") {
		t.Errorf("failed run must say it FAILED:\n%s", out)
	}
}
