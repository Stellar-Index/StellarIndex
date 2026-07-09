package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestAssetLookup_UnmarshalJSON_BothBranches decodes real
// kind:"stellar_asset" and kind:"catalogue" fixtures through
// AssetLookup and confirms each branch populates the right accessor.
// This is the plan's option (b) for the contract-test harness: the
// generic reflection-based TestSDKSchemasMatchSpec check (in
// spec_contract_test.go) can't walk AssetLookup's hand-written
// UnmarshalJSON/MarshalJSON, so the "Asset" coveredOperations entry
// there sets payload:nil and both branches are exercised directly
// here instead.
func TestAssetLookup_UnmarshalJSON_BothBranches(t *testing.T) {
	t.Run("stellar_asset branch", func(t *testing.T) {
		const body = `{
			"kind": "stellar_asset",
			"asset_id": "native",
			"type": "native",
			"code": "XLM",
			"decimals": 7,
			"sep1_status": "not_applicable"
		}`
		var got AssetLookup
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Kind() != "stellar_asset" {
			t.Fatalf("Kind() = %q, want stellar_asset", got.Kind())
		}
		asset, ok := got.StellarAsset()
		if !ok {
			t.Fatal("StellarAsset() ok = false")
		}
		if asset.AssetID != "native" || asset.Type != "native" || asset.Decimals != 7 {
			t.Errorf("StellarAsset() decoded wrong: %+v", asset)
		}
		if _, ok := got.Catalogue(); ok {
			t.Error("Catalogue() ok = true on the stellar_asset branch, want false")
		}

		// MarshalJSON round-trip.
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var again AssetLookup
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("re-Unmarshal: %v", err)
		}
		if reAsset, ok := again.StellarAsset(); !ok || reAsset.AssetID != "native" {
			t.Errorf("round-trip lost data: ok=%v asset=%+v", ok, reAsset)
		}
	})

	t.Run("catalogue branch", func(t *testing.T) {
		const body = `{
			"kind": "catalogue",
			"ticker": "USDC",
			"slug": "usdc",
			"name": "USD Coin",
			"class": "stablecoin"
		}`
		var got AssetLookup
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Kind() != "catalogue" {
			t.Fatalf("Kind() = %q, want catalogue", got.Kind())
		}
		view, ok := got.Catalogue()
		if !ok {
			t.Fatal("Catalogue() ok = false")
		}
		if view.Ticker != "USDC" || view.Slug != "usdc" || view.Name != "USD Coin" {
			t.Errorf("Catalogue() decoded wrong: %+v", view)
		}
		if _, ok := got.StellarAsset(); ok {
			t.Error("StellarAsset() ok = true on the catalogue branch, want false")
		}

		// MarshalJSON round-trip.
		out, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var again AssetLookup
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("re-Unmarshal: %v", err)
		}
		if reView, ok := again.Catalogue(); !ok || reView.Slug != "usdc" {
			t.Errorf("round-trip lost data: ok=%v view=%+v", ok, reView)
		}
	})

	// This is the live SDK bug ADR-0042's AssetLookup union fixes,
	// pinned as a regression test. Pre-fix, Client.Asset() decoded
	// unconditionally into Envelope[AssetDetail]: a catalogue-slug
	// response would "succeed" with AssetDetail's required fields
	// (AssetID, Type, Code, Decimals, Sep1Status) silently left at
	// their zero value — no error, no signal. The first sub-test
	// below proves that failure mode is real (plain json.Unmarshal
	// into AssetDetail "succeeds" on a catalogue payload); the
	// second proves AssetLookup makes it structurally impossible to
	// mistake one branch for the other.
	t.Run("silent-zero-fill bug is real for a bare AssetDetail decode", func(t *testing.T) {
		const catalogueBody = `{
			"kind": "catalogue",
			"ticker": "USDC",
			"slug": "usdc",
			"name": "USD Coin",
			"class": "stablecoin"
		}`
		var direct AssetDetail
		if err := json.Unmarshal([]byte(catalogueBody), &direct); err != nil {
			t.Fatalf("Unmarshal into AssetDetail: %v", err)
		}
		if direct.AssetID != "" || direct.Type != "" || direct.Decimals != 0 || direct.Sep1Status != "" {
			t.Fatalf("test assumption broke: AssetDetail decoded non-zero required fields from a catalogue payload: %+v", direct)
		}
		// direct.err is nil above: encoding/json silently "succeeded"
		// with a struct that has EVERY required field zero-valued.
		// That's the bug. AssetLookup must refuse to hand this back
		// as a StellarAsset().
		var lookup AssetLookup
		if err := json.Unmarshal([]byte(catalogueBody), &lookup); err != nil {
			t.Fatalf("AssetLookup.UnmarshalJSON: %v", err)
		}
		if _, ok := lookup.StellarAsset(); ok {
			t.Fatal("StellarAsset() ok = true for a catalogue payload — the silent zero-fill bug is back")
		}
		if lookup.Kind() != "catalogue" {
			t.Fatalf("Kind() = %q, want catalogue", lookup.Kind())
		}
	})

	t.Run("unrecognised kind is a hard error, not a zero-fill", func(t *testing.T) {
		var got AssetLookup
		if err := json.Unmarshal([]byte(`{"kind": "something_new"}`), &got); err == nil {
			t.Fatal("expected an error for an unrecognised kind, got nil")
		}
	})

	t.Run("missing kind is a hard error", func(t *testing.T) {
		var got AssetLookup
		if err := json.Unmarshal([]byte(`{"asset_id": "native"}`), &got); err == nil {
			t.Fatal("expected an error for a missing kind, got nil")
		}
	})

	t.Run("MarshalJSON on a zero-value AssetLookup errors instead of emitting null", func(t *testing.T) {
		var zero AssetLookup
		if _, err := json.Marshal(zero); err == nil {
			t.Fatal("expected an error marshalling a zero-value AssetLookup, got nil")
		}
	})
}

// TestAssetLookup_SpecSchemaCoverage is belt-and-braces beyond
// TestAssetLookup_UnmarshalJSON_BothBranches: for each branch,
// every property the spec's schema documents (Asset / GlobalAssetView)
// has a matching JSON tag on the Go struct that branch decodes into,
// and vice versa. Reuses the same doc-loading + schema-resolution
// helpers as TestSDKSchemasMatchSpec (spec_contract_test.go), which
// can't run this check itself because AssetLookup isn't a plain
// struct with static JSON tags (see the payload:nil note on the
// "Asset" coveredOperations entry).
func TestAssetLookup_SpecSchemaCoverage(t *testing.T) {
	doc := loadSpec(t)
	for _, tc := range []struct {
		name        string
		envelopeRef string
		payload     any
	}{
		{"stellar_asset branch (Asset schema)", "#/components/schemas/AssetEnvelope", AssetDetail{}},
		{"catalogue branch (GlobalAssetView schema)", "#/components/schemas/GlobalAssetEnvelope", GlobalAssetView{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specProps, ok := dataSchemaProps(t, doc, "GET", "/assets/{asset_id}", tc.envelopeRef)
			if !ok {
				t.Fatalf("could not resolve schema for envelopeRef %q", tc.envelopeRef)
			}
			goProps := jsonTags(reflect.TypeOf(tc.payload))
			for p := range specProps {
				if !goProps[p] {
					t.Errorf("spec property %q missing from %T's JSON tags", p, tc.payload)
				}
			}
			for p := range goProps {
				if !specProps[p] {
					t.Errorf("%T JSON tag %q not documented in the spec schema", tc.payload, p)
				}
			}
		})
	}
}
