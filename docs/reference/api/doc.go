// Package api holds the docs-site static assets deployed to
// docs.stellarindex.io by the Cloudflare Pages project stellarindex-docs,
// not to the API host (the OpenAPI spec, Cloudflare Pages `_redirects`
// / `_headers`, and the landing page). It exists only so the assets
// here can carry a Go test (redirects_test.go) verifying the
// `_redirects` rule table against its own header comment.
package api
