// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"mime"
	"net/http"
)

// requireJSONContentType is the CSRF gate for the anonymous durable-write endpoints /v1/signup and /v1/register (no
// Origin/Referer to require). `application/json` is not a CORS simple type, so a cross-site POST needs a preflight our CORS
// policy refuses; the header is REQUIRED, not validated-when-present, since a header-less POST (sendBeacon, no-cors fetch)
// skips preflight and would mint a credential and email per visitor. Writes 415 and returns false otherwise.
func requireJSONContentType(w http.ResponseWriter, r *http.Request, endpoint string) bool {
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == "application/json" {
		return true
	}
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/unsupported-media-type",
		"Unsupported media type", http.StatusUnsupportedMediaType,
		endpoint+" requires Content-Type: application/json")
	return false
}
