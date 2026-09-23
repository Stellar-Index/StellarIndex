// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval_test

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// FuzzEncodeSymbol checks EncodeSymbol accepts exactly the Soroban symbol
// alphabet ([A-Za-z0-9_], 1..32 bytes), round-trips what it accepts, and that
// IsSEP41BalanceKey recognises the `Balance` symbol and nothing else.
func FuzzEncodeSymbol(f *testing.F) {
	for _, s := range []string{
		"Balance", "balance", "Balanc", "Balance_", "mint", "_", "", "a b", "é",
		"abcdefghijklmnopqrstuvwxyz012345", "abcdefghijklmnopqrstuvwxyz0123456", "Z9", "`", "{", "@", "[", "/", ":",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		valid := len(s) >= 1 && len(s) <= 32
		for i := 0; i < len(s) && valid; i++ {
			c := s[i]
			valid = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
		}
		b64, err := scval.EncodeSymbol(s)
		if (err == nil) != valid {
			t.Fatalf("EncodeSymbol(%q) err=%v, want valid=%v", s, err, valid)
		}
		if !valid {
			return
		}
		sv, err := scval.Parse(b64)
		if err != nil {
			t.Fatalf("Parse(EncodeSymbol(%q)): %v", s, err)
		}
		got, err := scval.AsSymbol(sv)
		if err != nil || got != s {
			t.Fatalf("AsSymbol round trip = %q, %v; want %q", got, err, s)
		}
		if das, err := scval.DecodeAddressOrSymbol(sv); err != nil || das.Symbol != s || das.Address != "" {
			t.Fatalf("DecodeAddressOrSymbol(%q) = %+v, %v", s, das, err)
		}
		if _, err := scval.AsString(sv); !errors.Is(err, scval.ErrScValType) {
			t.Fatalf("AsString(symbol) err = %v, want ErrScValType", err)
		}

		var pk xdr.Uint256
		aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
		addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
		key := xdr.ScVec{sv, {Type: xdr.ScValTypeScvAddress, Address: &addr}}
		kp := &key
		if got := scval.IsSEP41BalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &kp}); got != (s == "Balance") {
			t.Fatalf("IsSEP41BalanceKey([%q, addr]) = %v", s, got)
		}
		// Same symbol, wrong arity: never a balance key.
		short := xdr.ScVec{sv}
		sp := &short
		if scval.IsSEP41BalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &sp}) {
			t.Fatalf("IsSEP41BalanceKey([%q]) = true for a 1-element Vec", s)
		}
		long := xdr.ScVec{sv, key[1], key[1]}
		lp := &long
		if scval.IsSEP41BalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &lp}) {
			t.Fatalf("IsSEP41BalanceKey([%q, a, a]) = true for a 3-element Vec", s)
		}
	})
}

// FuzzAddressStrkey builds each ScAddress variant from fuzzed key bytes and
// checks the strkey decodes back to exactly those bytes under exactly that
// variant's version byte — so a holder/contract id is never re-keyed onto a
// different identity. ContractIDFromScAddress must accept the contract arm
// only, and HolderFromBalanceKey must agree with AsAddressStrkey.
func FuzzAddressStrkey(f *testing.F) {
	f.Add(uint8(0), []byte{}, uint64(0))
	f.Add(uint8(1), []byte{0xff, 0x01}, uint64(1))
	f.Add(uint8(2), []byte{0xaa}, uint64(0x0102030405060708))
	f.Add(uint8(3), []byte{0x10}, uint64(0))
	f.Add(uint8(4), []byte{0x20}, uint64(0))
	f.Fuzz(func(t *testing.T, kind uint8, key []byte, id uint64) {
		var k32 [32]byte
		copy(k32[:], key)
		pk := xdr.Uint256(k32)
		var addr xdr.ScAddress
		var ver strkey.VersionByte
		var wantRaw []byte
		switch kind % 5 {
		case 0:
			addr = xdr.ScAddress{
				Type:      xdr.ScAddressTypeScAddressTypeAccount,
				AccountId: &xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk},
			}
			ver, wantRaw = strkey.VersionByteAccountID, k32[:]
		case 1:
			cid := xdr.ContractId(k32)
			addr = xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
			ver, wantRaw = strkey.VersionByteContract, k32[:]
		case 2:
			addr = xdr.ScAddress{
				Type:         xdr.ScAddressTypeScAddressTypeMuxedAccount,
				MuxedAccount: &xdr.MuxedEd25519Account{Id: xdr.Uint64(id), Ed25519: pk},
			}
			ver = strkey.VersionByteMuxedAccount
			wantRaw = binary.BigEndian.AppendUint64(append([]byte{}, k32[:]...), id)
		case 3:
			h := xdr.Hash(k32)
			addr = xdr.ScAddress{
				Type:               xdr.ScAddressTypeScAddressTypeClaimableBalance,
				ClaimableBalanceId: &xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &h},
			}
			ver, wantRaw = strkey.VersionByteClaimableBalance, append([]byte{0}, k32[:]...)
		case 4:
			lp := xdr.PoolId(k32)
			addr = xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeLiquidityPool, LiquidityPoolId: &lp}
			ver, wantRaw = strkey.VersionByteLiquidityPool, k32[:]
		}
		sv := xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
		raw, err := sv.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		sv, err = scval.Parse(base64.StdEncoding.EncodeToString(raw))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		s, err := scval.AsAddressStrkey(sv)
		if err != nil {
			t.Fatalf("AsAddressStrkey(kind %d): %v", kind%5, err)
		}
		dec, err := strkey.Decode(ver, s)
		if err != nil {
			t.Fatalf("strkey %q is not version %v: %v", s, ver, err)
		}
		if string(dec) != string(wantRaw) {
			t.Fatalf("strkey %q decodes to %x, want %x", s, dec, wantRaw)
		}
		if das, err := scval.DecodeAddressOrSymbol(sv); err != nil || das.Address != s || das.Symbol != "" {
			t.Fatalf("DecodeAddressOrSymbol = %+v, %v; want Address %q", das, err, s)
		}
		if got, err := scval.AsAddressOrVoid(sv); err != nil || got != s {
			t.Fatalf("AsAddressOrVoid = %q, %v; want %q", got, err, s)
		}

		cid, ok := scval.ContractIDFromScAddress(addr)
		if ok != (kind%5 == 1) {
			t.Fatalf("ContractIDFromScAddress(kind %d) ok=%v", kind%5, ok)
		}
		if ok && cid != s {
			t.Fatalf("ContractIDFromScAddress = %q, AsAddressStrkey = %q", cid, s)
		}

		bal := scval.MustEncodeSymbol("Balance")
		balSv, err := scval.Parse(bal)
		if err != nil {
			t.Fatal(err)
		}
		vec := xdr.ScVec{balSv, sv}
		vp := &vec
		holder, err := scval.HolderFromBalanceKey(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp})
		if err != nil || holder != s {
			t.Fatalf("HolderFromBalanceKey = %q, %v; want %q", holder, err, s)
		}
	})
}

// FuzzParseContractDataKey checks the contract-data ledger-key parser returns
// the owning contract's C-strkey and the key ScVal unchanged, and rejects —
// rather than mis-attributing — any key not owned by a contract.
func FuzzParseContractDataKey(f *testing.F) {
	f.Add("AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAFVVNUUlkAAAAAAAAB")
	f.Add("AAAABgAAAAAAAAAAO69jTYWNatYfiUrzAyynvPlhO6C0ydQV5mn3M6FzzjsAAAAOAAAABVVTVFJZAAAAAAAAAQ==")
	f.Add("AAAAAAAAAAA7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOw==")
	f.Add("")
	f.Fuzz(func(t *testing.T, b64 string) {
		cid, key, err := scval.ParseContractDataKey(b64)
		var lk xdr.LedgerKey
		if uerr := strictDecodeB64(b64, &lk); uerr != nil {
			if !errors.Is(err, scval.ErrScValDecode) {
				t.Fatalf("undecodable key: err = %v, want ErrScValDecode", err)
			}
			return
		}
		cd, isCD := lk.GetContractData()
		if !isCD || cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract {
			if !errors.Is(err, scval.ErrScValType) {
				t.Fatalf("non-contract-owned key (%s): err = %v, want ErrScValType", lk.Type, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("ParseContractDataKey: %v", err)
		}
		raw, err := strkey.Decode(strkey.VersionByteContract, cid)
		if err != nil {
			t.Fatalf("contract id %q is not a C-strkey: %v", cid, err)
		}
		want := cd.Contract.MustContractId()
		if string(raw) != string(want[:]) {
			t.Fatalf("contract id decodes to %x, want %x", raw, want[:])
		}
		gk, _ := key.MarshalBinary()
		wk, _ := cd.Key.MarshalBinary()
		if string(gk) != string(wk) {
			t.Fatalf("key = %x, want %x", gk, wk)
		}
	})
}

// strictDecodeB64 is the test oracle for a base64 XDR body: exactly one value,
// no bytes left over.
func strictDecodeB64(b64 string, dest any) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	return xdr.SafeUnmarshal(raw, dest)
}
