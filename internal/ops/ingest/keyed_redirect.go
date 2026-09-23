// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"fmt"
	"net/http"
)

// keyedRedirectLimit mirrors net/http's defaultCheckRedirect cap, which a
// non-nil Client.CheckRedirect replaces wholesale.
const keyedRedirectLimit = 10

// keyedSameOriginRedirect is the CheckRedirect policy for a client that
// sends a vendor API key in a custom header.
//
// Go's redirect header copier strips ONLY Authorization, WWW-Authenticate
// and Cookie when a hop crosses hosts; every other header — the key
// included — is re-sent verbatim. So a vendor 302, a hijacked edge or a
// mistyped -base-url would otherwise hand the paid key to whatever the
// Location names, including a plain-http downgrade. A hop that keeps the
// scheme and host is followed: the key goes nowhere it has not already
// been sent. The hop cap keeps a self-redirecting origin from issuing
// keyed requests at full speed until the client timeout.
func keyedSameOriginRedirect(tag string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= keyedRedirectLimit {
			return fmt.Errorf("%s: stopped after %d redirects", tag, keyedRedirectLimit)
		}
		if len(via) == 0 {
			return nil
		}
		origin := via[0].URL
		if req.URL.Scheme != origin.Scheme || req.URL.Host != origin.Host {
			return fmt.Errorf("%s: refusing to follow a redirect from %s://%s to %s://%s — the API key would follow it",
				tag, origin.Scheme, origin.Host, req.URL.Scheme, req.URL.Host)
		}
		return nil
	}
}
