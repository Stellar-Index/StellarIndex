//go:build integration

package integration_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAquariusPools_layoutSurvivesRestart is the restart path on real Postgres: a live add_pool is persisted to
// aquarius_pools by the indexer's gate hook, and a fresh registry warmed from the database decodes the pool's
// deposits in the layout the add_pool announced. A recorded type contradicting an in-code list fails the warm.
func TestAquariusPools_layoutSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	clPool, classicPool, oddPool := mustContractStrkey(t, 0xE1), mustContractStrkey(t, 0xE2), mustContractStrkey(t, 0xE3)
	live, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, true)
	if err != nil {
		t.Fatalf("live warm: %v", err)
	}
	dec := aquarius.NewDecoder(live[aquarius.SourceName]...)
	for pool, typ := range map[string]string{clPool: "concentrated", classicPool: "constant", oddPool: "constant_product"} {
		if _, err := dec.Decode(aqAddPool(t, pool, typ)); err != nil {
			t.Fatalf("add_pool %s: %v", typ, err)
		}
	}
	rows, err := store.LoadAquariusPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]timescale.AquariusPool{}
	for _, r := range rows {
		got[r.ContractID] = r
	}
	for pool, typ := range map[string]string{clPool: "concentrated", classicPool: "constant", oddPool: "constant_product"} {
		if r := got[pool]; r.PoolType != typ || r.FirstLedger != 66_000_000 {
			t.Errorf("%s: row %+v, want type %q at 66000000", pool, r, typ)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("aquarius_pools holds %d rows, want 3", len(rows))
	}

	// Re-announcing the same type is a no-op; a different type is refused and the row kept.
	if err := store.UpsertAquariusPool(ctx, timescale.AquariusPool{ContractID: clPool, PoolType: "concentrated", FirstLedger: 1}); err != nil {
		t.Errorf("same-type re-upsert: %v", err)
	}
	if err := store.UpsertAquariusPool(ctx, timescale.AquariusPool{ContractID: clPool, PoolType: "stable"}); !errors.Is(err, timescale.ErrAquariusPoolTypeConflict) {
		t.Errorf("different-type re-upsert = %v, want ErrAquariusPoolTypeConflict", err)
	}
	if rows, err = store.LoadAquariusPools(ctx); err != nil || len(rows) != 3 {
		t.Fatalf("reload: %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		if r.ContractID == clPool && (r.PoolType != "concentrated" || r.FirstLedger != 66_000_000) {
			t.Errorf("after re-records: %+v, want the first row (concentrated at 66000000) kept", r)
		}
	}

	restarted, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		t.Fatalf("restart warm: %v", err)
	}
	dec = aquarius.NewDecoder(restarted[aquarius.SourceName]...)
	for pool, wantCL := range map[string]bool{clPool: true, classicPool: false} {
		out, err := dec.Decode(aqDeposit(t, pool))
		if err != nil || len(out) != 1 {
			t.Fatalf("%s: Decode = %v, %v", pool, out, err)
		}
		lq, ok := out[0].(aquarius.LiquidityEvent)
		// body [7, 8, 9]: concentrated reads liquidity 7 and amounts [8 9]; shares-last reads amounts [7 8] and shares 9.
		if !ok || lq.Concentrated != wantCL || (wantCL && lq.Liquidity.String() != "7") || (!wantCL && lq.Shares.String() != "9") {
			t.Errorf("%s: decoded %+v, want concentrated=%v", pool, lq, wantCL)
		}
	}
	if _, err := dec.Decode(aqDeposit(t, oddPool)); !errors.Is(err, aquarius.ErrUnknownLiquidityLayout) {
		t.Errorf("deposit of a pool with an unmappable recorded type: %v, want ErrUnknownLiquidityLayout", err)
	}

	if err := store.UpsertAquariusPool(ctx, timescale.AquariusPool{ContractID: aquarius.MainnetPools[0], PoolType: "concentrated"}); err != nil {
		t.Fatal(err)
	}
	// A contradicting row fails closed for that pool only; the warm still starts.
	contradicted, err := pipeline.GatedRegistryOptions(ctx, store, logger, ctx, false)
	if err != nil {
		t.Fatalf("warm with a row contradicting MainnetPools: %v, want it to start", err)
	}
	dec = aquarius.NewDecoder(contradicted[aquarius.SourceName]...)
	if _, err := dec.Decode(aqDeposit(t, aquarius.MainnetPools[0])); !errors.Is(err, aquarius.ErrUnknownLiquidityLayout) {
		t.Errorf("deposit of the contradicted pool: %v, want ErrUnknownLiquidityLayout", err)
	}
	if out, err := dec.Decode(aqDeposit(t, clPool)); err != nil || len(out) != 1 {
		t.Errorf("deposit of an uncontested pool after the contradiction: %v, %v", out, err)
	}
}

func aqB64(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	s, err := xdr.MarshalBase64(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func aqContract(t *testing.T, id string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, id)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}}
}

func aqVec(vals ...xdr.ScVal) xdr.ScVal {
	vec := xdr.ScVec(vals)
	p := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &p}
}

func aqI128(n uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(n)}}
}

func aqAddPool(t *testing.T, pool, poolType string) events.Event {
	t.Helper()
	sym := xdr.ScSymbol(poolType)
	return events.Event{
		ContractID: aquarius.MainnetRouter, Ledger: 66_000_000, LedgerClosedAt: "2026-11-26T03:09:59Z",
		Topic: []string{aquarius.TopicSymbolAddPool},
		Value: aqB64(t, aqVec(aqContract(t, pool), xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})),
	}
}

func aqDeposit(t *testing.T, pool string) events.Event {
	t.Helper()
	return events.Event{
		ContractID: pool, Ledger: 66_000_001, LedgerClosedAt: "2026-11-26T03:10:04Z",
		Topic: []string{
			aquarius.TopicSymbolDepositLiquidity,
			aqB64(t, aqContract(t, mustContractStrkey(t, 0xE4))), aqB64(t, aqContract(t, mustContractStrkey(t, 0xE5))),
		},
		Value: aqB64(t, aqVec(aqI128(7), aqI128(8), aqI128(9))),
	}
}
