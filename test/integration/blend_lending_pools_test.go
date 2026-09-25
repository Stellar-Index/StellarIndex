//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

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
