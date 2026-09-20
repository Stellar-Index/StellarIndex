// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestAssetSupplyResponseFieldsMatchSpec pins the handler struct to the spec's
// AssetSupply schema in BOTH directions. The spec shipped three property names
// the server never emitted (`total_supply_lower_bound`,
// `contract_self_checks_agreed`, `declared_decimals`) while the handler served
// `circulating_supply_lower_bound`, `supply_consistent` and `decimals`; every
// generated consumer typed the phantom names and read undefined for the real
// ones. The Asset-schema sibling of this test only looks one way.
func TestAssetSupplyResponseFieldsMatchSpec(t *testing.T) {
	props := specSchemaProps(t, "AssetSupply")
	if len(props) == 0 {
		t.Fatal("resolved no properties for the AssetSupply schema — an empty subject set passes forever")
	}
	got := structJSONTags(reflect.TypeOf(AssetSupply{}))
	if len(got) == 0 {
		t.Fatal("AssetSupply exposed no json tags — the reflection walk is broken")
	}
	var undocumented, phantom []string
	for f := range got {
		if !props[f] {
			undocumented = append(undocumented, f)
		}
	}
	for p := range props {
		if !got[p] {
			phantom = append(phantom, p)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(phantom)
	if len(undocumented) > 0 || len(phantom) > 0 {
		t.Errorf("AssetSupply handler vs openapi schema drift:\n  served but undocumented: %v\n"+
			"  documented but never served: %v\nedit openapi/stellar-index.v1.yaml to the "+
			"handler's json tags (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts)", undocumented, phantom)
	}
}

// TestSupplyStorageFallbackOmitsConsistencyWithoutSelfChecks pins the field's
// own contract: `supply_consistent` is absent when the contract published
// neither a TotalSupply nor a HolderCount to check against. A bare token has
// nothing to agree with, and a `true` there is a confirmation it never earned.
func TestSupplyStorageFallbackOmitsConsistencyWithoutSelfChecks(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*fakeStorageSupply)
		want *bool
	}{
		{"no cross-checks published", func(f *fakeStorageSupply) {
			f.out.DeclaredTotal = nil
			f.out.DeclaredHolders = nil
		}, nil},
		{"only TotalSupply published, agreeing", func(f *fakeStorageSupply) {
			f.out.DeclaredHolders = nil
		}, ptrBool(true)},
		{"only HolderCount published, disagreeing", func(f *fakeStorageSupply) {
			f.out.DeclaredTotal = nil
			seven := uint32(7)
			f.out.DeclaredHolders = &seven
		}, ptrBool(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStorageSupply{out: dealStorageSupply()}
			tc.mut(st)
			rec := serveSupplyWithStorage(t, zeroFlows(), st, storageDealContractID)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
			}
			got := decodeSupply(t, rec.Body.Bytes())
			if got.Source != string(supply.BasisContractStorageBalances) {
				t.Fatalf("source = %q, want %q", got.Source, supply.BasisContractStorageBalances)
			}
			var raw struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			_, present := raw.Data["supply_consistent"]
			switch {
			case tc.want == nil && present:
				t.Errorf("supply_consistent = %v, want ABSENT — the contract offered no cross-checks, "+
					"which is not the same as a passed one", *got.SupplyConsistent)
			case tc.want != nil && (!present || got.SupplyConsistent == nil || *got.SupplyConsistent != *tc.want):
				t.Errorf("supply_consistent = %v (present=%v), want %v", got.SupplyConsistent, present, *tc.want)
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }
