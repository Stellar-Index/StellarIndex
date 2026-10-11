package v1

import "net/http"

// handleSACWrappers serves GET /v1/sac-wrappers.
//
// Returns the operator-configured map of SAC C-strkey contract IDs to their underlying
// classic-asset keys (supply.AssetKey form, "CODE:ISSUER"; see config.SupplyConfig.SACWrappers).
// Soroban DEX sources emit base/quote as the SAC address, not the classic asset key, so the
// explorer joins this map client-side to render readable symbols.
//
// Wire shape: `{ data: { "C…": "CODE:ISSUER", … } }`, an empty object when no wrappers are
// configured (the explorer then shows the truncated C-strkey).
//
// No query parameters. Cacheable; the map changes only when the API restarts with new config.
func (s *Server) handleSACWrappers(w http.ResponseWriter, r *http.Request) {
	out := s.SACWrappers
	if out == nil {
		out = map[string]string{}
	}
	writeJSON(w, out, Flags{})
}
