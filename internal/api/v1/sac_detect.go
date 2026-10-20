package v1

import "github.com/Stellar-Index/StellarIndex/internal/xdrjson"

// isKnownSAC reports whether contractID is a SAC identifiable WITHOUT a captured instance entry: a SAC id is the deterministic
// derivation of its classic asset (native or code:issuer), so we can answer "SAC, no WASM" even when the instance entry predates
// the lake's capture window. Unions the sac_wrappers registry with the computed ids of native and every verified-catalogue
// classic asset; built once, cached. Derivation goes through internal/xdrjson (ADR-0013); the v1 API layer must not import
// go-stellar-sdk/xdr directly.
func (s *Server) isKnownSAC(contractID string) bool {
	s.knownSACsOnce.Do(func() { s.knownSACs = s.buildKnownSACs() })
	_, ok := s.knownSACs[contractID]
	return ok
}

func (s *Server) buildKnownSACs() map[string]struct{} {
	set := make(map[string]struct{}, len(s.SACWrappers))
	for cid := range s.SACWrappers {
		set[cid] = struct{}{}
	}
	if s.NetworkPassphrase == "" {
		return set
	}
	add := func(assetID string) {
		if cid, ok := xdrjson.SACContractID(assetID, s.NetworkPassphrase); ok {
			set[cid] = struct{}{}
		}
	}
	add("native")
	if s.VerifiedCurrencies != nil {
		for _, vc := range s.VerifiedCurrencies.Browseable() {
			if se := vc.StellarEntry(); se != nil && se.AssetID != "" {
				add(se.AssetID)
			}
		}
	}
	return set
}
