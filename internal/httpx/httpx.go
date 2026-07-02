// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package httpx holds tiny shared HTTP response helpers for handler
// packages that do NOT speak the enveloped public v1 surface (the
// dashboard session-auth packages: dashboardauth / dashboardkeys /
// dashboardwebhooks). Extracted from three near-identical private
// copies (D3 cluster 10).
//
// The public v1 envelope + problem writers stay canonical in
// internal/api/v1/envelope.go — do not use this package for enveloped
// v1 responses.
package httpx

import (
	"encoding/json"
	"net/http"
)

// WriteJSON sends `body` as application/json with the given status.
// Cache-Control: no-store keeps responses out of intermediate caches —
// callers serve session-scoped data.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteProblem emits an RFC 9457 problem+json error body matching the
// rest of the v1 surface. typeURL identifies the emitting surface
// (e.g. "https://api.stellarindex.io/errors/dashboard"); title is
// derived from the status code.
func WriteProblem(w http.ResponseWriter, typeURL string, status int, detail, instance string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     typeURL,
		"title":    http.StatusText(status),
		"status":   status,
		"detail":   detail,
		"instance": instance,
	})
}
