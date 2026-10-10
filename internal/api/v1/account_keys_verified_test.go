// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"net/http"
	"strings"
	"testing"
)

func postJSONNoReason(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// With SignupRequireEmailVerification
// on (the default), a verified /v1/signup customer who rotated via
// POST /v1/account/keys got a child record with Identifier signup-<hash>
// and ZERO EmailVerifiedAt — RequireEmailVerified then 403'd that child
// forever, since nothing can verify a non-signup KeyID after the fact.
