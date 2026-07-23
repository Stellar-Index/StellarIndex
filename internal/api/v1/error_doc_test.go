package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleErrorDoc — the RFC 9457 problem `type` URIs must dereference
// (site-audit S6: all ~179 previously 404'd). The handler must echo the
// slug, humanise it, and point at the docs, in both JSON and HTML.
func TestHandleErrorDoc(t *testing.T) {
	s := newTestServerWithLogger()

	t.Run("json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/errors/account-not-found", nil)
		req.SetPathValue("slug", "account-not-found")
		s.handleErrorDoc(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		data, _ := body["data"].(map[string]any)
		if data == nil {
			data = body // some envelopes are flat
		}
		if got := data["slug"]; got != "account-not-found" {
			t.Errorf("slug = %v, want account-not-found", got)
		}
		if got, _ := data["title"].(string); got != "Account not found" {
			t.Errorf("title = %q, want %q", got, "Account not found")
		}
		if got, _ := data["type"].(string); got != "https://api.stellarindex.io/errors/account-not-found" {
			t.Errorf("type = %q, want the canonical URI", got)
		}
	})

	t.Run("html", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/errors/rate-limited", nil)
		req.Header.Set("Accept", "text/html")
		req.SetPathValue("slug", "rate-limited")
		s.handleErrorDoc(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		html := rec.Body.String()
		if !strings.Contains(html, "Rate limited") {
			t.Errorf("HTML missing humanised title; got %.120q", html)
		}
		if !strings.Contains(html, "docs.stellarindex.io") {
			t.Error("HTML missing docs link")
		}
	})
}

func TestHumaniseErrorSlug(t *testing.T) {
	cases := map[string]string{
		"account-not-found": "Account not found",
		"rate-limited":      "Rate limited",
		"invalid-max-age":   "Invalid max age",
		"auth":              "Auth",
		"":                  "Error",
	}
	for in, want := range cases {
		if got := humaniseErrorSlug(in); got != want {
			t.Errorf("humaniseErrorSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
