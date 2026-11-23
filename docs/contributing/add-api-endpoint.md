---
title: Checklist — add an API endpoint (v1)
last_verified: 2026-07-01
status: current
---

# Checklist — add an API endpoint (v1)

Reference: any `internal/api/v1/*.go` handler + `server.go::mountRoutes`.

- [ ] `openapi/stellar-index.v1.yaml` — add the path (no `/v1` prefix) + response schema. **Source of truth.**
- [ ] `internal/api/v1/<area>.go` — `func (s *Server) handleX(w, r)`. Reuse `envelope.go` for
      the response envelope; reuse the `Cached*Reader` wrappers if the read is hot — and if you
      prewarm, call the reader with **byte-identical args** to the handler (cache-key-drift trap).
- [ ] `internal/api/v1/server.go` → `mountRoutes`: `s.mux.HandleFunc("GET /v1/x", s.handleX)`.
      (If it needs middleware, wrap it — but note the route lint currently only greps `HandleFunc(`,
      so a `Handle(`-wrapped route escapes the spec drift-check.)
- [ ] **Auth** — nothing at the mount rejects anonymous callers: under `auth_mode = "apikey_optional"`
      (hosted) a keyless request reaches the handler as an anonymous subject. Public data routes use
      plain `HandleFunc` or `s.handlePublic` (spec `security: []`); operator routes use `s.handleAdmin`;
      any other private route must itself reject `auth.TierAnonymous` and the empty tier, and check
      that the subject owns any object id it takes. `auth.RequiredScope` (`internal/auth/scopes.go`)
      returns `read` for every path outside `/v1/admin/`, `/v1/account/` and `/v1/dashboard/`, so a
      private route elsewhere is open to a read-only key. No test enforces any of this for a new route.
- [ ] `internal/api/v1/<area>_test.go` — handler test (table-driven; assert the wire shape).
- [ ] Regenerate **all three** spec artifacts: `make docs-api` (drift-guarded), `make docs-postman`,
      `make web-generate-api`. Commit every diff.
- [ ] Bump the API minor (additive) / major (breaking).

**Guard:** `scripts/ci/lint-docs.sh` fails if a `HandleFunc("GET /v1/…")` route is missing from
the spec or vice-versa. **Done when:** handler test green; `make docs-all` produces no diff.
