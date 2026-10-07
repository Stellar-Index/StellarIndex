package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHandleErrorDoc — the RFC 9457 problem `type` URIs must dereference
// (otherwise ~179 of them would 404). The handler must echo the
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
		if got := strings.Join(rec.Header().Values("Vary"), ","); !strings.Contains(got, "Accept") {
			t.Errorf("Vary = %q, want it to include Accept (#1070: this handler "+
				"content-negotiates on Accept with no Vary, so a shared cache can "+
				"serve the wrong representation)", got)
		}
	})

	t.Run("unknown slug 404s", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/errors/retired-slug-that-never-shipped", nil)
		req.SetPathValue("slug", "retired-slug-that-never-shipped")
		s.handleErrorDoc(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 for a slug matching the kebab-case "+
				"charset but naming no real error type", rec.Code)
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

// TestAPIDesignDoc_AnonymousRateLimitNotHardcoded — §7.1 of
// api-design.md must not assert anonymous callers "see 60" as a live fact,
// while r1 runs anon_rate_limit_per_min far higher (docs/getting-started.md
// puts it at 6,000). The doc must describe 60 as the code default and point
// at the live-value source instead of restating it as fact.
func TestAPIDesignDoc_AnonymousRateLimitNotHardcoded(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	b, err := os.ReadFile(filepath.Join(root, "docs", "reference", "api-design.md"))
	if err != nil {
		t.Fatalf("read api-design.md: %v", err)
	}
	doc := string(b)

	if strings.Contains(doc, "anonymous callers see 60,") {
		t.Error("api-design.md still hard-codes the anonymous rate limit as 60; " +
			"r1's live anon_rate_limit_per_min differs (see docs/getting-started.md) " +
			"and the doc must describe 60 as the code default, not the live value")
	}
	if !strings.Contains(doc, "getting-started.md#rate-limits") {
		t.Error("api-design.md's rate-limit section no longer points readers at " +
			"docs/getting-started.md#rate-limits for the live anonymous limit")
	}
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
