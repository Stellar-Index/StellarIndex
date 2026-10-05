// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// Package httpxtest holds test fixtures for clients built on
// [httpx.NewKeyedClient].
package httpxtest

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// RedirectTrap is an origin that 302s every request to a second server on
// another port, recording whether the key header reached either side. The
// port change is the case net/http itself never strips Authorization on.
type RedirectTrap struct {
	// URL is the origin's base URL; point the client under test at it.
	URL string

	header          string
	originHits      atomic.Int32
	originSawKey    atomic.Bool
	elsewhereHits   atomic.Int32
	elsewhereSawKey atomic.Bool
}

// NewRedirectTrap starts both servers; they close with the test.
func NewRedirectTrap(t testing.TB, header string) *RedirectTrap {
	t.Helper()
	trap := &RedirectTrap{header: header}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trap.elsewhereHits.Add(1)
		if r.Header.Get(header) != "" {
			trap.elsewhereSawKey.Store(true)
		}
		http.Error(w, "redirect target", http.StatusTeapot)
	}))
	t.Cleanup(elsewhere.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trap.originHits.Add(1)
		if r.Header.Get(header) != "" {
			trap.originSawKey.Store(true)
		}
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusFound) //nolint:gosec // G710: test fixture; the off-origin redirect to its own server is the point
	}))
	t.Cleanup(origin.Close)
	trap.URL = origin.URL
	return trap
}

// AssertKeyStayedOnOrigin fails unless the client sent the key to the
// origin and then did not dial the redirect target at all.
func (r *RedirectTrap) AssertKeyStayedOnOrigin(t testing.TB) {
	t.Helper()
	if r.originHits.Load() == 0 || !r.originSawKey.Load() {
		t.Fatalf("origin hits = %d, saw %s = %v; the client under test never sent its key, so this proves nothing",
			r.originHits.Load(), r.header, r.originSawKey.Load())
	}
	if n := r.elsewhereHits.Load(); n != 0 {
		t.Errorf("redirect target dialled %d time(s), saw %s = %v; a keyed client must refuse an off-origin redirect",
			n, r.header, r.elsewhereSawKey.Load())
	}
}
