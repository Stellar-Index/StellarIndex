// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tvlExclusionFor returns the headline's excluded entry for subject.
func tvlExclusionFor(total *DEXTVLTotalView, subject string) (DEXTVLExclusion, bool) {
	if total == nil {
		return DEXTVLExclusion{}, false
	}
	for _, ex := range total.Excluded {
		if ex.Subject == subject {
			return ex, true
		}
	}
	return DEXTVLExclusion{}, false
}

// tvlNotDerivedDetail GETs /v1/protocols/{name}/tvl and returns the
// status and problem detail.
func tvlNotDerivedDetail(t *testing.T, c *DEXTVLCache, name string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Options{DEXTVL: c}).Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/v1/protocols/"+name+"/tvl", nil))
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, p.Detail
}
