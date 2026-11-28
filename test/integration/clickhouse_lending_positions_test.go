//go:build integration

package integration_test

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func positionsSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func positionsContractVal(id xdr.ContractId) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}}
}

// positionsEntryB64 is a persistent contract_data LedgerEntry of pool holding val under key.
func positionsEntryB64(t *testing.T, pool xdr.ContractId, key, val xdr.ScVal, ledger uint32) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{LastModifiedLedgerSeq: xdr.Uint32(ledger), Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pool},
			Key:      key, Durability: xdr.ContractDataDurabilityPersistent, Val: val,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// TestLendingPoolPositions_TruncationCountsLiveRowsOnly: exactly LendingPoolPositionsLimit live Positions entries is a
// complete read even beside a removed entry in the same key range; one more live entry makes it truncated.
func TestLendingPoolPositions_TruncationCountsLiveRowsOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	pool := xdr.ContractId{0xA7, 0x51, 0x0C}
	const ledger = uint32(91_000_001)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	empty := func() xdr.ScVal {
		m := &xdr.ScMap{}
		return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &m}
	}
	posMap := xdr.ScMap{
		{Key: positionsSym("collateral"), Val: empty()},
		{Key: positionsSym("liabilities"), Val: empty()},
		{Key: positionsSym("supply"), Val: empty()},
	}
	posMapRef := &posMap
	posVal := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &posMapRef}
	account := func(i uint32) xdr.ScVal {
		var id xdr.ContractId
		binary.BigEndian.PutUint32(id[:4], i)
		id[31] = 0x5A
		return positionsContractVal(id)
	}
	positionsKey := func(i uint32) (key xdr.ScVal, keyB64 string) {
		vec := &xdr.ScVec{positionsSym("Positions"), account(i)}
		key = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
		return key, lendingKeyB64(t, pool, key)
	}
	insert := func(from, to uint32, removed bool, extra func(b driver.Batch) error) {
		t.Helper()
		b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
			(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if extra != nil {
			if err := extra(b); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		for i := from; i < to; i++ {
			key, keyB64 := positionsKey(i)
			change, entry := "updated", positionsEntryB64(t, pool, key, posVal, ledger)
			if removed {
				change, entry = "removed", ""
			}
			if err := b.Append("contract_data", keyB64, "", "", int64(0), change, ledger, at, entry); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	limit := uint32(chstore.LendingPoolPositionsLimit)
	resList := positionsSym("ResList")
	listVec := &xdr.ScVec{positionsContractVal(xdr.ContractId{0x01})}
	listVal := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &listVec}
	insert(0, limit, false, func(b driver.Batch) error {
		return b.Append("contract_data", lendingKeyB64(t, pool, resList), "", "", int64(0), "updated", ledger, at,
			positionsEntryB64(t, pool, resList, listVal, ledger))
	})
	insert(limit, limit+1, true, nil)

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	poolStrkey := positionsStrkey(t, pool)
	got, err := er.LendingPoolPositions(ctx, poolStrkey)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Readable || got.Truncated || len(got.Accounts) != int(limit) || got.Unreadable != 0 {
		t.Fatalf("cap live rows + one removed: readable=%v truncated=%v accounts=%d unreadable=%d",
			got.Readable, got.Truncated, len(got.Accounts), got.Unreadable)
	}

	insert(limit+1, limit+2, false, nil)
	got, err = er.LendingPoolPositions(ctx, poolStrkey)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Fatalf("cap+1 live rows read as complete: accounts=%d", len(got.Accounts))
	}
}

func positionsI128(n uint64) xdr.ScVal {
	v := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &v}
}

func positionsIndexMap(kv map[uint32]uint64) xdr.ScVal {
	m := xdr.ScMap{}
	for i := uint32(0); i < 4; i++ {
		if n, ok := kv[i]; ok {
			idx := xdr.Uint32(i)
			m = append(m, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &idx}, Val: positionsI128(n)})
		}
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// TestLendingPoolPositions_SplitsCollateralFromSupply: a stored Positions entry reads back with its collateral and
// non-collateral b-tokens apart and Supply as their exact sum, on the pool-wide and the per-account read.
func TestLendingPoolPositions_SplitsCollateralFromSupply(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	pool, reserve, holder := xdr.ContractId{0xA7, 0x51, 0x0D}, xdr.ContractId{0x01}, xdr.ContractId{0xB1}
	const ledger = uint32(91_000_002)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	resList := positionsSym("ResList")
	listVec := &xdr.ScVec{positionsContractVal(reserve)}
	listVal := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &listVec}
	posVec := &xdr.ScVec{positionsSym("Positions"), positionsContractVal(holder)}
	posKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &posVec}
	posMap := xdr.ScMap{
		{Key: positionsSym("collateral"), Val: positionsIndexMap(map[uint32]uint64{0: 5})},
		{Key: positionsSym("liabilities"), Val: positionsIndexMap(map[uint32]uint64{0: 1})},
		{Key: positionsSym("supply"), Val: positionsIndexMap(map[uint32]uint64{0: 2})},
	}
	posMapRef := &posMap
	posVal := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &posMapRef}

	b, err := raw.PrepareBatch(ctx, `INSERT INTO stellar.ledger_entries_current
		(entry_type, key_xdr, account_id, asset, balance, change_type, ledger_seq, close_time, entry_xdr)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for _, e := range []struct{ key, val xdr.ScVal }{{resList, listVal}, {posKey, posVal}} {
		if err := b.Append("contract_data", lendingKeyB64(t, pool, e.key), "", "", int64(0), "updated", ledger, at,
			positionsEntryB64(t, pool, e.key, e.val, ledger)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := b.Send(); err != nil {
		t.Fatalf("send: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })
	asset, account := positionsStrkey(t, reserve), positionsStrkey(t, holder)

	pp, err := er.LendingPoolPositions(ctx, positionsStrkey(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	if len(pp.Accounts) != 1 || pp.Accounts[0].Account != account {
		t.Fatalf("pool positions = %+v", pp)
	}
	p := pp.Accounts[0]
	if p.Supply[asset].String() != "7" || p.Collateral[asset].String() != "5" || p.NonCollateral[asset].String() != "2" || p.Borrow[asset].String() != "1" {
		t.Fatalf("pool read: supply=%v collateral=%v non-collateral=%v borrow=%v", p.Supply, p.Collateral, p.NonCollateral, p.Borrow)
	}
	ap, err := er.LendingAccountPositions(ctx, positionsStrkey(t, pool), account)
	if err != nil {
		t.Fatal(err)
	}
	if !ap.Readable || ap.Supply[asset].String() != "7" || ap.Collateral[asset].String() != "5" || ap.NonCollateral[asset].String() != "2" {
		t.Fatalf("account read: %+v", ap)
	}
}

func positionsStrkey(t *testing.T, id xdr.ContractId) string {
	t.Helper()
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id}
	s, err := addr.String()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
