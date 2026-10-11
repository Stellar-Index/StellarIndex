// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

// pollutil.go holds shared scaffolding for the polling connectors (FX pollers in particular): HTTP
// GET plumbing, the secret-redacting transport-error formatter, and the fiat interest-set derivation.

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

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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

	// RedactURL, when non-empty, is the log-safe request URL substituted into transport errors.
	// Vendors that take the API key only as a query parameter leak it through *url.Error, which
	// embeds the full URL. Setting it also refuses redirects, because Go copies the full previous
	// URL into the next hop's Referer header.
	RedactURL string
}

var errRedirectRefused = errors.New("redirect refused: request URL carries a secret")

// GetBody performs one context-scoped GET with a 30s-timeout client and a size-capped body read.
// Interpreting non-2xx statuses stays with the caller because vendors disagree about error
// transport (exchangeratesapi serves auth errors as 200 plus a success:false field).
func GetBody(ctx context.Context, r GetRequest) (int, []byte, error) {
	// A zero cap makes io.LimitReader yield an empty body with no error,
	// which a caller would parse as "no rates" rather than a config bug.
	if r.LimitBytes <= 0 {
		return 0, nil, fmt.Errorf("GetRequest.LimitBytes must be positive, got %d", r.LimitBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	if r.RedactURL != "" {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirectRefused }
	}
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

// redactURLError converts a transport error into a string with the secret-bearing URL scrubbed:
// *url.Error embeds the full request URL, including any key-carrying query param, so it is
// replaced by a path-only form. Other error types do not carry the URL and pass through.
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

// DefaultFXPairTargets is the G10-ish fiat cross-rate set the FX pollers target against a base
// currency. It is shared by production wiring and the verify-external diagnostic so the two cannot
// drift apart and silently drop a reference rate from one of them.
var DefaultFXPairTargets = []string{
	"USD", "EUR", "GBP", "JPY", "CAD", "AUD", "CHF", "NZD", "SEK", "NOK", "MXN",
}

// DefaultFXPairs builds the fiat/base cross pairs described by
// DefaultFXPairTargets, skipping base itself. An unrecognised base
// (not on the ADR-0010 allow-list) returns nil — the caller no-ops.
func DefaultFXPairs(base string) []canonical.Pair {
	baseAsset, err := canonical.NewFiatAsset(base)
	if err != nil {
		return nil
	}
	out := make([]canonical.Pair, 0, len(DefaultFXPairTargets))
	for _, code := range DefaultFXPairTargets {
		if code == base {
			continue
		}
		a, err := canonical.NewFiatAsset(code)
		if err != nil {
			continue
		}
		p, err := canonical.NewPair(a, baseAsset)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// FiatCodesFromPairs derives an FX poller's interest set: every fiat code on either side of any
// configured pair, uppercased and mapped to its Asset, excluding excludeCode (the venue's base,
// whose rate of 1 is useless). An empty result means no fiat cross-rates, so the poller no-ops.
//
// Either side counts because an operator configuring XLM/EUR still needs USD/EUR to triangulate.
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
