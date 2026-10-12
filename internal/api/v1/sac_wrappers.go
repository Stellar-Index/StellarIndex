package v1

import "net/http"

// handleSACWrappers serves GET /v1/sac-wrappers: the operator-configured map of SAC C-strkey ids to classic-asset keys (supply.AssetKey
// form "CODE:ISSUER"; see config.SupplyConfig.SACWrappers).
// Soroban DEX sources emit base/quote as the SAC address, so the explorer joins this map client-side. Wire: `{ data: { "C...":
// "CODE:ISSUER" } }`, empty object when none configured.
// No query parameters; cacheable, since the map changes only when the API restarts with new config.
func (s *Server) handleSACWrappers(w http.ResponseWriter, r *http.Request) {
	out := s.SACWrappers
	if out == nil {
		out = map[string]string{}
	}
	writeJSON(w, out, Flags{})
}
