package v1

import (
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// handleAssetMetadata serves GET /v1/assets/{asset_id}/metadata (SEP-1 fields only): 404 when not indexed, else 200 with
// sep1_status. Never 503: resolver failure shows in sep1_status, as on /v1/assets/{id}. The id resolves exactly as the detail's
// does (slug, XLM aliases, configured and lake-confirmed SACs), so both routes describe the same asset.
func (s *Server) handleAssetMetadata(w http.ResponseWriter, r *http.Request) {
	parsed, ok := s.parseAssetGetID(w, r, normaliseAssetIDInput(r.PathValue("asset_id")))
	if !ok {
		return
	}
	parsed, wrapperUnread := s.unwrapSAC(r.Context(), canonical.CanonicalAsset(normalizeXLMAliases(parsed)))
	s.assetMetadataBody(w, r, parsed, wrapperUnread)
}
