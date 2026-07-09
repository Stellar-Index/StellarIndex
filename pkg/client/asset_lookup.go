package client

import (
	"encoding/json"
	"fmt"
)

// AssetLookup is the /v1/assets/{asset_id} dual-shape response
// (ADR-0042 LC-040). [Client.Asset] returns one of these instead of
// a bare [AssetDetail] because the endpoint serves two different
// wire shapes depending on whether the caller passed a canonical
// Stellar asset_id or a verified-currency catalogue slug — see the
// server-side dispatch note on GET /v1/assets/{asset_id} in
// openapi/stellar-index.v1.yaml.
//
// Before this type existed, [Client.Asset] unconditionally decoded
// into [AssetDetail]. encoding/json silently drops unrecognised
// keys and leaves untouched struct fields at their zero value, so a
// catalogue-slug request (server shape: [GlobalAssetView]) decoded
// "successfully" into an [AssetDetail] with every required field
// (AssetID, Type, Code, Decimals, Sep1Status) zero-valued — a live,
// undetected bug (2026-07-09 ADR-0042 follow-through recon). Callers
// MUST check [AssetLookup.Kind] (or use the StellarAsset/Catalogue
// accessors, which return ok=false instead of a lie) rather than
// assume the payload shape.
type AssetLookup struct {
	kind      string
	stellar   *AssetDetail
	catalogue *GlobalAssetView
}

// Kind reports the wire-shape discriminator the server sent:
// "stellar_asset" or "catalogue". Empty on a zero-value AssetLookup
// (e.g. one that was never unmarshalled).
func (a AssetLookup) Kind() string { return a.kind }

// StellarAsset returns the per-Stellar-asset detail view and
// ok=true when Kind() == "stellar_asset". Returns a zero
// [AssetDetail] and ok=false for the catalogue branch — never a
// silently-zero-valued struct passed off as real data.
func (a AssetLookup) StellarAsset() (AssetDetail, bool) {
	if a.stellar == nil {
		return AssetDetail{}, false
	}
	return *a.stellar, true
}

// Catalogue returns the verified-currency global view and ok=true
// when Kind() == "catalogue". Returns a zero [GlobalAssetView] and
// ok=false for the stellar_asset branch.
func (a AssetLookup) Catalogue() (GlobalAssetView, bool) {
	if a.catalogue == nil {
		return GlobalAssetView{}, false
	}
	return *a.catalogue, true
}

// UnmarshalJSON implements [json.Unmarshaler]. It peeks the `kind`
// field before deciding which struct to populate — mirroring the
// server-side dispatch pattern internal/api/v1's tryServeGlobalAsset
// uses (mux-dispatch-before-parse), but client-side via JSON rather
// than a URL path. An unrecognised or missing `kind` is a hard error,
// not a silent zero-fill: this is the whole point of the type.
func (a *AssetLookup) UnmarshalJSON(data []byte) error {
	var peek struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &peek); err != nil {
		return fmt.Errorf("client: AssetLookup: %w", err)
	}
	switch peek.Kind {
	case "stellar_asset":
		var d AssetDetail
		if err := json.Unmarshal(data, &d); err != nil {
			return fmt.Errorf("client: AssetLookup: decode stellar_asset branch: %w", err)
		}
		a.kind, a.stellar, a.catalogue = peek.Kind, &d, nil
		return nil
	case "catalogue":
		var g GlobalAssetView
		if err := json.Unmarshal(data, &g); err != nil {
			return fmt.Errorf("client: AssetLookup: decode catalogue branch: %w", err)
		}
		a.kind, a.catalogue, a.stellar = peek.Kind, &g, nil
		return nil
	default:
		return fmt.Errorf("client: AssetLookup: unrecognised kind %q (want \"stellar_asset\" or \"catalogue\") — server/SDK version mismatch?", peek.Kind)
	}
}

// MarshalJSON implements [json.Marshaler], so an [AssetLookup] a
// caller received can be re-encoded (logging, caching, test
// fixtures) without unwrapping it by hand first.
func (a AssetLookup) MarshalJSON() ([]byte, error) {
	switch a.kind {
	case "stellar_asset":
		if a.stellar == nil {
			return nil, fmt.Errorf("client: AssetLookup: kind=stellar_asset but no StellarAsset value set")
		}
		return json.Marshal(a.stellar)
	case "catalogue":
		if a.catalogue == nil {
			return nil, fmt.Errorf("client: AssetLookup: kind=catalogue but no Catalogue value set")
		}
		return json.Marshal(a.catalogue)
	default:
		return nil, fmt.Errorf("client: AssetLookup: zero value has no kind set")
	}
}
