// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"context"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// End-to-end coverage for every internal/storage/timescale/mev.go store
// method over the scripted driver: the SQL each one issues, the arguments
// it binds, and the values it hands back. Before this file the six methods
// behind the MEV worker and the public /v1/mev feed had no SQL-level test
// at all — the worker's own tests drive a fakeScanner, so a regression in
// the placeholder order, the on-chain predicates or the row mapping was
// invisible.

const (
	mevUSDC     = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	mevXLMSAC   = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	mevTxHashA  = "aaa0000000000000000000000000000000000000000000000000000000000000"
	mevTxHashB  = "bbb0000000000000000000000000000000000000000000000000000000000000"
	mevScanCap  = 50_000
	mevListCap  = 500
	mevListDflt = 50
)

// ─── TradesForArbScan ─────────────────────────────────────────────────

// TestTradesForArbScan_ValuesAndParallelUSDSlice drives the read the
// arbitrage detector runs every tick. The two returned slices are
// POSITIONALLY PAIRED — trades[i]'s USD notional is usd[i] — and the
// second fixture row is a token/token leg with no USD basis, so an
// implementation that only appends priced legs would silently shift every
// later leg's notional onto the wrong trade.
func TestTradesForArbScan_ValuesAndParallelUSDSlice(t *testing.T) {
	ts := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{
			"source", "ledger", "tx_hash", "op_index", "ts",
			"base_asset", "quote_asset", "base_amount", "quote_amount",
			"maker", "taker", "usd",
		},
		rows: [][]driver.Value{
			{
				"sdex", int64(58_000_001), mevTxHashA, int64(3), ts,
				"native", mevUSDC, "1000000000", "1250000",
				"GMAKER", "GTAKER", "12.50",
			},
			{
				"soroswap", int64(58_000_001), mevTxHashA, int64(4), ts,
				mevUSDC, mevXLMSAC, "1250000", "1000000000",
				"", "GTAKER", "",
			},
		},
	})

	since := time.Date(2026, 8, 29, 11, 55, 0, 0, time.UTC)
	trades, usd, err := store.TradesForArbScan(context.Background(), since, 0)
	if err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	if len(trades) != 2 || len(usd) != 2 {
		t.Fatalf("got %d trades / %d usd entries, want 2 / 2", len(trades), len(usd))
	}
	if got := []string{usd[0], usd[1]}; !reflect.DeepEqual(got, []string{"12.50", ""}) {
		t.Errorf("usd = %#v, want [12.50 \"\"] positionally paired with trades", got)
	}

	first := trades[0]
	if first.Source != "sdex" || first.Ledger != 58_000_001 || first.TxHash != mevTxHashA || first.OpIndex != 3 {
		t.Errorf("trade identity = %+v", first)
	}
	if !first.Timestamp.Equal(ts) {
		t.Errorf("trade ts = %s, want %s", first.Timestamp, ts)
	}
	if got := first.Pair.Base.String(); got != "native" {
		t.Errorf("base asset = %q, want native", got)
	}
	if got := first.Pair.Quote.String(); got != mevUSDC {
		t.Errorf("quote asset = %q, want %q", got, mevUSDC)
	}
	if got := first.BaseAmount.String(); got != "1000000000" {
		t.Errorf("base_amount = %q, want 1000000000 (stroops, verbatim)", got)
	}
	if got := first.QuoteAmount.String(); got != "1250000" {
		t.Errorf("quote_amount = %q, want 1250000", got)
	}
	if first.Maker != "GMAKER" || first.Taker != "GTAKER" {
		t.Errorf("maker/taker = %q/%q, want GMAKER/GTAKER", first.Maker, first.Taker)
	}
	if trades[1].Maker != "" {
		t.Errorf("second trade maker = %q, want \"\" (COALESCE of a NULL maker)", trades[1].Maker)
	}

	// Window + cap binding: limit <= 0 means the method's own cap, not
	// an unbounded walk of the trades hypertable.
	stmt := conn.only(t)
	wantTime(t, stmt.arg(t, 1), since)
	if got := stmt.arg(t, 2); got != mevScanCap {
		t.Errorf("$2 = %v, want the default cap %d", got, mevScanCap)
	}
}

// TestTradesForArbScan_ExplicitLimitIsBound — a caller-supplied cap is
// what reaches the LIMIT, unchanged.
func TestTradesForArbScan_ExplicitLimitIsBound(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{cols: []string{
		"source", "ledger", "tx_hash", "op_index", "ts",
		"base_asset", "quote_asset", "base_amount", "quote_amount",
		"maker", "taker", "usd",
	}})
	if _, _, err := store.TradesForArbScan(context.Background(), time.Now(), 250); err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	if got := conn.only(t).arg(t, 2); got != 250 {
		t.Errorf("$2 = %v, want 250", got)
	}
}

// TestTradesForArbScanQueryShape pins the predicates that make this scan
// a MEV input rather than a trade dump:
//
//	ledger > 0 + taker present — off-chain CEX/FX prints carry neither,
//	  and an "atomic arbitrage" assembled across exchange prints would be
//	  a fabricated public accusation;
//	the (ledger, tx_hash, op_index) ordering — the grouping the detector
//	  relies on to see one transaction's legs together;
//	the XLM-leg USD fallback — SDEX arb legs quote XLM/token and carry a
//	  NULL usd_volume, so without it the feed reported "$0" notionals on
//	  real cycles (2026-06-19).
func TestTradesForArbScanQueryShape(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{cols: []string{
		"source", "ledger", "tx_hash", "op_index", "ts",
		"base_asset", "quote_asset", "base_amount", "quote_amount",
		"maker", "taker", "usd",
	}})
	if _, _, err := store.TradesForArbScan(context.Background(), time.Now(), 10); err != nil {
		t.Fatalf("TradesForArbScan: %v", err)
	}
	q := conn.only(t).sql

	for _, want := range []string{
		"ts > $1",
		"ledger > 0",
		"taker IS NOT NULL AND taker <> ''",
		"ORDER BY ledger ASC, tx_hash ASC, op_index ASC",
		"LIMIT $2",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("TradesForArbScan SQL missing %q:\n%s", want, q)
		}
	}
	if !strings.Contains(q, "base_amount / 1e7::numeric") ||
		!strings.Contains(q, "quote_amount / 1e7::numeric") {
		t.Error("TradesForArbScan lost the XLM-leg USD fallback — SDEX arb legs would report $0 notionals again")
	}
	if !strings.Contains(q, mevXLMSAC) {
		t.Errorf("the XLM-leg fallback must also recognise the native-XLM SAC %s", mevXLMSAC)
	}
}

// TestTradesForArbScan_UnparseableAssetIsAnError — a row whose asset
// string is not canonical fails the scan loudly instead of being skipped:
// a silently-dropped leg breaks the closed cycle the detector looks for.
func TestTradesForArbScan_UnparseableAssetIsAnError(t *testing.T) {
	store, _ := newScriptedStore(t, scriptedResult{
		cols: []string{
			"source", "ledger", "tx_hash", "op_index", "ts",
			"base_asset", "quote_asset", "base_amount", "quote_amount",
			"maker", "taker", "usd",
		},
		rows: [][]driver.Value{
			{
				"sdex", int64(1), mevTxHashA, int64(0), time.Now().UTC(),
				"", mevUSDC, "1", "1", "", "GTAKER", "",
			},
		},
	})
	trades, usd, err := store.TradesForArbScan(context.Background(), time.Now(), 10)
	if err == nil {
		t.Fatalf("unparseable base asset returned (%v, %v, nil), want an error", trades, usd)
	}
	if !strings.Contains(err.Error(), "TradesForArbScan base") {
		t.Errorf("error = %v, want it to name the offending base asset", err)
	}
}

// ─── OracleUpdatesForMEVScan ──────────────────────────────────────────

// TestOracleUpdatesForMEVScan_ValuesAndArgs covers the mapping + the
// window/cap binding. The raw:-row exclusion this query also carries is
// pinned separately in mev_shape_test.go.
func TestOracleUpdatesForMEVScan_ValuesAndArgs(t *testing.T) {
	ts := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "contract_id", "ledger", "tx_hash", "op_index", "asset", "quote", "ts"},
		rows: [][]driver.Value{
			{"reflector-dex", "CORACLE", int64(58_000_010), mevTxHashB, int64(1), mevUSDC, "fiat:USD", ts},
			{"reflector-cex", "", int64(58_000_011), mevTxHashB, int64(2), "native", "fiat:USD", ts},
		},
	})

	since := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	got, err := store.OracleUpdatesForMEVScan(context.Background(), since, 0)
	if err != nil {
		t.Fatalf("OracleUpdatesForMEVScan: %v", err)
	}
	want := []domain.MEVOracleRef{
		{Source: "reflector-dex", ContractID: "CORACLE", Ledger: 58_000_010, TxHash: mevTxHashB, OpIndex: 1, Asset: mevUSDC, Quote: "fiat:USD", Timestamp: ts},
		{Source: "reflector-cex", ContractID: "", Ledger: 58_000_011, TxHash: mevTxHashB, OpIndex: 2, Asset: "native", Quote: "fiat:USD", Timestamp: ts},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %+v\nwant %+v", got, want)
	}

	stmt := conn.only(t)
	wantTime(t, stmt.arg(t, 1), since)
	if v := stmt.arg(t, 2); v != mevScanCap {
		t.Errorf("$2 = %v, want the default cap %d", v, mevScanCap)
	}
}

// TestOracleUpdatesForMEVScan_ExcludesRawRowsFromTheIssuedSQL is the
// behavioural counterpart of mev_shape_test.go: it asserts the predicate
// on the statement the store ACTUALLY issues, not on the const it is
// built from, so a refactor that stops using the const cannot drop the
// guard unnoticed.
//
// Why it matters: this scan feeds the liquidation_cascade correlator,
// the one oracle_updates consumer with no asset keying — ANY oracle row
// inside a fill's ledger bracket becomes evidence. `raw:<symbol>` rows
// are unmapped oracle symbols recorded verbatim for capture totality
// (canonical.AssetOracleRaw): orientation-unknown reference data that
// must never become interpretation input. Without the predicate a busy
// unmapped feed manufactures cascade candidates, and the /v1/mev feed
// publicly accuses real accounts on that evidence.
//
// This assertion was RED on origin/main at 0f13aa14: PR #305's squash
// merge silently reverted PR #248's predicate.
func TestOracleUpdatesForMEVScan_ExcludesRawRowsFromTheIssuedSQL(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"source", "contract_id", "ledger", "tx_hash", "op_index", "asset", "quote", "ts"},
	})
	if _, err := store.OracleUpdatesForMEVScan(context.Background(), time.Now(), 10); err != nil {
		t.Fatalf("OracleUpdatesForMEVScan: %v", err)
	}
	q := conn.only(t).sql
	if !strings.Contains(q, "asset NOT LIKE 'raw:%'") {
		t.Errorf("the MEV oracle scan must exclude unmapped raw: rows — the cascade "+
			"correlator has no asset keying, so they would manufacture evidence:\n%s", q)
	}
	if !strings.Contains(q, "ledger > 0") {
		t.Errorf("the MEV oracle scan must stay on-chain-only (ledger > 0):\n%s", q)
	}
}

// ─── BlendFillsForMEVScan ─────────────────────────────────────────────

// TestBlendFillsForMEVScan_ValuesArgsAndLiquidationOnlyFilter. The
// cascade detector accuses accounts of clustered liquidation activity, so
// the filter is load-bearing: only `fill` events, and only the two
// LIQUIDATION auction types (0 = UserLiquidation, 1 = BadDebt). Blend's
// type 2 is an Interest auction — routine protocol housekeeping, not a
// liquidation — and letting it through would manufacture cascades out of
// ordinary interest auctions.
func TestBlendFillsForMEVScan_ValuesArgsAndLiquidationOnlyFilter(t *testing.T) {
	ts := time.Date(2026, 8, 29, 8, 15, 0, 0, time.UTC)
	store, conn := newScriptedStore(t, scriptedResult{
		cols: []string{"pool", "user_address", "filler", "auction_type", "ledger", "tx_hash", "op_index", "ts"},
		rows: [][]driver.Value{
			{"CPOOL", "GUSER", "GFILLER", int64(0), int64(58_000_020), mevTxHashA, int64(0), ts},
			{"CPOOL", "GUSER2", "", int64(1), int64(58_000_021), mevTxHashB, int64(1), ts},
		},
	})

	since := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	got, err := store.BlendFillsForMEVScan(context.Background(), since, 25)
	if err != nil {
		t.Fatalf("BlendFillsForMEVScan: %v", err)
	}
	want := []domain.MEVAuctionFill{
		{Pool: "CPOOL", User: "GUSER", Filler: "GFILLER", AuctionType: 0, Ledger: 58_000_020, TxHash: mevTxHashA, OpIndex: 0, Timestamp: ts},
		{Pool: "CPOOL", User: "GUSER2", Filler: "", AuctionType: 1, Ledger: 58_000_021, TxHash: mevTxHashB, OpIndex: 1, Timestamp: ts},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fills = %+v\nwant %+v", got, want)
	}

	stmt := conn.only(t)
	wantTime(t, stmt.arg(t, 1), since)
	if v := stmt.arg(t, 2); v != 25 {
		t.Errorf("$2 = %v, want 25", v)
	}
	if !strings.Contains(stmt.sql, "event_kind = 'fill'") {
		t.Errorf("BlendFillsForMEVScan must read only fill events:\n%s", stmt.sql)
	}
	if !strings.Contains(stmt.sql, "auction_type IN (0, 1)") {
		t.Errorf("BlendFillsForMEVScan must restrict to the liquidation auction types 0/1 "+
			"— Interest auctions (2) are not liquidations:\n%s", stmt.sql)
	}
}

// ─── InsertMEVEvent ───────────────────────────────────────────────────

// TestInsertMEVEvent_ArgsAndIdempotency: the detector re-scans an
// overlapping window every tick, so the write must be idempotent on
// dedup_key and must report whether the row was NEW — the worker's
// "flagged N events" counter and its logging both hang off that bool.
func TestInsertMEVEvent_ArgsAndIdempotency(t *testing.T) {
	ts := time.Date(2026, 8, 29, 7, 0, 0, 0, time.UTC)
	ev := domain.MEVStoredEvent{
		Kind:             "arbitrage",
		DetectedAtLedger: 58_000_030,
		Timestamp:        ts,
		TxHashes:         []string{mevTxHashA},
		Accounts:         []string{"GTAKER"},
		DedupKey:         "arbitrage:" + mevTxHashA + ":GTAKER",
		DetailJSON:       []byte(`{"legs":2}`),
	}

	store, conn := newScriptedStore(t,
		scriptedResult{rowsAffected: 1},
		scriptedResult{rowsAffected: 0},
	)

	inserted, err := store.InsertMEVEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("InsertMEVEvent: %v", err)
	}
	if !inserted {
		t.Error("first insert reported inserted=false; one affected row is a new event")
	}

	again, err := store.InsertMEVEvent(context.Background(), ev)
	if err != nil {
		t.Fatalf("InsertMEVEvent (repeat): %v", err)
	}
	if again {
		t.Error("a conflicting re-scan reported inserted=true; zero affected rows means the event already existed")
	}

	stmt := conn.stmts[0]
	wantTime(t, stmt.arg(t, 1), ts)
	if v := stmt.arg(t, 2); v != 58_000_030 {
		t.Errorf("$2 = %v, want the detected-at ledger 58000030", v)
	}
	if v := stmt.arg(t, 3); v != "arbitrage" {
		t.Errorf("$3 = %v, want the kind", v)
	}
	if v, ok := stmt.arg(t, 4).([]string); !ok || !reflect.DeepEqual(v, []string{mevTxHashA}) {
		t.Errorf("$4 = %#v, want the tx_hashes []string (pgx encodes Go slices as Postgres arrays)", stmt.arg(t, 4))
	}
	if v, ok := stmt.arg(t, 5).([]string); !ok || !reflect.DeepEqual(v, []string{"GTAKER"}) {
		t.Errorf("$5 = %#v, want the accounts []string", stmt.arg(t, 5))
	}
	if v := stmt.arg(t, 6); v != `{"legs":2}` {
		t.Errorf("$6 = %v, want the detail JSON text", v)
	}
	if v := stmt.arg(t, 7); v != ev.DedupKey {
		t.Errorf("$7 = %v, want the dedup key %q", v, ev.DedupKey)
	}
	if !strings.Contains(stmt.sql, "ON CONFLICT (dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING") {
		t.Errorf("InsertMEVEvent lost its idempotency arm — a re-scanned window would mint duplicate public accusations:\n%s", stmt.sql)
	}
}

// ─── PruneMEVEvents ───────────────────────────────────────────────────

// TestPruneMEVEvents_BoundAndCount: mev_events is a plain table with no
// retention policy, so this delete is the only thing bounding it. The
// cutoff is exclusive (rows exactly at `before` survive) and the removed
// count is returned for the worker's retention metric.
func TestPruneMEVEvents_BoundAndCount(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 17})

	before := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	removed, err := store.PruneMEVEvents(context.Background(), before)
	if err != nil {
		t.Fatalf("PruneMEVEvents: %v", err)
	}
	if removed != 17 {
		t.Errorf("removed = %d, want 17", removed)
	}
	stmt := conn.only(t)
	wantTime(t, stmt.arg(t, 1), before)
	if !strings.Contains(stmt.sql, "DELETE FROM mev_events WHERE detected_at < $1") {
		t.Errorf("PruneMEVEvents SQL = %q; want an exclusive detected_at < $1 delete", stmt.sql)
	}
}

// ─── ListMEVEvents ────────────────────────────────────────────────────

// TestListMEVEvents_KindFilterPlaceholderOrder. With a kind filter the
// statement binds ($1 = kind, $2 = limit); without one it binds ($1 =
// limit). Getting that pairing wrong is a runtime "invalid input syntax
// for integer" on the public feed, and no test would have seen it.
func TestListMEVEvents_KindFilterPlaceholderOrder(t *testing.T) {
	cols := []string{
		"event_id", "detected_at", "detected_at_ledger", "kind",
		"asset_id", "quote_id", "tx_hashes", "accounts", "detail", "profit_usd",
	}

	t.Run("with kind", func(t *testing.T) {
		store, conn := newScriptedStore(t, scriptedResult{cols: cols})
		if _, err := store.ListMEVEvents(context.Background(), "arbitrage", 25); err != nil {
			t.Fatalf("ListMEVEvents: %v", err)
		}
		stmt := conn.only(t)
		if v := stmt.arg(t, 1); v != "arbitrage" {
			t.Errorf("$1 = %v, want the kind", v)
		}
		if v := stmt.arg(t, 2); v != 25 {
			t.Errorf("$2 = %v, want the limit", v)
		}
		if !strings.Contains(stmt.sql, "WHERE kind = $1") || !strings.Contains(stmt.sql, "LIMIT $2") {
			t.Errorf("kind-filtered SQL = %s", stmt.sql)
		}
	})

	t.Run("without kind", func(t *testing.T) {
		store, conn := newScriptedStore(t, scriptedResult{cols: cols})
		if _, err := store.ListMEVEvents(context.Background(), "", 25); err != nil {
			t.Fatalf("ListMEVEvents: %v", err)
		}
		stmt := conn.only(t)
		if len(stmt.args) != 1 {
			t.Fatalf("unfiltered read bound %d args, want 1: %#v", len(stmt.args), stmt.args)
		}
		if v := stmt.arg(t, 1); v != 25 {
			t.Errorf("$1 = %v, want the limit", v)
		}
		if strings.Contains(stmt.sql, "WHERE kind") {
			t.Errorf("empty kind must not filter:\n%s", stmt.sql)
		}
		if !strings.Contains(stmt.sql, "LIMIT $1") {
			t.Errorf("unfiltered SQL = %s", stmt.sql)
		}
	})

	// Newest-first is the feed's contract either way.
	store, conn := newScriptedStore(t, scriptedResult{cols: cols})
	if _, err := store.ListMEVEvents(context.Background(), "", 25); err != nil {
		t.Fatalf("ListMEVEvents: %v", err)
	}
	if !strings.Contains(conn.only(t).sql, "ORDER BY detected_at DESC") {
		t.Error("ListMEVEvents must serve newest-first")
	}
}

// TestListMEVEvents_LimitNormalisation pins the store-side bound. Out of
// range means the 50-row default, NOT a silent clamp to the ceiling —
// the same convention as ListIssuers / ListFreezeEvents /
// ListDivergenceLatest — and the ceiling itself must still be reachable.
// The doc comment used to claim "capped at 500", which is what a caller
// asking for 1000 would NOT get.
func TestListMEVEvents_LimitNormalisation(t *testing.T) {
	cols := []string{
		"event_id", "detected_at", "detected_at_ledger", "kind",
		"asset_id", "quote_id", "tx_hashes", "accounts", "detail", "profit_usd",
	}
	for _, tc := range []struct{ in, want int }{
		{0, mevListDflt},
		{-1, mevListDflt},
		{mevListCap + 1, mevListDflt},
		{10_000, mevListDflt},
		{1, 1},
		{mevListCap, mevListCap},
	} {
		store, conn := newScriptedStore(t, scriptedResult{cols: cols})
		if _, err := store.ListMEVEvents(context.Background(), "", tc.in); err != nil {
			t.Fatalf("ListMEVEvents(limit=%d): %v", tc.in, err)
		}
		if got := conn.only(t).arg(t, 1); got != tc.want {
			t.Errorf("ListMEVEvents(limit=%d) bound %v, want %d", tc.in, got, tc.want)
		}
	}
}

// TestListMEVEvents_RowMapping covers the wire-visible shape of /v1/mev:
// the Postgres text[] columns decode through internal/pgarray (a plain
// *[]string scan destination is a runtime error under pgx), an empty
// array stays a non-nil empty slice so the JSON is `[]` and not `null`,
// and a NULL profit_usd arrives as "" so the handler emits JSON null.
func TestListMEVEvents_RowMapping(t *testing.T) {
	detected := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	store, _ := newScriptedStore(t, scriptedResult{
		cols: []string{
			"event_id", "detected_at", "detected_at_ledger", "kind",
			"asset_id", "quote_id", "tx_hashes", "accounts", "detail", "profit_usd",
		},
		rows: [][]driver.Value{
			{
				"5f1b9a6e-0000-4000-8000-000000000001", detected, int64(58_000_040), "arbitrage",
				"", "", "{" + mevTxHashA + "," + mevTxHashB + "}", "{}", `{"legs":3}`, "",
			},
		},
	})

	rows, err := store.ListMEVEvents(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListMEVEvents: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.EventID != "5f1b9a6e-0000-4000-8000-000000000001" || r.Kind != "arbitrage" {
		t.Errorf("identity = %+v", r)
	}
	if !r.DetectedAt.Equal(detected) || r.DetectedAtLedger != 58_000_040 {
		t.Errorf("detected at %s / ledger %d", r.DetectedAt, r.DetectedAtLedger)
	}
	if want := []string{mevTxHashA, mevTxHashB}; !reflect.DeepEqual(r.TxHashes, want) {
		t.Errorf("tx_hashes = %#v, want %#v", r.TxHashes, want)
	}
	if r.Accounts == nil || len(r.Accounts) != 0 {
		t.Errorf("accounts = %#v, want a non-nil empty slice so the feed serves [] not null", r.Accounts)
	}
	if r.Detail != `{"legs":3}` {
		t.Errorf("detail = %q, want the raw jsonb text", r.Detail)
	}
	if r.ProfitUSD != "" {
		t.Errorf("profit_usd = %q, want \"\" for the NULL the handler renders as null", r.ProfitUSD)
	}
}
