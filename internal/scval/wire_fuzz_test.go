// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval_test

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// TestParseBytes_rejectsTrailingBytes pins that the raw-bytes decoders reject
// bytes left over after one ScVal, exactly as Parse does. Accepting them lets
// a price body (redstone's Bytes-wrapped XDR) or a stored op-args column carry
// arbitrary unvalidated trailing data and still decode as a valid value.
func TestParseBytes_rejectsTrailingBytes(t *testing.T) {
	sym, err := base64.StdEncoding.DecodeString(scval.MustEncodeSymbol("mint"))
	if err != nil {
		t.Fatal(err)
	}
	vec, err := scval.EncodeArgsAsScVec([]string{scval.MustEncodeSymbol("swap")})
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]byte{{0}, {0, 0, 0, 0}, {0xde, 0xad, 0xbe, 0xef}} {
		b := append(append([]byte{}, sym...), extra...)
		if _, err := scval.ParseBytes(b); !errors.Is(err, scval.ErrScValDecode) {
			t.Errorf("ParseBytes(symbol + %x) err = %v, want ErrScValDecode", extra, err)
		}
		v := append(append([]byte{}, vec...), extra...)
		if args, err := scval.DecodeScVecToArgs(v); err == nil {
			t.Errorf("DecodeScVecToArgs(vec + %x) = %v, want an error", extra, args)
		}
	}
	if _, err := scval.ParseBytes(sym); err != nil {
		t.Fatalf("ParseBytes(exact symbol): %v", err)
	}
	if args, err := scval.DecodeScVecToArgs(vec); err != nil || len(args) != 1 {
		t.Fatalf("DecodeScVecToArgs(exact vec) = %v, %v", args, err)
	}
}

// TestParse_rejectsTrailingBytesInFinalQuantum pins that the base64 entry
// points reject one or two trailing bytes that complete the final base64
// quantum — xdr.SafeUnmarshalBase64's consumed-length check counts base64
// characters, so it misses them (found by FuzzParseBytesTwin).
func TestParse_rejectsTrailingBytesInFinalQuantum(t *testing.T) {
	for _, tc := range []struct {
		b64   string
		extra int
	}{
		{"AAAAAQ==", 1},                     // Void (4 bytes)
		{"AAAAAQ==", 2},                     // Void (4 bytes)
		{"AAAAEAAAAAA=", 1},                 // Vec None (8 bytes)
		{"AAAACgAAAAAAAAAAAAAAABOrZoA=", 1}, // i128 SEP-41 amount (20 bytes)
	} {
		raw, err := base64.StdEncoding.DecodeString(tc.b64)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scval.Parse(tc.b64); err != nil {
			t.Fatalf("Parse(%s) exact: %v", tc.b64, err)
		}
		padded := base64.StdEncoding.EncodeToString(append(raw, make([]byte, tc.extra)...))
		if sv, err := scval.Parse(padded); !errors.Is(err, scval.ErrScValDecode) {
			t.Errorf("Parse(%s + %d byte(s)) = %s, %v; want ErrScValDecode", tc.b64, tc.extra, scval.Display(sv), err)
		}
	}

	// A contract-data key whose length is 1 mod 3, so two trailing bytes
	// fall inside the final base64 quantum.
	var cid xdr.ContractId
	cid[0] = 0x3b
	var raw []byte
	var err error
	for s := "USTRY"; ; s += "Y" {
		str := xdr.ScString(s)
		lk := xdr.LedgerKey{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.LedgerKeyContractData{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str},
			Durability: xdr.ContractDataDurabilityPersistent,
		}}
		if raw, err = lk.MarshalBinary(); err != nil {
			t.Fatal(err)
		}
		if len(raw)%3 == 1 {
			break
		}
	}
	if _, _, err := scval.ParseContractDataKey(base64.StdEncoding.EncodeToString(raw)); err != nil {
		t.Fatalf("ParseContractDataKey exact: %v", err)
	}
	for extra := 1; extra <= 2; extra++ {
		padded := base64.StdEncoding.EncodeToString(append(append([]byte{}, raw...), make([]byte, extra)...))
		if cid, _, err := scval.ParseContractDataKey(padded); !errors.Is(err, scval.ErrScValDecode) {
			t.Errorf("ParseContractDataKey(key + %d byte(s)) = %q, %v; want ErrScValDecode", extra, cid, err)
		}
	}
}

// FuzzSEP41BalanceAmountWire feeds arbitrary wire bytes to the balance/
// transfer-body decoder. Oracle: the documented contract — a bare i128, or a
// map whose FIRST Symbol-keyed `amount` entry is an i128; everything else is
// ErrUnknownBalanceValShape. Also checks the decoded body re-encodes to the
// exact input bytes, so no two distinct wire bodies decode to one amount.
func FuzzSEP41BalanceAmountWire(f *testing.F) {
	for _, b64 := range []string{
		"AAAACgAAAAAAAAAAAAAAABOrZoA=",
		"AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAABZ+LFaAAAADwAAAAt0b19tdXhlZF9pZAAAAAAOAAAAGUF1dG8gcmVjaGFyZ2UgdHJhbnNhY3Rpb24AAAA=",
		"AAAADwAAAARtaW50",
		"AAAAAQ==",
	} {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
		f.Add(append(append([]byte{}, raw...), 0, 0, 0, 0)) // trailing bytes
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		sv, err := scval.ParseBytes(raw)
		if err != nil {
			return
		}
		if again, err := sv.MarshalBinary(); err != nil || string(again) != string(raw) {
			t.Fatalf("ParseBytes accepted %x but it re-encodes as %x (%v): the input is not one canonical ScVal", raw, again, err)
		}

		var want *big.Int
		switch sv.Type {
		case xdr.ScValTypeScvI128:
			want = refSigned(int64(sv.I128.Hi), uint64(sv.I128.Lo))
		case xdr.ScValTypeScvMap:
			if sv.Map != nil && *sv.Map != nil {
				for _, e := range **sv.Map {
					if e.Key.Type == xdr.ScValTypeScvSymbol && string(*e.Key.Sym) == "amount" {
						if e.Val.Type == xdr.ScValTypeScvI128 {
							want = refSigned(int64(e.Val.I128.Hi), uint64(e.Val.I128.Lo))
						}
						break
					}
				}
			}
		}

		got, err := scval.SEP41BalanceAmount(sv)
		if want == nil {
			if !errors.Is(err, scval.ErrUnknownBalanceValShape) {
				t.Fatalf("SEP41BalanceAmount(%s) = %v, %v; want ErrUnknownBalanceValShape", scval.Display(sv), got, err)
			}
			return
		}
		if err != nil || got.Cmp(want) != 0 {
			t.Fatalf("SEP41BalanceAmount(%s) = %v, %v; want %s", scval.Display(sv), got, err, want)
		}
	})
}

// FuzzParseBytesTwin checks ParseBytes is the exact raw-bytes twin of Parse:
// the same accept/reject verdict and the same value for every input.
func FuzzParseBytesTwin(f *testing.F) {
	for _, b64 := range []string{"AAAADwAAAARtaW50", "AAAAAQ==", "AAAAEAAAAAEAAAACAAAADgAAAANFVEgAAAAADgAAAANCVEMA", "AAAA"} {
		raw, _ := base64.StdEncoding.DecodeString(b64)
		f.Add(raw)
		f.Add(append(append([]byte{}, raw...), 0xde, 0xad, 0xbe, 0xef))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		a, errA := scval.Parse(base64.StdEncoding.EncodeToString(raw))
		b, errB := scval.ParseBytes(raw)
		if (errA == nil) != (errB == nil) {
			t.Fatalf("verdicts differ on %x: Parse err=%v, ParseBytes err=%v", raw, errA, errB)
		}
		if errB != nil {
			if !errors.Is(errB, scval.ErrScValDecode) {
				t.Fatalf("ParseBytes error %v does not wrap ErrScValDecode", errB)
			}
			return
		}
		ea, _ := a.MarshalBinary()
		eb, _ := b.MarshalBinary()
		if string(ea) != string(eb) {
			t.Fatalf("values differ on %x: Parse=%x ParseBytes=%x", raw, ea, eb)
		}
	})
}

// FuzzScVecArgsRoundTrip checks the op-args column codec. Decode→encode must
// reproduce the stored bytes exactly (for a non-empty Vec), and every decoded
// arg must itself parse, so a stored column is never silently reinterpreted.
func FuzzScVecArgsRoundTrip(f *testing.F) {
	for _, args := range [][]string{
		{scval.MustEncodeSymbol("swap"), scval.MustEncodeString("x")},
		{"AAAACgAAAAAAAAAAAAAAABOrZoA="},
	} {
		b, err := scval.EncodeArgsAsScVec(args)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
		f.Add(append(append([]byte{}, b...), 0, 0, 0, 1))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		args, err := scval.DecodeScVecToArgs(raw)
		if err != nil || len(args) == 0 {
			return
		}
		for i, a := range args {
			if _, err := scval.Parse(a); err != nil {
				t.Fatalf("arg[%d] %q does not parse: %v", i, a, err)
			}
		}
		again, err := scval.EncodeArgsAsScVec(args)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		if string(again) != string(raw) {
			t.Fatalf("DecodeScVecToArgs accepted %x but its args re-encode as %x", raw, again)
		}
	})
}
