package clickhouse

import (
	"math/big"
	"testing"

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
