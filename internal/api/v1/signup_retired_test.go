// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// requestIDPat strips the one per-request field (the trace id) before the
// byte comparison.
var requestIDPat = regexp.MustCompile(`,"request_id":"[^"]*"`)

// TestSignupRetired_IdenticalForEveryRequest pins the invariant: no unauthenticated endpoint
// answers differently depending on whether an email is known. /v1/signup must not return 201+key for a new
// address and 409 for a known one.
func TestSignupRetired_IdenticalForEveryRequest(t *testing.T) {
	srv := v1.New(v1.Options{Auth: fakeAuthMiddleware(auth.Subject{})})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	do := func(method, path, ctype, body string) (int, string, string) {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), requestIDPat.ReplaceAllString(string(b), "")
	}

	for _, path := range []string{"/v1/signup", "/v1/signup/verify"} {
		cases := []struct{ name, method, ctype, body string }{
			{"no body", http.MethodPost, "", ""},
			{"fresh email", http.MethodPost, "application/json", `{"email":"fresh@example.com"}`},
			{"previously signed up email", http.MethodPost, "application/json", `{"email":"known@example.com","label":"again"}`},
			{"known email, mixed case", http.MethodPost, "application/json", `{"email":"Known@Example.com"}`},
			{"malformed json", http.MethodPost, "application/json", `{not json`},
			{"wrong content type", http.MethodPost, "text/plain", `known@example.com`},
		}
		if path == "/v1/signup/verify" {
			cases = append(cases, struct{ name, method, ctype, body string }{"get", http.MethodGet, "", ""})
		}
		var wantStatus int
		var wantCT, wantBody string
		for i, tc := range cases {
			status, ct, body := do(tc.method, path, tc.ctype, tc.body)
			if i == 0 {
				wantStatus, wantCT, wantBody = status, ct, body
				if status != http.StatusGone {
					t.Fatalf("%s: status = %d, want 410", path, status)
				}
				if !strings.HasPrefix(ct, "application/problem+json") {
					t.Fatalf("%s: content-type = %q, want application/problem+json", path, ct)
				}
				if !strings.Contains(body, "/v1/register") {
					t.Fatalf("%s: detail does not point at /v1/register: %s", path, body)
				}
				continue
			}
			if status != wantStatus || ct != wantCT || body != wantBody {
				t.Errorf("%s %q differs from baseline:\n got %d %q %s\nwant %d %q %s",
					path, tc.name, status, ct, body, wantStatus, wantCT, wantBody)
			}
		}
	}
}
