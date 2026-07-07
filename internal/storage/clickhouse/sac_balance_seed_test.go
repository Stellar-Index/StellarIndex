package clickhouse

import (
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

const (
	// Valid C-strkey (zero contract id) + G-strkey holder, generated at
	// test-design time so the fixtures don't depend on encoding helpers
	// (matches the sac_balances dispatcher-adapter test constants).
	seedSAC    = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	seedHolder = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"
	seedAsset  = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	otherSAC   = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	seedLedger = uint32(62_400_123)
)

func seedWatched() map[string]string { return map[string]string{seedSAC: seedAsset} }

func mustContractScAddr(t *testing.T, cAddr string) xdr.ScAddress {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cAddr, err)
	}
	var cid [32]byte
	copy(cid[:], raw)
	return xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)}
}

func seedBalanceKey(t *testing.T, holder string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, holder)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", holder, err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	addrSV := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	sym := xdr.ScSymbol("Balance")
	symSV := xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
	vec := xdr.ScVec{symSV, addrSV}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func seedI128Val(amount *big.Int) xdr.ScVal {
	// Split a (possibly > 2^63) non-negative big.Int into hi/lo i128 parts.
	lo := new(big.Int).And(amount, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(amount, 64)
	return xdr.ScVal{
		Type: xdr.ScValTypeScvI128,
		I128: &xdr.Int128Parts{Hi: xdr.Int64(hi.Int64()), Lo: xdr.Uint64(lo.Uint64())},
	}
}

func seedMapVal(amount int64) xdr.ScVal {
	amtSV := seedI128Val(big.NewInt(amount))
	amtSym := xdr.ScSymbol("amount")
	authSym := xdr.ScSymbol("authorized")
	trueB := true
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtSym}, Val: amtSV},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &authSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &trueB}},
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// mustKeyXDR builds the base64 LedgerKey (the shape ledger_entries_current
// stores in key_xdr) for a ContractData Balance entry.
func mustKeyXDR(t *testing.T, contract xdr.ScAddress, key xdr.ScVal) string {
	t.Helper()
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.LedgerKeyContractData{
			Contract:   contract,
			Key:        key,
			Durability: xdr.ContractDataDurabilityPersistent,
		},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatalf("MarshalBase64 key: %v", err)
	}
	return b64
}

// mustEntryXDR builds the base64 LedgerEntry (the shape stored in
// entry_xdr) for a ContractData Balance entry with the supplied value.
func mustEntryXDR(t *testing.T, contract xdr.ScAddress, key, val xdr.ScVal, lastMod uint32) string {
	t.Helper()
	le := xdr.LedgerEntry{
		LastModifiedLedgerSeq: xdr.Uint32(lastMod),
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   contract,
				Key:        key,
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(le)
	if err != nil {
		t.Fatalf("MarshalBase64 entry: %v", err)
	}
	return b64
}

// TestSACBalanceSeedFromRow_I128Val — the common shape: a watched SAC
// wrapper's Balance(Address) entry with a bare i128 value decodes to the
// right (holder, amount, ledger). Uses a value > 2^63 to prove the i128
// hi bits survive (ADR-0003 — never truncated to int64).
func TestSACBalanceSeedFromRow_I128Val(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	// 12_345_678_901_234_567_890 > math.MaxInt64 (9.22e18).
	want, _ := new(big.Int).SetString("12345678901234567890", 10)
	entryXDR := mustEntryXDR(t, contract, key, seedI128Val(want), seedLedger)
	keyXDR := mustKeyXDR(t, contract, key)
	closeTime := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, closeTime, seedWatched())
	if err != nil {
		t.Fatalf("sacBalanceSeedFromRow: %v", err)
	}
	if !matched {
		t.Fatal("matched=false, want true for a watched SAC Balance entry")
	}
	if seed.ContractID != seedSAC {
		t.Errorf("ContractID=%q want %q", seed.ContractID, seedSAC)
	}
	if seed.AssetKey != seedAsset {
		t.Errorf("AssetKey=%q want %q", seed.AssetKey, seedAsset)
	}
	if seed.Holder != seedHolder {
		t.Errorf("Holder=%q want %q", seed.Holder, seedHolder)
	}
	if seed.Balance.Cmp(want) != 0 {
		t.Errorf("Balance=%s want %s (i128 hi bits truncated?)", seed.Balance, want)
	}
	if seed.LedgerSeq != seedLedger {
		t.Errorf("LedgerSeq=%d want %d", seed.LedgerSeq, seedLedger)
	}
	if !seed.CloseTime.Equal(closeTime) {
		t.Errorf("CloseTime=%v want %v", seed.CloseTime, closeTime)
	}
}

// TestSACBalanceSeedFromRow_MapVal — the native SAC BalanceValue map
// shape ({amount, authorized, ...}) decodes its `amount` field.
func TestSACBalanceSeedFromRow_MapVal(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	entryXDR := mustEntryXDR(t, contract, key, seedMapVal(599_880_000_000_000), seedLedger)
	keyXDR := mustKeyXDR(t, contract, key)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("sacBalanceSeedFromRow: %v", err)
	}
	if !matched {
		t.Fatal("matched=false, want true for a map-shaped BalanceValue")
	}
	if seed.Balance.Cmp(big.NewInt(599_880_000_000_000)) != 0 {
		t.Errorf("Balance=%s want 599880000000000 (from BalanceValue map)", seed.Balance)
	}
}

// TestSACBalanceSeedFromRow_NonBalanceKeySkipped — a watched contract's
// non-Balance storage key (e.g. Allowance / metadata) is skipped with no
// error, not mis-seeded.
func TestSACBalanceSeedFromRow_NonBalanceKeySkipped(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	wrongSym := xdr.ScSymbol("Allowance")
	wrongVec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &wrongSym}}
	wp := &wrongVec
	wrongKey := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &wp}
	keyXDR := mustKeyXDR(t, contract, wrongKey)
	entryXDR := mustEntryXDR(t, contract, wrongKey, seedI128Val(big.NewInt(1)), seedLedger)

	seed, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Errorf("matched=true for an Allowance key; want skip (seed=%+v)", seed)
	}
}

// TestSACBalanceSeedFromRow_WrongContractSkipped — a Balance entry of a
// contract NOT in the watched set is skipped (the watched-set filter
// that SQL can't apply because there's no contract_id column).
func TestSACBalanceSeedFromRow_WrongContractSkipped(t *testing.T) {
	contract := mustContractScAddr(t, otherSAC) // not in seedWatched()
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)
	entryXDR := mustEntryXDR(t, contract, key, seedI128Val(big.NewInt(42)), seedLedger)

	_, matched, err := sacBalanceSeedFromRow(keyXDR, entryXDR, "updated", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("matched=true for an unwatched contract; want skip")
	}
}

// TestSACBalanceSeedFromRow_RemovedSkipped — a removed current-state row
// (holder's balance entry deleted) is skipped: it holds nothing.
func TestSACBalanceSeedFromRow_RemovedSkipped(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)

	_, matched, err := sacBalanceSeedFromRow(keyXDR, "", "removed", seedLedger, time.Now().UTC(), seedWatched())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if matched {
		t.Error("matched=true for a removed entry; want skip")
	}
}

// TestSACBalanceSeedFromRow_CorruptEntryErrors — a watched Balance key
// whose entry_xdr is corrupt is a HARD error (the caller is about to
// persist into the served tier; silently dropping it would masquerade as
// "holder holds nothing" — the exact under-count this seed fixes).
func TestSACBalanceSeedFromRow_CorruptEntryErrors(t *testing.T) {
	contract := mustContractScAddr(t, seedSAC)
	key := seedBalanceKey(t, seedHolder)
	keyXDR := mustKeyXDR(t, contract, key)

	if _, _, err := sacBalanceSeedFromRow(keyXDR, "not-base64-xdr!", "updated", seedLedger, time.Now().UTC(), seedWatched()); err == nil {
		t.Fatal("expected error for corrupt entry_xdr on a watched Balance entry")
	}
}
