//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests are the sibling proof of the intra-ledger tie-break: three current-
// state readers that fold ledger_entry_changes / ledger_entries_current to the
// latest entry per key with a SINGLE-COLUMN ledger_seq tie-break, which resolves
// same-ledger multi-change keys to an ARBITRARY row (a stale mid-ledger value,
// or a resurrected removal). The fix tie-breaks on the composite intra-ledger
// order — (ledger_seq, intra_ledger_seq) over the base table, or the equivalent
// materialized `version` over ledger_entries_current — so the LAST change in a
// ledger deterministically wins, exactly as ledger_entries_current FINAL does.
//
// Every test seeds two changes to ONE key in the SAME ledger, in the adversarial
// physical order an argMax over ledger_seq alone mis-resolves, and asserts the reader returns
// the LAST change. They go RED against a plain argMax reader and GREEN with
// the composite order.

// TestQueryAccountBalance_SameLedgerLastChangeWins proves
// account_balance_reader.go's argMax(balance, (ledger_seq, intra_ledger_seq)).
// The two 'account' changes share a ledger; the stale one (intra 8) sorts FIRST
// in the base table's ORDER BY (ledger_seq, tx_hash, op_index, change_index), so
// a plain argMax(balance, ledger_seq) — which keeps the first row on a
// version tie — returns the stale balance. The composite order keeps the later
// change (intra 9).
func TestQueryAccountBalance_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		account  = "c24c-sibling-account-balance-GTEST"
		ledger   = uint32(71_000_001)
		staleBal = int64(100)
		finalBal = int64(200)
	)
	// Valid base64: lake-wide scans (the SAC full-history seed) base64Decode every key_xdr.
	key := base64.StdEncoding.EncodeToString([]byte("c24c-account-balance-same-ledger-key"))
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
//   - reserveVal (asset seeded update→update in one ledger): a plain
//     argMax(entry_xdr, ledger_seq) keeps the first-sorted row (op 0, the stale
//     b_rate); the composite order keeps the final b_rate.
//   - reserveGone (asset seeded update→remove in one ledger): a
//     non-empty-entry_xdr WHERE filter excluded the removal from the argMax, so
//     an earlier same-ledger update resurrected the key; the fix lets the
//     removal participate and drops it via HAVING on the winning change_type.
//
// The reader gets both properties from ledger_entries_current rather
// than folding them itself — FINAL over ReplacingMergeTree(version), where
// version = (ledger_seq << 32) | intra_ledger_seq, and a non-empty `entry_xdr` on the
// row FINAL kept. The assertions are unchanged BECAUSE the semantics are: this
// test is what proves the new path did not quietly relax either one.
func TestBlendPoolReserves_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A very high ledger, so the fixture sits at the lake's tip whichever
	// tests seeded before it. It must NOT outlive this test: the lake is one
	// process-shared container, and max(ledger_seq) over this table is the
	// upper bound of every whole-lake walker (the claimable-balance and SAC
	// full-history seeds step it in 250k-ledger windows). Left in place, a row
	// here turns each of their walks into ~16,000 empty windows — 53-58 s a
	// walk unloaded, past the 5-minute test deadline on a loaded machine
	// purgeLakeFixtureLedgers removes it again, synchronously.
	const ledger = uint32(4_000_000_000)
	purgeLakeFixtureLedgers(t, addr, ledger, ledger)
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
		// naive `entry_xdr != ''` filter would resurrect.
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
	states, err := reader.BlendPoolReserves(ctx, poolStr, blend.PoolV2, []string{assetValStr, assetGoneStr}, nil)
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

// lakeFixturePartitionLedgers mirrors stellar.ledger_entry_changes'
// `PARTITION BY intDiv(ledger_seq, 1000000)` (deploy/clickhouse/tier1_schema.sql).
const lakeFixturePartitionLedgers = uint32(1_000_000)

// purgeLakeFixtureLedgers registers a t.Cleanup that deletes every
// stellar.ledger_entry_changes row in [from, to] once the calling test has
// finished, and fails that test if any survive.
//
// It exists for fixtures seeded far ABOVE any realistic chain tip. The suite's
// isolation convention is "unique keys per test, every read filters by them"
// (clickhouse_harness_test.go), and that holds for keyed readers — but the
// table's max(ledger_seq) is global state no key can scope. Every whole-lake
// walker steps from min to max in fixed-width windows, so one abandoned
// 4,000,000,000-ledger row costs each later walk ~16,000 round-trips in the
// same process. The range must be the caller's OWN: ledger ranges are
// per-test here, which is what makes a range delete safe.
//
// The delete is a mutation scoped IN PARTITION, so it rewrites only the
// fixture's own parts rather than every part of a table the whole suite writes
// to, and mutations_sync = 2 makes it synchronous — when Cleanup returns the
// rows are gone, not scheduled to go. It runs on a fresh context: the test's
// own is already cancelled by its deferred cancel() when Cleanup fires.
//
// Only the append-log is purged. The rows these fixtures projected into
// ledger_entries_current / the TTL projection stay; those tables are read by
// key and feed no walk bound.
func purgeLakeFixtureLedgers(t *testing.T, addr string, from, to uint32) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		conn := dialClickHouse(t, ctx, "stellar")

		const countQ = `SELECT count() FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN ? AND ?`
		var before, after uint64
		if err := conn.QueryRow(ctx, countQ, from, to).Scan(&before); err != nil {
			t.Errorf("purge lake fixture [%d, %d]: count before: %v", from, to, err)
			return
		}
		for part := from / lakeFixturePartitionLedgers; part <= to/lakeFixturePartitionLedgers; part++ {
			q := fmt.Sprintf(`ALTER TABLE stellar.ledger_entry_changes DELETE IN PARTITION %d
				WHERE ledger_seq BETWEEN %d AND %d SETTINGS mutations_sync = 2`, part, from, to)
			if err := conn.Exec(ctx, q); err != nil {
				t.Errorf("purge lake fixture [%d, %d] in partition %d: %v", from, to, part, err)
				return
			}
		}
		if err := conn.QueryRow(ctx, countQ, from, to).Scan(&after); err != nil {
			t.Errorf("purge lake fixture [%d, %d]: count after: %v", from, to, err)
			return
		}
		if after != 0 {
			t.Errorf("purge lake fixture [%d, %d]: %d of %d rows survived the delete — every later whole-lake walk in this process will step to ledger %d",
				from, to, after, before, to)
			return
		}
		t.Logf("purged lake fixture [%d, %d]: %d of %d rows deleted", from, to, before-after, before)
	})
}

// TestNativeLiquidityPoolsRanked_SameLedgerLastChangeWins proves
// liquidity_pool_state_reader.go's argMax(entry_xdr, version) over
// ledger_entries_current. Two same-ledger changes to one pool key differ only in
// intra_ledger_seq (and thus the materialized `version`); a plain
// argMax(entry_xdr, ledger_seq) ties on ledger_seq and can serve the stale
// reserves, while the `version` tie-break keeps the final change.
func TestNativeLiquidityPoolsRanked_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		ledger   = uint32(72_000_001)
		staleRes = int64(111_0000000)
		finalRes = int64(222_0000000)
	)
	// Valid base64: lake-wide scans (the SAC full-history seed) base64Decode every key_xdr.
	key := base64.StdEncoding.EncodeToString([]byte("c24c-native-lp-same-ledger-key"))
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
	// parts). Both rows share ledger_seq, so a plain argMax(entry_xdr,
	// ledger_seq) ties; on the pinned ClickHouse image the tie resolves to the
	// LAST-created part, so seeding FINAL first and STALE last makes a plain
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
// consistent with mustContractStrkey(t, seed) (pg_sources_misc_test.go) so
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

// TestLedgerEntriesCurrent_SameLedgerLastChangeWins is the end-to-end proof of
// the intra-ledger tie-break against real ClickHouse: when one storage key is
// changed twice within the SAME ledger (here update-then-remove), the
// current-state projection stellar.ledger_entries_current must FINAL-resolve to
// the LAST change (the removal), never resurrect the before-image.
//
// Topology exercised: InsertEntryChanges → stellar.ledger_entry_changes → the
// ledger_entries_current_mv materialized view → stellar.ledger_entries_current
// (ReplacingMergeTree on version = ledger_seq<<32 | intra_ledger_seq) → FINAL.
//
// The two rows are inserted in the ADVERSARIAL order [removed, updated] on
// purpose: with a plain ReplacingMergeTree(ledger_seq) both rows tie on
// version, and ClickHouse keeps the LAST-inserted (the 'updated' before-image)
// — i.e. this exact test goes RED on a plain-ledger_seq schema. The composite version
// makes the removal (intra_ledger_seq 6 > 5) win regardless of insert order.
func TestLedgerEntriesCurrent_SameLedgerLastChangeWins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		key    = "c24c-update-then-remove-key"
		ledger = uint32(70_000_000)
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Canonical walk order in the ledger: the update (intra_ledger_seq 5) comes
	// before the removal (intra_ledger_seq 6). Rows are handed to the writer in
	// the REVERSE (adversarial) order so a ledger_seq-only version would keep
	// the wrong one.
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 6, ChangeType: "removed", EntryType: "account", KeyXDR: key,
		},
		{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: "aa", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 5, ChangeType: "updated", EntryType: "account", KeyXDR: key,
			EntryXDR: "before-image-should-not-win",
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{Database: "stellar"},
	})
	if err != nil {
		t.Fatalf("open clickhouse: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var (
		gotChangeType string
		gotVersion    uint64
		gotSeq        uint32
	)
	const q = `SELECT change_type, version, intra_ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND key_xdr = ?`
	if err := conn.QueryRow(ctx, q, key).Scan(&gotChangeType, &gotVersion, &gotSeq); err != nil {
		t.Fatalf("read ledger_entries_current FINAL: %v", err)
	}

	// The last intra-ledger change (the removal, seq 6) must be the survivor.
	if gotChangeType != "removed" {
		t.Errorf("FINAL current-state change_type = %q, want \"removed\" (the deleted entry was RESURRECTED — same-ledger tie resolved to a stale before-image)", gotChangeType)
	}
	if gotSeq != 6 {
		t.Errorf("winning intra_ledger_seq = %d, want 6 (the LAST change in the ledger must win)", gotSeq)
	}
	// version = (ledger_seq << 32) | intra_ledger_seq — the monotonic composite.
	if want := (uint64(ledger) << 32) | 6; gotVersion != want {
		t.Errorf("winning version = %d, want %d (ledger_seq<<32 | intra_ledger_seq)", gotVersion, want)
	}
}

// This file is the live-ClickHouse cost proof for the op-stream lake readers
// (StreamSDEXOps, StreamClassicOps). It pins TWO properties that a text grep
// for "CreatingSet" or "Join" cannot tell apart, because three different query
// shapes satisfy one each:
//
//	IN-subquery   (the defect): operation_results IS pruned, but the
//	              window's whole successful-tx set is materialised first —
//	              CreatingSetsTransform, a 10 GiB memory blowout.
//	derived-outer (the rejected first fix): no set-build, but the outer ledger
//	              window is hoisted into a derived table, so ClickHouse cannot
//	              propagate it through o.ledger_seq = r.ledger_seq and
//	              stellar.operation_results full-scans as grace_hash's spilled
//	              build side. One unbounded read traded for another.
//	shipped       (contractCallOpsQuery's shape): the successful-tx join spelled
//	              BEFORE the outer WHERE — no set-build AND pruned.
//
// Both rejected shapes are frozen below as oracles and both are asserted to
// still exhibit their pathology against this fixture, so neither assertion can
// pass vacuously.

const (
	// A ledger range of this test's own (partitions 30..31), clear of every
	// other ClickHouse integration fixture's.
	opsPruneBase = uint32(30_500_001)
	// One op / result / tx per ledger. ~123 granules of operation_results, so
	// the 1,000-ledger read window is one granule when pruning works and all
	// of them when it does not.
	opsPruneRows = 1_000_000
	// The window every query under test reads.
	opsPruneWindow = uint32(1_000)
	// Every opsPruneFailMod'th tx is a FAILED tx: the successful-tx filter has
	// to actually exclude rows, or the differential proves nothing.
	opsPruneFailMod = 7
	// A re-ingested duplicate PART of stellar.transactions covering the read
	// window. These readers take no FINAL, so it puts the same tx_hash on the
	// join's build side twice — what `GROUP BY tx_hash` in the derived table
	// exists to absorb.
	opsPruneDupRows = 5_000

	opsPruneSDEXType    = "OperationTypeManageSellOffer"
	opsPruneClassicType = "OperationTypePayment"
)

// ── the shapes this test exists to rule out ─────────────────────────────────

// inSubqueryOpsSQL is the IN-subquery successful-tx filter that sdexOpsQuery
// and classicOpsQuery must not use: its CreatingSet step materialises the
// whole window's tx-hash set before the join runs. The sibling
// contractCallOpsQuery is likewise off this shape.
func inSubqueryOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		  AND o.tx_hash IN (
		      SELECT tx_hash FROM stellar.transactions
		      WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		  )
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// derivedWindowOpsSQL is the REJECTED first remediation: the IN-set is gone,
// but the outer ledger window is wrapped in a derived table, which costs
// stellar.operation_results its primary-key pruning entirely.
func derivedWindowOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM (
		    SELECT ledger_seq, close_time, tx_hash, op_index, source_account, body_xdr
		    FROM stellar.operations
		    WHERE ledger_seq BETWEEN %d AND %d
		      AND op_type IN (%s)
		) AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		    GROUP BY tx_hash
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, opTypes, from, to)
}

// joinNoDedupeOpsSQL is the SHIPPED shape with the derived table's
// `GROUP BY tx_hash` removed — the correctness hazard IN's set semantics used
// to cover for free, and the reason the dedupe is not cosmetic.
func joinNoDedupeOpsSQL(opTypes string, from, to uint32) string {
	return fmt.Sprintf(`
		SELECT o.ledger_seq, o.close_time, o.tx_hash, o.op_index, o.source_account,
		       o.body_xdr, r.result_xdr
		FROM stellar.operations AS o
		INNER JOIN (
		    SELECT tx_hash FROM stellar.transactions
		    WHERE successful = 1 AND ledger_seq BETWEEN %d AND %d
		) AS t ON o.tx_hash = t.tx_hash
		INNER JOIN stellar.operation_results AS r
		  ON o.ledger_seq = r.ledger_seq AND o.tx_hash = r.tx_hash AND o.op_index = r.op_index
		WHERE o.ledger_seq BETWEEN %d AND %d
		  AND o.op_type IN (%s)
		ORDER BY o.ledger_seq, o.tx_hash, o.op_index
		SETTINGS join_algorithm = 'grace_hash', grace_hash_join_initial_buckets = 32`,
		from, to, from, to, opTypes)
}

// ── fixture ─────────────────────────────────────────────────────────────────

type opsPruneFixture struct {
	from, to    uint32
	wantSDEX    int // trade-typed ops in the window whose tx succeeded
	wantClassic int // payment-typed ops in the window whose tx succeeded
}

// seedOpsPruneFixture writes opsPruneRows ledgers of one op / one result / one
// tx each, alternating a trade op type and a classic one, with every
// opsPruneFailMod'th tx failed, plus a duplicate transactions part over the
// read window. Merges on stellar.transactions are stopped for the duration so
// that duplicate part is still there when the queries run.
func seedOpsPruneFixture(t *testing.T, ctx context.Context, raw driver.Conn) opsPruneFixture {
	t.Helper()
	const sourceAccount = "GTEST_OPSPRUNE_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}
	mustExec(`SYSTEM STOP MERGES stellar.transactions`)
	// WithoutCancel, not the test's ctx: cleanups run after the test function
	// has returned and cancelled it, and leaving merges stopped would leak into
	// every later test sharing this container.
	restoreCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() { _ = raw.Exec(restoreCtx, `SYSTEM START MERGES stellar.transactions`) })

	seq := fmt.Sprintf(`toUInt32(%d + number)`, opsPruneBase)
	hash := `lpad(toString(` + seq + `), 64, '0')`
	// Kept well before the reserved tip day so the fixture never becomes the global close_time
	// tip another test anchors on (lake_test.go).
	closeAt := `toDateTime('2026-04-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	isTrade := `number % 2 = 0`
	body := fmt.Sprintf(`if(%s, '%s', '%s')`, isTrade,
		opsPruneBodyB64(t, opsPruneSellOfferBody()), opsPruneBodyB64(t, opsPrunePaymentBody()))

	mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		SELECT %s, %s, %s, 0, 0, if(%s, '%s', '%s'), '%s', %s FROM numbers(%d)`,
		seq, closeAt, hash, isTrade, opsPruneSDEXType, opsPruneClassicType, sourceAccount, body, opsPruneRows))
	mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_results
		(ledger_seq, tx_hash, op_index, result_code, result_xdr)
		SELECT %s, %s, 0, 0, '%s' FROM numbers(%d)`,
		seq, hash, opsPruneResultB64(t), opsPruneRows))
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee,
			 operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, '%s', 100, 200, 1, if(number %% %d = 0, 0, 1), 0, 'none', ''
			FROM numbers(%d) WHERE %s`,
			seq, closeAt, hash, sourceAccount, opsPruneFailMod, opsPruneRows, where))
	}
	insertTxs(`1`)
	insertTxs(fmt.Sprintf(`number < %d`, opsPruneDupRows))

	f := opsPruneFixture{from: opsPruneBase, to: opsPruneBase + opsPruneWindow - 1}
	for n := uint32(0); n < opsPruneWindow; n++ {
		if n%opsPruneFailMod == 0 {
			continue
		}
		if n%2 == 0 {
			f.wantSDEX++
		} else {
			f.wantClassic++
		}
	}
	if f.wantSDEX == 0 || f.wantClassic == 0 {
		t.Fatalf("fixture window yields no rows (sdex %d, classic %d)", f.wantSDEX, f.wantClassic)
	}
	return f
}

func opsPruneSellOfferBody() xdr.OperationBody {
	const issuerAccount = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	return xdr.OperationBody{
		Type: xdr.OperationTypeManageSellOffer,
		ManageSellOfferOp: &xdr.ManageSellOfferOp{
			Selling: xdr.MustNewNativeAsset(),
			Buying:  xdr.MustNewCreditAsset("USDC", issuerAccount),
			Amount:  xdr.Int64(1_000_0000),
			Price:   xdr.Price{N: 1, D: 2},
		},
	}
}

func opsPrunePaymentBody() xdr.OperationBody {
	const destAccount = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	return xdr.OperationBody{
		Type: xdr.OperationTypePayment,
		PaymentOp: &xdr.PaymentOp{
			Destination: xdr.MustMuxedAddress(destAccount),
			Asset:       xdr.MustNewNativeAsset(),
			Amount:      xdr.Int64(42_0000000),
		},
	}
}

func opsPruneBodyB64(t *testing.T, body xdr.OperationBody) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(body)
	if err != nil {
		t.Fatalf("marshal op body: %v", err)
	}
	return b64
}

func opsPruneResultB64(t *testing.T) string {
	t.Helper()
	b64, err := xdr.MarshalBase64(xdr.OperationResult{Code: xdr.OperationResultCodeOpBadAuth})
	if err != nil {
		t.Fatalf("marshal op result: %v", err)
	}
	return b64
}

// ── plan + cost helpers ─────────────────────────────────────────────────────

type opsPruneKey struct {
	ledger  uint32
	txHash  string
	opIndex uint32
}

// explainIndexes returns `EXPLAIN indexes = 1` for sql, one plan line per
// element (indentation preserved — granulesFor walks the subtree by indent).
func explainIndexes(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []string {
	t.Helper()
	rows, err := raw.Query(ctx, "EXPLAIN indexes = 1 "+sql)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN %s: %v", label, err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN %s rows: %v", label, err)
	}
	if len(plan) == 0 {
		t.Fatalf("EXPLAIN %s returned no plan", label)
	}
	return plan
}

func planHasStep(plan []string, step string) bool {
	for _, l := range plan {
		if strings.Contains(l, step) {
			return true
		}
	}
	return false
}

// granulesFor reads the LAST `Granules: read/total` of the ReadFromMergeTree
// node for `table` — the count after the final index step (Min-Max, then
// Partition, then PrimaryKey, then any skip index), i.e. what the query
// actually reads. ok is false when the node carries no index analysis at all,
// which is itself the unpruned verdict.
//
// The node's `Indexes:` block sits at the SAME indentation as the
// ReadFromMergeTree line, with its sections nested under it; the subtree walk
// has to allow that one sibling or it stops before reading anything.
func granulesFor(plan []string, table string) (read, total int, ok bool) {
	node := -1
	for i, l := range plan {
		if strings.Contains(l, "ReadFromMergeTree") && strings.Contains(l, table) {
			node = i
			break
		}
	}
	if node < 0 {
		return 0, 0, false
	}
	indent := len(plan[node]) - len(strings.TrimLeft(plan[node], " "))
	for _, l := range plan[node+1:] {
		body := strings.TrimLeft(l, " ")
		if body == "" {
			continue
		}
		switch depth := len(l) - len(body); {
		case depth < indent, depth == indent && body != "Indexes:":
			return read, total, ok
		}
		var r, tot int
		if _, err := fmt.Sscanf(body, "Granules: %d/%d", &r, &tot); err == nil {
			read, total, ok = r, tot, true
		}
	}
	return read, total, ok
}

// requirePrunedOperationResults is the assertion the previous remediation
// failed: the ledger window must still reach stellar.operation_results through
// the join key, so the read is a primary-key range and not the whole table.
func requirePrunedOperationResults(t *testing.T, label string, plan []string) {
	t.Helper()
	read, total, ok := granulesFor(plan, "operation_results")
	if !ok {
		t.Fatalf("%s: stellar.operation_results is read with NO primary-key index analysis — "+
			"the ledger window is not propagated into the join's build side, so it full-scans "+
			"full chain history of wide result_xdr:\n%s", label, strings.Join(plan, "\n"))
	}
	if read >= total {
		t.Fatalf("%s: stellar.operation_results reads ALL %d/%d granules — primary-key pruning lost:\n%s",
			label, read, total, strings.Join(plan, "\n"))
	}
	t.Logf("%s: operation_results granules %d/%d (pruned)", label, read, total)
}

// oracleKeys runs a frozen oracle query and returns its emitted keys.
func oracleKeys(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) []opsPruneKey {
	t.Helper()
	rows, err := raw.Query(ctx, sql)
	if err != nil {
		t.Fatalf("%s: %v\n%s", label, err, sql)
	}
	defer func() { _ = rows.Close() }()
	var out []opsPruneKey
	for rows.Next() {
		var (
			k                    opsPruneKey
			closeTime            time.Time
			source, body, result string
		)
		if err := rows.Scan(&k.ledger, &closeTime, &k.txHash, &k.opIndex, &source, &body, &result); err != nil {
			t.Fatalf("%s scan: %v", label, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s rows: %v", label, err)
	}
	return out
}

// readRowsOf runs sql under a fresh query id and reports its read_rows.
func readRowsOf(t *testing.T, ctx context.Context, raw driver.Conn, label, sql string) uint64 {
	t.Helper()
	id := uuid.NewString()
	_ = oracleKeys(t, clickhouse.Context(ctx, clickhouse.WithQueryID(id)), raw, label, sql)
	return queryLogReadRows(t, ctx, raw, label, id)
}

func queryLogReadRows(t *testing.T, ctx context.Context, raw driver.Conn, label, queryID string) uint64 {
	t.Helper()
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	var rr uint64
	if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, queryID).Scan(&rr); err != nil {
		t.Fatalf("query_log read_rows (%s): %v", label, err)
	}
	return rr
}

// captureShippedQuery drives a reader through its PRODUCTION entry point and
// recovers the exact SQL ClickHouse executed (the driver binds client-side, so
// system.query_log holds the statement with its literals) plus its read_rows.
func captureShippedQuery(t *testing.T, ctx context.Context, raw driver.Conn,
	label string, stream func(context.Context) error,
) (sql string, readRows uint64) {
	t.Helper()
	id := uuid.NewString()
	if err := stream(clickhouse.Context(ctx, clickhouse.WithQueryID(id))); err != nil {
		t.Fatalf("%s stream: %v", label, err)
	}
	if err := raw.Exec(ctx, `SYSTEM FLUSH LOGS`); err != nil {
		t.Fatalf("flush logs: %v", err)
	}
	if err := raw.QueryRow(ctx, `SELECT query, read_rows FROM system.query_log
		WHERE query_id = ? AND type = 'QueryFinish'
		ORDER BY event_time_microseconds DESC LIMIT 1`, id).Scan(&sql, &readRows); err != nil {
		t.Fatalf("query_log (%s): %v", label, err)
	}
	return sql, readRows
}

// opTypeInListFrom lifts the op-type IN list out of the shipped statement, so
// the frozen oracles filter on EXACTLY what the reader filtered on and any row
// difference between them is attributable to the query SHAPE alone.
func opTypeInListFrom(t *testing.T, label, sql string) string {
	t.Helper()
	// Unqualified, so a reshaped statement that spells the filter inside a
	// derived table still reaches the pruning verdict below rather than dying
	// here on a cosmetic alias change.
	const marker = "op_type IN ("
	i := strings.Index(sql, marker)
	if i < 0 {
		t.Fatalf("%s: shipped statement has no op-type filter:\n%s", label, sql)
	}
	rest := sql[i+len(marker):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("%s: unterminated op-type filter:\n%s", label, sql)
	}
	return rest[:j]
}

// ── the test ────────────────────────────────────────────────────────────────

// TestStreamOpsQueriesPruneOperationResults is the live proof.
func TestStreamOpsQueriesPruneOperationResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	f := seedOpsPruneFixture(t, ctx, raw)

	var sdexKeys []opsPruneKey
	sdexSQL, sdexRead := captureShippedQuery(t, ctx, raw, "sdex", func(qctx context.Context) error {
		sdexKeys = nil
		return chstore.StreamSDEXOps(qctx, addr, f.from, f.to, func(op chstore.SDEXOp) error {
			sdexKeys = append(sdexKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
			return nil
		})
	})
	assertOpStreamBounded(t, ctx, raw, f, "sdex", sdexSQL, sdexRead, sdexKeys, f.wantSDEX)

	var classicKeys []opsPruneKey
	classicSQL, classicRead := captureShippedQuery(t, ctx, raw, "classic", func(qctx context.Context) error {
		classicKeys = nil
		return chstore.StreamClassicOps(qctx, addr, f.from, f.to,
			[]string{opsPruneClassicType}, func(op chstore.ClassicOp) error {
				classicKeys = append(classicKeys, opsPruneKey{op.Ledger, op.TxHash, op.OpIndex})
				return nil
			})
	})
	assertOpStreamBounded(t, ctx, raw, f, "classic", classicSQL, classicRead, classicKeys, f.wantClassic)
}

// assertOpStreamBounded pins, for one shipped op-stream statement: no
// successful-tx set-build, primary-key pruning retained on
// stellar.operation_results (the rejected remediation), the exact rows the
// IN-subquery shape served (no over- or under-count from the join), and a
// read_rows bound. Each assertion is paired with a non-vacuity guard against
// the frozen oracle that is supposed to violate it.
func assertOpStreamBounded(t *testing.T, ctx context.Context, raw driver.Conn, f opsPruneFixture,
	name, shippedSQL string, shippedRead uint64, got []opsPruneKey, want int,
) {
	t.Helper()
	opTypes := opTypeInListFrom(t, name, shippedSQL)
	legacySQL := inSubqueryOpsSQL(opTypes, f.from, f.to)
	rejectedSQL := derivedWindowOpsSQL(opTypes, f.from, f.to)

	// (1) The defect: the successful-tx set must not be materialised.
	plan := explainIndexes(t, ctx, raw, name+" shipped", shippedSQL)
	if planHasStep(plan, "CreatingSet") {
		t.Errorf("%s: the shipped statement still plans a CreatingSet — the window's whole "+
			"successful-tx hash set is materialised before the join (the 10 GiB blowout of "+
			"2026-07-11):\n%s", name, strings.Join(plan, "\n"))
	}
	if !planHasStep(explainIndexes(t, ctx, raw, name+" in-subquery oracle", legacySQL), "CreatingSet") {
		t.Fatalf("%s: the frozen IN-subquery oracle no longer plans a CreatingSet against this "+
			"fixture — the assertion above cannot fail and is vacuous", name)
	}

	// (2) The rejected remediation: pruning must survive the reshape.
	requirePrunedOperationResults(t, name+" shipped", plan)
	rejectedPlan := explainIndexes(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	if read, total, ok := granulesFor(rejectedPlan, "operation_results"); ok && read < total {
		t.Fatalf("%s: the frozen derived-window oracle still prunes operation_results (%d/%d "+
			"granules) — the pruning assertion above cannot fail and is vacuous", name, read, total)
	}

	// (3) Rows: exactly what the IN-subquery shape served, and no more.
	wantKeys := oracleKeys(t, ctx, raw, name+" in-subquery oracle", legacySQL)
	if len(got) != len(wantKeys) || len(got) != want {
		t.Fatalf("%s: reader served %d rows, IN-subquery oracle %d, fixture expects %d",
			name, len(got), len(wantKeys), want)
	}
	for i := range got {
		if got[i] != wantKeys[i] {
			t.Fatalf("%s row %d differs: reader %+v, IN-subquery oracle %+v", name, i, got[i], wantKeys[i])
		}
	}
	if fanned := oracleKeys(t, ctx, raw, name+" no-dedupe oracle",
		joinNoDedupeOpsSQL(opTypes, f.from, f.to)); len(fanned) <= len(got) {
		t.Fatalf("%s: dropping GROUP BY tx_hash served %d rows, not more than the shipped %d — "+
			"the duplicate transactions part no longer fans out, so the dedupe is unproven",
			name, len(fanned), len(got))
	}

	// (4) Cost: the shipped read stays a window read.
	rejectedRead := readRowsOf(t, ctx, raw, name+" derived-window oracle", rejectedSQL)
	t.Logf("%s read_rows: shipped=%d derived-window=%d (window %d ledgers of %d)",
		name, shippedRead, rejectedRead, opsPruneWindow, opsPruneRows)
	if rejectedRead < opsPruneRows {
		t.Fatalf("%s: the derived-window oracle read only %d rows — it is not the unbounded scan "+
			"this bound is calibrated against", name, rejectedRead)
	}
	if shippedRead*4 > rejectedRead {
		t.Fatalf("%s: the shipped statement read %d rows against the unpruned shape's %d — the read "+
			"must be bounded by the ledger window, not by chain history", name, shippedRead, rejectedRead)
	}
}

// legacyAccountOperationsSQL is the AccountOperations page query as it stood
// before the primary-key-pruning rewrite (accountOperationsQuery(hasCursor,
// hasBound=true) with opCols expanded), frozen here as the differential
// oracle: the rewrite must serve byte-identical pages — same rows, order,
// cursor semantics and source/participant dedupe — while no longer reading
// one stellar.operations granule per key the account ever touched.
func legacyAccountOperationsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index, op_index) < (?, ?, ?)`
	}
	boundClause := ` AND ledger_seq <= ?`
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr FROM stellar.operations
		WHERE (ledger_seq, tx_index, op_index) IN (
		  SELECT ledger_seq, tx_index, op_index FROM (
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.ops_by_source
		            WHERE source_account = ? AND op_index != 4294967295)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index, op_index FROM stellar.operations
		       WHERE (ledger_seq, tx_index, op_index) IN (
		            SELECT ledger_seq, tx_index, op_index FROM stellar.operation_participants WHERE account = ?)` + boundClause + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		       LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC
		LIMIT 1 BY ledger_seq, tx_index, op_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// legacyAccountTransactionsSQL is AccountTransactions' page query as it stood
// before the same rewrite (origin/main 6762d0a1, accountTransactionsQuery with
// txCols expanded) — the differential oracle for the transactions walk.
func legacyAccountTransactionsSQL(hasCursor bool) string {
	cursorClause := ""
	if hasCursor {
		cursorClause = ` AND (ledger_seq, tx_index) < (?, ?)`
	}
	return `SELECT ledger_seq, close_time, tx_hash, tx_index, source_account,
	fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo FROM stellar.transactions
		WHERE (ledger_seq, tx_index) IN (
		  SELECT ledger_seq, tx_index FROM (
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.ops_by_source WHERE source_account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		    UNION ALL
		    (SELECT ledger_seq, tx_index FROM stellar.transactions
		       WHERE (ledger_seq, tx_index) IN (
		            SELECT DISTINCT ledger_seq, tx_index FROM stellar.operation_participants WHERE account = ?)` + cursorClause + `
		       ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?)
		  ) ORDER BY ledger_seq DESC, tx_index DESC LIMIT ?)
		ORDER BY ledger_seq DESC, tx_index DESC LIMIT 1 BY ledger_seq, tx_index LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_bytes_before_external_group_by = 4000000000, max_bytes_before_external_sort = 4000000000`
}

// TestClickHouseAccountOperationsPageBoundedByPageSize is the live-ClickHouse
// proof for the AccountOperations rewrite (explorer `AccountOperations
// deadline exceeded` 503s for an account with 11,925 sourced + 26,064
// participant ops).
//
// Pathology: the arms resolved their keys OVER stellar.operations with
// `pk IN (SELECT pk FROM ops_by_source WHERE source_account = ?)`.
// ClickHouse's set-based index analysis prunes that to exact granules — one
// granule PER KEY IN THE SET, before the LIMIT — so a page cost the account's
// whole history in granules (live query_log: 164–238 M rows, ~8 s, page of
// 50). The fix pages ops_by_source / operation_participants directly
// (primary-key-prefix range reads) and hydrates ≤ 3×limit keys.
//
// Fixture: 2 M ledgers × one tx × one op, in ONE ingest so they land in the
// same part and each 8192-row granule is distinct. The hot account:
//   - sources every 10,000th op AND its tx (200 keys, one per granule);
//   - is a non-source participant on a different 200 ops (offset 5,000),
//     whose tx is sourced by the filler account;
//   - sources a further 200 TXs (offset 2,500) whose only op is sourced by
//     the filler account and has hot as a participant — the tx appears in
//     BOTH transaction arms (legitimate per accountTransactionsQuery's
//     "DISTINCT dedups the rare tx that is both sourced and participated"),
//     while on the OPERATIONS side it is participant-only, honouring the
//     op-level XOR invariant accountOperationsQuery relies on (participants
//     exclude the op's own source; that listing's merge would serve a
//     SHORT page if the invariant were violated — see the "no cross-arm
//     dedupe needed" note there). On the TRANSACTIONS side this overlap
//     was the bug: the key was emitted by both arms and ate two of
//     the merge's LIMIT slots.
//
// A re-ingested duplicate part covers the ReplacingMergeTree dedupe on
// both listings. The test then:
//
//  1. DIFFERENTIAL: walks the ENTIRE history of BOTH listings (page size
//     7, ~58 / ~86 pages) through the reader and through the frozen
//     legacy SQL with the same args, plus a cursor set MID-page. The
//     OPERATIONS walk asserts every page identical — rows, order, cursor
//     continuation, dedupe. The TRANSACTIONS walk asserts the same
//     SEQUENCE of rows in the same order, but not the same page
//     boundaries: the reader dedupes the two overlapping arms
//     at the keyset merge, so its pages are full where the frozen
//     legacy shape still hands back a SHORT one (the walk requires the
//     legacy side to still produce one, else the reader-side fullness
//     assertion would be vacuous). Also pins the absolute expectations
//     (600 distinct ops / 600 distinct txs, strictly descending, no key
//     served twice, and no non-final short page on EITHER listing from
//     the reader — the handler withholds next_cursor on a short page, so
//     one truncates the client's history walk).
//  2. READ-ROWS: one page of 50 via each path, `system.query_log`
//     read_rows. The legacy shape reads ≥ one granule per key (≥ 200
//     granules ≈ 1.6 M rows here; 38 k granules live); the new shape reads
//     ≤ 3×limit granules. Red-proof: with the legacy query text in the reader
//     the reader's read_rows equal the legacy figure and the 4× bound
//     fails.
func TestClickHouseAccountOperationsPageBoundedByPageSize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		hot  = "GTEST_PKP_HOT_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		fill = "GTEST_PKP_FILLER_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		base = uint32(9_500_001) // partitions 9..11; nowhere near other tests' ledgers
		n    = 2_000_000
		// Sourced every `stride` ledgers (one key per 8192-row granule);
		// participant on the same stride at `partOff`; overlap every
		// `overlapStride` (a sourced op HOT also participates in).
		stride  = 10_000
		partOff = 5_000
		txOff   = 2_500
	)
	// close_time: ms offsets keep the whole fixture before the reserved tip day so
	// it never becomes the global close_time tip another test anchors on
	// (see lake_test.go).
	closeExpr := `toDateTime('2026-05-01 00:00:00', 'UTC') + toIntervalMillisecond(number)`
	seqExpr := fmt.Sprintf(`toUInt32(%d + number)`, base)
	hashExpr := `lpad(toString(` + seqExpr + `), 64, '0')`

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.80q: %v", q, err)
		}
	}
	hotOp := fmt.Sprintf(`number %% %d = 0`, stride)
	hotPart := fmt.Sprintf(`(number %% %d = %d OR number %% %d = %d)`, stride, partOff, stride, txOff)
	hotTx := fmt.Sprintf(`(number %% %d = 0 OR number %% %d = %d)`, stride, stride, txOff)
	insertOps := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operations
			(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
			SELECT %s, %s, %s, 0, 0, 'OperationTypePayment', if(%s, '%s', '%s'), 'Ym9keQ=='
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotOp, hot, fill, n, where))
	}
	insertTxs := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.transactions
			(ledger_seq, close_time, tx_hash, tx_index, source_account, fee_charged, max_fee, operation_count, successful, result_code, memo_type, memo)
			SELECT %s, %s, %s, 0, if(%s, '%s', '%s'), 100, 200, 1, 1, 0, 'none', ''
			FROM numbers(%d) WHERE %s`, seqExpr, closeExpr, hashExpr, hotTx, hot, fill, n, where))
	}
	insertParts := func(where string) {
		mustExec(fmt.Sprintf(`INSERT INTO stellar.operation_participants
			(account, ledger_seq, close_time, tx_hash, tx_index, op_index)
			SELECT '%s', %s, %s, %s, 0, 0 FROM numbers(%d) WHERE %s AND %s`,
			hot, seqExpr, closeExpr, hashExpr, n, hotPart, where))
	}
	insertOps(`1`)
	insertTxs(`1`)
	insertParts(`1`)
	// Re-ingested duplicate PARTS near the bottom of the range (MVs
	// re-fire, so ops_by_source and account_activity get duplicates too).
	dup := fmt.Sprintf(`number < %d`, 3*stride)
	insertOps(dup)
	insertTxs(dup)
	insertParts(dup)

	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, hot).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm == 0 {
		t.Fatal("fixture: account_activity watermark missing — the reader would take the unbounded path and the legacy oracle (bounded) would not be the same query")
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	legacyPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.OpRow {
		t.Helper()
		args := []any{hot, wm}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, hot, wm)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A, cur.B)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountOperationsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.OpRow
		for rows.Next() {
			var r chstore.OpRow
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.OpIndex, &r.OpType, &r.SourceAccount, &r.BodyXDR); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	legacyTxPage := func(qctx context.Context, limit int, cur chstore.ExplorerCursor) []chstore.TxSummary {
		t.Helper()
		args := []any{hot}
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, hot)
		if cur.IsSet() {
			args = append(args, cur.Ledger, cur.A)
		}
		args = append(args, limit, limit, limit)
		rows, err := raw.Query(qctx, legacyAccountTransactionsSQL(cur.IsSet()), args...)
		if err != nil {
			t.Fatalf("legacy tx page: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []chstore.TxSummary
		for rows.Next() {
			var r chstore.TxSummary
			var ok uint8
			if err := rows.Scan(&r.Seq, &r.CloseTime, &r.TxHash, &r.TxIndex, &r.SourceAccount,
				&r.FeeCharged, &r.MaxFee, &r.OperationCount, &ok, &r.ResultCode, &r.MemoType, &r.Memo); err != nil {
				t.Fatalf("legacy tx scan: %v", err)
			}
			r.Successful = ok != 0
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("legacy tx rows: %v", err)
		}
		return out
	}

	// (1) Full-history differential walk, page size 7.
	const pageSize = 7
	type key [3]uint32
	older := func(k, prev key) bool {
		return k[0] < prev[0] || (k[0] == prev[0] && (k[1] < prev[1] || (k[1] == prev[1] && k[2] < prev[2])))
	}
	// walk pages `name` through reader vs legacy until the reader serves
	// an empty page, asserting page-for-page equality; returns the
	// distinct keys served. Every page is `pageSize` long but the last:
	// a non-final short page is exactly what makes a client stop early
	// (the handler withholds next_cursor on it — see the transactions
	// walk below).
	walk := func(name string, page func(cur chstore.ExplorerCursor) (got, want []key)) map[key]bool {
		t.Helper()
		var (
			cur   chstore.ExplorerCursor
			seen  = map[key]bool{}
			pages int
			last  key
			short int // pages shorter than pageSize; only the final one may be
		)
		for {
			got, want := page(cur)
			if len(got) > 0 {
				if short > 0 {
					t.Fatalf("%s page %d (cursor %+v) follows a SHORT page — a non-final short page means a client stopping on it truncates history", name, pages, cur)
				}
				if len(got) < pageSize {
					short++
				}
			}
			if len(got) != len(want) {
				t.Fatalf("%s page %d (cursor %+v): reader %d rows, legacy %d rows", name, pages, cur, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s page %d row %d differs (cursor %+v): reader %v legacy %v", name, pages, i, cur, got[i], want[i])
				}
				if seen[got[i]] {
					t.Fatalf("%s page %d row %d: key %v served twice (dedupe / cursor regression)", name, pages, i, got[i])
				}
				if (pages > 0 || i > 0) && !older(got[i], last) {
					t.Fatalf("%s page %d row %d: key %v not strictly older than previous %v", name, pages, i, got[i], last)
				}
				seen[got[i]] = true
				last = got[i]
			}
			if len(got) == 0 {
				break
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1], B: last[2]}
		}
		t.Logf("%s: %d distinct keys over %d pages", name, len(seen), pages)
		return seen
	}

	opKeys := func(rows []chstore.OpRow) []key {
		out := make([]key, len(rows))
		for i, r := range rows {
			out[i] = key{r.Seq, r.TxIndex, r.OpIndex}
		}
		return out
	}
	opsSeen := walk("operations", func(cur chstore.ExplorerCursor) ([]key, []key) {
		got, _, err := er.AccountOperations(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountOperations (cursor %+v): %v", cur, err)
		}
		want := legacyPage(ctx, pageSize, cur)
		// Full-row equality (close_time, hash, type, source, body), not
		// just keys — the hydration must serve the same wide row.
		for i := range got {
			if i < len(want) && got[i] != want[i] {
				t.Fatalf("operations row differs (cursor %+v):\n reader %+v\n legacy %+v", cur, got[i], want[i])
			}
		}
		return opKeys(got), opKeys(want)
	})
	if want := 3 * (n / stride); len(opsSeen) != want {
		t.Fatalf("operations: %d distinct ops, want %d (sourced %d + participant %d + participant-on-hot-sourced-tx %d)",
			len(opsSeen), want, n/stride, n/stride, n/stride)
	}
	// The tx listing's two arms legitimately overlap (the txOff txs, sourced
	// by hot AND carrying it as a non-source participant). Without dedupe an
	// overlap key occupied TWO of the keyset merge's LIMIT slots — the merge
	// took its LIMIT before anything deduped — so the page came back SHORT
	// while older history remained, and the handler emits next_cursor only on
	// a FULL page (internal/api/v1/explorer/accounts.go): a client walking
	// the history stopped there, silently truncated. The reader now dedupes
	// at the merge; the frozen legacy shape still serves those short pages,
	// so the differential here is over the WHOLE walk (same rows, same order,
	// none gained, lost, reordered or repeated) rather than page-for-page,
	// and the reader's pages must additionally be full except the last.
	txWalk := func(name string, page func(cur chstore.ExplorerCursor) []chstore.TxSummary) (rows []chstore.TxSummary, nonFinalShort int) {
		t.Helper()
		var (
			cur       chstore.ExplorerCursor
			seen      = map[key]bool{}
			pages     int
			last      key
			prevShort bool
		)
		for {
			got := page(cur)
			if len(got) > 0 && prevShort {
				nonFinalShort++
			}
			if len(got) == 0 {
				break
			}
			prevShort = len(got) < pageSize
			for i, r := range got {
				k := key{r.Seq, r.TxIndex, 0}
				if seen[k] {
					t.Fatalf("%s page %d row %d: tx %v served twice (dedupe / cursor regression)", name, pages, i, k)
				}
				if (pages > 0 || i > 0) && !older(k, last) {
					t.Fatalf("%s page %d row %d: tx %v not strictly older than previous %v", name, pages, i, k, last)
				}
				seen[k] = true
				last = k
				rows = append(rows, r)
			}
			pages++
			cur = chstore.ExplorerCursor{Ledger: last[0], A: last[1]}
		}
		t.Logf("%s: %d txs over %d pages (%d non-final short pages)", name, len(rows), pages, nonFinalShort)
		return rows, nonFinalShort
	}

	readerTxs, readerShort := txWalk("transactions (reader)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		got, _, err := er.AccountTransactions(ctx, hot, pageSize, cur)
		if err != nil {
			t.Fatalf("AccountTransactions (cursor %+v): %v", cur, err)
		}
		return got
	})
	legacyTxs, legacyShort := txWalk("transactions (legacy)", func(cur chstore.ExplorerCursor) []chstore.TxSummary {
		return legacyTxPage(ctx, pageSize, cur)
	})
	if readerShort != 0 {
		t.Fatalf("transactions: %d non-final SHORT pages from the reader — the handler withholds next_cursor on a short page, so a client stops there with older history unreached (#290)", readerShort)
	}
	if legacyShort == 0 {
		t.Fatal("fixture no longer reproduces #290: the pre-fix query text served no non-final short page, so the fullness assertion above is vacuous — the arms must still overlap (the txOff txs)")
	}
	if len(readerTxs) != len(legacyTxs) {
		t.Fatalf("transactions: reader walked %d txs, legacy %d — the merge dedupe must change page BOUNDARIES, never the rows served", len(readerTxs), len(legacyTxs))
	}
	for i := range readerTxs {
		if readerTxs[i] != legacyTxs[i] {
			t.Fatalf("transactions row %d differs:\n reader %+v\n legacy %+v", i, readerTxs[i], legacyTxs[i])
		}
	}
	if want := 3 * (n / stride); len(readerTxs) != want {
		t.Fatalf("transactions: %d distinct txs, want %d (sourced-with-op %d + participant-only %d + sourced-tx-and-participant %d)",
			len(readerTxs), want, n/stride, n/stride, n/stride)
	}

	// A cursor set MID-page (not at a page boundary): the next page must
	// start exactly at the following row, identically on both paths.
	first, _, err := er.AccountOperations(ctx, hot, 10, chstore.ExplorerCursor{})
	if err != nil || len(first) != 10 {
		t.Fatalf("first page: %v (%d rows)", err, len(first))
	}
	mid := chstore.ExplorerCursor{Ledger: first[3].Seq, A: first[3].TxIndex, B: first[3].OpIndex}
	fromMid, _, err := er.AccountOperations(ctx, hot, 10, mid)
	if err != nil {
		t.Fatalf("mid-page cursor: %v", err)
	}
	legacyMid := legacyPage(ctx, 10, mid)
	if len(fromMid) != 10 || fromMid[0] != first[4] || len(legacyMid) != 10 || legacyMid[0] != first[4] {
		t.Fatalf("mid-page cursor %+v: reader first row %+v, legacy first row %+v, want %+v",
			mid, fromMid[0], legacyMid[0], first[4])
	}

	// (2) read_rows: one page of 50 via each path.
	const limit = 50
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyPage(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)), limit, chstore.ExplorerCursor{})
	// The reader issues several statements per page (two windowed key arms and
	// the hydration), so its cost is the SUM over every statement it ran,
	// found through the log_comment this context stamps on each of them.
	readerTag := uuid.NewString()
	readerCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": readerTag}))
	if _, _, err := er.AccountOperations(readerCtx, hot, limit, chstore.ExplorerCursor{}); err != nil {
		t.Fatalf("AccountOperations read_rows page: %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)
	readRows := func(where string, args ...any) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND `+where+`
			ORDER BY event_time_microseconds DESC LIMIT 1`, append([]any{t0}, args...)...).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", where, err)
		}
		return rr
	}
	legacyRead := readRows(`query_id = ?`, legacyID)
	var readerRead uint64
	if err := raw.QueryRow(ctx, `SELECT sum(read_rows) FROM system.query_log
		WHERE type = 'QueryFinish' AND event_time >= ? AND log_comment = ?`, t0, readerTag).Scan(&readerRead); err != nil {
		t.Fatalf("query_log (reader statements): %v", err)
	}
	if readerRead == 0 {
		t.Fatal("no reader statement found in query_log — the log_comment tag did not reach the server")
	}
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d hot op keys, one per 8192-row granule; page %d)",
		legacyRead, readerRead, len(opsSeen), limit)
	if legacyRead < uint64(n/stride)*4096 {
		t.Fatalf("fixture no longer reproduces the pathology: legacy read %d rows, expected ≥ one granule per hot key (~%d) — the differential still holds but the read_rows bound below would be vacuous",
			legacyRead, (n/stride)*8192)
	}
	if readerRead*4 > legacyRead {
		t.Fatalf("reader read %d rows vs legacy %d — the page must be bounded by the page size (≤ 3×limit granules), not by the account's history",
			readerRead, legacyRead)
	}
}

// TestClickHouseAccountActivityBackfillVerifySeesGapInLaterWindow executes
// the SQL of deploy/clickhouse/account_activity.sql's operator runbook — the
// Step-2 backfill INSERTs and the Step-3 verify, extracted from the file, not
// copied — against a live ClickHouse.
//
// stellar.account_activity feeds a HARD `ledger_seq <= watermark` bound on
// the account-history readers, so a Step-2 backfill that skipped a window
// silently hides history. Step 3 is the only thing standing between a
// partial backfill and the reader deploy, and both of its earlier shapes
// were blind to a gap outside the first window: sampling recently active
// accounts tests what the live MVs cover anyway, and sampling the accounts
// with the OLDEST last activity tests only window 1.
//
// Fixture (the one the blind shapes return 0 on): window 1 backfilled, a
// LATER window skipped, an account active in both — in the later window
// only as a NON-SOURCE participant, the role a narrowed check would miss.
// The verify must count it (> 0) and must return 0 once the skipped window
// is re-run.
func TestClickHouseAccountActivityBackfillVerifySeesGapInLaterWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	conn := dialClickHouse(t, ctx, "stellar")
	rb := loadAccountActivityRunbook(t)

	// Two adjacent windows of the runbook's own grid (2 + k*2M), far from
	// any ledger another integration test seeds. Rows go straight into the
	// three source tables — no stellar.ledgers row, so no other test's
	// "tip" anchor moves.
	const (
		win1 = uint32(120_000_002)
		win2 = uint32(122_000_002)
		l1   = win1 + 500
		l2   = win2 + 700
	)
	sampled := f112Accounts(t, ctx, conn, rb.sampleMod, true, 3)
	both, late, early := sampled[0], sampled[1], sampled[2]
	unsampled := f112Accounts(t, ctx, conn, rb.sampleMod, false, 1)[0]

	seedF112Ledger(t, ctx, conn, l1, both, nil)
	seedF112Ledger(t, ctx, conn, l1+1, early, nil)
	seedF112Ledger(t, ctx, conn, l2, late, []string{both})
	seedF112Ledger(t, ctx, conn, l2+1, unsampled, nil)

	// The live MVs just covered every fixture account: both windows clean.
	// (Also the isolation check — a stray row in these windows fails here.)
	for _, w := range []uint32{win1, win2} {
		if n := rb.verify(t, ctx, conn, w, rb.sampleMod); n != 0 {
			t.Fatalf("MV-covered fixture: verify(window %d) = %d, want 0", w, n)
		}
	}

	// Pre-MV history: the source rows exist, the watermark rows do not.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": "2"}))
	if err := conn.Exec(syncCtx,
		`ALTER TABLE stellar.account_activity DELETE WHERE account_id IN (?, ?, ?, ?)`,
		both, late, early, unsampled); err != nil {
		t.Fatalf("drop fixture watermarks: %v", err)
	}

	// Step 2 covers window 1 and SKIPS window 2.
	rb.backfill(t, ctx, conn, win1)

	if n := rb.verify(t, ctx, conn, win1, rb.sampleMod); n != 0 {
		t.Errorf("verify(backfilled window %d) = %d, want 0", win1, n)
	}
	// `both` now has a watermark in window 1, BELOW its window-2 participant
	// row (the reader would hide that row); `late` has none at all.
	if wm := f112Watermark(t, ctx, conn, both); wm != l1 {
		t.Fatalf("fixture: watermark(both) = %d after backfilling window 1 only, want %d", wm, l1)
	}
	if n := rb.verify(t, ctx, conn, win2, rb.sampleMod); n != 2 {
		t.Errorf("verify(SKIPPED window %d) = %d, want 2 (one too-LOW watermark, one missing) — a verify "+
			"that returns 0 here cannot fail for a gap outside the first window (F112)", win2, n)
	}
	// AA_MOD=1 is the exhaustive check: it also counts the unsampled account.
	if n := rb.verify(t, ctx, conn, win2, 1); n != 3 {
		t.Errorf("exhaustive verify(SKIPPED window %d) = %d, want 3", win2, n)
	}

	// Re-run the skipped window: the gap closes and the verify says so.
	rb.backfill(t, ctx, conn, win2)
	for _, mod := range []uint64{rb.sampleMod, 1} {
		if n := rb.verify(t, ctx, conn, win2, mod); n != 0 {
			t.Errorf("verify(window %d, 1-in-%d) = %d after re-running it, want 0", win2, mod, n)
		}
	}
	if wm := f112Watermark(t, ctx, conn, both); wm != l2 {
		t.Errorf("watermark(both) = %d after the full backfill, want %d (its participant row)", wm, l2)
	}
}

// accountActivityRunbook is the executable SQL of the runbook, still carrying
// the shell variables the operator's loop expands.
type accountActivityRunbook struct {
	inserts   []string
	verifySQL string
	sampleMod uint64
}

var (
	f112InsertRE = regexp.MustCompile(`(?s)"\s*(INSERT INTO stellar\.account_activity.*?)"`)
	f112VerifyRE = regexp.MustCompile(`(?s)-q "\s*(SELECT countIf\(wm < truth\).*?)" </dev/null`)
	f112ModRE    = regexp.MustCompile(`AA_MOD="\$\{AA_MOD:-(\d+)\}"`)
)

func loadAccountActivityRunbook(t *testing.T) accountActivityRunbook {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "deploy", "clickhouse", "account_activity.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runbook: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "--   "); ok {
			code = append(code, rest)
		}
	}
	text := strings.Join(code, "\n")

	var rb accountActivityRunbook
	for _, m := range f112InsertRE.FindAllStringSubmatch(text, -1) {
		rb.inserts = append(rb.inserts, m[1])
	}
	if len(rb.inserts) != 3 {
		t.Fatalf("runbook Step 2 carries %d backfill INSERTs, want 3 (operations, transactions, "+
			"operation_participants)", len(rb.inserts))
	}
	v := f112VerifyRE.FindAllStringSubmatch(text, -1)
	if len(v) != 1 {
		t.Fatalf("runbook Step 3 carries %d per-window verify queries, want 1 — without one the verify "+
			"cannot fail for a gap in a later window (F112)", len(v))
	}
	rb.verifySQL = v[0][1]
	m := f112ModRE.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("runbook Step 3 does not declare its default AA_MOD")
	}
	if rb.sampleMod, err = strconv.ParseUint(m[1], 10, 64); err != nil || rb.sampleMod == 0 {
		t.Fatalf("runbook default AA_MOD %q is not a positive integer", m[1])
	}
	return rb
}

// expand does what the operator's bash loop does to the embedded SQL.
func (accountActivityRunbook) expand(t *testing.T, sql string, window uint32, mod uint64) string {
	t.Helper()
	out := strings.NewReplacer(
		"$((W + 2000000))", strconv.FormatUint(uint64(window)+2_000_000, 10),
		"$W", strconv.FormatUint(uint64(window), 10),
		"$AA_MOD", strconv.FormatUint(mod, 10),
	).Replace(sql)
	if strings.Contains(out, "$") {
		t.Fatalf("runbook SQL still carries a shell variable after expansion:\n%s", out)
	}
	return out
}

func (rb accountActivityRunbook) backfill(t *testing.T, ctx context.Context, conn driver.Conn, window uint32) {
	t.Helper()
	for _, ins := range rb.inserts {
		if err := conn.Exec(ctx, rb.expand(t, ins, window, rb.sampleMod)); err != nil {
			t.Fatalf("runbook backfill INSERT (window %d): %v", window, err)
		}
	}
}

func (rb accountActivityRunbook) verify(t *testing.T, ctx context.Context, conn driver.Conn, window uint32, mod uint64) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, rb.expand(t, rb.verifySQL, window, mod)).Scan(&n); err != nil {
		t.Fatalf("runbook verify (window %d): %v", window, err)
	}
	return n
}

// f112Accounts returns n fixture account ids that are inside (or outside)
// the runbook's 1-in-mod hash sample, as ClickHouse itself computes it.
func f112Accounts(t *testing.T, ctx context.Context, conn driver.Conn, mod uint64, inSample bool, n int) []string {
	t.Helper()
	cmp := "="
	if !inSample {
		cmp = "!="
	}
	rows, err := conn.Query(ctx, fmt.Sprintf(
		`SELECT a FROM (SELECT concat('GTESTF112BACKFILLVERIFY', leftPad(toString(number), 33, 'A')) AS a
		                FROM numbers(100000))
		 WHERE cityHash64(a) %% %d %s 0 ORDER BY a LIMIT %d`, mod, cmp, n))
	if err != nil {
		t.Fatalf("pick fixture accounts: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan fixture account: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fixture accounts: %v", err)
	}
	if len(out) != n {
		t.Fatalf("picked %d fixture accounts (inSample=%v), want %d", len(out), inSample, n)
	}
	return out
}

// seedF112Ledger writes one transaction + one operation sourced by `source`
// at `seq`, naming `participants` as non-source participants of that op.
func seedF112Ledger(t *testing.T, ctx context.Context, conn driver.Conn, seq uint32, source string, participants []string) {
	t.Helper()
	at := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	hash := fmt.Sprintf("%064d", seq)
	if err := conn.Exec(ctx,
		`INSERT INTO stellar.transactions (ledger_seq, close_time, tx_hash, tx_index, source_account) VALUES (?, ?, ?, 0, ?)`,
		seq, at, hash, source); err != nil {
		t.Fatalf("seed transaction %d: %v", seq, err)
	}
	if err := conn.Exec(ctx,
		`INSERT INTO stellar.operations (ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr)
		 VALUES (?, ?, ?, 0, 0, 'OperationTypePayment', ?, 'Ym9keQ==')`,
		seq, at, hash, source); err != nil {
		t.Fatalf("seed operation %d: %v", seq, err)
	}
	for _, p := range participants {
		if err := conn.Exec(ctx,
			`INSERT INTO stellar.operation_participants (account, ledger_seq, close_time, tx_hash, tx_index, op_index)
			 VALUES (?, ?, ?, ?, 0, 0)`,
			p, seq, at, hash); err != nil {
			t.Fatalf("seed participant %d: %v", seq, err)
		}
	}
}

func f112Watermark(t *testing.T, ctx context.Context, conn driver.Conn, account string) uint32 {
	t.Helper()
	var wm uint32
	if err := conn.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, account).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	return wm
}

// TestClickHouseAccountActivityWatermarkBoundedOps is the live-ClickHouse
// proof for the activity-watermark bound on AccountOperations.
//
// Scenario (the measured live pathology): a long-idle account whose rows all
// sit far below the tip — without the bound, the reader's reverse primary-key
// resolves walk granules from the tip back to the account's last activity
// (seconds live for a 46d-idle account). The fix bounds each arm with
// `ledger_seq <= max(account_activity.last_ledger)`.
//
// What this test proves, in order of importance:
//
//  1. DATA-HIDING invariant: the bounded read returns EXACTLY the rows the
//     account has — including an op where the account is only a NON-SOURCE
//     participant at a ledger ABOVE its last SOURCED activity (the
//     Soroban/SAC "token movement postdates the classic account's own
//     activity" shape). A watermark tracking only source_account would bound
//     below that row and silently hide it. Red-proof: force the reader's
//     bound to min(last_ledger) instead of max(...) and this test fails on
//     the missing participant row.
//  2. The watermark itself is the max over ALL roles (participant ledger,
//     not the higher of the sourced ledgers).
//  3. The bound actually prunes: EXPLAIN ESTIMATE over the reverse
//     primary-key scan shape with the bound reads strictly fewer
//     stellar.operations rows than without it (the decoy tip partition is
//     pruned) — see the inline note on why the scan shape, not the IN-arm,
//     is what demonstrates the mechanism.
func TestClickHouseAccountActivityWatermarkBoundedOps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		idle  = "GTEST_WM31_IDLE_ACCOUNT_AAAAAAAAAAAAAAAAAAAAAAAAA"
		other = "GTEST_WM31_OTHER_SOURCE_AAAAAAAAAAAAAAAAAAAAAAAAA"
		decoy = "GTEST_WM31_DECOY_TIP_AAAAAAAAAAAAAAAAAAAAAAAAAAAA"

		sourcedLo   = uint32(6_210_001)  // idle's sourced ops: 3 consecutive ledgers from here
		participant = uint32(6_310_007)  // idle is a NON-SOURCE participant here (postdates sourced)
		tip         = uint32(82_310_001) // decoy activity in a far-higher partition (the "tip")
	)
	// Shared-ClickHouse isolation: these ledgers land in the same
	// stellar.ledgers as every other integration test, and
	// TestNetworkThroughput_DedupsReingestedLedger anchors its window on the
	// GLOBAL max(close_time) (reserving a 2027 close_time as the tip). The
	// decoy `tip` seq is ~82M, so a per-second offset would put its
	// close_time in ~2028 and steal that global tip — pushing the throughput
	// test's ledger out of its 1-day window. Scale the offset to
	// MILLISECONDS so even the far-higher decoy partition stays before
	// the reserved tip day (max ~+21h) and never becomes the global close_time tip.
	// close_time granularity is irrelevant to THIS test — every assertion
	// keys on ledger_seq (the watermark is max(last_ledger); the bounded
	// read orders by ledger_seq), so same-second sourced rows are fine.
	closeAt := func(seq uint32) time.Time {
		return time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seq-sourcedLo) * time.Millisecond)
	}
	hash := func(seq uint32) string { return fmt.Sprintf("%064d", seq) }

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	addLedger := func(seq uint32, txSource, opSource string, participants []string) {
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeAt(seq), LedgerHash: "aa", PrevHash: "bb",
				ProtocolVersion: 22, TxCount: 1, OpCount: 1,
			},
			Txs: []chstore.TransactionRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, SourceAccount: txSource, Successful: 1,
			}},
			Ops: []chstore.OperationRow{{
				LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq),
				TxIndex: 0, OpIndex: 0, OpType: "OperationTypePayment", SourceAccount: opSource, BodyXDR: "Ym9keQ==",
			}},
		}
		for _, p := range participants {
			ext.Participants = append(ext.Participants, chstore.OperationParticipantRow{
				Account: p, LedgerSeq: seq, CloseTime: closeAt(seq), TxHash: hash(seq), TxIndex: 0, OpIndex: 0,
			})
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
	}

	// The idle account's own sourced activity, far below the tip.
	for i := uint32(0); i < 3; i++ {
		addLedger(sourcedLo+i, idle, idle, nil)
	}
	// A LATER op sourced by someone else in which idle only participates —
	// the row a sourced-only watermark would hide.
	addLedger(participant, other, other, []string{idle})
	// Decoy tip activity in a far-higher partition: the granules the
	// unbounded resolve walks through and the bounded one must skip.
	for i := uint32(0); i < 3; i++ {
		addLedger(tip+i, decoy, decoy, nil)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")

	// (2) The MV-maintained watermark must be the max over ALL roles: the
	// participant ledger, not the higher sourced ledger.
	var wm uint32
	if err := raw.QueryRow(ctx,
		`SELECT max(last_ledger) FROM stellar.account_activity WHERE account_id = ?`, idle).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if wm != participant {
		t.Fatalf("watermark = %d, want %d — account_activity must cover EVERY role the ops query "+
			"filters on (a source-only watermark = %d would bound below the participant row and HIDE it)",
			wm, participant, sourcedLo+2)
	}

	// (1) Bounded read loses nothing: newest-first, participant row first,
	// then the three sourced rows. The reader takes the bounded path here
	// (the watermark row exists — asserted above), so equality against the
	// seeded ground truth IS bounded-vs-unbounded equality.
	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, _, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{})
	if err != nil {
		t.Fatalf("AccountOperations: %v", err)
	}
	wantSeqs := []uint32{participant, sourcedLo + 2, sourcedLo + 1, sourcedLo}
	if len(rows) != len(wantSeqs) {
		got := make([]uint32, len(rows))
		for i, r := range rows {
			got[i] = r.Seq
		}
		t.Fatalf("AccountOperations returned %d rows %v, want %d %v — a missing row means the "+
			"watermark bound HID history (the data-hiding failure the #31 invariant forbids)",
			len(rows), got, len(wantSeqs), wantSeqs)
	}
	for i, want := range wantSeqs {
		if rows[i].Seq != want {
			t.Fatalf("row %d at ledger %d, want %d (newest first)", i, rows[i].Seq, want)
		}
	}

	// Pagination across the bound: cursor below the participant row must
	// serve the sourced rows, nothing lost at the seam.
	page2, _, err := er.AccountOperations(ctx, idle, 50, chstore.ExplorerCursor{Ledger: participant, A: 0, B: 0})
	if err != nil {
		t.Fatalf("AccountOperations page 2: %v", err)
	}
	if len(page2) != 3 || page2[0].Seq != sourcedLo+2 {
		t.Fatalf("cursor page returned %d rows (first seq %v), want the 3 sourced rows from %d",
			len(page2), page2, sourcedLo+2)
	}

	// (3) The bound prunes: EXPLAIN ESTIMATE of the reverse primary-key
	// scan must read strictly fewer stellar.operations rows WITH the bound
	// (the decoy tip partition — intDiv 82 vs the bound's 6 — cannot be
	// pruned without it). The scan SHAPE, not the full IN-arm: on a toy
	// dataset ClickHouse's set-based index analysis resolves the IN
	// subquery to exact granules either way, which is precisely the
	// heuristic that does NOT save the 10.6B-row live table (multi-second tip
	// walk) — the watermark's value is DETERMINISTIC partition +
	// primary-key range pruning, independent of set-analysis heuristics
	// and their size caps, and that is the mechanism asserted here.
	armSQL := func(bound string) string {
		where := ""
		if bound != "" {
			where = ` WHERE ` + bound
		}
		return `EXPLAIN ESTIMATE SELECT ledger_seq, tx_index, op_index FROM stellar.operations` + where + `
			ORDER BY ledger_seq DESC, tx_index DESC, op_index DESC LIMIT 50`
	}
	opsRowsEstimate := func(sql string) uint64 {
		res, err := raw.Query(ctx, sql)
		if err != nil {
			t.Fatalf("EXPLAIN ESTIMATE: %v", err)
		}
		defer func() { _ = res.Close() }()
		var total uint64
		for res.Next() {
			var db, table string
			var parts, nrows, marks uint64
			if err := res.Scan(&db, &table, &parts, &nrows, &marks); err != nil {
				t.Fatalf("scan EXPLAIN ESTIMATE: %v", err)
			}
			if table == "operations" {
				total += nrows
			}
		}
		if err := res.Err(); err != nil {
			t.Fatalf("EXPLAIN ESTIMATE rows: %v", err)
		}
		return total
	}
	unbounded := opsRowsEstimate(armSQL(""))
	bounded := opsRowsEstimate(armSQL(fmt.Sprintf("ledger_seq <= %d", wm)))
	if bounded >= unbounded {
		t.Fatalf("bounded arm reads %d operations rows, unbounded %d — the watermark bound must "+
			"prune the tip partitions (granule-bounded scan is the whole point of #31)", bounded, unbounded)
	}
}

// TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled is the served-data proof
// for the order book's cursor discipline.
//
// The LiveSink drops a WHOLE ledger under buffer pressure, leaving a hole in
// the lake that ch-live-catchup back-fills minutes later. A reader that
// bounds its incremental read by max(ledger_seq) of ledger_entry_changes
// lets Advance read straight across the hole and commit the cursor PAST it;
// the healed rows then land BELOW the cursor and are never read. An offer
// removed in the dropped ledger would stay on /v1/sdex/orderbook as resting
// liquidity — and an offer created in it would never appear — until the API
// process restarted.
//
// This drives the REAL reader and the REAL cache through the REAL endpoint
// against a real ClickHouse, in the production order of events:
//
//	B+1  offer A created                   → Load
//	B+2  offer A REMOVED, offer B created  ← this ledger is DROPPED
//	B+3  (empty), B+4 offer C created      → Advance  (hole still open)
//	B+2  healed by catch-up                → Advance  (no restart, no re-Load)
//
// and asserts both halves of the contract: while the hole is open the cursor
// HOLDS at B+1 (it never crosses the hole), and once the hole is filled the
// SAME cache advances to B+4 and serves exactly {B, C}.
//
// Proven red: on a reader without the hole check the final book is {A, C} — phantom A
// still served, B never seen — with as_of_ledger already at B+4 after the
// first Advance.
func TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated range ABOVE every other ledger the suite seeds (the
	// watermark tests sit at 215M/216M): the book's Load cursor is derived
	// from the lake's global tip, so this test's ledgers must be that tip.
	const base = uint32(217_000_000)
	const (
		sellerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		issuerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		assetCode     = "LAKEHOLE"
	)
	selling := assetCode + "-" + issuerAccount
	// Shared-ClickHouse isolation: TestNetworkThroughput_DedupsReingestedLedger
	// anchors its window on the GLOBAL max(close_time) and reserves a far-future day
	// as that tip. Nothing here keys on close_time (the book orders by
	// ledger_seq), so stay far below it rather than compete for the tip.
	closeTime := time.Date(2025, 3, 3, 4, 5, 6, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// land writes one whole ledger the way the live sink / ch-backfill does:
	// its entry changes AND its stellar.ledgers commit marker, in one extract.
	land := func(seq uint32, changes ...chstore.LedgerEntryChangeRow) {
		t.Helper()
		for i := range changes {
			changes[i].LedgerSeq = seq
			changes[i].CloseTime = closeTime
		}
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeTime,
				LedgerHash: "aa62", PrevHash: "bb62", ProtocolVersion: 23, BucketListHash: "cc62",
				TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
			},
			Changes: changes,
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush ledger %d: %v", seq, err)
		}
	}
	created := func(offerID int64, priceN int32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR:   lakeHoleOfferKeyB64(t, sellerAccount, offerID),
			EntryXDR: lakeHoleOfferEntryB64(t, sellerAccount, offerID, assetCode, issuerAccount, priceN),
		}
	}
	// A distinct tx from `created`: ledger_entry_changes is a
	// ReplacingMergeTree over (ledger_seq, tx_hash, op_index, change_index),
	// so two same-ledger rows sharing that identity collapse into one.
	removed := func(offerID int64) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162-take", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 2,
			ChangeType: "removed", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR: lakeHoleOfferKeyB64(t, sellerAccount, offerID),
		}
	}
	const offerA, offerB, offerC = int64(9_162_001), int64(9_162_002), int64(9_162_003)

	// ── Process start: [B, B+1] landed; offer A rests at price 2. ─────────
	land(base)
	land(base+1, created(offerA, 2))

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	cache := v1.NewSDEXOrderBookCache(reader, nil)
	if err := cache.Load(ctx); err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	ts := httptest.NewServer(v1.New(v1.Options{SDEXOrderBook: cache}).Handler())
	t.Cleanup(ts.Close)
	book := func() v1.SDEXOrderBookView {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/sdex/orderbook?selling=" + selling + "&buying=native")
		if err != nil {
			t.Fatalf("GET orderbook: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET orderbook status = %d, want 200", resp.StatusCode)
		}
		var env struct {
			Data v1.SDEXOrderBookView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode orderbook: %v", err)
		}
		return env.Data
	}
	askPrices := func(b v1.SDEXOrderBookView) []string {
		out := make([]string, 0, len(b.Asks))
		for _, lvl := range b.Asks {
			out = append(out, lvl.Price)
		}
		return out
	}

	loaded := book()
	if loaded.AsOfLedger != base+1 {
		t.Fatalf("precondition: as_of_ledger after Load = %d, want %d — this test's range must be the "+
			"lake's global tip; another test now seeds stellar.ledgers above %d", loaded.AsOfLedger, base+1, base)
	}
	if got := askPrices(loaded); len(got) != 1 || got[0] != "2.0000000" {
		t.Fatalf("precondition: asks after Load = %v, want [2.0000000] (offer A)", got)
	}

	// ── The drop: B+2 (A removed, B created) never lands; B+3, B+4 do. ────
	land(base + 3)
	land(base+4, created(offerC, 5))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance across the open hole: %v", err)
	}
	held := book()
	if held.AsOfLedger != base+1 {
		t.Errorf("as_of_ledger with the hole at %d still open = %d, want %d — the cursor must HOLD below "+
			"a lake hole; past it, the healed ledger's offer changes land below the cursor and are never read",
			base+2, held.AsOfLedger, base+1)
	}

	// ── The heal: ch-live-catchup back-fills B+2. Same cache, no restart. ─
	land(base+2, removed(offerA), created(offerB, 3))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance after the heal: %v", err)
	}
	healed := book()
	if healed.AsOfLedger != base+4 {
		t.Errorf("as_of_ledger after the hole healed = %d, want %d — a held cursor must RESUME to the "+
			"lake tip once the hole is filled (holding forever is not a fix)", healed.AsOfLedger, base+4)
	}
	got := askPrices(healed)
	if len(got) != 2 || got[0] != "3.0000000" || got[1] != "5.0000000" {
		t.Fatalf("asks after the hole healed = %v (ask_offers=%d), want [3.0000000 5.0000000]: offer A was "+
			"REMOVED in the healed ledger %d and must leave the book (a 2.0000000 level is the phantom), "+
			"offer B was CREATED in it and must appear, offer C landed above it",
			got, healed.AskOffers, base+2)
	}
}

func lakeHoleOfferKeyB64(t *testing.T, seller string, offerID int64) string {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetOffer(xdr.MustAddress(seller), uint64(offerID)); err != nil {
		t.Fatalf("offer ledger key: %v", err)
	}
	b64, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	return b64
}

// lakeHoleOfferEntryB64 builds an offer LedgerEntry selling 10 units of
// code-issuer for native at priceN/1. lastModifiedLedgerSeq is left zero:
// the book versions an offer by its change row's own ledger, not the entry's.
func lakeHoleOfferEntryB64(t *testing.T, seller string, offerID int64, code, issuer string, priceN int32) string {
	t.Helper()
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: xdr.MustAddress(seller),
				OfferId:  xdr.Int64(offerID),
				Selling:  xdr.MustNewCreditAsset(code, issuer),
				Buying:   xdr.MustNewNativeAsset(),
				Amount:   100_000_000,
				Price:    xdr.Price{N: xdr.Int32(priceN), D: 1},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	return b64
}

// TestSampleAccountIDs_SeededChangeLogFrame proves reconcile-balances' -sample
// frame on real ClickHouse:
//
//   - it is drawn from stellar.ledger_entry_changes, so an account the
//     ledger_entries_current projection lost is still drawable (the old frame
//     read the projection and returned nothing here);
//   - one seed reproduces its cohort exactly, and different seeds draw
//     different cohorts (the old unseeded cityHash64 order froze one cohort);
//   - the floor excludes accounts at or below it.
func TestSampleAccountIDs_SeededChangeLogFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// Far above any other fixture so the floor isolates this test's rows;
	// purged afterwards because max(ledger_seq) bounds every whole-lake walk.
	const (
		below     = uint32(4_000_001_000)
		floor     = uint32(4_000_001_050)
		above     = uint32(4_000_001_100)
		accounts  = 40
		cohort    = 8
		belowAcct = "gh1096-sample-frame-below-floor"
	)
	purgeLakeFixtureLedgers(t, addr, below, above)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	want := make([]string, 0, accounts)
	rows := make([]chstore.LedgerEntryChangeRow, 0, accounts+1)
	for i := range uint32(accounts) {
		acct := fmt.Sprintf("gh1096-sample-frame-%02d", i)
		want = append(want, acct)
		rows = append(rows, chstore.LedgerEntryChangeRow{
			LedgerSeq: above, CloseTime: closeTime, TxHash: "gh1096", ChangeIndex: i,
			IntraLedgerSeq: i, ChangeType: "updated", EntryType: "account",
			KeyXDR: "gh1096-key-" + acct, AccountID: acct, Balance: int64(i),
		})
	}
	rows = append(rows, chstore.LedgerEntryChangeRow{
		LedgerSeq: below, CloseTime: closeTime, TxHash: "gh1096-below", ChangeType: "updated", EntryType: "account",
		KeyXDR: "gh1096-key-" + belowAcct, AccountID: belowAcct, Balance: 1,
	})
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Simulate a projection that lost these accounts: the change log still
	// holds them, the current-state view does not.
	conn := dialClickHouse(t, ctx, "stellar")
	if err := conn.Exec(ctx, `ALTER TABLE stellar.ledger_entries_current DELETE
		WHERE startsWith(account_id, 'gh1096-sample-frame-') SETTINGS mutations_sync = 2`); err != nil {
		t.Fatalf("drop fixture accounts from ledger_entries_current: %v", err)
	}

	all, err := chstore.SampleAccountIDs(ctx, addr, floor, 1, accounts+10)
	if err != nil {
		t.Fatalf("SampleAccountIDs(all): %v", err)
	}
	slices.Sort(all)
	if !slices.Equal(all, want) {
		t.Fatalf("frame above floor = %v, want the %d change-log accounts (none below the floor, none lost with the projection)", all, accounts)
	}

	draw := func(seed uint64) []string {
		t.Helper()
		ids, err := chstore.SampleAccountIDs(ctx, addr, floor, seed, cohort)
		if err != nil {
			t.Fatalf("SampleAccountIDs(seed %d): %v", seed, err)
		}
		if len(ids) != cohort {
			t.Fatalf("SampleAccountIDs(seed %d) = %d ids, want %d", seed, len(ids), cohort)
		}
		return ids
	}
	first := draw(1)
	if again := draw(1); !slices.Equal(first, again) {
		t.Errorf("seed 1 drew %v then %v; one seed must reproduce its cohort", first, again)
	}
	distinct := 1
	for seed := uint64(2); seed <= 4; seed++ {
		if !slices.Equal(draw(seed), first) {
			distinct++
		}
	}
	if distinct == 1 {
		t.Errorf("seeds 1..4 all drew cohort %v; the seed does not rotate the sample", first)
	}
}

// TestCHRebuildPreflight_AnswersWithoutTouchingTheLakeOrTheServedTier is
// the executing proof for `ch-rebuild -write -preflight`, driven
// through the real subcommand on real TimescaleDB.
//
// scripts/ops/ch-rebuild-projected.sh DELETEs a window and only then asks
// ch-rebuild to re-derive it. The refusals must not live inside that second
// step, or a guard doing its job leaves the window empty. The preflight lets
// the script ask first — which is only safe if the preflight:
//
//  1. really runs the guards — a live projector cursor below -to, and a
//     range over the buffered ceiling, are each refused here exactly as
//     the real run refuses them, with NO verdict line on stdout;
//  2. really stops before the lake — ClickHouse is unreachable in this
//     test, so the same command WITHOUT -preflight fails on the lake read
//     (the control), while the preflight succeeds;
//  3. names what the run would re-derive, in the form the script parses;
//  4. writes nothing.
func TestCHRebuildPreflight_AnswersWithoutTouchingTheLakeOrTheServedTier(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	const from, to = 61_000_000, 61_100_000
	// Nothing listens here: any lake read fails at once.
	const deadLake = "127.0.0.1:1"
	args := func(extra ...string) []string {
		return append([]string{
			"ch-rebuild", "-config", cfgPath, "-ch-addr", deadLake,
			"-from", fmt.Sprint(from), "-to", fmt.Sprint(to),
			"-sources", "aquarius,soroswap", "-write",
		}, extra...)
	}
	servedRows := func(t *testing.T) int {
		t.Helper()
		var n int
		const q = `SELECT (SELECT count(*) FROM trades) + (SELECT count(*) FROM soroswap_skim_events) + (SELECT count(*) FROM projection_dirty_windows)`
		if err := store.DB().QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count served rows: %v", err)
		}
		return n
	}

	// ── 1a. live cursor below -to: refused, no verdict ────────────────
	if err := store.UpsertCursor(ctx, "projector", "soroswap", to+1_000); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCursor(ctx, "projector", "aquarius", to-50_000); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return chops.Run(args("-preflight")) })
	if err == nil || !strings.Contains(err.Error(), "live projector's cursor is below") || !strings.Contains(err.Error(), "aquarius") {
		t.Fatalf("preflight over a range the live projector is still inside was not refused: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "preflight ok") {
		t.Fatalf("a REFUSED preflight still printed a verdict line — the script would delete on it:\n%s", out)
	}

	// ── 1b. range over the buffered ceiling: refused, no verdict ──────
	if err := store.UpsertCursor(ctx, "projector", "aquarius", 70_000_000); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCursor(ctx, "projector", "soroswap", 70_000_000); err != nil {
		t.Fatal(err)
	}
	wide := []string{
		"ch-rebuild", "-config", cfgPath, "-ch-addr", deadLake,
		"-from", "55000000", "-to", "62894000", "-sources", "aquarius,soroswap", "-write", "-preflight",
	}
	out, err = captureStdout(t, func() error { return chops.Run(wide) })
	if err == nil || !strings.Contains(err.Error(), "window invocations") {
		t.Fatalf("preflight over a 7.9M-ledger range was not refused by the buffered-range guard: err=%v\n%s", err, out)
	}
	if strings.Contains(out, "preflight ok") {
		t.Fatalf("a REFUSED preflight still printed a verdict line:\n%s", out)
	}

	// ── 2. control: the real run reaches the lake, and the lake is dead ─
	// The first lake read is the per-WASM replay gate, so the failure
	// names the dead address, not the event stream.
	before := servedRows(t)
	if _, err = captureStdout(t, func() error { return chops.Run(args()) }); err == nil || !strings.Contains(err.Error(), "wasm replay gate") || !strings.Contains(err.Error(), deadLake) {
		t.Fatalf("control: the un-preflighted run should fail on the unreachable lake; got err=%v — "+
			"without this, a passing preflight proves nothing about WHERE it stopped", err)
	}

	// ── 2+3. the preflight passes the guards and stops short of it ─────
	out, err = captureStdout(t, func() error { return chops.Run(args("-preflight")) })
	if err != nil {
		t.Fatalf("preflight: %v\n%s", err, out)
	}
	// Catalogue order, not -sources order: it is the list the run decodes.
	want := fmt.Sprintf("ch-rebuild: preflight ok [%d,%d] rederive=soroswap,aquarius\n", from, to)
	if out != want {
		t.Fatalf("preflight stdout = %q, want exactly %q", out, want)
	}

	// ── 4. nothing was written ────────────────────────────────────────
	if after := servedRows(t); after != before {
		t.Fatalf("preflight changed the served tier: %d rows before, %d after", before, after)
	}
}

// ReapCursors is the only irreversible statement in the cursor-cleanup
// path. Its safety rests on three SQL predicates — the cutoff, the
// -source scope, and `source <> ALL($3)` — and the shape test in
// internal/storage/timescale pins the text of all three but cannot
// prove Postgres agrees with the reading. In particular `<> ALL(...)`
// over a text[] bound as a Go []string is a pgx encode path, and this
// repo has shipped a param-typing bug before (see
// divergence_observations_test.go). So: a real table, real rows, a real
// DELETE, and the live cursor still there afterwards.
func TestReapCursorsProtectsLiveNamespaces(t *testing.T) {
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

	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)

	// The r1 population in miniature: two live cursors stuck for a
	// month, two dead one-shot shards, and one shard still walking.
	seedCursor := func(source, sub string, ledger int64, age time.Duration) {
		t.Helper()
		const q = `
            INSERT INTO ingestion_cursors (source, sub_source, first_ledger, last_ledger, last_updated)
            VALUES ($1, $2, $3, $3, $4)
        `
		if _, err := db.ExecContext(ctx, q, source, sub, ledger, now.Add(-age)); err != nil {
			t.Fatalf("seed %s/%s: %v", source, sub, err)
		}
	}
	seed := func() {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM ingestion_cursors`); err != nil {
			t.Fatalf("clear cursors: %v", err)
		}
		seedCursor("ledgerstream", "", 63302110, 30*24*time.Hour)
		seedCursor("projector", "soroswap", 63302110, 30*24*time.Hour)
		seedCursor("backfill", "11474999-15299997:sdex", 15299997, 112*24*time.Hour)
		seedCursor("projected-rebuild", "shard-1", 40000000, 40*24*time.Hour)
		seedCursor("backfill", "still-walking", 61000000, 20*time.Minute)
	}
	remaining := func() map[string]bool {
		t.Helper()
		rows, lerr := store.ListCursors(ctx)
		if lerr != nil {
			t.Fatalf("ListCursors: %v", lerr)
		}
		out := map[string]bool{}
		for _, c := range rows {
			out[c.Source+"/"+c.Sub] = true
		}
		return out
	}

	seed()
	deleted, err := store.ReapCursors(ctx, cutoff, "", timescale.LiveCursorSources())
	if err != nil {
		t.Fatalf("ReapCursors: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (the sdex shard + the projected-rebuild shard)", deleted)
	}
	left := remaining()
	for _, want := range []string{"ledgerstream/", "projector/soroswap"} {
		if !left[want] {
			t.Errorf("%s was DELETED — a stuck live cursor is an incident to investigate, and losing its resume point restarts ingest from the configured start ledger", want)
		}
	}
	if !left["backfill/still-walking"] {
		t.Error("backfill/still-walking was deleted — it is inside the cutoff")
	}
	if left["backfill/11474999-15299997:sdex"] || left["projected-rebuild/shard-1"] {
		t.Errorf("an abandoned shard survived the reap: %v", left)
	}

	// -source narrows to one job without weakening either other clause.
	seed()
	deleted, err = store.ReapCursors(ctx, cutoff, "projected-rebuild", timescale.LiveCursorSources())
	if err != nil {
		t.Fatalf("ReapCursors(-source): %v", err)
	}
	if deleted != 1 {
		t.Errorf("scoped delete = %d, want 1", deleted)
	}
	left = remaining()
	if !left["backfill/11474999-15299997:sdex"] {
		t.Error("-source projected-rebuild deleted a backfill row")
	}
	if !left["ledgerstream/"] || !left["projector/soroswap"] {
		t.Errorf("a scoped run deleted a live cursor: %v", left)
	}

	// An empty protected list is what a caller passing nil would get:
	// the guard must be the only thing that was holding those rows, so
	// this run proves the earlier survival came from the predicate and
	// not from the rows being out of range anyway.
	seed()
	deleted, err = store.ReapCursors(ctx, cutoff, "", nil)
	if err != nil {
		t.Fatalf("ReapCursors(nil protected): %v", err)
	}
	if deleted != 4 {
		t.Errorf("unprotected delete = %d, want 4 — the two live rows ARE past the cutoff, so only `source <> ALL($3)` was sparing them", deleted)
	}
}
