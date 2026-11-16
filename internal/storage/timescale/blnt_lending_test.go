package timescale

import (
	"context"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	blntTestPool = "CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7"
	blntTestUser = "GBUSER00000000000000000000000000000000000000000000000000"
	blntBig      = "18446744073709551616" // 2^64, past int64
)

// TestListBLNTPools_ReadsPoolRoleOnly — the list is BLNT pool events only, and a nil net flow (multi-asset
// window) stays nil rather than reading as zero.
func TestListBLNTPools_ReadsPoolRoleOnly(t *testing.T) {
	seen := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"pool", "a24", "atot", "users", "seen", "sup", "bor"},
		rows: [][]driver.Value{{blntTestPool, int64(1), int64(4), int64(7), seen, blntBig, nil}},
	})
	got, err := store.ListBLNTPools(context.Background())
	if err != nil {
		t.Fatalf("ListBLNTPools: %v", err)
	}
	if len(got) != 1 || got[0].Pool != blntTestPool || got[0].AuctionsTotal != 4 || got[0].UniqueUsers30d != 7 ||
		!got[0].LastSeen.Equal(seen) || got[0].NetSupplied30d == nil || *got[0].NetSupplied30d != blntBig || got[0].NetBorrowed30d != nil {
		t.Fatalf("rows = %+v", got)
	}
	sql := conn.only(t).sql
	for _, s := range []string{"FROM blnt_events", "contract_role = 'pool'", "'fill_auction'", "'clawback'", "GROUP BY contract_id"} {
		if !strings.Contains(sql, s) {
			t.Errorf("ListBLNTPools SQL missing %q:\n%s", s, sql)
		}
	}
}

// TestBLNTPoolAssets_ScopedToThePoolContract — reserve assets come only from the asked contract's pool-role
// events, so a non-BLNT contract id yields none.
func TestBLNTPoolAssets_ScopedToThePoolContract(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{cols: []string{"asset"}, rows: [][]driver.Value{{"CUSDC"}, {"CXLM"}}})
	got, err := store.BLNTPoolAssets(context.Background(), blntTestPool)
	if err != nil {
		t.Fatalf("BLNTPoolAssets: %v", err)
	}
	if want := []string{"CUSDC", "CXLM"}; !reflect.DeepEqual(got, want) {
		t.Errorf("assets = %v, want %v", got, want)
	}
	stmt := conn.only(t)
	if stmt.arg(t, 1) != blntTestPool || !strings.Contains(stmt.sql, "contract_id = $1 AND contract_role = 'pool'") ||
		!strings.Contains(stmt.sql, "'set_reserve'") {
		t.Errorf("BLNTPoolAssets args/SQL:\n%s", stmt.sql)
	}
}

// TestBLNTReserveConfigMetadata_AppliedOnly — a queued config counts only when set_reserve follows it.
func TestBLNTReserveConfigMetadata_AppliedOnly(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{cols: []string{"asset", "metadata"}, rows: [][]driver.Value{{"CUSDC", `{"util":8000000}`}}})
	got, err := store.BLNTReserveConfigMetadata(context.Background(), blntTestPool)
	if err != nil {
		t.Fatalf("BLNTReserveConfigMetadata: %v", err)
	}
	if string(got["CUSDC"]) != `{"util":8000000}` {
		t.Errorf("metadata = %q", got["CUSDC"])
	}
	if sql := conn.only(t).sql; !strings.Contains(sql, "next_kind = 'set_reserve'") || !strings.Contains(sql, "FROM blnt_events") {
		t.Errorf("BLNTReserveConfigMetadata SQL:\n%s", sql)
	}
}

// TestBLNTPositionsByUser_FoldShape — the fold scans like Blend's, binds the user and the venue cap, and
// counts both auction moves (fill_auction bid/lot, bad_debt) as superseding a leg.
func TestBLNTPositionsByUser_FoldShape(t *testing.T) {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"pool", "asset", "hs", "sn", "ss", "st", "sts", "sl", "hb", "bn", "bs", "bt", "bts", "bl"},
		rows: [][]driver.Value{{
			blntTestPool, "CUSDC", true, blntBig, false, blntBig, at, int64(65_500_000),
			true, "0", true, "0", at, int64(65_500_001),
		}},
	})
	got, err := store.BLNTPositionsByUser(context.Background(), blntTestUser)
	if err != nil {
		t.Fatalf("BLNTPositionsByUser: %v", err)
	}
	want := BlendPositionFold{
		Pool: blntTestPool, Asset: "CUSDC", HasSupplyLeg: true, SupplyNet: blntBig, SupplyTokens: blntBig,
		SupplyLastActivity: at, SupplyLastLedger: 65_500_000, HasBorrowLeg: true, BorrowNet: "0", BorrowSuperseded: true,
		BorrowTokens: "0", BorrowLastActivity: at, BorrowLastLedger: 65_500_001,
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("fold = %+v, want %+v", got, want)
	}
	stmt := conn.only(t)
	if len(stmt.args) != 2 || stmt.args[0] != blntTestUser || fmt.Sprint(stmt.args[1]) != fmt.Sprint(positionsVenueLimit) {
		t.Errorf("args = %v", stmt.args)
	}
	for _, s := range []string{"event_kind = 'fill_auction'", "event_kind = 'bad_debt'", "filled_auction_data", "supply_burned", "WHERE user_address = $1"} {
		if !strings.Contains(stmt.sql, s) {
			t.Errorf("fold SQL missing %q", s)
		}
	}
}
