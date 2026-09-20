// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

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

// TestSupplyStorageFallbackAsOfLedgerIsLakeWatermark pins ADR-0041 Decision 4
// on the storage-derived arm: `as_of_ledger` is the lake watermark — the same
// ledger `flags.stale` is judged from — not the storage reader's own
// max(last_modified_ledger), which is when a balance last moved and trails
// the tip by however long the holders have been idle. The two were stamped
// from different sources, so a response could carry a fresh-looking stale
// flag beside a months-old as_of_ledger, or the reverse.
func TestSupplyStorageFallbackAsOfLedgerIsLakeWatermark(t *testing.T) {
	const (
		lastMoved = 64165090 // dealStorageSupply().AsOfLedger
		lakeTip   = 64200000
	)
	cases := []struct {
		name      string
		wm        LakeWatermarkReader
		wantAsOf  uint32
		wantStale bool
	}{
		{"fresh watermark", &stubWatermark{ledger: lakeTip, closedAt: time.Now()}, lakeTip, false},
		{"stale watermark", &stubWatermark{ledger: lakeTip, closedAt: time.Now().Add(-lakeStaleThreshold - 30*time.Second)}, lakeTip, true},
		{"no watermark reader wired", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStorageSupply{out: dealStorageSupply()}
			if st.out.AsOfLedger != lastMoved {
				t.Fatalf("fixture AsOfLedger = %d, want %d", st.out.AsOfLedger, lastMoved)
			}
			srv := &Server{
				tokenSupply:         &fakeTokenSupply{supply: zeroFlows()},
				storageSupply:       st,
				lakeWatermarkReader: tc.wm,
				logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/assets/{asset_id}/supply", srv.handleAssetSupply)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+storageDealContractID+"/supply", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body)
			}
			got, flags := decodeSupplyEnvelope(t, rec.Body.Bytes())
			if got.Source != string(supply.BasisContractStorageBalances) {
				t.Fatalf("source = %q, want %q", got.Source, supply.BasisContractStorageBalances)
			}
			if got.AsOfLedger != tc.wantAsOf {
				t.Errorf("as_of_ledger = %d, want %d (the lake watermark flags.stale is judged from, "+
					"not the last ledger a balance entry moved at)", got.AsOfLedger, tc.wantAsOf)
			}
			if flags.Stale != tc.wantStale {
				t.Errorf("flags.stale = %v, want %v", flags.Stale, tc.wantStale)
			}
		})
	}
}
