// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"
)

// healthySnapshotCounts scripts a cycle whose boards and snapshot are both in
// line with what is live.
func healthySnapshotCounts() map[string]uint64 {
	return map[string]uint64{
		"stellar.asset_holders_rollup_staging": 500_100,
		"stellar.asset_holders_rollup":         500_000,
		"stellar.asset_holders_counts_staging": 505,
		"stellar.asset_holders_counts":         500,
		"stellar.asset_stats_daily_staging":    905,
		assetStatsDailyLatestDay:               900,
	}
}

// TestRunHoldersRollupSnapshotsTheDayAfterTheSwap pins the snapshot's place
// in the cycle: every asset_stats_daily statement runs after the board swap,
// so a host where the snapshot fails still publishes boards, and the day is
// swapped in whole as the cycle's last statement.
func TestRunHoldersRollupSnapshotsTheDayAfterTheSwap(t *testing.T) {
	conn := &shrinkGuardConn{counts: healthySnapshotCounts()}
	if err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {}); err != nil {
		t.Fatalf("runHoldersRollupSteps: %v", err)
	}

	exchangeAt, firstSnapshotAt := -1, -1
	for i, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			exchangeAt = i
		}
		if firstSnapshotAt < 0 && strings.Contains(q, "asset_stats_daily") {
			firstSnapshotAt = i
		}
	}
	if exchangeAt < 0 || firstSnapshotAt < 0 {
		t.Fatalf("cycle issued exchange at %d, first snapshot statement at %d — want both", exchangeAt, firstSnapshotAt)
	}
	if firstSnapshotAt < exchangeAt {
		t.Errorf("snapshot statement %d ran before the board swap %d — a snapshot failure would hold back the boards", firstSnapshotAt, exchangeAt)
	}
	last := conn.execCalls[len(conn.execCalls)-1]
	if !strings.HasPrefix(last, "ALTER TABLE stellar.asset_stats_daily REPLACE PARTITION '") {
		t.Errorf("the day's REPLACE PARTITION must be the cycle's last statement, got %q", last)
	}
}

// TestRunHoldersRollupRefusesAShrunkenSnapshot: a snapshot scan that comes
// back far smaller than the newest stored day must not replace it, while the
// boards (guarded separately) still go live.
func TestRunHoldersRollupRefusesAShrunkenSnapshot(t *testing.T) {
	counts := healthySnapshotCounts()
	counts["stellar.asset_stats_daily_staging"] = 3
	conn := &shrinkGuardConn{counts: counts}

	err := runHoldersRollupSteps(context.Background(), conn, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "shrink guard") {
		t.Fatalf("a snapshot that shrank from 900 to 3 rows was not refused by the shrink guard: %v", err)
	}
	exchanged := false
	for _, q := range conn.execCalls {
		if strings.Contains(q, "EXCHANGE TABLES") {
			exchanged = true
		}
		if strings.Contains(q, "REPLACE PARTITION") {
			t.Errorf("REPLACE PARTITION was issued despite the shrink — the stored day must stay:\n%s", q)
		}
	}
	if !exchanged {
		t.Error("a healthy board was held back by the snapshot's shrink")
	}
}

// TestAssetStatsSnapshotSharesTheCycleDayAndStamp: both fill arms and the
// partition swap name the cycle's UTC day, and the rows carry the board's
// cycle stamp — a cycle started late in a zone behind UTC must not land on
// the previous day.
func TestAssetStatsSnapshotSharesTheCycleDayAndStamp(t *testing.T) {
	cycleAt := time.Date(2026, 9, 21, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600))
	stamp := "toDateTime('2026-09-22 04:30:00', 'UTC')"
	inserts := 0
	for _, s := range assetStatsSnapshotFills(cycleAt) {
		if !strings.Contains(s, "INSERT INTO") {
			continue
		}
		inserts++
		if !strings.Contains(s, "toDate('2026-09-22')") || !strings.Contains(s, stamp) {
			t.Errorf("snapshot insert does not carry the cycle's UTC day and stamp %s:\n%s", stamp, s)
		}
	}
	if inserts != 2 {
		t.Fatalf("snapshot fills have %d INSERTs, want 2 (trustline arm + native arm)", inserts)
	}
	want := "ALTER TABLE stellar.asset_stats_daily REPLACE PARTITION '2026-09-22' FROM stellar.asset_stats_daily_staging"
	if got := assetStatsSnapshotReplace(cycleAt); got != want {
		t.Errorf("replace = %q, want %q", got, want)
	}
}
