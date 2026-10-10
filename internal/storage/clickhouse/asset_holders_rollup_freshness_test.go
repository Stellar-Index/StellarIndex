// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func isHoldersCycleStamp(q string) bool {
	return strings.Contains(q, "SELECT computed_at FROM stellar.asset_holders_rollup LIMIT 1")
}

// holdersRollupLake stubs the rollup reads: the asset's board rows and count
// row (both stamped boardAt; omitted when listed is false) and the live
// table's cycle stamp tableAt.
func holdersRollupLake(t *testing.T, listed bool, boardAt, tableAt time.Time) *stubConn {
	t.Helper()
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isHoldersProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isHoldersRows(q):
			if !listed {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{{"GHOLDER1", int64(200), boardAt}}}, nil
		case isHoldersCount(q):
			if !listed {
				// max() over no rows: 0 holders, epoch stamp.
				return &stubRows{data: [][]any{{int64(0), time.Unix(0, 0).UTC()}}}, nil
			}
			return &stubRows{data: [][]any{{int64(500), boardAt}}}, nil
		case isHoldersCycleStamp(q):
			return &stubRows{data: [][]any{{tableAt}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}
}

// TestHoldersRollupBoard_RefusesAStaleCycle pins: a board whose cycle
// stamp is older than holdersRollupMaxAge (the rollup timer has wedged) must
// not be served as the current holders board — ok=false sends AssetHolders
// to the live per-request scans instead.
//
// Red against a holdersRollupBoard that never judges
// computed_at's age: it returns ok=true with the 3h-old balance=200/total=500.
func TestHoldersRollupBoard_RefusesAStaleCycle(t *testing.T) {
	stale := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	r := &ExplorerReader{conn: holdersRollupLake(t, true, stale, stale)}

	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if ok {
		t.Fatalf("ok = true for a cycle stamped %s ago — a wedged rollup's board was served as current: board=%+v total=%d",
			time.Since(stale).Round(time.Minute), out, total)
	}
}

// TestHoldersRollupBoard_RefusesAStaleCycleForAnUnlistedAsset pins the other
// half: an asset with no row in either table is "authoritatively zero
// holders" only for a CURRENT cycle. Under a wedged rollup, an asset issued
// since the last cycle would otherwise be served as having no holders.
//
// Red against a holdersRollupBoard that never judges age: ok=true, total=0.
func TestHoldersRollupBoard_RefusesAStaleCycleForAnUnlistedAsset(t *testing.T) {
	stale := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	r := &ExplorerReader{conn: holdersRollupLake(t, false, time.Time{}, stale)}

	_, total, ok, err := r.holdersRollupBoard(t.Context(), "NEW-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if ok {
		t.Fatalf("ok = true (total=%d) for an unlisted asset under a cycle stamped %s ago — a stale empty board was served as authoritative",
			total, time.Since(stale).Round(time.Minute))
	}
}

// TestHoldersRollupBoard_ServesAFreshCycle is the non-vacuity guard for the
// two tests above: the same shapes with a current stamp must still be served
// from the rollup (a gate that refused everything would pass them too).
func TestHoldersRollupBoard_ServesAFreshCycle(t *testing.T) {
	fresh := time.Now().UTC().Truncate(time.Second).Add(-40 * time.Minute)

	r := &ExplorerReader{conn: holdersRollupLake(t, true, fresh, fresh)}
	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil || !ok {
		t.Fatalf("listed asset, fresh cycle: ok=%v err=%v, want served", ok, err)
	}
	if len(out) != 1 || out[0].Balance != 200 || total != 500 {
		t.Errorf("board=%+v total=%d, want balance=200 total=500", out, total)
	}

	r = &ExplorerReader{conn: holdersRollupLake(t, false, time.Time{}, fresh)}
	out, total, ok, err = r.holdersRollupBoard(t.Context(), "NEW-"+testIssuer, 100)
	if err != nil || !ok {
		t.Fatalf("unlisted asset, fresh cycle: ok=%v err=%v, want served as zero holders", ok, err)
	}
	if len(out) != 0 || total != 0 {
		t.Errorf("board=%+v total=%d, want empty board, total=0", out, total)
	}
}

// TestHoldersRollupBoard_RetriesOnceWhenACycleSwapsMidRead pins: a
// group EXCHANGE landing between the board-rows read and the count read must
// not serve a board from one cycle paired with a count from another. The
// stub reports the rows query as still on cycle1 the first time (as if read
// before the swap) and the count query as already on cycle2 (as if read
// after) — the exact shape one EXCHANGE landing between the two queries
// produces — so holdersRollupBoard must retry, and the retried (self-
// consistent, both cycle2) pair is what must be served.
//
// Red against a holdersRollupBoard that has no
// computed_at comparison at all: it returns the FIRST (torn) pair —
// balance=100 paired with total=999 — instead of retrying to the
// self-consistent balance=200/total=500.
func TestHoldersRollupBoard_RetriesOnceWhenACycleSwapsMidRead(t *testing.T) {
	cycle1 := time.Now().UTC().Truncate(time.Second).Add(-50 * time.Minute)
	cycle2 := cycle1.Add(30 * time.Minute)

	rowsCalls := 0
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isHoldersProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case isHoldersRows(q):
			rowsCalls++
			if rowsCalls == 1 {
				return &stubRows{data: [][]any{{"GHOLDER1", int64(100), cycle1}}}, nil
			}
			return &stubRows{data: [][]any{{"GHOLDER1", int64(200), cycle2}}}, nil
		case isHoldersCount(q):
			// The count side always reports the CURRENT live cycle
			// (cycle2), so read #1's rows (cycle1) mismatch and read #2's
			// rows (cycle2) match.
			return &stubRows{data: [][]any{{int64(500), cycle2}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	r := &ExplorerReader{conn: conn}

	out, total, ok, err := r.holdersRollupBoard(t.Context(), "USDC-"+testIssuer, 100)
	if err != nil {
		t.Fatalf("holdersRollupBoard: %v", err)
	}
	if !ok {
		t.Fatal("ok = false on a read that resolved consistent after one retry")
	}
	if len(out) != 1 || out[0].Balance != 200 {
		t.Errorf("board = %+v, want balance=200 (the retried, self-consistent read) — 100 paired with total=500 means a torn read reached the caller", out)
	}
	if total != 500 {
		t.Errorf("total = %d, want 500", total)
	}
	if rowsCalls != 2 {
		t.Errorf("board-rows read ran %d time(s), want exactly 2 (one retry after the cycle-stamp mismatch)", rowsCalls)
	}
}
