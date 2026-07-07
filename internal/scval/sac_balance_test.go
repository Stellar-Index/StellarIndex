package scval

import (
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func balanceKey(t *testing.T, holderG string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, holderG)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var pk [32]byte
	copy(pk[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	sym := xdr.ScSymbol("Balance")
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &addr},
	}
	vp := &vec
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func i128(v *big.Int) xdr.ScVal {
	lo := new(big.Int).And(v, new(big.Int).SetUint64(^uint64(0)))
	hi := new(big.Int).Rsh(v, 64)
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi.Int64()), Lo: xdr.Uint64(lo.Uint64())}}
}

const holderG = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF"

func TestIsSEP41BalanceKey(t *testing.T) {
	if !IsSEP41BalanceKey(balanceKey(t, holderG)) {
		t.Error("Balance(Address) key not recognised")
	}
	// Wrong symbol.
	wrongSym := xdr.ScSymbol("Allowance")
	wrongVec := xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &wrongSym}}
	wp := &wrongVec
	if IsSEP41BalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &wp}) {
		t.Error("Allowance key wrongly recognised as Balance")
	}
	// Not a Vec.
	sym := xdr.ScSymbol("Balance")
	if IsSEP41BalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}) {
		t.Error("bare Symbol wrongly recognised as Balance key")
	}
}

func TestHolderFromBalanceKey(t *testing.T) {
	got, err := HolderFromBalanceKey(balanceKey(t, holderG))
	if err != nil {
		t.Fatalf("HolderFromBalanceKey: %v", err)
	}
	if got != holderG {
		t.Errorf("holder=%q want %q", got, holderG)
	}
}

func TestSEP41BalanceAmount_I128NoTruncation(t *testing.T) {
	// > math.MaxInt64 to prove the i128 hi word survives (ADR-0003).
	want, _ := new(big.Int).SetString("12345678901234567890", 10)
	got, err := SEP41BalanceAmount(i128(want))
	if err != nil {
		t.Fatalf("SEP41BalanceAmount: %v", err)
	}
	if got.Cmp(want) != 0 {
		t.Errorf("amount=%s want %s (i128 truncated)", got, want)
	}
}

func TestSEP41BalanceAmount_Map(t *testing.T) {
	amtSym := xdr.ScSymbol("amount")
	m := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &amtSym}, Val: i128(big.NewInt(777))}}
	mp := &m
	got, err := SEP41BalanceAmount(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp})
	if err != nil {
		t.Fatalf("SEP41BalanceAmount(map): %v", err)
	}
	if got.Cmp(big.NewInt(777)) != 0 {
		t.Errorf("map amount=%s want 777", got)
	}
}

func TestSEP41BalanceAmount_UnknownShapeErrors(t *testing.T) {
	sym := xdr.ScSymbol("nope")
	_, err := SEP41BalanceAmount(xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym})
	if !errors.Is(err, ErrUnknownBalanceValShape) {
		t.Errorf("err=%v want ErrUnknownBalanceValShape", err)
	}
	// Map without an `amount` field is also the unknown-shape sentinel.
	other := xdr.ScSymbol("other")
	m := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &other}, Val: i128(big.NewInt(1))}}
	mp := &m
	_, err = SEP41BalanceAmount(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp})
	if !errors.Is(err, ErrUnknownBalanceValShape) {
		t.Errorf("map-without-amount err=%v want ErrUnknownBalanceValShape", err)
	}
}

func TestContractIDFromScAddress(t *testing.T) {
	const cAddr = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	raw, err := strkey.Decode(strkey.VersionByteContract, cAddr)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var cid [32]byte
	copy(cid[:], raw)
	got, ok := ContractIDFromScAddress(xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: (*xdr.ContractId)(&cid)})
	if !ok || got != cAddr {
		t.Errorf("contract addr = (%q,%v) want (%q,true)", got, ok, cAddr)
	}
	// An account scAddress is not a contract id.
	var pk [32]byte
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: (*xdr.Uint256)(&pk)}
	if _, ok := ContractIDFromScAddress(xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}); ok {
		t.Error("account scAddress wrongly accepted as a contract id")
	}
}
