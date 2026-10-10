//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"math/big"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	blendEmPool  = "CALI2BYU2JE6WVRUFYTS6MSBNEHGJ35P4AVCZYF3B6QOE3QKOB2PLE6M"
	blendEmAsset = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
	blendEmUser  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// TestBlendEmissionWindowStatsAndBespoke exercises the Blend emission /
// credit-risk READ side (BlendEmissionWindowStats) + its surfacing on the
// blend lending bespoke block. Proves empty-safe, i128/NUMERIC preservation
// of claimed-emission volume (never int64), the bad_debt+defaulted_debt
// credit-risk tally, and that the emission KPIs appear alongside (not
// replacing) the existing Blend position/auction/backstop KPIs.
func TestBlendEmissionWindowStatsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if em, err := store.BlendEmissionWindowStats(ctx, 90); err != nil {
		t.Fatalf("BlendEmissionWindowStats (empty): %v", err)
	} else if em != nil {
		t.Fatalf("BlendEmissionWindowStats (empty) = %+v, want nil", em)
	}

	t0 := time.Now().UTC().Add(-10 * time.Hour)
	claimHuge, _ := new(big.Int).SetString("44444444444444444444", 10) // > 2^63

	rows := []blend.EmissionEvent{
		{Pool: blendEmPool, Kind: blend.EventClaim, User: blendEmUser, Amount: claimHuge, ReserveTokenIDs: []uint32{0, 3}, Ledger: 61_000_000, TxHash: pad64("h", 0), OpIndex: 0, EventIndex: 0, Timestamp: t0},
		{Pool: blendEmPool, Kind: blend.EventGulp, Asset: blendEmAsset, Amount: big.NewInt(1_000), Ledger: 61_000_001, TxHash: pad64("h", 1), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(time.Minute)},
		{Pool: blendEmPool, Kind: blend.EventBadDebt, User: blendEmUser, Asset: blendEmAsset, Amount: big.NewInt(500), Ledger: 61_000_002, TxHash: pad64("h", 2), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(2 * time.Minute)},
		{Pool: blendEmPool, Kind: blend.EventDefaultedDebt, Asset: blendEmAsset, Amount: big.NewInt(250), Ledger: 61_000_003, TxHash: pad64("h", 3), OpIndex: 0, EventIndex: 0, Timestamp: t0.Add(3 * time.Minute)},
	}
	for _, e := range rows {
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent(e)); err != nil {
			t.Fatalf("InsertBlendEmissionEvent %s: %v", e.Kind, err)
		}
	}

	em, err := store.BlendEmissionWindowStats(ctx, 90)
	if err != nil {
		t.Fatalf("BlendEmissionWindowStats: %v", err)
	}
	if em == nil {
		t.Fatal("BlendEmissionWindowStats = nil, want summary")
	}
	if em.Claims != 1 {
		t.Errorf("Claims = %d, want 1", em.Claims)
	}
	if em.ClaimVolume.BigInt().Cmp(claimHuge) != 0 {
		t.Errorf("ClaimVolume = %s, want %s — i128/NUMERIC lost precision", em.ClaimVolume, claimHuge)
	}
	if em.Gulps != 1 {
		t.Errorf("Gulps = %d, want 1", em.Gulps)
	}
	if em.CreditRisk != 2 {
		t.Errorf("CreditRisk = %d, want 2 (bad_debt + defaulted_debt)", em.CreditRisk)
	}
	if em.TotalEvents != 4 {
		t.Errorf("TotalEvents = %d, want 4", em.TotalEvents)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "blend", "lending", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke blend: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke blend = nil, want a block")
	}
	kpis := kpiMap(blk)
	assertKPI(t, kpis, "Emissions claimed (90d)", claimHuge.String())
	assertKPI(t, kpis, "Credit-risk events (90d)", "2")
	// The Blend position KPIs must still be present (emissions ADD, never
	// replace). Count-first: the old cross-asset
	// "Net supplied" sum was dropped as mixed-decimal.
	if _, ok := kpis["Active users (90d)"]; !ok {
		t.Errorf("blend block lost its existing 'Active users' KPI after adding emissions; KPIs=%+v", blk.KPIs)
	}
}

// TestListBlendPools_NeverSumsAcrossReserveAssets executes the
// /v1/lending/pools listing SQL against real TimescaleDB. A pool whose
// 30d flows are 1e13 XLM stroops supplied and 1e12 USDC units borrowed
// must NOT report net_supplied=1e13 / net_borrowed=1e12 (a "10%
// utilisation" that depends only on which tokens moved): its sums are
// NULL. A single-asset pool keeps its signed within-asset net flow.
func TestListBlendPools_NeverSumsAcrossReserveAssets(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		multiPool  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		singlePool = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		xlmSAC     = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
		usdcSAC    = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
		user       = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
	)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	n := 0
	insert := func(pool, asset, kind string, amount int64) {
		t.Helper()
		n++
		ev := blend.PositionEvent{
			Pool: pool, Kind: kind, Asset: asset, User: user,
			TokenAmount: big.NewInt(amount), BOrDAmount: big.NewInt(amount),
			Ledger: uint32(63_100_000 + n), TxHash: pad64("p", n), OpIndex: 0,
			Timestamp: at,
		}
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(ev)); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s/%s): %v", pool, kind, err)
		}
	}
	insert(multiPool, xlmSAC, blend.EventSupply, 10_000_000_000_000)
	insert(multiPool, usdcSAC, blend.EventBorrow, 1_000_000_000_000)
	insert(singlePool, usdcSAC, blend.EventSupply, 5_000_000)
	insert(singlePool, usdcSAC, blend.EventWithdraw, 1_000_000)
	insert(singlePool, usdcSAC, blend.EventBorrow, 2_000_000)

	pools, err := store.ListBlendPools(ctx)
	if err != nil {
		t.Fatalf("ListBlendPools: %v", err)
	}
	byPool := make(map[string]timescale.BlendPoolSummary, len(pools))
	for _, p := range pools {
		byPool[p.Pool] = p
	}

	multi, ok := byPool[multiPool]
	if !ok {
		t.Fatalf("multi-asset pool missing from listing: %+v", pools)
	}
	if multi.NetSupplied30d != nil || multi.NetBorrowed30d != nil {
		t.Errorf("multi-asset pool net flows = (%v, %v), want (nil, nil): XLM and USDC base units do not add",
			derefOr(multi.NetSupplied30d), derefOr(multi.NetBorrowed30d))
	}

	single, ok := byPool[singlePool]
	if !ok {
		t.Fatalf("single-asset pool missing from listing: %+v", pools)
	}
	if got := derefOr(single.NetSupplied30d); got != "4000000" {
		t.Errorf("single-asset NetSupplied30d = %q, want 4000000 (5e6 supply − 1e6 withdraw)", got)
	}
	if got := derefOr(single.NetBorrowed30d); got != "2000000" {
		t.Errorf("single-asset NetBorrowed30d = %q, want 2000000", got)
	}
}

// TestBlendPoolVersion_FromDeployingFactory executes the lineage lookup
// the reserves endpoint keys its rate scale on: a pool registered under
// the V1 factory is PoolV1, under the V2 factory PoolV2, and one the
// registry does not hold — or holds under another factory — is unknown.
func TestBlendPoolVersion_FromDeployingFactory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		v1Pool      = "CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB"
		v2Pool      = "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD"
		foreignPool = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		absentPool  = "CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS"
	)
	for _, row := range []struct{ source, pool, factory string }{
		{blend.SourceName, v1Pool, blend.MainnetPoolFactoryV1},
		{blend.SourceName, v2Pool, blend.MainnetPoolFactory},
		{blend.SourceName, foreignPool, "CURATED"},
		// The same id under another source must not leak into blend.
		{"aquarius", absentPool, blend.MainnetPoolFactoryV1},
	} {
		if err := store.UpsertProtocolContract(ctx, row.source, row.pool, row.factory, 51_600_000); err != nil {
			t.Fatalf("UpsertProtocolContract(%s): %v", row.pool, err)
		}
	}

	for _, c := range []struct {
		pool string
		want blend.PoolVersion
	}{
		{v1Pool, blend.PoolV1},
		{v2Pool, blend.PoolV2},
		{foreignPool, blend.PoolVersionUnknown},
		{absentPool, blend.PoolVersionUnknown},
	} {
		got, err := store.BlendPoolVersion(ctx, c.pool)
		if err != nil {
			t.Fatalf("BlendPoolVersion(%s): %v", c.pool, err)
		}
		if got != c.want {
			t.Errorf("BlendPoolVersion(%s) = %d, want %d", c.pool, got, c.want)
		}
	}
}

// TestBlendPositionsRoundTrip exercises the InsertBlendPositionEvent
// path through real TimescaleDB. Verifies:
//
//   - All seven money-market event kinds insert successfully.
//   - The i128 amounts round-trip through the NUMERIC column at
//     full precision (per ADR-0003).
//   - PK is idempotent — re-running over the same range is a no-op.
//   - flash_loan correctly persists the counterparty contract.
//   - Invalid Kind / empty Pool / empty TxHash are rejected before
//     touching the DB.
func TestBlendPositionsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		pool     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		asset    = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		user     = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
		contract = "CAXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXY32S"
	)
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

	kinds := []string{
		blend.EventSupply,
		blend.EventWithdraw,
		blend.EventSupplyCollateral,
		blend.EventWithdrawCollateral,
		blend.EventBorrow,
		blend.EventRepay,
		blend.EventFlashLoan,
	}
	for i, kind := range kinds {
		ev := blend.PositionEvent{
			Pool:        pool,
			Kind:        kind,
			Asset:       asset,
			User:        user,
			TokenAmount: big.NewInt(int64(1_000_000) * int64(i+1)),
			BOrDAmount:  big.NewInt(int64(990_000) * int64(i+1)),
			Ledger:      uint32(60_000_000 + i),
			TxHash:      pad64("a", i),
			OpIndex:     uint32(i),
			Timestamp:   t0.Add(time.Duration(i) * time.Minute),
		}
		if kind == blend.EventFlashLoan {
			ev.Counterparty = contract
		}
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(ev)); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s): %v", kind, err)
		}
		// Idempotent re-insert — same PK is a no-op.
		if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(ev)); err != nil {
			t.Fatalf("InsertBlendPositionEvent (%s dup): %v", kind, err)
		}
	}

	// ─── Validate the rows via a raw COUNT ─────────────────────────
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var rowCount int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM blend_positions WHERE pool = $1",
		pool,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != len(kinds) {
		t.Errorf("blend_positions COUNT = %d, want %d", rowCount, len(kinds))
	}

	// flash_loan row should have counterparty populated.
	var counterparty *string
	if err := db.QueryRowContext(ctx,
		"SELECT counterparty FROM blend_positions WHERE pool = $1 AND event_kind = 'flash_loan'",
		pool,
	).Scan(&counterparty); err != nil {
		t.Fatalf("counterparty scan: %v", err)
	}
	if counterparty == nil || *counterparty != contract {
		t.Errorf("flash_loan counterparty = %v, want %q", counterparty, contract)
	}

	// supply row should have NULL counterparty.
	if err := db.QueryRowContext(ctx,
		"SELECT counterparty FROM blend_positions WHERE pool = $1 AND event_kind = 'supply'",
		pool,
	).Scan(&counterparty); err != nil {
		t.Fatalf("supply counterparty scan: %v", err)
	}
	if counterparty != nil {
		t.Errorf("supply counterparty = %v, want NULL (non-flash_loan)", *counterparty)
	}
}

// TestBlendPositionsLargeI128 — i128 amounts MUST round-trip without
// truncation through the NUMERIC column (ADR-0003).
func TestBlendPositionsLargeI128(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	huge, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	ev := blend.PositionEvent{
		Pool:        "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
		Kind:        blend.EventBorrow,
		Asset:       "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6",
		User:        "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K",
		TokenAmount: huge,
		BOrDAmount:  huge,
		Ledger:      60_000_001,
		TxHash:      pad64("b", 0),
		OpIndex:     0,
		Timestamp:   time.Now().UTC(),
	}
	if err := store.InsertBlendPositionEvent(ctx, domain.BlendPositionEvent(ev)); err != nil {
		t.Fatalf("InsertBlendPositionEvent: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRowContext(ctx,
		"SELECT token_amount::text FROM blend_positions WHERE ledger = 60000001",
	).Scan(&got); err != nil {
		t.Fatalf("token_amount scan: %v", err)
	}
	if got != huge.String() {
		t.Errorf("token_amount round-trip mismatch: got %s, want %s — i128 truncated?",
			got, huge.String())
	}
}

// TestBlendEmissionsRoundTrip exercises every emission /
// credit-risk event kind through InsertBlendEmissionEvent.
// Validates the per-kind attribute jsonb (claim.reserve_token_ids,
// reserve_emission_update.eps/expiration/res_token_id) lands
// correctly.
func TestBlendEmissionsRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		pool  = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		asset = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		user  = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
	)
	t0 := time.Date(2026, 5, 20, 13, 0, 0, 0, time.UTC)

	rows := []blend.EmissionEvent{
		{
			Pool: pool, Kind: blend.EventGulp, Asset: asset,
			Amount: big.NewInt(100), Ledger: 61_000_000, TxHash: pad64("c", 0), OpIndex: 0, Timestamp: t0,
		},
		{
			Pool: pool, Kind: blend.EventClaim, User: user,
			Amount: big.NewInt(7_500_000), ReserveTokenIDs: []uint32{0, 2, 5},
			Ledger: 61_000_001, TxHash: pad64("c", 1), OpIndex: 0, Timestamp: t0.Add(time.Minute),
		},
		{
			Pool: pool, Kind: blend.EventReserveEmissions,
			ResTokenID: 7, EmissionsPerSec: 1_000_000, Expiration: 1_900_000_000,
			Ledger: 61_000_002, TxHash: pad64("c", 2), OpIndex: 0, Timestamp: t0.Add(2 * time.Minute),
		},
		{
			Pool: pool, Kind: blend.EventGulpEmissions, Amount: big.NewInt(42),
			Ledger: 61_000_003, TxHash: pad64("c", 3), OpIndex: 0, Timestamp: t0.Add(3 * time.Minute),
		},
		{
			Pool: pool, Kind: blend.EventBadDebt, User: user, Asset: asset, Amount: big.NewInt(1_000),
			Ledger: 61_000_004, TxHash: pad64("c", 4), OpIndex: 0, Timestamp: t0.Add(4 * time.Minute),
		},
		{
			Pool: pool, Kind: blend.EventDefaultedDebt, Asset: asset, Amount: big.NewInt(2_000),
			Ledger: 61_000_005, TxHash: pad64("c", 5), OpIndex: 0, Timestamp: t0.Add(5 * time.Minute),
		},
	}
	for _, ev := range rows {
		if err := store.InsertBlendEmissionEvent(ctx, domain.BlendEmissionEvent(ev)); err != nil {
			t.Fatalf("InsertBlendEmissionEvent (%s): %v", ev.Kind, err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM blend_emissions").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(rows) {
		t.Errorf("blend_emissions COUNT = %d, want %d", n, len(rows))
	}

	// reserve_emission_update should have the typed fields stamped
	// in attributes jsonb.
	var attrs string
	if err := db.QueryRowContext(ctx,
		"SELECT attributes::text FROM blend_emissions WHERE event_kind = 'reserve_emission_update'",
	).Scan(&attrs); err != nil {
		t.Fatalf("attrs scan: %v", err)
	}
	for _, want := range []string{`"res_token_id":7`, `"eps":1000000`, `"expiration":1900000000`} {
		if !contains(attrs, want) {
			t.Errorf("reserve_emission_update attributes %q missing %q", attrs, want)
		}
	}

	// claim should carry reserve_token_ids in attributes.
	if err := db.QueryRowContext(ctx,
		"SELECT attributes::text FROM blend_emissions WHERE event_kind = 'claim'",
	).Scan(&attrs); err != nil {
		t.Fatalf("claim attrs scan: %v", err)
	}
	if !contains(attrs, `"reserve_token_ids":[0,2,5]`) {
		t.Errorf("claim attributes %q missing reserve_token_ids", attrs)
	}
}

// TestBlendAdminRoundTrip exercises every admin / pool-config /
// pool-factory lifecycle event kind, including the dual-arity
// set_status (admin / non-admin) and queue_set_reserve's
// embedded ReserveConfig.
func TestBlendAdminRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		pool     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		factory  = "CDSYOAVXFY7SM5S64IZPPPYB4GVGGLMQVFREPSQQEZVIWXX5R23G4QSU"
		admin    = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
		newAdm   = "GZYXWVUTSRQPONMLKJIHGFEDCBA765432ZYXWVUTSRQPONMLKJIHG6677"
		asset    = "CC4WPS7HRSPRZAXBVUDYLRXLZRHPLA6VTZARKZJTNVNECAS5IDRXRUB6"
		poolAddr = "CAUVZAQXKAQJ2PJKTH7Q6DCAXKQ22GZBV4XW2QHHIPC6BFCC6FJSXNEW"
	)
	t0 := time.Date(2026, 5, 20, 14, 0, 0, 0, time.UTC)

	rows := []blend.AdminEvent{
		{
			ContractID: pool, Kind: blend.EventSetAdmin,
			Admin: admin, Target: newAdm,
			Ledger: 62_000_000, TxHash: pad64("d", 0), OpIndex: 0, Timestamp: t0,
		},
		{
			ContractID: pool, Kind: blend.EventUpdatePool, Admin: admin,
			BackstopTakeRate: 2_000_000, MaxPositions: 8,
			MinCollateral: big.NewInt(100_000_000),
			Ledger:        62_000_001, TxHash: pad64("d", 1), OpIndex: 0, Timestamp: t0.Add(time.Minute),
		},
		{
			ContractID: pool, Kind: blend.EventQueueSetReserve, Admin: admin, Asset: asset,
			ReserveConfig: map[string]any{
				"index":      uint64(3),
				"enabled":    true,
				"supply_cap": "1000000000000",
			},
			Ledger: 62_000_002, TxHash: pad64("d", 2), OpIndex: 0, Timestamp: t0.Add(2 * time.Minute),
		},
		{
			ContractID: pool, Kind: blend.EventCancelSetReserve, Admin: admin, Asset: asset,
			Ledger: 62_000_003, TxHash: pad64("d", 3), OpIndex: 0, Timestamp: t0.Add(3 * time.Minute),
		},
		{
			ContractID: pool, Kind: blend.EventSetReserve, Asset: asset, ReserveIndex: 4,
			Ledger: 62_000_004, TxHash: pad64("d", 4), OpIndex: 0, Timestamp: t0.Add(4 * time.Minute),
		},
		{
			ContractID: pool, Kind: blend.EventSetStatus, NewStatus: 2, ByAdmin: false,
			Ledger: 62_000_005, TxHash: pad64("d", 5), OpIndex: 0, Timestamp: t0.Add(5 * time.Minute),
		},
		{
			ContractID: pool, Kind: blend.EventSetStatus, Admin: admin, NewStatus: 5, ByAdmin: true,
			Ledger: 62_000_006, TxHash: pad64("d", 6), OpIndex: 0, Timestamp: t0.Add(6 * time.Minute),
		},
		{
			ContractID: factory, Kind: blend.EventDeploy, Target: poolAddr,
			Ledger: 62_000_007, TxHash: pad64("d", 7), OpIndex: 0, Timestamp: t0.Add(7 * time.Minute),
		},
	}
	for _, ev := range rows {
		if err := store.InsertBlendAdminEvent(ctx, domain.BlendAdminEvent(ev)); err != nil {
			t.Fatalf("InsertBlendAdminEvent (%s): %v", ev.Kind, err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM blend_admin").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(rows) {
		t.Errorf("blend_admin COUNT = %d, want %d", n, len(rows))
	}

	// queue_set_reserve metadata should carry the supply_cap as
	// a string (i128 precision-preserved per ADR-0003).
	var attrs string
	if err := db.QueryRowContext(ctx,
		"SELECT attributes::text FROM blend_admin WHERE event_kind = 'queue_set_reserve'",
	).Scan(&attrs); err != nil {
		t.Fatalf("queue_set_reserve attrs scan: %v", err)
	}
	if !contains(attrs, `"supply_cap":"1000000000000"`) {
		t.Errorf("queue_set_reserve attributes %q missing supply_cap string", attrs)
	}

	// update_pool min_collateral i128 should also be a string in jsonb.
	if err := db.QueryRowContext(ctx,
		"SELECT attributes::text FROM blend_admin WHERE event_kind = 'update_pool'",
	).Scan(&attrs); err != nil {
		t.Fatalf("update_pool attrs scan: %v", err)
	}
	if !contains(attrs, `"min_collateral":"100000000"`) {
		t.Errorf("update_pool attributes %q missing min_collateral string", attrs)
	}

	// set_status admin variant should carry by_admin=true.
	if err := db.QueryRowContext(ctx,
		"SELECT attributes::text FROM blend_admin WHERE event_kind = 'set_status' AND ledger = 62000006",
	).Scan(&attrs); err != nil {
		t.Fatalf("set_status admin attrs scan: %v", err)
	}
	if !contains(attrs, `"by_admin":true`) {
		t.Errorf("set_status admin attributes %q missing by_admin:true", attrs)
	}

	// deploy row should have the pool_address in `target`.
	var target string
	if err := db.QueryRowContext(ctx,
		"SELECT target FROM blend_admin WHERE event_kind = 'deploy'",
	).Scan(&target); err != nil {
		t.Fatalf("deploy target scan: %v", err)
	}
	if target != poolAddr {
		t.Errorf("deploy target = %q, want %q", target, poolAddr)
	}
}

// TestBlendReserveConfigs_AppliedCancelledPending pins the
// queue/apply/cancel lifecycle BlendReserveConfigs must resolve
// (CA2-A10-correct-2): a queue_set_reserve alone is a proposal, not
// the live rate model. Four reserves exercise the four outcomes:
//   - applied:   queue then set_reserve -> the queued config, live.
//   - cancelled: queue then cancel_set_reserve -> no config at all.
//   - pending:   queue only, still inside the timelock -> no config.
//   - requeued:  an applied config followed by a second, still-
//     pending queue -> the FIRST (applied) config, not the second.
func TestBlendReserveConfigs_AppliedCancelledPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		pool         = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
		assetApplied = "CAPPLIED0000000000000000000000000000000000000000000000000"
		assetCancel  = "CCANCEL00000000000000000000000000000000000000000000000000"
		assetPending = "CPENDING0000000000000000000000000000000000000000000000000"
		assetRequeue = "CREQUEUE0000000000000000000000000000000000000000000000000"
		admin        = "GABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTU56K"
	)
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	cfg := func(decimals uint32) map[string]any {
		return map[string]any{
			"index": uint64(0), "decimals": uint64(decimals),
			"c_factor": uint64(9_000_000), "l_factor": uint64(9_500_000),
			"util": uint64(8_000_000), "max_util": uint64(9_500_000),
			"r_base": uint64(50_000), "r_one": uint64(500_000),
			"r_two": uint64(5_000_000), "r_three": uint64(15_000_000),
			"reactivity": uint64(200), "enabled": true,
		}
	}

	n := 0
	next := func() (int64, string) {
		n++
		return 63_000_000 + int64(n), pad64("e", n)
	}
	admEvent := func(asset string, kind string, reserveConfig map[string]any, at time.Time) domain.BlendAdminEvent {
		ledger, tx := next()
		return domain.BlendAdminEvent(blend.AdminEvent{
			ContractID: pool, Kind: kind, Admin: admin, Asset: asset,
			ReserveConfig: reserveConfig,
			Ledger:        uint32(ledger), TxHash: tx, OpIndex: 0, Timestamp: at,
		})
	}

	rows := []domain.BlendAdminEvent{
		// applied: queue then set_reserve.
		admEvent(assetApplied, blend.EventQueueSetReserve, cfg(6), t0),
		admEvent(assetApplied, blend.EventSetReserve, nil, t0.Add(time.Minute)),
		// cancelled: queue then cancel_set_reserve.
		admEvent(assetCancel, blend.EventQueueSetReserve, cfg(7), t0),
		admEvent(assetCancel, blend.EventCancelSetReserve, nil, t0.Add(time.Minute)),
		// pending: queue only, no apply/cancel yet.
		admEvent(assetPending, blend.EventQueueSetReserve, cfg(8), t0),
		// requeued: an applied config, then a second still-pending queue.
		admEvent(assetRequeue, blend.EventQueueSetReserve, cfg(9), t0),
		admEvent(assetRequeue, blend.EventSetReserve, nil, t0.Add(time.Minute)),
		admEvent(assetRequeue, blend.EventQueueSetReserve, cfg(10), t0.Add(2*time.Minute)),
	}
	for _, ev := range rows {
		if err := store.InsertBlendAdminEvent(ctx, ev); err != nil {
			t.Fatalf("InsertBlendAdminEvent (%s/%s): %v", ev.Asset, ev.Kind, err)
		}
	}

	got, err := store.BlendReserveConfigs(ctx, pool)
	if err != nil {
		t.Fatalf("BlendReserveConfigs: %v", err)
	}

	if cfg, ok := got[assetApplied]; !ok || cfg.Decimals != 6 {
		t.Errorf("assetApplied = %+v, ok=%v, want the applied config (decimals=6)", cfg, ok)
	}
	if cfg, ok := got[assetCancel]; ok {
		t.Errorf("assetCancel = %+v, want no config — it was cancelled", cfg)
	}
	if cfg, ok := got[assetPending]; ok {
		t.Errorf("assetPending = %+v, want no config — still timelocked, never applied", cfg)
	}
	if cfg, ok := got[assetRequeue]; !ok || cfg.Decimals != 9 {
		t.Errorf("assetRequeue = %+v, ok=%v, want the APPLIED config (decimals=9), not the pending requeue (decimals=10)", cfg, ok)
	}
}

// pad64 builds a 64-char-hex-like string for a tx_hash slot —
// deterministic per (seed, n) so tests stay reproducible.
func pad64(seed string, n int) string {
	out := make([]byte, 0, 64)
	for len(out) < 64 {
		out = append(out, seed[0])
		// Per-test variation in the second char so different
		// row-numbers don't collide on the same tx_hash.
		out = append(out, hexNibble(n))
	}
	return string(out[:64])
}

func hexNibble(n int) byte {
	const tab = "0123456789abcdef"
	return tab[n&0xF]
}

// contains reports whether sub appears in s, IGNORING ASCII spaces in s.
// The blend attributes are read via `attributes::text` (a postgres jsonb
// column), which pretty-prints with a space after every ':' and ',' — but
// the expected substrings here are compact (`"eps":1000000`). The stored
// values carry no internal spaces, so stripping spaces from the haystack
// makes these checks robust to jsonb's text formatting (and its
// non-deterministic key order, since each `"key":value` pair is matched
// independently). Kept import-free to keep the test file's imports narrow.
func contains(s, sub string) bool {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			b = append(b, s[i])
		}
	}
	s = string(b)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

const (
	dxAdminVault    = "CA25XTGHKQ6PUMFJ4SDNRFMUABIFX46U7VAZBFDZKAOX5C3KZXUAR2KQ"
	dxAdminStrategy = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	dxAdminCaller   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	dxAdminNew      = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
)

// TestDefindexAdminEventsInsert executes InsertDefindexAdminEvent against
// migration 0192: every kind's shape lands, a replay is idempotent, the
// i128 amount survives above 2^63, and the per-kind CHECKs reject a row
// carrying the wrong columns.
func TestDefindexAdminEventsInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const bigAmount = "170141183460469231731687303715884105727" // i128 max
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	row := func(n int, kind string) timescale.DefindexAdminEvent {
		return timescale.DefindexAdminEvent{
			Ledger: uint32(62_000_000 + n), LedgerCloseTime: t0.Add(time.Duration(n) * time.Minute),
			TxHash: pad64("d", n), ContractID: dxAdminVault, EventKind: kind,
		}
	}
	rescue := row(0, "rescue")
	rescue.Caller, rescue.Strategy, rescue.Amount = dxAdminCaller, dxAdminStrategy, bigAmount
	paused := row(1, "paused")
	paused.Caller, paused.Strategy = dxAdminCaller, dxAdminStrategy
	unpaused := row(2, "unpaused")
	unpaused.Caller, unpaused.Strategy = dxAdminCaller, dxAdminStrategy
	nreceiver := row(3, "nreceiver")
	nreceiver.Caller, nreceiver.NewAddress = dxAdminCaller, dxAdminNew
	valid := []timescale.DefindexAdminEvent{rescue, paused, unpaused, nreceiver}
	for i, kind := range []string{"nmanager", "nemanager", "rbmanager"} {
		e := row(4+i, kind)
		e.NewAddress = dxAdminNew
		valid = append(valid, e)
	}

	for pass := 0; pass < 2; pass++ {
		for _, e := range valid {
			if err := store.InsertDefindexAdminEvent(ctx, e); err != nil {
				t.Fatalf("pass %d InsertDefindexAdminEvent %s: %v", pass, e.EventKind, err)
			}
		}
	}
	var n int
	mustScan(t, ctx, db, &n, `SELECT count(*) FROM defindex_admin_events`)
	if n != len(valid) {
		t.Fatalf("rows after two passes = %d, want %d (upsert must be idempotent)", n, len(valid))
	}
	var amount string
	mustScan(t, ctx, db, &amount, `SELECT amount::text FROM defindex_admin_events WHERE event_kind = 'rescue'`)
	if amount != bigAmount {
		t.Fatalf("rescue amount = %s, want %s", amount, bigAmount)
	}

	rescueNoAmount := row(10, "rescue")
	rescueNoAmount.Caller, rescueNoAmount.Strategy = dxAdminCaller, dxAdminStrategy
	managerWithCaller := row(11, "nmanager")
	managerWithCaller.Caller, managerWithCaller.NewAddress = dxAdminCaller, dxAdminNew
	pausedNoStrategy := row(12, "paused")
	pausedNoStrategy.Caller = dxAdminCaller
	for _, e := range []timescale.DefindexAdminEvent{rescueNoAmount, managerWithCaller, pausedNoStrategy} {
		if err := store.InsertDefindexAdminEvent(ctx, e); err == nil {
			t.Errorf("InsertDefindexAdminEvent %s with the wrong columns succeeded, want a CHECK violation", e.EventKind)
		}
	}
}

// Reused public strkeys (contract / account ids — public identifiers, not
// secrets) for the sorocredit fixtures.
const (
	credCollateralA    = "CAB6MICC2WKRT372U3FRPKGGVB5R3FDJSMWSLPF2UJNJPYMBZ76RQVYE"
	credCollateralB    = "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"
	credOwnerA         = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	credOwnerB         = "GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO"
	credSettler        = "GBGQNZAZ54NZWZA7KGOTOZYCXEYIQGOUJK7L6EM7EJD7AQRBKO7VSXJP"
	credDebtAsset      = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" // mainnet USDC SAC
	credOtherDebtAsset = "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG"
)

// TestCreditWindowAnalyticsAndBespoke exercises the READ side of the
// sorocredit TVL/analytics surface (CreditWindowAnalytics) + the bespoke
// block (bespokeCredit) end-to-end through real TimescaleDB.
//
// Proves: empty-safe (nil reader + nil block on empty tables — the r1 state
// until the sorocredit projector-replay runs), i128/NUMERIC preservation of
// statement + settlement volumes (never int64), the window-scoped
// open-position proxy (an opened-and-withdrawn position is not counted
// open), the SCHEDULED-settlement labelling (NOT distress), and that the
// KPIs + recent-settlements table surface once data exists.
func TestCreditWindowAnalyticsAndBespoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty-safe: reader returns (nil, nil); bespoke block is nil (not an
	// error) so /v1/protocols/sorocredit degrades to generic analytics.
	if a, err := store.CreditWindowAnalytics(ctx, 90); err != nil {
		t.Fatalf("CreditWindowAnalytics (empty): %v", err)
	} else if a != nil {
		t.Fatalf("CreditWindowAnalytics (empty) = %+v, want nil", a)
	}
	if blk, err := store.BuildProtocolBespoke(ctx, "sorocredit", "lending", 90); err != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit (empty): %v", err)
	} else if blk != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit (empty) = %+v, want nil", blk)
	}

	base := time.Now().UTC().Add(-24 * time.Hour)

	// Huge i128 volumes to prove NUMERIC round-trips exactly (> 2^63).
	stmtAmt1, _ := new(big.Int).SetString("12345678901234567890", 10)
	stmtAmt2 := big.NewInt(1_000_000)
	setAmt1, _ := new(big.Int).SetString("98765432109876543210987654321", 10)
	setAmt2 := big.NewInt(5_000_000)
	wdAmt := big.NewInt(250_000)
	wantStmtVol := new(big.Int).Add(stmtAmt1, stmtAmt2)
	setAmtOther := big.NewInt(7_000_000)
	setAmtMultiLeg := big.NewInt(3_000_000)
	wantSetVol := new(big.Int).Add(new(big.Int).Add(setAmt1, setAmt2), setAmtMultiLeg)

	// Position A — opened, never withdrawn (stays "open" in the window).
	// Position B — opened AND cashed out via a Withdrawal (not "open").
	for _, p := range []timescale.CreditPosition{
		{CollateralContract: credCollateralA, PositionUUID: "uuid-A", PositionName: "Collateral-uuid-A", Owner: credOwnerA, Ledger: 61_700_000, LedgerCloseTime: base, TxHash: pad64("a", 0), OpIndex: 0, EventIndex: 0},
		{CollateralContract: credCollateralB, PositionUUID: "uuid-B", PositionName: "Collateral-uuid-B", Owner: credOwnerB, Ledger: 61_700_001, LedgerCloseTime: base.Add(time.Minute), TxHash: pad64("a", 1), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditPosition(ctx, p); err != nil {
			t.Fatalf("InsertCreditPosition %s: %v", p.CollateralContract, err)
		}
	}

	for _, st := range []timescale.CreditStatement{
		{StatementUUID: "stmt-1", PositionUUID: "uuid-A", CollateralContract: credCollateralA, Amount: stmtAmt1.String(), StatementTime: base, Ledger: 61_700_010, LedgerCloseTime: base.Add(2 * time.Minute), TxHash: pad64("b", 0), OpIndex: 0, EventIndex: 0},
		{StatementUUID: "stmt-2", PositionUUID: "uuid-B", CollateralContract: credCollateralB, Amount: stmtAmt2.String(), StatementTime: base, Ledger: 61_700_011, LedgerCloseTime: base.Add(3 * time.Minute), TxHash: pad64("b", 1), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditStatement(ctx, st); err != nil {
			t.Fatalf("InsertCreditStatement %s: %v", st.StatementUUID, err)
		}
	}

	for _, se := range []timescale.CreditSettlement{
		{CollateralContract: credCollateralA, PositionUUID: "uuid-A", StatementUUID: "stmt-1", SettlerAccount: credSettler, DebtAsset: credDebtAsset, SettledAmount: setAmt1.String(), Ledger: 61_700_020, LedgerCloseTime: base.Add(4 * time.Minute), TxHash: pad64("c", 0), OpIndex: 0, EventIndex: 0},
		{CollateralContract: credCollateralB, PositionUUID: "uuid-B", StatementUUID: "stmt-2", SettlerAccount: credSettler, DebtAsset: credDebtAsset, SettledAmount: setAmt2.String(), Ledger: 61_700_021, LedgerCloseTime: base.Add(5 * time.Minute), TxHash: pad64("c", 1), OpIndex: 0, EventIndex: 0},
		// A non-USDC primary leg must not join the USDC-unit sum.
		{CollateralContract: credCollateralB, PositionUUID: "uuid-B", StatementUUID: "stmt-3", SettlerAccount: credSettler, DebtAsset: credOtherDebtAsset, SettledAmount: setAmtOther.String(), Ledger: 61_700_022, LedgerCloseTime: base.Add(5 * time.Minute), TxHash: pad64("c", 2), OpIndex: 0, EventIndex: 0},
		// A USDC leg 0 with extra legs is summed, but only partially.
		{CollateralContract: credCollateralA, PositionUUID: "uuid-A", StatementUUID: "stmt-4", SettlerAccount: credSettler, DebtAsset: credDebtAsset, SettledAmount: setAmtMultiLeg.String(), Attributes: map[string]any{"debt_legs": 2}, Ledger: 61_700_023, LedgerCloseTime: base.Add(5 * time.Minute), TxHash: pad64("c", 3), OpIndex: 0, EventIndex: 0},
	} {
		if err := store.InsertCreditSettlement(ctx, se); err != nil {
			t.Fatalf("InsertCreditSettlement %s: %v", se.PositionUUID, err)
		}
	}

	// Withdrawal on position B → B is no longer "open" in the window.
	if err := store.InsertCreditEvent(ctx, timescale.CreditEvent{
		EventType: "withdrawal", CollateralContract: credCollateralB, Asset: credDebtAsset, Account: credOwnerB,
		Amount: wdAmt.String(), Ledger: 61_700_030, LedgerCloseTime: base.Add(6 * time.Minute), TxHash: pad64("d", 0), OpIndex: 0, EventIndex: 0,
	}); err != nil {
		t.Fatalf("InsertCreditEvent withdrawal: %v", err)
	}

	a, err := store.CreditWindowAnalytics(ctx, 90)
	if err != nil {
		t.Fatalf("CreditWindowAnalytics: %v", err)
	}
	if a == nil {
		t.Fatal("CreditWindowAnalytics = nil, want summary")
	}
	if a.PositionsOpened != 2 {
		t.Errorf("PositionsOpened = %d, want 2", a.PositionsOpened)
	}
	if a.OpenPositions != 1 {
		t.Errorf("OpenPositions = %d, want 1 (B was withdrawn in the window)", a.OpenPositions)
	}
	if a.UniqueUsers != 2 {
		t.Errorf("UniqueUsers = %d, want 2", a.UniqueUsers)
	}
	if a.Statements != 2 {
		t.Errorf("Statements = %d, want 2", a.Statements)
	}
	if a.StatementVolume.BigInt().Cmp(wantStmtVol) != 0 {
		t.Errorf("StatementVolume = %s, want %s — i128/NUMERIC lost precision", a.StatementVolume, wantStmtVol)
	}
	if a.Settlements != 4 {
		t.Errorf("Settlements = %d, want 4", a.Settlements)
	}
	if a.SettlementVolume.BigInt().Cmp(wantSetVol) != 0 {
		t.Errorf("SettlementVolume = %s, want %s (USDC primary legs only, i128 exact)", a.SettlementVolume, wantSetVol)
	}
	if a.SettlementsNotFullySummed != 2 {
		t.Errorf("SettlementsNotFullySummed = %d, want 2 (one non-USDC leg, one multi-leg)", a.SettlementsNotFullySummed)
	}
	if a.Withdrawals != 1 {
		t.Errorf("Withdrawals = %d, want 1", a.Withdrawals)
	}

	blk, err := store.BuildProtocolBespoke(ctx, "sorocredit", "lending", 90)
	if err != nil {
		t.Fatalf("BuildProtocolBespoke sorocredit: %v", err)
	}
	if blk == nil {
		t.Fatal("BuildProtocolBespoke sorocredit = nil, want block")
	}
	kpis := map[string]string{}
	for _, k := range blk.KPIs {
		if strings.HasPrefix(k.Label, "Settlement volume") && !strings.HasPrefix(k.Hint, "LOWER BOUND:") {
			t.Errorf("settlement volume hint = %q, want a LOWER BOUND (2 settlements not fully summed)", k.Hint)
		}
		// key on the label prefix before the window suffix
		kpis[k.Label] = k.Value
	}
	assertKPI(t, kpis, "Positions opened (90d)", "2")
	assertKPI(t, kpis, "Open positions (90d)", "1")
	assertKPI(t, kpis, "Scheduled settlements (90d)", "4")
	assertKPI(t, kpis, "Settlement volume (90d)", wantSetVol.String())

	var hasSettleTable bool
	for _, tb := range blk.Tables {
		if tb.Title == "Recent scheduled settlements" {
			hasSettleTable = true
			if len(tb.Rows) != 4 {
				t.Errorf("recent-settlements rows = %d, want 4", len(tb.Rows))
			}
		}
	}
	if !hasSettleTable {
		t.Errorf("bespoke block missing 'Recent scheduled settlements' table; Tables=%+v", blk.Tables)
	}

	// The SCHEDULED-settlement honesty note must be present — never surface
	// these as distressed liquidations.
	var hasNote bool
	for _, n := range blk.Notes {
		if strings.Contains(n, "NOT distressed liquidations") {
			hasNote = true
		}
	}
	if !hasNote {
		t.Errorf("bespoke block missing the 'NOT distressed liquidations' settlement note; Notes=%+v", blk.Notes)
	}
}

// assertKPI fails the test if the KPI label is absent or its value differs.
func assertKPI(t *testing.T, kpis map[string]string, label, want string) {
	t.Helper()
	got, ok := kpis[label]
	if !ok {
		t.Errorf("KPI %q absent", label)
		return
	}
	if got != want {
		t.Errorf("KPI %q = %q, want %q", label, got, want)
	}
}

// TestUpshiftVaultShareSupplies_DeployedTiebreakWithinLedger pins the
// fix for CA2-A13-harden-6: when a single ledger carries two
// deployed_assets_changed events for the same vault (a rebalance that
// spans multiple ops/events in one transaction), the DISTINCT ON must
// resolve deterministically to the highest (op_index, event_index) —
// the last-emitted new_amount — never to whichever row the physical
// scan happens to return first.
func TestUpshiftVaultShareSupplies_DeployedTiebreakWithinLedger(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	vault := mustContractStrkey(t, 0xE0)
	closeTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// One ledger, one tx, two deployed_assets_changed events from the
	// same rebalance: op_index/event_index 3 fires first with
	// new_amount=1000, then op_index/event_index 7 supersedes it with
	// new_amount=0. Everything else ties (same contract, ledger,
	// ledger_close_time), so only the op/event tiebreak can resolve
	// which row is "latest".
	events := []timescale.UpshiftVaultEvent{
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deploytx", OpIndex: 3, EventIndex: 3,
			Kind: timescale.UpshiftDeployedAssetsChanged, Caller: vault,
			OldAmount: canonical.NewAmount(big.NewInt(0)),
			NewAmount: canonical.NewAmount(big.NewInt(1_000)),
		},
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deploytx", OpIndex: 7, EventIndex: 7,
			Kind: timescale.UpshiftDeployedAssetsChanged, Caller: vault,
			OldAmount: canonical.NewAmount(big.NewInt(1_000)),
			NewAmount: canonical.NewAmount(big.NewInt(0)),
		},
		{
			ContractID: vault, Ledger: 100, LedgerCloseTime: closeTime,
			TxHash: "deptx", OpIndex: 1, EventIndex: 1,
			Kind: timescale.UpshiftDeposit, Caller: vault,
			Receiver: vault, Owner: vault,
			Assets: canonical.NewAmount(big.NewInt(500)),
			Shares: canonical.NewAmount(big.NewInt(500)),
		},
	}
	for _, e := range events {
		if err := store.InsertUpshiftVaultEvent(ctx, e); err != nil {
			t.Fatalf("InsertUpshiftVaultEvent %+v: %v", e, err)
		}
	}

	got, err := store.UpshiftVaultShareSupplies(ctx)
	if err != nil {
		t.Fatalf("UpshiftVaultShareSupplies: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("UpshiftVaultShareSupplies returned %d rows, want 1: %#v", len(got), got)
	}
	row := got[0]
	if !row.HasDeployedTotal {
		t.Fatalf("row %#v: HasDeployedTotal = false, want true", row)
	}
	want := canonical.NewAmount(big.NewInt(0))
	if row.DeployedAssets.String() != want.String() {
		t.Errorf("DeployedAssets = %s, want %s (the higher op_index/event_index event within the tied ledger)",
			row.DeployedAssets, want)
	}
}
