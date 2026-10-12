package client

import (
	"encoding/json"
	"fmt"
)

// AssetLookup is the dual-shape /v1/assets/{asset_id} response (ADR-0042); callers MUST check [AssetLookup.Kind] (or the StellarAsset/Catalogue accessors, which return ok=false) because decoding a catalogue slug into [AssetDetail] would yield zero-valued required fields.
type AssetLookup struct {
	kind      string
	stellar   *AssetDetail
	catalogue *GlobalAssetView
}

// Kind reports the server's wire-shape discriminator, "stellar_asset" or "catalogue"; empty on a zero-value AssetLookup.
func (a AssetLookup) Kind() string { return a.kind }

// StellarAsset returns the per-Stellar-asset detail and ok=true when Kind() == "stellar_asset"; otherwise a zero [AssetDetail] and ok=false.
func (a AssetLookup) StellarAsset() (AssetDetail, bool) {
	if a.stellar == nil {
		return AssetDetail{}, false
	}
	return *a.stellar, true
}

// Catalogue returns the verified-currency global view and ok=true when Kind() == "catalogue"; otherwise a zero [GlobalAssetView] and ok=false.
func (a AssetLookup) Catalogue() (GlobalAssetView, bool) {
	if a.catalogue == nil {
		return GlobalAssetView{}, false
	}
	return *a.catalogue, true
}

// UnmarshalJSON implements [json.Unmarshaler] by peeking the `kind` field; an unrecognised or missing `kind` is a hard error, never a silent zero-fill.
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

// MarshalJSON implements [json.Marshaler], so a received [AssetLookup] can be re-encoded without unwrapping it.
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
