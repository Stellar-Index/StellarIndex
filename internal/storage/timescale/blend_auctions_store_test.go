// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
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

// ─── BlendPoolVersion ───────────────────────────────────

// TestBlendPoolVersion_KeysOnDeployingFactory — the reserve rate scale
// depends on the pool's generation, resolved from the blend-scoped
// protocol_contracts row; an unregistered pool is unknown, not V2.
func TestBlendPoolVersion_KeysOnDeployingFactory(t *testing.T) {
	for _, c := range []struct {
		name string
		rows [][]driver.Value
		want blend.PoolVersion
	}{
		{"v1 factory", [][]driver.Value{{blend.MainnetPoolFactoryV1}}, blend.PoolV1},
		{"v2 factory", [][]driver.Value{{blend.MainnetPoolFactory}}, blend.PoolV2},
		{"unregistered", nil, blend.PoolVersionUnknown},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, conn := newScriptedStore(t, scriptedResult{cols: []string{"factory_id"}, rows: c.rows})
			got, err := store.BlendPoolVersion(context.Background(), blendPool)
			if err != nil {
				t.Fatalf("BlendPoolVersion: %v", err)
			}
			if got != c.want {
				t.Errorf("BlendPoolVersion = %d, want %d", got, c.want)
			}
			stmt := conn.only(t)
			if !reflect.DeepEqual(stmt.args, []driver.Value{blend.SourceName, blendPool}) {
				t.Errorf("BlendPoolVersion args = %v, want [%s %s]", stmt.args, blend.SourceName, blendPool)
			}
		})
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
		// CA2-A10-correct-2: a queue_set_reserve alone is a proposal —
		// only one immediately followed (same asset) by set_reserve
		// is applied. A cancel_set_reserve, a superseding queue, or
		// nothing yet must all be excluded (see the executing
		// coverage in test/integration/blend_money_market_storage_test.go).
		"LEAD(event_kind)",
		"next_kind = 'set_reserve'",
	} {
		if !strings.Contains(stmt.sql, want) {
			t.Errorf("BlendReserveConfigs SQL missing %q — it must take the LATEST APPLIED config per reserve, not merely the latest queued one:\n%s", want, stmt.sql)
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
			// Flows across several reserve assets: the SQL withholds both sums.
			{"CPOOL3", int64(0), int64(0), int64(2), last, nil, nil},
		},
	})

	got, err := store.ListBlendPools(context.Background())
	if err != nil {
		t.Fatalf("ListBlendPools: %v", err)
	}
	want := []BlendPoolSummary{
		{
			Pool: blendPool, Auctions24h: 3, AuctionsTotal: 41, UniqueUsers30d: 12, LastSeen: last,
			NetSupplied30d: strptr(blendMaxI128), NetBorrowed30d: strptr("-250000000"),
		},
		{
			Pool: "CPOOL2", Auctions24h: 0, AuctionsTotal: 0, UniqueUsers30d: 1, LastSeen: last,
			NetSupplied30d: strptr("0"), NetBorrowed30d: strptr("0"),
		},
		{Pool: "CPOOL3", UniqueUsers30d: 2, LastSeen: last},
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
		// Base units of different reserve assets never add: the net-flow
		// sums are only emitted for a single-asset window.
		"COUNT(DISTINCT asset)",
		"CASE WHEN COALESCE(pos.flow_assets30, 0) <= 1",
	} {
		if !strings.Contains(stmt.sql, sub) {
			t.Errorf("ListBlendPools SQL missing %q:\n%s", sub, stmt.sql)
		}
	}
	// Net flow must be signed by direction: a withdraw subtracts from
	// supplied and a repay subtracts from borrowed. Summing them all
	// positive would render outflow as inflow.
	assertBlendFold(t, "ListBlendPools", stmt.sql, blendBothLegSigns)
}
