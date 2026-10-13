// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestChCensusRollupDryRunTouchesNoClickHouse: against an unreachable ClickHouse, the default (no -write) run returns
// nil without dialling, while -write must fail trying to connect. -from-day sidesteps the incremental path's
// CensusMaxDay read so only the loop body is tested.
func TestChCensusRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1" // reserved port, nothing ever listens here
	today := time.Now().UTC().Format("2006-01-02")

	if err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}

// TestChHoldersRollupDryRunTouchesNoClickHouse mirrors the census case: a
// dry run must neither take the advisory lock nor dial ClickHouse, and
// -write must fail against an unreachable address.
func TestChHoldersRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1"
	lockPath := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")

	if err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}

// TestBoardRollupsRequireAStatedMode pins the timer-driven board rollups to the shared RequireStatedMode gate: a
// flagless unit must fail, not preview and exit 0 while boards go stale. A dry run takes no lock and dials nothing;
// -write passes the gate and fails on the unreachable store.
func TestBoardRollupsRequireAStatedMode(t *testing.T) {
	unreachable := "127.0.0.1:1"
	missingConfig := filepath.Join(t.TempDir(), "absent.toml")
	for _, tc := range []struct {
		verb  string
		extra []string
	}{
		{"ch-creators-rollup", nil},
		{"ch-sponsors-rollup", nil},
		// The cohort rollup loads its config before any store; an absent file
		// fails there, which is past the gate.
		{"ch-cohort-rollup", []string{"-config", missingConfig}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			lockPath := filepath.Join(t.TempDir(), tc.verb+".lock")
			args := append([]string{tc.verb, "-ch-addr", unreachable, "-lock-file", lockPath}, tc.extra...)

			if err := Run(args); !errors.Is(err, opsutil.ErrWriteModeUnstated) {
				t.Errorf("flagless run returned %v, want ErrWriteModeUnstated", err)
			}
			if err := Run(append(args, "-dry-run")); err != nil {
				t.Errorf("-dry-run returned %v, want nil: it must not touch the lock, config or ClickHouse", err)
			}
			if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a gated-out run created the rollup lock %s (stat err %v)", lockPath, err)
			}
			err := Run(append(args, "-write"))
			if err == nil || errors.Is(err, opsutil.ErrWriteModeUnstated) {
				t.Fatalf("-write returned %v, want a store or config error: the write path was never reached", err)
			}
		})
	}
}

// TestBoardRollupUnitsPassWrite pins the unit side: every rollup timer runs
// a write-gated command, so a unit missing -write would only ever preview.
func TestBoardRollupUnitsPassWrite(t *testing.T) {
	for _, unit := range []string{
		"census-rollup.service.j2",
		"holders-rollup.service.j2",
		"creators-rollup.service.j2",
		"sponsors-rollup.service.j2",
		"cohort-rollup.service.j2",
		"entry-index.service.j2",
	} {
		body := readRepoFile(t, "configs/ansible/roles/archival-node/templates/systemd/"+unit)
		start := strings.Index(body, "ExecStart=")
		if start < 0 {
			t.Fatalf("%s has no ExecStart", unit)
		}
		execStart := body[start:]
		if end := strings.Index(execStart, "\n\n"); end >= 0 {
			execStart = execStart[:end]
		}
		if !strings.Contains(execStart, "\n  -write \\\n") {
			t.Errorf("%s ExecStart does not pass -write; the timer would fail on every run:\n%s", unit, execStart)
		}
	}
}

// Day D holds a LiveSink hole, so the contiguous lake tip sits on D. The walk must stop at D: computing D+1 would make
// it the newest day present, and the next resume (max(day)) would never revisit D after ch-live-catchup heals it.
func TestCensusWalkEnd_StopsAtHoledDay(t *testing.T) {
	d := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	today := d.Add(24 * time.Hour)
	var logged int
	logf := func(string, ...any) { logged++ }

	if got := censusWalkEnd(d, today, d, true, logf); !got.Equal(d) {
		t.Fatalf("tip on %s, today %s: walk ends %s, want %s", d, today, got, d)
	}
	if logged == 0 {
		t.Error("clamped walk logged nothing; the stall must be visible in the journal")
	}

	// fromDay's own first ledger is missing: recompute nothing.
	if got := censusWalkEnd(d, today, time.Time{}, false, logf); !got.Before(d) {
		t.Fatalf("no contiguous ledger from %s: walk ends %s, want before it", d, got)
	}

	// A contiguous lake walks through today.
	if got := censusWalkEnd(d, today, today, true, logf); !got.Equal(today) {
		t.Fatalf("contiguous lake: walk ends %s, want today %s", got, today)
	}
}

// TestClosedMonthEdge_ExcludesTheInProgressMonth: the cycle must read MonthlyUSDVWAPs up to the START of now's calendar
// month, never `now`, or the still-accumulating prices_1mo bucket is admitted as a settled monthly VWAP.
func TestClosedMonthEdge_ExcludesTheInProgressMonth(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "mid-month",
			now:  time.Date(2026, time.September, 24, 18, 45, 0, 0, time.UTC),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "exactly on the month boundary",
			now:  time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "year boundary",
			now:  time.Date(2026, time.January, 3, 5, 0, 0, 0, time.UTC),
			want: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "non-UTC input is normalized",
			now:  time.Date(2026, time.September, 24, 23, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60)),
			want: time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := closedMonthEdge(tc.now)
			if !got.Equal(tc.want) {
				t.Errorf("closedMonthEdge(%s) = %s, want %s",
					tc.now.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			// The in-progress month's own bucket timestamp (its start)
			// must be excluded by `bucket < edge` — i.e. the edge is
			// never AFTER the current month's start.
			monthStart := time.Date(tc.now.UTC().Year(), tc.now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
			if got.After(monthStart) {
				t.Errorf("closedMonthEdge(%s) = %s, is after the current month's own bucket start %s — would admit the open month",
					tc.now.Format(time.RFC3339), got.Format(time.RFC3339), monthStart.Format(time.RFC3339))
			}
		})
	}
}

// TestValidateCreatorsBoundary: the boundary is rejected only when the
// chain has not reached it. Test nets legitimately set it to 1, below the
// lake's first ledger (2).
func TestValidateCreatorsBoundary(t *testing.T) {
	const (
		testnetTip = 1_500_000
		pubnetTip  = 63_000_000
	)
	for _, tc := range []struct {
		name              string
		boundary, lakeTip uint32
		wantErr           bool
	}{
		{"pubnet boundary on a test net that never reached it", clickhouse.P23BoundaryLedger, testnetTip, true},
		{"boundary one past the tip", testnetTip + 1, testnetTip, true},
		{"test-net setting movements_floor_ledger=1", 1, testnetTip, false},
		{"boundary equal to the lake's first ledger", 2, testnetTip, false},
		{"boundary equal to the tip", testnetTip, testnetTip, false},
		{"pubnet boundary on pubnet", clickhouse.P23BoundaryLedger, pubnetTip, false},
		{"empty lake has nothing to split", clickhouse.P23BoundaryLedger, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCreatorsBoundary(tc.boundary, tc.lakeTip)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeTip=%d) = nil, want an error",
					tc.boundary, tc.lakeTip)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCreatorsBoundary(boundary=%d, lakeTip=%d) = %v, want nil",
					tc.boundary, tc.lakeTip, err)
			}
		})
	}
}

// A rollup unit under ProtectSystem=strict cannot create its lock file unless the lock's directory is in ReadWritePaths,
// and it then exits 1 before doing any work; this pins the unit side of acquireRollupLock.
func TestRollupUnitsCanWriteTheirLock(t *testing.T) {
	for _, tc := range []struct{ unit, lock string }{
		{"holders-rollup.service.j2", holdersRollupLockPath},
		{"creators-rollup.service.j2", creatorsRollupLockPath},
		{"sponsors-rollup.service.j2", sponsorsRollupLockPath},
		{"cohort-rollup.service.j2", cohortRollupLockPath},
		{"entry-index.service.j2", entryIndexLockPath},
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

// ch-creators-rollup, ch-sponsors-rollup and ch-cohort-rollup must refuse to run while another invocation of the same
// job holds its lock, as ch-holders-rollup does. Unguarded, a manual run races the 30-minute timer mid TRUNCATE -> fill
// -> EXCHANGE on shared staging tables; each case asserts the contention error before any network call.
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
			name: "ch-entry-index",
			run: func(lockFile string) error {
				return chEntryIndex([]string{"-write", "-lock-file", lockFile})
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

// TestAcquireHoldersRollupLockSerializesConcurrentRuns: a manual ch-holders-rollup cannot run while another invocation
// (the 30-minute timer or a manual run) is mid-cycle. Without acquireHoldersRollupLock a second run enters
// RunHoldersRollup alongside the first.
func TestAcquireHoldersRollupLockSerializesConcurrentRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")

	release, err := acquireRollupLock("ch-holders-rollup", path)
	if err != nil {
		t.Fatalf("first acquireHoldersRollupLock: %v", err)
	}

	if _, err := acquireRollupLock("ch-holders-rollup", path); err == nil {
		t.Fatal("a second run acquired the lock while the first still holds it — runs are not serialized")
	} else if !strings.Contains(err.Error(), "already locked") {
		t.Errorf("error does not describe the contention: %v", err)
	}

	release()

	release2, err := acquireRollupLock("ch-holders-rollup", path)
	if err != nil {
		t.Fatalf("acquireHoldersRollupLock after release: %v", err)
	}
	release2()
}

// TestAcquireHoldersRollupLockFallsBackWhenDirMissing: a dev box (or a
// host before deploy has provisioned /var/lib/stellarindex) must still get
// a working exclusion, not a silently-disabled one.
func TestAcquireHoldersRollupLockFallsBackWhenDirMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist", "ch-holders-rollup.lock")

	release, err := acquireRollupLock("ch-holders-rollup", missing)
	if err != nil {
		t.Fatalf("acquireHoldersRollupLock with a missing preferred dir: %v", err)
	}
	defer release()

	if _, err := acquireRollupLock("ch-holders-rollup", missing); err == nil {
		t.Fatal("a second run acquired the lock via the fallback path while the first still holds it")
	}
}

// TestChHoldersRollupWiresHeartbeat: without a heartbeat the only signal is a systemd unit-failed state, so a cycle that
// falls behind silently is invisible. -write against an unreachable ClickHouse must still publish the shared ops_job
// textfile with last_exit_ok=0, as ch-backfill and usd-volume-restamp do.
func TestChHoldersRollupWiresHeartbeat(t *testing.T) {
	unreachable := "127.0.0.1:1"
	lockPath := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")
	hbPath := filepath.Join(t.TempDir(), "ops_job_ch-holders-rollup.prom")

	err := Run([]string{
		"ch-holders-rollup",
		"-ch-addr", unreachable,
		"-lock-file", lockPath,
		"-heartbeat", hbPath,
		"-write",
	})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}

	raw, readErr := os.ReadFile(hbPath) //nolint:gosec // test-controlled temp path
	if readErr != nil {
		t.Fatalf("heartbeat textfile was not written at %s: %v", hbPath, readErr)
	}
	body := string(raw)

	if !strings.Contains(body, `stellarindex_ops_job_running{ops_job="ch-holders-rollup"} 0`) {
		t.Errorf("running gauge missing/wrong for ops_job=ch-holders-rollup:\n%s", body)
	}
	if !strings.Contains(body, `stellarindex_ops_job_last_exit_ok{ops_job="ch-holders-rollup"} 0`) {
		t.Errorf("last_exit_ok must be 0 for a failed cycle (the freshness/failure signal T391 found missing):\n%s", body)
	}
	if !strings.Contains(body, "stellarindex_ops_job_last_finish_unix{ops_job=\"ch-holders-rollup\"}") {
		t.Errorf("last_finish_unix missing — no last-run timestamp published:\n%s", body)
	}
}
