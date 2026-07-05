// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// pollutil.go — shared scaffolding for the polling connectors (the
// FX pollers in particular: ecb / exchangeratesapi / polygonforex).
// Extracted from three near-identical per-package copies
// (maintainability audit D3 cluster 1 / D1 M0-1 follow-up): the HTTP
// GET plumbing, the secret-redacting transport-error formatter, and
// the fiat interest-set derivation from the configured pair list.
// New pollers should build on these instead of re-pasting them.

package external

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// GetRequest describes one polling GET. Headers are set verbatim on
// the request; LimitBytes caps the body read (vendor boards range
// from ~100 KB rate lists to multi-MB snapshots, so each poller
// states its own ceiling).
type GetRequest struct {
	URL     string
	Headers map[string]string

	// LimitBytes caps io.ReadAll on the response body.
	LimitBytes int64

	// RedactURL, when non-empty, is the log-safe form of the request
	// URL substituted into transport-error messages. Vendors that
	// only accept an API key as a query parameter (exchangeratesapi's
	// access_key) leak the key through *url.Error, which embeds the
	// full request URL — pass the query-less endpoint here and the
	// error is rewritten with the query string redacted (G10-04).
	RedactURL string
}

// GetBody performs one GET with the shared poller conventions: a
// context-scoped request, a 30s-timeout client, and a size-capped
// body read. It returns the HTTP status code and body; interpreting
// non-2xx statuses stays with the caller because vendors disagree
// about error transport (ECB uses plain HTTP statuses, Polygon puts
// detail in a 200 body, exchangeratesapi serves auth errors as 200 +
// a success:false field).
func GetBody(ctx context.Context, r GetRequest) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if r.RedactURL != "" {
			return 0, nil, fmt.Errorf("http: %s", redactURLError(err, r.RedactURL))
		}
		return 0, nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, r.LimitBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, body, nil
}

// redactURLError converts a transport error into a string with the
// secret-bearing URL scrubbed. *url.Error.Error() embeds the full
// request URL — including any key-carrying query param — so we
// replace it with a query-stripped, path-only form. G10-04.
//
// Non-*url.Error inputs are returned via Error() unchanged (they
// don't carry the request URL).
func redactURLError(err error, safeURL string) string {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err.Error()
	}
	return fmt.Sprintf("%s %q: %v", ue.Op, redactQuery(safeURL), ue.Err)
}

// redactQuery returns `rawURL` with any query string replaced by
// "?<redacted>". Falls back to a constant on parse failure so we
// never echo the raw (secret-bearing) input.
func redactQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "[redacted-url]"
	}
	u.RawQuery = ""
	return u.String() + "?<redacted>"
}

// FiatCodesFromPairs derives an FX poller's interest set from the
// configured pair list: every fiat currency code appearing on either
// side of any pair, uppercased and mapped to its Asset, excluding
// excludeCode (the venue's own base currency — the base-to-base
// "rate 1" is never useful). An empty result means the pair list has
// no fiat cross-rates to cover (e.g. all crypto-crypto) and the
// poller should no-op.
//
// Deriving from *either side* is deliberate: an operator who
// configures XLM/EUR still wants the USD/EUR rate available to
// triangulate through.
func FiatCodesFromPairs(pairs []canonical.Pair, excludeCode string) map[string]canonical.Asset {
	excl := strings.ToUpper(excludeCode)
	out := map[string]canonical.Asset{}
	for _, p := range pairs {
		for _, a := range []canonical.Asset{p.Base, p.Quote} {
			if a.Type != canonical.AssetFiat {
				continue
			}
			code := strings.ToUpper(a.Code)
			if code == excl {
				continue
			}
			out[code] = a
		}
	}
	return out
}
