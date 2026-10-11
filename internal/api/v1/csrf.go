// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"mime"
	"net/http"
)

// requireJSONContentType is the CSRF gate shared by the anonymous
// durable-write endpoints /v1/signup and /v1/register. They cannot use
// middleware.RequireSameSiteWrite like the browser-only /v1/auth/*
// siblings: both are public API endpoints, so a legitimate curl or
// server-side caller has no Origin/Referer and would be refused.
//
// `application/json` is NOT a CORS "simple request" type, so a cross-site
// browser POST carrying it must be preflighted, and our CORS policy refuses
// the preflight. The header is therefore REQUIRED, not just
// validated-when-present: a header-less POST (sendBeacon with an empty-type
// Blob, fetch with mode:"no-cors") is a CORS simple request with no
// preflight to refuse, so `if ct != ""` would skip the check and mint a
// credential plus a verification email per visitor.
// credential + relaying a verification email per visitor and burning a
// token from the per-IP throttle the two endpoints share.
//
// Returns true when the request carries Content-Type: application/json
// and the caller may proceed; otherwise it writes a 415 problem
// response and returns false. endpoint is the request path (e.g.
// "/v1/signup") used only for the human-readable detail message.
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
