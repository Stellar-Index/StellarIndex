package clickhouse

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const lumenTestHolder = "GC3C4AKRBQLHOJ45U4XG35ESVWRDECWO5XLDGYADO6DPR3L7KIDVUMML"

func lumenTestCredit() xdr.Asset {
	return xdr.Asset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: xdr.AssetCode4{'U', 'S', 'D', 'C'}, Issuer: xdr.MustAddress(lumenTestHolder)},
	}
}

func lumenTestB64(t *testing.T, d xdr.LedgerEntryData) string {
	t.Helper()
	s, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: d})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return s
}

func lumenTestCB(t *testing.T, asset xdr.Asset, amount int64) string {
	return lumenTestB64(t, xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{1}},
			Asset:     asset,
			Amount:    xdr.Int64(amount),
		},
	})
}

func lumenTestLP(t *testing.T, a, b xdr.Asset, reserveA, reserveB int64) string {
	return lumenTestB64(t, xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{Body: xdr.LiquidityPoolEntryBody{
			Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
			ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
				Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: a, AssetB: b, Fee: 30},
				ReserveA: xdr.Int64(reserveA), ReserveB: xdr.Int64(reserveB),
			},
		}},
	})
}

func lumenTestContractData(t *testing.T, contract xdr.ContractId, key, val xdr.ScVal) string {
	return lumenTestB64(t, xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        val,
		},
	})
}

func lumenTestBalanceKey() xdr.ScVal {
	sym := xdr.ScSymbol("Balance")
	acct := xdr.MustAddress(lumenTestHolder)
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}},
	}
	pv := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
}

func lumenTestI128(hi int64, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}
}

// The lumen sum folds only native XLM, in exact integers: a credit asset's
// amount, another contract's balance or a non-Balance key adds nothing, and a
// SAC balance above 2^63 is carried whole.
func TestFoldNativeHolding_SumsOnlyNativeExactly(t *testing.T) {
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	sac := xdr.ContractId{0x5a}
	other := xdr.ContractId{0x5a, 0x01}
	meta := xdr.ScSymbol("METADATA")

	tally := newLumenTally(100, 0, 0)
	rows := []struct {
		entryType string
		balance   int64
		entryXDR  string
	}{
		{"account", 1_000, ""},
		{"claimable_balance", 0, lumenTestCB(t, native, 70)},
		{"claimable_balance", 0, lumenTestCB(t, lumenTestCredit(), 999)},
		{"liquidity_pool", 0, lumenTestLP(t, native, lumenTestCredit(), 40, 1_000)},
		{"liquidity_pool", 0, lumenTestLP(t, lumenTestCredit(), native, 1_000, 30)},
		{"contract_data", 0, lumenTestContractData(t, sac, lumenTestBalanceKey(), lumenTestI128(1, 5))},
		{"contract_data", 0, lumenTestContractData(t, other, lumenTestBalanceKey(), lumenTestI128(0, 777))},
		{"contract_data", 0, lumenTestContractData(t, sac, xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &meta}, lumenTestI128(0, 888))},
	}
	for _, r := range rows {
		if err := foldNativeHolding(tally, r.entryType, r.balance, r.entryXDR, sac); err != nil {
			t.Fatalf("fold %s: %v", r.entryType, err)
		}
	}
	twoTo64Plus5, _ := new(big.Int).SetString("18446744073709551621", 10)
	checks := []struct {
		name string
		got  *big.Int
		want *big.Int
	}{
		{"accounts", tally.Accounts, big.NewInt(1_000)},
		{"claimable_balances", tally.ClaimableBalances, big.NewInt(70)},
		{"liquidity_pools", tally.LiquidityPools, big.NewInt(70)},
		{"contract_balances", tally.ContractBalances, twoTo64Plus5},
	}
	for _, c := range checks {
		if c.got.Cmp(c.want) != 0 {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}

	tally.TotalCoins, tally.FeePool = 2_000, 860
	wantResidual := new(big.Int).Add(twoTo64Plus5, big.NewInt(1_000+70+70+860-2_000))
	if r := tally.Residual(); r.Cmp(wantResidual) != 0 {
		t.Fatalf("residual = %s, want %s", r, wantResidual)
	}
}

func TestFoldNativeHolding_UnreadableEntriesError(t *testing.T) {
	sac := xdr.ContractId{0x5a}
	tally := newLumenTally(1, 0, 0)
	if err := foldNativeHolding(tally, "claimable_balance", 0, "not-xdr", sac); err == nil {
		t.Error("undecodable entry folded silently")
	}
	str := xdr.ScString("x")
	bad := lumenTestContractData(t, sac, lumenTestBalanceKey(), xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str})
	if err := foldNativeHolding(tally, "contract_data", 0, bad, sac); err == nil {
		t.Error("native SAC balance with no readable amount folded silently")
	}
}

// A key newer than the tally ledger needs a pre-image only if its entry can
// hold native XLM. An evicted temporary allowance has a `removed` row and no
// pre-image; it must resolve to zero from its key, not abort the run.
func TestKeyHoldsNativeLumens(t *testing.T) {
	sac := xdr.ContractId{0x5a}
	allowance := xdr.ScSymbol("Allowance")
	allowanceKey := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &allowance}
	key := func(contract xdr.ContractId, k xdr.ScVal, d xdr.ContractDataDurability) string {
		s, err := xdr.MarshalBase64(xdr.LedgerKey{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.LedgerKeyContractData{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &contract},
				Key:        k,
				Durability: d,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, c := range []struct {
		name, entryType, key string
		want                 bool
	}{
		{"native SAC balance", "contract_data", key(sac, lumenTestBalanceKey(), xdr.ContractDataDurabilityPersistent), true},
		{"evicted native SAC allowance", "contract_data", key(sac, allowanceKey, xdr.ContractDataDurabilityTemporary), false},
		{"other contract's balance", "contract_data", key(xdr.ContractId{0x5a, 0x01}, lumenTestBalanceKey(), xdr.ContractDataDurabilityPersistent), false},
		{"account", "account", "", true},
		{"claimable balance", "claimable_balance", "", true},
	} {
		got, err := keyHoldsNativeLumens(c.entryType, c.key, sac)
		if err != nil || got != c.want {
			t.Errorf("%s: = (%v, %v), want %v", c.name, got, err, c.want)
		}
	}
	if _, err := keyHoldsNativeLumens("contract_data", "not-xdr", sac); err == nil {
		t.Error("undecodable contract_data key accepted")
	}
}

func TestNativeSACKey(t *testing.T) {
	want := xdr.ContractId{0x5a, 0x02}
	id, err := strkey.Encode(strkey.VersionByteContract, want[:])
	if err != nil {
		t.Fatal(err)
	}
	got, prefix, err := nativeSACKey(id)
	if err != nil || got != want || prefix == "" {
		t.Fatalf("nativeSACKey = (%x, %q, %v)", got, prefix, err)
	}
	if _, _, err := nativeSACKey("GABC"); err == nil {
		t.Fatal("bad contract id accepted")
	}
}

// preimageRow is one ledger_entry_changes row as foldPreimageLumens selects it.
type preimageRow struct {
	key, entryType, changeType string
	balance                    int64
	entryXDR, txHash           string
	opIndex                    int32
}

// preimageFakeConn serves rows already in the query's (ledger_seq,
// intra_ledger_seq) order.
type preimageFakeConn struct {
	driver.Conn
	rows []preimageRow
}

func (c preimageFakeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &preimageFakeRows{rows: c.rows, i: -1}, nil
}

type preimageFakeRows struct {
	driver.Rows
	rows []preimageRow
	i    int
}

func (r *preimageFakeRows) Next() bool {
	r.i++
	return r.i < len(r.rows)
}

func (r *preimageFakeRows) Scan(dest ...any) error {
	row := r.rows[r.i]
	*dest[0].(*string) = row.key
	*dest[1].(*string) = row.entryType
	*dest[2].(*string) = row.changeType
	*dest[3].(*int64) = row.balance
	*dest[4].(*string) = row.entryXDR
	*dest[5].(*string) = row.txHash
	*dest[6].(*int32) = row.opIndex
	return nil
}

func (r *preimageFakeRows) Err() error   { return nil }
func (r *preimageFakeRows) Close() error { return nil }

// A legacy snapshot seed row (intra_ledger_seq 0) sharing a ledger with a live
// change sorts first, but it is the entry's post-state: the pre-image is the
// live change's `state` row.
func TestFoldPreimageLumens_SkipsSnapshotSeedRow(t *testing.T) {
	t.Parallel()
	const key = "account-key"
	r := &NetworkStateReader{conn: preimageFakeConn{rows: []preimageRow{
		{key: key, entryType: "account", changeType: "state", balance: 999, opIndex: -1},
		{key: key, entryType: "account", changeType: "state", balance: 100, txHash: "ab", opIndex: 0},
		{key: key, entryType: "account", changeType: "updated", balance: 200, txHash: "ab", opIndex: 0},
	}}}
	tally := newLumenTally(10, 0, 0)
	if err := r.foldPreimageLumens(context.Background(), tally, xdr.ContractId{}, []string{key}); err != nil {
		t.Fatalf("foldPreimageLumens: %v", err)
	}
	if tally.Accounts.Int64() != 100 || tally.Preimages != 1 {
		t.Errorf("Accounts = %s, Preimages = %d; want 100 (the live pre-image) and 1", tally.Accounts, tally.Preimages)
	}
}

// An eviction row is tx-less like a seed but is a real change: it must still
// be read as the key's first change, not skipped to the later `created`.
func TestFoldPreimageLumens_HonoursEvictionRow(t *testing.T) {
	t.Parallel()
	const key = "claimable-balance-key"
	r := &NetworkStateReader{conn: preimageFakeConn{rows: []preimageRow{
		{key: key, entryType: "claimable_balance", changeType: "removed", opIndex: -1},
		{key: key, entryType: "claimable_balance", changeType: "created", txHash: "cd", opIndex: 0},
	}}}
	err := r.foldPreimageLumens(context.Background(), newLumenTally(10, 0, 0), xdr.ContractId{}, []string{key})
	if err == nil || !strings.Contains(err.Error(), `as "removed"`) {
		t.Fatalf("foldPreimageLumens err = %v, want the eviction read as a `removed` first change", err)
	}
}

// The seed predicate matches every row SnapshotEntryRow writes and no row the
// live extractor writes, including its tx-level fee rows and evictions.
func TestIsSnapshotSeedRow_MatchesSeedWriterOnly(t *testing.T) {
	t.Parallel()
	seed, ok := SnapshotEntryRow(accountEntry(t, testSource, 42, 7), time.Unix(0, 0).UTC())
	if !ok {
		t.Fatal("SnapshotEntryRow refused a well-formed account entry")
	}
	if !isSnapshotSeedRow(seed.TxHash, seed.OpIndex, seed.ChangeType) {
		t.Errorf("seed row %+v not matched", seed)
	}

	fee, ok := entryChangeRow(42, time.Unix(0, 0).UTC(), "ab", -1, 0, xdr.LedgerEntryChange{
		Type: xdr.LedgerEntryChangeTypeLedgerEntryState, State: accountEntry(t, testSource, 41, 7),
	})
	if !ok {
		t.Fatal("entryChangeRow refused a fee-phase state change")
	}
	if isSnapshotSeedRow(fee.TxHash, fee.OpIndex, fee.ChangeType) {
		t.Errorf("live fee-phase state row %+v matched as a seed", fee)
	}

	var ext LedgerExtract
	temp := xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
		Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &xdr.ContractId{1}},
		Key:        lumenTestBalanceKey(),
		Durability: xdr.ContractDataDurabilityTemporary,
	}}
	emitEvictions(&ext, []xdr.LedgerKey{temp}, 42, time.Unix(0, 0).UTC(), 0)
	if len(ext.Changes) != 1 {
		t.Fatalf("emitEvictions wrote %d rows, want 1", len(ext.Changes))
	}
	if ev := ext.Changes[0]; isSnapshotSeedRow(ev.TxHash, ev.OpIndex, ev.ChangeType) {
		t.Errorf("eviction row %+v matched as a seed", ev)
	}
}
