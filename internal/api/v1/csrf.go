// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"mime"
	"net/http"
)

// requireJSONContentType is the CSRF gate shared by the two anonymous
// durable-write endpoints /v1/signup and /v1/register, neither of which
// can use middleware.RequireSameSiteWrite the way their browser-only
// siblings (/v1/auth/login, /v1/auth/verify-code) do: both are public
// API endpoints, so a legitimate curl or server-side caller has no
// Origin/Referer and would be refused.
//
// `application/json` is NOT a CORS "simple request" type, so any
// cross-site browser POST carrying it must be preflighted — and this
// deployment's CORS policy refuses the preflight. The header is
// therefore REQUIRED, not merely validated-when-present: tolerating an
// absent header for "bare curl ergonomics" leaves the exact hole the
// gate exists to close, because a header-less POST (e.g.
// navigator.sendBeacon with an empty-type Blob, or fetch with
// mode:"no-cors") is itself a CORS *simple* request that issues no
// preflight to refuse. Both endpoints previously drifted on this: the
// presence-form `if ct != ""` skipped the check entirely for a
// header-less request, silently minting a credential + relaying a
// verification email per visitor and burning a token from the per-IP
// throttle the two endpoints share (audit 2026-08-13 F4, 2026-08-14
// W1-flow-register-1).
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
