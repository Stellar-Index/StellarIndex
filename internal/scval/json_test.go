// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package scval

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestToJSON_WideIntegersAreExactDecimalStrings(t *testing.T) {
	t.Parallel()
	u64 := xdr.Uint64(1<<63 + 1)
	sym := xdr.ScSymbol("amount")
	// 2^100 + 5: past int64 and past float64's 2^53.
	i128 := xdr.Int128Parts{Hi: 1 << 36, Lo: 5}
	entries := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &i128}},
	}
	pm := &entries
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvU64, U64: &u64},
		{Type: xdr.ScValTypeScvMap, Map: &pm},
		{Type: xdr.ScValTypeScvVoid},
	}
	pv := &vec

	got, err := ToJSON(xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `["9223372036854775809",{"amount":"1267650600228229401496703205381"},null]`
	if string(out) != want {
		t.Errorf("ToJSON = %s, want %s", out, want)
	}
}

func TestToJSON_RefusesWhatItCannotRenderLosslessly(t *testing.T) {
	t.Parallel()
	one := xdr.Uint32(1)
	leaf := xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &one}

	deep := leaf
	for range jsonMaxDepth + 1 {
		vec := xdr.ScVec{deep}
		pv := &vec
		deep = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
	}
	if _, err := ToJSON(deep); !errors.Is(err, ErrNotJSONable) {
		t.Errorf("over-deep value: err = %v, want ErrNotJSONable", err)
	}

	dup := xdr.ScMap{{Key: leaf, Val: leaf}, {Key: leaf, Val: leaf}}
	pm := &dup
	if _, err := ToJSON(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}); !errors.Is(err, ErrNotJSONable) {
		t.Errorf("duplicate key: err = %v, want ErrNotJSONable", err)
	}

	vecKey := xdr.ScVec{leaf}
	pv := &vecKey
	bad := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}, Val: leaf}}
	pb := &bad
	if _, err := ToJSON(xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pb}); !errors.Is(err, ErrNotJSONable) {
		t.Errorf("container key: err = %v, want ErrNotJSONable", err)
	}
}
