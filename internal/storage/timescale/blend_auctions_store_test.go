// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// End-to-end coverage for every blend_auctions.go STORE method over the
// scripted driver. blend_auctions_test.go covers the two JSON helpers;
// the seven methods that actually talk to Postgres — the three auction
// writers behind the pipeline sink and the four readers behind
// /v1/lending — had none, so the INV-3 generation guard, the i128
// precision of the bid/lot amounts and the read-side null handling were
// all unpinned.

const (
	blendPool     = "CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PUYYAK7SDECESAQXWCJHDIVDGY6L"
	blendUser     = "GBUSER00000000000000000000000000000000000000000000000000"
	blendFiller   = "GBFILL00000000000000000000000000000000000000000000000000"
	blendTxHash   = "ccc0000000000000000000000000000000000000000000000000000000000000"
	blendGen      = int64(9)
	blendMaxI128  = "170141183460469231731687303715884105727" // 2^127-1 — far past int64
	blendBigFillP = "99999999999999999999"                    // i128 fill_percent, past int64
)

// blendBidLot builds a two-entry bid map whose first amount is the
// maximum i128 — the value that proves nothing on the write path narrows
// through float64 or int64 (ADR-0003).
func blendBidLot(t *testing.T) []blend.AssetAmount {
	t.Helper()
	amt, ok := new(big.Int).SetString(blendMaxI128, 10)
	if !ok {
		t.Fatalf("bad fixture amount %q", blendMaxI128)
	}
	return []blend.AssetAmount{
		{Asset: canonicalSorobanAsset(t, "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"), Amount: amt},
		{Asset: canonicalSorobanAsset(t, "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"), Amount: big.NewInt(7)},
	}
}

// assertGenerationGuardedUpsert pins the INV-3 (migration 0110) shape
// shared by all three writers: a corrected re-derive lands in place, and
// a live gen-0 replay can never revert it. The pre-0110 `DO NOTHING` is
// the re-derive trap this replaced, so its return is a failure.
func assertGenerationGuardedUpsert(t *testing.T, sql, eventKind string) {
	t.Helper()
	if strings.Contains(sql, "DO NOTHING") {
		t.Errorf("%s writer is back on ON CONFLICT DO NOTHING — the INV-3 re-derive trap:\n%s", eventKind, sql)
	}
	if !strings.Contains(sql, "ON CONFLICT (ledger, tx_hash, op_index, ts, event_kind, event_index) DO UPDATE SET") {
		t.Errorf("%s writer lost the event_index-discriminated conflict key (migration 0058 / F-1324):\n%s", eventKind, sql)
	}
	if !strings.Contains(sql, "WHERE blend_auctions.derive_generation <= EXCLUDED.derive_generation") {
		t.Errorf("%s writer lost the derive_generation guard — a live gen-0 replay could revert a corrected re-derive:\n%s", eventKind, sql)
	}
	if !strings.Contains(sql, "'"+eventKind+"'") {
		t.Errorf("writer must stamp event_kind '%s':\n%s", eventKind, sql)
	}
}

// ─── InsertBlendNewAuction ────────────────────────────────────────────

func TestInsertBlendNewAuction_ArgsAndGenerationGuard(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 1})
	store.SetDeriveGeneration(blendGen)

	bid := blendBidLot(t)
	err := store.InsertBlendNewAuction(context.Background(), blend.NewAuctionEvent{
		Pool: blendPool, AuctionType: 1, User: blendUser, Percent: 45,
		Data:    blend.AuctionData{Bid: bid, Lot: bid, Block: 58_000_000},
		Ledger:  58_000_050,
		TxHash:  blendTxHash,
		OpIndex: 2, EventIndex: 3,
		Timestamp: ts,
	})
	if err != nil {
		t.Fatalf("InsertBlendNewAuction: %v", err)
	}

	stmt := conn.only(t)
	for _, tc := range []struct {
		n    int
		want driver.Value
	}{
		{1, blendPool},
		{2, 1},
		{3, blendUser},
		{4, 58_000_050},
		{5, blendTxHash},
		{6, 2},
		{8, 3},
		{9, 45},
		{10, 58_000_000},
		{13, blendGen},
	} {
		if got := stmt.arg(t, tc.n); got != tc.want {
			t.Errorf("$%d = %#v, want %#v", tc.n, got, tc.want)
		}
	}
	wantTime(t, stmt.arg(t, 7), ts)

	// bid/lot ride as JSONB bytes with the i128 amount as a decimal
	// string — the whole point of the encode helper.
	for _, n := range []int{11, 12} {
		raw, ok := stmt.arg(t, n).([]byte)
		if !ok {
			t.Fatalf("$%d = %T, want the JSONB []byte", n, stmt.arg(t, n))
		}
		if !strings.Contains(string(raw), blendMaxI128) {
			t.Errorf("$%d = %s; the max-i128 amount %s must survive verbatim (ADR-0003)", n, raw, blendMaxI128)
		}
	}
	assertGenerationGuardedUpsert(t, stmt.sql, "new")
}

// TestInsertBlendNewAuction_EmptyBidLotBindNULL — a body with no
// amounts must bind SQL NULL, not an empty JSON array: the columns are
// nullable precisely so "no amounts" is distinguishable from "[]".
func TestInsertBlendNewAuction_EmptyBidLotBindNULL(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 1})
	if err := store.InsertBlendNewAuction(context.Background(), blend.NewAuctionEvent{
		Pool: blendPool, User: blendUser, TxHash: blendTxHash, Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("InsertBlendNewAuction: %v", err)
	}
	stmt := conn.only(t)
	for _, n := range []int{11, 12} {
		got := stmt.arg(t, n)
		raw, isBytes := got.([]byte)
		if !isBytes || raw != nil {
			t.Errorf("$%d = %#v, want a nil []byte — the encoder returns nil for an empty "+
				"bid/lot and pgx writes that as SQL NULL, which is how a delete-shaped "+
				"body stays distinguishable from an empty JSON array", n, got)
		}
	}
}

// ─── InsertBlendFillAuction ───────────────────────────────────────────

// TestInsertBlendFillAuction_I128FillPercentIsText. fill_percent is an
// i128 and the column is NUMERIC: it crosses as its decimal STRING with
// an explicit ::numeric cast, never through a Go numeric type that would
// round it.
func TestInsertBlendFillAuction_I128FillPercentIsText(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 5, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 1})
	store.SetDeriveGeneration(blendGen)

	pct, ok := new(big.Int).SetString(blendBigFillP, 10)
	if !ok {
		t.Fatalf("bad fixture fill percent")
	}
	bid := blendBidLot(t)
	err := store.InsertBlendFillAuction(context.Background(), blend.FillAuctionEvent{
		Pool: blendPool, AuctionType: 0, User: blendUser,
		Filler: blendFiller, FillPercent: pct,
		Data:    blend.AuctionData{Bid: bid, Lot: bid, Block: 58_000_000},
		Ledger:  58_000_051,
		TxHash:  blendTxHash,
		OpIndex: 4, EventIndex: 1,
		Timestamp: ts,
	})
	if err != nil {
		t.Fatalf("InsertBlendFillAuction: %v", err)
	}

	stmt := conn.only(t)
	if got := stmt.arg(t, 9); got != blendFiller {
		t.Errorf("$9 = %#v, want the filler", got)
	}
	if got := stmt.arg(t, 10); got != blendBigFillP {
		t.Errorf("$10 = %#v, want the i128 fill_percent as the decimal string %s", got, blendBigFillP)
	}
	if got := stmt.arg(t, 8); got != 1 {
		t.Errorf("$8 = %#v, want the event_index", got)
	}
	if got := stmt.arg(t, 14); got != blendGen {
		t.Errorf("$14 = %#v, want the derive generation %d", got, blendGen)
	}
	wantTime(t, stmt.arg(t, 7), ts)
	if !strings.Contains(stmt.sql, "$10::numeric") {
		t.Errorf("fill_percent must be cast to numeric explicitly:\n%s", stmt.sql)
	}
	assertGenerationGuardedUpsert(t, stmt.sql, "fill")
}

// ─── InsertBlendDeleteAuction ─────────────────────────────────────────

func TestInsertBlendDeleteAuction_ArgsAndGenerationGuard(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 10, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 1})
	store.SetDeriveGeneration(blendGen)

	err := store.InsertBlendDeleteAuction(context.Background(), blend.DeleteAuctionEvent{
		Pool: blendPool, AuctionType: 2, User: blendUser,
		Ledger: 58_000_052, TxHash: blendTxHash, OpIndex: 0, EventIndex: 5,
		Timestamp: ts,
	})
	if err != nil {
		t.Fatalf("InsertBlendDeleteAuction: %v", err)
	}

	stmt := conn.only(t)
	if len(stmt.args) != 9 {
		t.Fatalf("delete bound %d args, want 9 (no body amounts): %#v", len(stmt.args), stmt.args)
	}
	for _, tc := range []struct {
		n    int
		want driver.Value
	}{
		{1, blendPool},
		{2, 2},
		{3, blendUser},
		{4, 58_000_052},
		{5, blendTxHash},
		{6, 0},
		{8, 5},
		{9, blendGen},
	} {
		if got := stmt.arg(t, tc.n); got != tc.want {
			t.Errorf("$%d = %#v, want %#v", tc.n, got, tc.want)
		}
	}
	wantTime(t, stmt.arg(t, 7), ts)
	assertGenerationGuardedUpsert(t, stmt.sql, "delete")
}

// ─── LatestBlendAuctionEvent ──────────────────────────────────────────

var blendAuctionRowCols = []string{
	"pool", "auction_type", "user_address",
	"ledger", "tx_hash", "op_index", "ts",
	"event_kind", "percent", "filler", "fill_percent",
	"block", "bid", "lot",
}

// TestLatestBlendAuctionEvent_FillRowMapping — the nullable columns are
// pointers so a caller can tell "absent" from "zero". A fill row has a
// filler + fill_percent and no percent; the i128 amounts come back as
// decimal strings.
func TestLatestBlendAuctionEvent_FillRowMapping(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 15, 0, 0, time.UTC)
	bidJSON := `[{"asset":"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA","amount":"` + blendMaxI128 + `"}]`
	store, conn := newScriptedStore(t, scriptedResult{
		cols: blendAuctionRowCols,
		rows: [][]driver.Value{{
			blendPool, int64(1), blendUser,
			int64(58_000_060), blendTxHash, int64(7), ts,
			"fill", nil, blendFiller, blendBigFillP,
			int64(58_000_000), bidJSON, nil,
		}},
	})

	got, err := store.LatestBlendAuctionEvent(context.Background(), blendPool, 1, blendUser)
	if err != nil {
		t.Fatalf("LatestBlendAuctionEvent: %v", err)
	}
	if got.Pool != blendPool || got.AuctionType != 1 || got.User != blendUser {
		t.Errorf("identity = %+v", got)
	}
	if got.Ledger != 58_000_060 || got.OpIndex != 7 || got.EventKind != "fill" || !got.Timestamp.Equal(ts) {
		t.Errorf("row = %+v", got)
	}
	if got.Percent != nil {
		t.Errorf("percent = %v, want nil for a fill row", *got.Percent)
	}
	if got.Filler == nil || *got.Filler != blendFiller {
		t.Errorf("filler = %v, want %q", got.Filler, blendFiller)
	}
	if got.FillPercent == nil || *got.FillPercent != blendBigFillP {
		t.Errorf("fill_percent = %v, want the i128 string %s", got.FillPercent, blendBigFillP)
	}
	if got.Block == nil || *got.Block != 58_000_000 {
		t.Errorf("block = %v, want 58000000", got.Block)
	}
	want := []BlendAssetAmount{{
		Asset:  "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Amount: blendMaxI128,
	}}
	if !reflect.DeepEqual(got.Bid, want) {
		t.Errorf("bid = %#v, want %#v", got.Bid, want)
	}
	if got.Lot != nil {
		t.Errorf("lot = %#v, want nil for a NULL column", got.Lot)
	}

	stmt := conn.only(t)
	if a, b, c := stmt.arg(t, 1), stmt.arg(t, 2), stmt.arg(t, 3); a != blendPool || b != 1 || c != blendUser {
		t.Errorf("args = (%v, %v, %v), want (pool, auction_type, user)", a, b, c)
	}
	if !strings.Contains(stmt.sql, "ORDER BY ledger DESC") {
		t.Errorf("latest-event read must order newest ledger first:\n%s", stmt.sql)
	}
}

// TestLatestBlendAuctionEvent_NewRowMapping — a `new` row carries a
// percent and no filler; the reverse of the fill case.
func TestLatestBlendAuctionEvent_NewRowMapping(t *testing.T) {
	ts := time.Date(2026, 8, 29, 10, 20, 0, 0, time.UTC)
	store, _ := newScriptedStore(t, scriptedResult{
		cols: blendAuctionRowCols,
		rows: [][]driver.Value{{
			blendPool, int64(0), blendUser,
			int64(58_000_061), blendTxHash, int64(1), ts,
			"new", int64(45), nil, nil,
			int64(58_000_001), nil, nil,
		}},
	})
	got, err := store.LatestBlendAuctionEvent(context.Background(), blendPool, 0, blendUser)
	if err != nil {
		t.Fatalf("LatestBlendAuctionEvent: %v", err)
	}
	if got.Percent == nil || *got.Percent != 45 {
		t.Errorf("percent = %v, want 45", got.Percent)
	}
	if got.Filler != nil || got.FillPercent != nil {
		t.Errorf("filler/fill_percent = %v/%v, want nil on a new-auction row", got.Filler, got.FillPercent)
	}
}

// TestLatestBlendAuctionEvent_NoRowsIsErrNotFound — the sentinel the
// callers switch on, not a bare sql.ErrNoRows leaking the driver.
func TestLatestBlendAuctionEvent_NoRowsIsErrNotFound(t *testing.T) {
	store, _ := newScriptedStore(t, scriptedResult{cols: blendAuctionRowCols})
	got, err := store.LatestBlendAuctionEvent(context.Background(), blendPool, 0, blendUser)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v (row %+v), want ErrNotFound", err, got)
	}
}

// ─── BlendPoolAssets ──────────────────────────────────────────────────

// TestBlendPoolAssets_OrderAndArgs — the reserve set the ADR-0039
// pool-state reader walks, busiest first. The order is the query's, so
// the reader must not re-sort it away.
func TestBlendPoolAssets_OrderAndArgs(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"asset"},
		rows: [][]driver.Value{{"CUSDC"}, {"CXLM"}, {"CEURC"}},
	})
	got, err := store.BlendPoolAssets(context.Background(), blendPool)
	if err != nil {
		t.Fatalf("BlendPoolAssets: %v", err)
	}
	if want := []string{"CUSDC", "CXLM", "CEURC"}; !reflect.DeepEqual(got, want) {
		t.Errorf("assets = %#v, want %#v in query order", got, want)
	}
	stmt := conn.only(t)
	if v := stmt.arg(t, 1); v != blendPool {
		t.Errorf("$1 = %v, want the pool", v)
	}
	if !strings.Contains(stmt.sql, "FROM blend_positions") || !strings.Contains(stmt.sql, "ORDER BY count(*) DESC") {
		t.Errorf("BlendPoolAssets must rank the pool's position-event assets by volume:\n%s", stmt.sql)
	}
}

// ─── BlendReserveConfigs ──────────────────────────────────────────────

// TestBlendReserveConfigs_ParsesAndSkipsUnparseable — one bad metadata
// blob must cost that reserve its config, not the whole pool's APY
// inputs.
func TestBlendReserveConfigs_ParsesAndSkipsUnparseable(t *testing.T) {
	good := `{"index":0,"decimals":7,"c_factor":9000000,"l_factor":9500000,` +
		`"util":8000000,"max_util":9500000,"r_base":50000,"r_one":500000,` +
		`"r_two":5000000,"r_three":15000000,"reactivity":200,` +
		`"supply_cap":"` + blendMaxI128 + `","enabled":true}`
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"asset", "metadata"},
		rows: [][]driver.Value{
			{"CUSDC", good},
			{"CBROKEN", `{"util":`},
		},
	})

	got, err := store.BlendReserveConfigs(context.Background(), blendPool)
	if err != nil {
		t.Fatalf("BlendReserveConfigs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("configs = %#v, want only the parseable reserve", got)
	}
	cfg, ok := got["CUSDC"]
	if !ok {
		t.Fatalf("configs missing CUSDC: %#v", got)
	}
	if cfg.Util != 8_000_000 || cfg.MaxUtil != 9_500_000 || cfg.RBase != 50_000 ||
		cfg.ROne != 500_000 || cfg.RTwo != 5_000_000 || cfg.RThree != 15_000_000 ||
		cfg.Reactivity != 200 || cfg.Decimals != 7 || !cfg.Enabled {
		t.Errorf("rate-model params = %+v", cfg)
	}
	if cfg.SupplyCap == nil || cfg.SupplyCap.String() != blendMaxI128 {
		t.Errorf("supply_cap = %v, want the i128 %s verbatim", cfg.SupplyCap, blendMaxI128)
	}

	stmt := conn.only(t)
	if v := stmt.arg(t, 1); v != blendPool {
		t.Errorf("$1 = %v, want the pool contract", v)
	}
	for _, want := range []string{
		"DISTINCT ON (asset)",
		"event_kind = 'queue_set_reserve'",
		"ORDER BY asset, ledger_close_time DESC",
	} {
		if !strings.Contains(stmt.sql, want) {
			t.Errorf("BlendReserveConfigs SQL missing %q — it must take the LATEST config per reserve:\n%s", want, stmt.sql)
		}
	}
}

// ─── ListBlendPools ───────────────────────────────────────────────────

// TestListBlendPools_ValuesAndWindows covers /v1/lending/pools' listing
// row. The net-flow figures are 30-day event DELTAS in token base units,
// not TVL, and the sign convention (supply/withdraw, borrow/repay) is
// the substance of the number, so both directions are pinned.
func TestListBlendPools_ValuesAndWindows(t *testing.T) {
	last := time.Date(2026, 8, 29, 10, 30, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"pool", "a24", "atot", "users30", "last_seen", "net_supplied", "net_borrowed"},
		rows: [][]driver.Value{
			{blendPool, int64(3), int64(41), int64(12), last, blendMaxI128, "-250000000"},
			{"CPOOL2", int64(0), int64(0), int64(1), last, "0", "0"},
		},
	})

	got, err := store.ListBlendPools(context.Background())
	if err != nil {
		t.Fatalf("ListBlendPools: %v", err)
	}
	want := []BlendPoolSummary{
		{
			Pool: blendPool, Auctions24h: 3, AuctionsTotal: 41, UniqueUsers30d: 12, LastSeen: last,
			NetSupplied30d: blendMaxI128, NetBorrowed30d: "-250000000",
		},
		{
			Pool: "CPOOL2", Auctions24h: 0, AuctionsTotal: 0, UniqueUsers30d: 1, LastSeen: last,
			NetSupplied30d: "0", NetBorrowed30d: "0",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pools = %+v\nwant %+v", got, want)
	}

	stmt := conn.only(t)
	if len(stmt.args) != 0 {
		t.Errorf("ListBlendPools bound %d args, want none", len(stmt.args))
	}
	for _, sub := range []string{
		"SELECT DISTINCT pool FROM blend_auctions",
		"SELECT DISTINCT pool FROM blend_positions",
		"INTERVAL '24 hours'",
		"INTERVAL '30 days'",
		"ORDER BY COALESCE(auc.atot, 0) DESC, p.pool ASC",
	} {
		if !strings.Contains(stmt.sql, sub) {
			t.Errorf("ListBlendPools SQL missing %q:\n%s", sub, stmt.sql)
		}
	}
	// Net flow must be signed by direction: a withdraw subtracts from
	// supplied and a repay subtracts from borrowed. Summing them all
	// positive would render outflow as inflow.
	for _, sub := range []string{
		"WHEN event_kind IN ('supply','supply_collateral')    THEN token_amount",
		"WHEN event_kind IN ('withdraw','withdraw_collateral') THEN -token_amount",
		"WHEN event_kind = 'borrow' THEN token_amount",
		"WHEN event_kind = 'repay'  THEN -token_amount",
	} {
		if !strings.Contains(stmt.sql, sub) {
			t.Errorf("ListBlendPools net-flow sign convention missing %q:\n%s", sub, stmt.sql)
		}
	}
}
