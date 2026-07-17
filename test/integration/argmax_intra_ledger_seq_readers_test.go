//go:build integration

package integration_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests are the sibling proof of audit-2026-07-16 C2-4c: three current-
// state readers that fold ledger_entry_changes / ledger_entries_current to the
// latest entry per key with a SINGLE-COLUMN ledger_seq tie-break, which resolves
// same-ledger multi-change keys to an ARBITRARY row (a stale mid-ledger value,
// or a resurrected removal). The fix tie-breaks on the composite intra-ledger
// order — (ledger_seq, intra_ledger_seq) over the base table, or the equivalent
// materialized `version` over ledger_entries_current — so the LAST change in a
// ledger deterministically wins, exactly as ledger_entries_current FINAL does.
//
// Every test seeds two changes to ONE key in the SAME ledger, in the adversarial
// physical order the pre-fix query mis-resolves, and asserts the reader returns
// the LAST change. They go RED against the un-fixed reader query and GREEN with
// the composite order.

// TestQueryAccountBalance_SameLedgerLastChangeWins proves
// account_balance_reader.go's argMax(balance, (ledger_seq, intra_ledger_seq)).
// The two 'account' changes share a ledger; the stale one (intra 8) sorts FIRST
// in the base table's ORDER BY (ledger_seq, tx_hash, op_index, change_index), so
// the pre-fix argMax(balance, ledger_seq) — which keeps the first row on a
// version tie — returns the stale balance. The composite order keeps the later
// change (intra 9).
func TestQueryAccountBalance_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		account  = "c24c-sibling-account-balance-GTEST"
		key      = "c24c-account-balance-same-ledger-key"
		ledger   = uint32(71_000_001)
		staleBal = int64(100)
		finalBal = int64(200)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	rows := []chstore.LedgerEntryChangeRow{
		// The LATER change (intra 9, op 1) — the final balance. Handed to the
		// writer first (adversarial), but sorts LAST in the table.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "acctbal", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			AccountID: account, Balance: finalBal,
		},
		// The EARLIER change (intra 8, op 0) — a stale mid-ledger balance. Sorts
		// FIRST, so a ledger_seq-only argMax keeps it.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "acctbal", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			AccountID: account, Balance: staleBal,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	snap, found, err := chstore.QueryAccountBalance(ctx, addr, account)
	if err != nil {
		t.Fatalf("QueryAccountBalance: %v", err)
	}
	if !found {
		t.Fatalf("QueryAccountBalance: account not found (the two seeded rows should count)")
	}
	if snap.Stroops != finalBal {
		t.Errorf("Stroops = %d, want %d (same-ledger tie resolved to a stale mid-ledger balance instead of the LAST change)", snap.Stroops, finalBal)
	}
	if snap.AtLedger != ledger {
		t.Errorf("AtLedger = %d, want %d", snap.AtLedger, ledger)
	}
}

// TestBlendPoolReserves_SameLedgerLastChangeWins proves
// blend_pool_state_reader.go on both axes of the fix:
//
//   - reserveVal (asset seeded update→update in one ledger): the pre-fix
//     argMax(entry_xdr, ledger_seq) keeps the first-sorted row (op 0, the stale
//     b_rate); the composite order keeps the final b_rate.
//   - reserveGone (asset seeded update→remove in one ledger): the pre-fix
//     non-empty-entry_xdr WHERE filter excluded the removal from the argMax, so
//     an earlier same-ledger update resurrected the key; the fix lets the
//     removal participate and drops it via HAVING on the winning change_type.
func TestBlendPoolReserves_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A very high ledger so these rows are the global max(ledger_seq): the reader
	// bounds its scan to the recent window `max - 250000`, and only this test
	// exercises that window.
	const ledger = uint32(4_000_000_000)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	const poolSeed, assetValSeed, assetGoneSeed = byte(0xB1), byte(0xB2), byte(0xB3)
	pool := contractIDFromSeed(poolSeed)
	assetVal := contractIDFromSeed(assetValSeed)
	assetGone := contractIDFromSeed(assetGoneSeed)
	poolStr := mustContractStrkey(t, poolSeed)
	assetValStr := mustContractStrkey(t, assetValSeed)
	assetGoneStr := mustContractStrkey(t, assetGoneSeed)

	const (
		staleBRate = uint64(1_000_000_000_000) // 1.0 at 12 decimals — a mid-ledger rate
		finalBRate = uint64(2_000_000_000_000) // 2.0 — the last change in the ledger
	)

	rows := []chstore.LedgerEntryChangeRow{
		// reserveVal: LATER update (op 1, intra 9) — the final b_rate. Written
		// first (adversarial); sorts last.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendval", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetVal), EntryXDR: resDataEntryB64(t, pool, assetVal, ledger, finalBRate),
		},
		// reserveVal: EARLIER update (op 0, intra 8) — a stale b_rate. Sorts
		// first, so a ledger_seq-only argMax keeps it.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendval", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetVal), EntryXDR: resDataEntryB64(t, pool, assetVal, ledger, staleBRate),
		},
		// reserveGone: the removal (op 1, intra 9) — the LAST change; the key must
		// drop out.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendgone", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "removed", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetGone), EntryXDR: "",
		},
		// reserveGone: the earlier update (op 0, intra 8) — a live entry the
		// pre-fix `entry_xdr != ''` filter would resurrect.
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "blendgone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: resDataKeyB64(t, pool, assetGone), EntryXDR: resDataEntryB64(t, pool, assetGone, ledger, staleBRate),
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	states, err := reader.BlendPoolReserves(ctx, poolStr, []string{assetValStr, assetGoneStr}, nil)
	if err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}

	byAsset := make(map[string]chstore.BlendReserveState, len(states))
	for _, s := range states {
		byAsset[s.Asset] = s
	}

	got, ok := byAsset[assetValStr]
	if !ok {
		t.Fatalf("reserveVal absent from result; want present with the final b_rate")
	}
	if want := new(big.Int).SetUint64(finalBRate); got.Data.BRate == nil || got.Data.BRate.Cmp(want) != 0 {
		t.Errorf("reserveVal b_rate = %v, want %d (same-ledger tie resolved to a stale mid-ledger reserve state)", got.Data.BRate, finalBRate)
	}
	if _, present := byAsset[assetGoneStr]; present {
		t.Errorf("reserveGone present in result; want ABSENT (its last same-ledger change was a removal — the pre-fix query resurrected it)")
	}
}

// TestNativeLiquidityPoolsRanked_SameLedgerLastChangeWins proves
// liquidity_pool_state_reader.go's argMax(entry_xdr, version) over
// ledger_entries_current. Two same-ledger changes to one pool key differ only in
// intra_ledger_seq (and thus the materialized `version`); the pre-fix
// argMax(entry_xdr, ledger_seq) ties on ledger_seq and can serve the stale
// reserves, while the `version` tie-break keeps the final change.
func TestNativeLiquidityPoolsRanked_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		key      = "c24c-native-lp-same-ledger-key"
		ledger   = uint32(72_000_001)
		staleRes = int64(111_0000000)
		finalRes = int64(222_0000000)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	var poolID [32]byte
	poolID[0] = 0xB4
	wantPoolStrkey, err := strkey.Encode(strkey.VersionByteLiquidityPool, poolID[:])
	if err != nil {
		t.Fatalf("encode pool strkey: %v", err)
	}

	// Two separate inserts → two un-merged ledger_entries_current parts, each
	// with one row for the key (optimize_on_insert would collapse duplicates
	// within a single block, so the reader-level tie must be exercised across
	// parts). Both rows share ledger_seq, so the pre-fix argMax(entry_xdr,
	// ledger_seq) ties; on the pinned ClickHouse image the tie resolves to the
	// LAST-created part, so seeding FINAL first and STALE last makes the pre-fix
	// query serve the stale reserves — while argMax(entry_xdr, version) keeps the
	// higher-version final row regardless of part order.
	final := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: closeTime, TxHash: "lpfinal", OpIndex: 1, ChangeIndex: 0,
		IntraLedgerSeq: 4, ChangeType: "updated", EntryType: "liquidity_pool", KeyXDR: key,
		EntryXDR: lpEntryB64(t, poolID, finalRes),
	}}
	stale := []chstore.LedgerEntryChangeRow{{
		LedgerSeq: ledger, CloseTime: closeTime, TxHash: "lpstale", OpIndex: 0, ChangeIndex: 0,
		IntraLedgerSeq: 3, ChangeType: "updated", EntryType: "liquidity_pool", KeyXDR: key,
		EntryXDR: lpEntryB64(t, poolID, staleRes),
	}}
	if _, err := chstore.InsertEntryChanges(ctx, addr, final, 0); err != nil {
		t.Fatalf("InsertEntryChanges (final): %v", err)
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, stale, 0); err != nil {
		t.Fatalf("InsertEntryChanges (stale): %v", err)
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	ranked, err := reader.NativeLiquidityPoolsRanked(ctx, 0)
	if err != nil {
		t.Fatalf("NativeLiquidityPoolsRanked: %v", err)
	}

	var found *chstore.NativeLiquidityPoolState
	for i := range ranked {
		if ranked[i].PoolStrkey == wantPoolStrkey {
			found = &ranked[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("seeded pool %s absent from ranked result", wantPoolStrkey)
	}
	if want := big.NewInt(finalRes); found.ReserveA == nil || found.ReserveA.Cmp(want) != 0 {
		t.Errorf("ReserveA = %v, want %d (same-ledger tie resolved to a stale mid-ledger reserve instead of the LAST change)", found.ReserveA, finalRes)
	}
}

// --- fixture builders (scaffolding only — assertions go through the real readers) ---

// contractIDFromSeed fills a 32-byte contract id with a single seed byte,
// consistent with mustContractStrkey(t, seed) (decoders_to_storage_test.go) so
// the id and its C-strkey refer to the same contract.
func contractIDFromSeed(seed byte) xdr.ContractId {
	var id xdr.ContractId
	id[0] = seed
	return id
}

// resDataKey mirrors the unexported clickhouse.poolDataKeyXDR key ScVal for a
// Blend PoolDataKey::ResData(asset) entry: Vec[Symbol("ResData"), Address(asset)].
func resDataKey(pool, asset xdr.ContractId) xdr.ScVal {
	a := asset
	sym := xdr.ScSymbol("ResData")
	assetAddr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &a}
	vec := &xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &assetAddr},
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
}

// resDataKeyB64 is the base64 LedgerKey the reader matches on (key_xdr column) —
// identical to clickhouse.poolDataKeyXDR(pool, "ResData", asset).
func resDataKeyB64(t *testing.T, pool, asset xdr.ContractId) string {
	t.Helper()
	p := pool
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &p},
			Key:        resDataKey(pool, asset),
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("marshal ResData key: %v", err)
	}
	return b64
}

// resDataEntryB64 builds a Blend ResData contract_data LedgerEntry whose value is
// the ScMap blend.DecodeReserveData expects, carrying the given b_rate.
func resDataEntryB64(t *testing.T, pool, asset xdr.ContractId, ledger uint32, bRate uint64) string {
	t.Helper()
	i128 := func(v uint64) xdr.ScVal {
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(v)}}
	}
	sym := func(s string) xdr.ScVal {
		ss := xdr.ScSymbol(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &ss}
	}
	u64 := func(v uint64) xdr.ScVal {
		u := xdr.Uint64(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u}
	}
	// Symbol-sorted map entries (canonical ScMap order).
	mp := &xdr.ScMap{
		{Key: sym("b_rate"), Val: i128(bRate)},
		{Key: sym("b_supply"), Val: i128(0)},
		{Key: sym("backstop_credit"), Val: i128(0)},
		{Key: sym("d_rate"), Val: i128(1_000_000_000_000)},
		{Key: sym("d_supply"), Val: i128(0)},
		{Key: sym("ir_mod"), Val: i128(10_000_000)},
		{Key: sym("last_time"), Val: u64(0)},
	}
	val := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
	p := pool
	entry := xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(ledger),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &p},
				Key:        resDataKey(pool, asset),
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal ResData entry: %v", err)
	}
	return b64
}

// lpEntryB64 builds a classic ConstantProduct liquidity_pool LedgerEntry with the
// given ReserveA (native / USDC pair) — the shape nativeLPStateFromEntry decodes.
func lpEntryB64(t *testing.T, poolID [32]byte, reserveA int64) string {
	t.Helper()
	var issuer xdr.Uint256
	copy(issuer[:], []byte("c24c-lp-usdc-issuer-seed--------"))
	var code xdr.AssetCode4
	copy(code[:], "USDC")
	assetB := xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: code, Issuer: xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &issuer}},
	}
	var pid xdr.PoolId
	copy(pid[:], poolID[:])
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeLiquidityPool,
			LiquidityPool: &xdr.LiquidityPoolEntry{
				LiquidityPoolId: pid,
				Body: xdr.LiquidityPoolEntryBody{
					Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
					ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
						Params:                   xdr.LiquidityPoolConstantProductParameters{AssetA: xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}, AssetB: assetB, Fee: 30},
						ReserveA:                 xdr.Int64(reserveA),
						ReserveB:                 1_000,
						TotalPoolShares:          1_000,
						PoolSharesTrustLineCount: 5,
					},
				},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal LP entry: %v", err)
	}
	return b64
}
