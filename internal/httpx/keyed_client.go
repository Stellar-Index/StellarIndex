// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"fmt"
	"net/http"
	"time"
)

// keyedRedirectLimit mirrors net/http's defaultCheckRedirect cap, which a
// non-nil Client.CheckRedirect replaces wholesale.
const keyedRedirectLimit = 10

// NewKeyedClient returns the client an outbound call carrying a vendor API
// key in a request header must use: timeout-bounded, with
// [KeyedSameOriginRedirect] as its redirect policy. tag prefixes errors.
func NewKeyedClient(tag string, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: KeyedSameOriginRedirect(tag)}
}

// KeyedSameOriginRedirect is the CheckRedirect policy for a client that
// sends a vendor API key in a request header.
//
// Go's redirect header copier re-sends every custom header verbatim on
// each hop, and keeps Authorization whenever the hostname matches — even
// across a port or an https -> http change. A hop is followed only when it
// keeps the scheme and host (port included), so the key goes nowhere it has
// not already been sent. The hop cap keeps a self-redirecting origin from
// re-issuing keyed requests until the client timeout.
func KeyedSameOriginRedirect(tag string) func(*http.Request, []*http.Request) error {
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
