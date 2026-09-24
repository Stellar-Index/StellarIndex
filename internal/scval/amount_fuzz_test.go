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

// Amount-decoding fuzz targets. Each checks the decoded value against an
// independent *big.Int reference built from the raw limbs (ADR-0003: no
// int64/float64 truncation anywhere between the wire and canonical.Amount).

var two64 = new(big.Int).Lsh(big.NewInt(1), 64)

// refSigned returns hi*2^64 + lo for a signed high limb.
func refSigned(hi int64, lo uint64) *big.Int {
	r := new(big.Int).Mul(big.NewInt(hi), two64)
	return r.Add(r, new(big.Int).SetUint64(lo))
}

// refLimbs folds big-endian 64-bit limbs; the first limb is signed iff signed.
func refLimbs(signed bool, limbs ...uint64) *big.Int {
	r := new(big.Int).SetUint64(limbs[0])
	if signed {
		r.SetInt64(int64(limbs[0]))
	}
	for _, l := range limbs[1:] {
		r.Mul(r, two64)
		r.Add(r, new(big.Int).SetUint64(l))
	}
	return r
}

func fzI128(hi int64, lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}}
}

func fzSym(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func fzStr(s string) xdr.ScVal {
	str := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &str}
}

func fzMap(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// wireRoundTrip encodes sv, re-parses it through the public base64 entry
// point, and returns the parsed value — the path every decoder takes.
func wireRoundTrip(t *testing.T, sv xdr.ScVal) xdr.ScVal {
	t.Helper()
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := scval.Parse(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("Parse(own encoding): %v", err)
	}
	return got
}

// FuzzAmountFromI128 pins every i128 read path — the typed accessor, both
// SEP-41 balance/transfer body shapes (bare i128 and the CAP-67 map carrying
// `amount` + `to_muxed_id`), and the explorer renderer — to the same
// big.Int reference, across the full signed 128-bit range.
func FuzzAmountFromI128(f *testing.F) {
	f.Add(int64(0), uint64(0))
	f.Add(int64(0), uint64(330000000))           // bare i128 burn fixture (display_fuzz_test seed)
	f.Add(int64(0), uint64(1509470554))          // CAP-67 map fixture amount
	f.Add(int64(0), uint64(1)<<63)               // first value past int64
	f.Add(int64(1), uint64(0))                   // first value past u64
	f.Add(int64(-1), ^uint64(0))                 // -1
	f.Add(int64(-1), uint64(0))                  // -2^64
	f.Add(int64(1<<63-1), ^uint64(0))            // i128 max
	f.Add(int64(-1<<63), uint64(0))              // i128 min
	f.Add(int64(0x7fffffff), uint64(0x1234abcd)) // mixed limbs
	f.Fuzz(func(t *testing.T, hi int64, lo uint64) {
		want := refSigned(hi, lo)
		sv := wireRoundTrip(t, fzI128(hi, lo))

		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			t.Fatalf("AsAmountFromI128: %v", err)
		}
		if amt.BigInt().Cmp(want) != 0 {
			t.Fatalf("AsAmountFromI128(hi=%d, lo=%d) = %s, want %s", hi, lo, amt.BigInt(), want)
		}
		if amt.String() != want.String() {
			t.Fatalf("Amount.String() = %q, want %q", amt.String(), want.String())
		}

		// Bare i128 body.
		got, err := scval.SEP41BalanceAmount(sv)
		if err != nil || got.Cmp(want) != 0 {
			t.Fatalf("SEP41BalanceAmount(bare) = %v, %v; want %s", got, err, want)
		}

		// CAP-67 map body, `amount` in either position: decode is by name.
		for _, body := range []xdr.ScVal{
			fzMap(xdr.ScMapEntry{Key: fzSym("amount"), Val: sv}, xdr.ScMapEntry{Key: fzSym("to_muxed_id"), Val: fzStr("memo")}),
			fzMap(xdr.ScMapEntry{Key: fzSym("to_muxed_id"), Val: fzI128(^hi, ^lo)}, xdr.ScMapEntry{Key: fzSym("amount"), Val: sv}),
		} {
			got, err := scval.SEP41BalanceAmount(wireRoundTrip(t, body))
			if err != nil || got.Cmp(want) != 0 {
				t.Fatalf("SEP41BalanceAmount(map) = %v, %v; want %s", got, err, want)
			}
		}

		// A foreign shape carrying the same limbs must be REJECTED, not
		// mis-decoded: `amount` under a String key (not a Symbol), `amount`
		// as u128, and a map with no `amount` at all.
		u := xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Hi: xdr.Uint64(uint64(hi)), Lo: xdr.Uint64(lo)}}
		for name, body := range map[string]xdr.ScVal{
			"string key":    fzMap(xdr.ScMapEntry{Key: fzStr("amount"), Val: sv}),
			"u128 amount":   fzMap(xdr.ScMapEntry{Key: fzSym("amount"), Val: u}),
			"no amount":     fzMap(xdr.ScMapEntry{Key: fzSym("value"), Val: sv}),
			"Amount (case)": fzMap(xdr.ScMapEntry{Key: fzSym("Amount"), Val: sv}),
			"bare u128":     u,
			"amount symbol": fzSym("amount"),
		} {
			if got, err := scval.SEP41BalanceAmount(wireRoundTrip(t, body)); !errors.Is(err, scval.ErrUnknownBalanceValShape) {
				t.Fatalf("SEP41BalanceAmount(%s) = %v, %v; want ErrUnknownBalanceValShape", name, got, err)
			}
		}
		if _, err := scval.AsAmountFromU128(sv); !errors.Is(err, scval.ErrScValType) {
			t.Fatalf("AsAmountFromU128(i128) err = %v, want ErrScValType", err)
		}

		if d := scval.Display(sv); d != want.String() {
			t.Fatalf("Display(i128) = %q, want %q", d, want.String())
		}
	})
}

// FuzzI128Ordering checks the decoded amounts order exactly as the two's-
// complement 128-bit integers do: (hi signed, lo unsigned) lexicographic.
// A limb swap or sign slip in the decode inverts some pair.
func FuzzI128Ordering(f *testing.F) {
	f.Add(int64(0), uint64(1)<<63, int64(0), uint64(1)<<63-1)
	f.Add(int64(-1), ^uint64(0), int64(0), uint64(0))
	f.Add(int64(1), uint64(0), int64(0), ^uint64(0))
	f.Add(int64(-1<<63), uint64(0), int64(1<<63-1), ^uint64(0))
	f.Fuzz(func(t *testing.T, hi1 int64, lo1 uint64, hi2 int64, lo2 uint64) {
		a, err := scval.AsAmountFromI128(fzI128(hi1, lo1))
		if err != nil {
			t.Fatal(err)
		}
		b, err := scval.AsAmountFromI128(fzI128(hi2, lo2))
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		switch {
		case hi1 < hi2 || (hi1 == hi2 && lo1 < lo2):
			want = -1
		case hi1 > hi2 || (hi1 == hi2 && lo1 > lo2):
			want = 1
		}
		if got := a.Cmp(b); got != want {
			t.Fatalf("Cmp(%s, %s) = %d, want %d", a, b, got, want)
		}
	})
}

// FuzzAmountFromU128 pins the u128 accessor and renderer to the reference
// and checks the result is never negative (a signed reinterpretation of the
// high limb would flip values ≥ 2^127).
func FuzzAmountFromU128(f *testing.F) {
	f.Add(uint64(0), uint64(0))
	f.Add(uint64(0), ^uint64(0))
	f.Add(uint64(1)<<63, uint64(0))
	f.Add(^uint64(0), ^uint64(0))
	f.Add(uint64(1), uint64(1))
	f.Fuzz(func(t *testing.T, hi, lo uint64) {
		want := refLimbs(false, hi, lo)
		sv := wireRoundTrip(t, xdr.ScVal{Type: xdr.ScValTypeScvU128, U128: &xdr.UInt128Parts{Hi: xdr.Uint64(hi), Lo: xdr.Uint64(lo)}})
		amt, err := scval.AsAmountFromU128(sv)
		if err != nil {
			t.Fatalf("AsAmountFromU128: %v", err)
		}
		if amt.BigInt().Cmp(want) != 0 || amt.BigInt().Sign() < 0 {
			t.Fatalf("AsAmountFromU128(hi=%d, lo=%d) = %s, want %s", hi, lo, amt.BigInt(), want)
		}
		if d := scval.Display(sv); d != want.String() {
			t.Fatalf("Display(u128) = %q, want %q", d, want.String())
		}
		if _, err := scval.AsAmountFromI128(sv); !errors.Is(err, scval.ErrScValType) {
			t.Fatalf("AsAmountFromI128(u128) err = %v, want ErrScValType", err)
		}
	})
}

// FuzzAmountFrom256 covers the 256-bit arms: the u256 accessor + renderer and
// the i256 renderer (two's complement with a signed top limb).
func FuzzAmountFrom256(f *testing.F) {
	f.Add(uint64(0), uint64(0), uint64(0), uint64(0))
	f.Add(uint64(0), uint64(0), uint64(0), uint64(1))
	f.Add(uint64(1), uint64(2), uint64(3), uint64(4))
	f.Add(^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0))
	f.Add(uint64(1)<<63, uint64(0), uint64(0), uint64(0))
	f.Add(uint64(1)<<63-1, ^uint64(0), ^uint64(0), ^uint64(0))
	f.Fuzz(func(t *testing.T, hh, hl, lh, ll uint64) {
		wantU := refLimbs(false, hh, hl, lh, ll)
		u := wireRoundTrip(t, xdr.ScVal{Type: xdr.ScValTypeScvU256, U256: &xdr.UInt256Parts{
			HiHi: xdr.Uint64(hh), HiLo: xdr.Uint64(hl), LoHi: xdr.Uint64(lh), LoLo: xdr.Uint64(ll),
		}})
		amt, err := scval.AsAmountFromU256(u)
		if err != nil {
			t.Fatalf("AsAmountFromU256: %v", err)
		}
		if amt.BigInt().Cmp(wantU) != 0 {
			t.Fatalf("AsAmountFromU256 = %s, want %s", amt.BigInt(), wantU)
		}
		if d := scval.Display(u); d != wantU.String() {
			t.Fatalf("Display(u256) = %q, want %q", d, wantU.String())
		}

		wantI := refLimbs(true, hh, hl, lh, ll)
		i := wireRoundTrip(t, xdr.ScVal{Type: xdr.ScValTypeScvI256, I256: &xdr.Int256Parts{
			HiHi: xdr.Int64(int64(hh)), HiLo: xdr.Uint64(hl), LoHi: xdr.Uint64(lh), LoLo: xdr.Uint64(ll),
		}})
		if d := scval.Display(i); d != wantI.String() {
			t.Fatalf("Display(i256) = %q, want %q", d, wantI.String())
		}
		if _, err := scval.AsAmountFromU256(i); !errors.Is(err, scval.ErrScValType) {
			t.Fatalf("AsAmountFromU256(i256) err = %v, want ErrScValType", err)
		}
	})
}
