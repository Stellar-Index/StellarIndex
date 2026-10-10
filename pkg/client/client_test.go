package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Stellar-Index/StellarIndex/pkg/client"
)

// newTestServer wires an httptest.Server with the supplied handler
// and returns a Client pointed at its URL. Encodes the typical
// boilerplate one-liner.
func newTestServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *client.Client) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := client.New(client.Options{BaseURL: ts.URL})
	return ts, c
}

// TestNew_DefaultsAreSensible — zero Options produces a usable
// client. Defaults are documented in package comments; the test
// pins them so a future change is deliberate.
func TestNew_DefaultsAreSensible(t *testing.T) {
	c := client.New(client.Options{})
	if c == nil {
		t.Fatal("New returned nil")
	}
}

// TestPrice_HappyPath — canonical 200 response decodes cleanly.
func TestPrice_HappyPath(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/price" {
			t.Errorf("path = %q, want /v1/price", r.URL.Path)
		}
		if r.URL.Query().Get("asset") != "native" {
			t.Errorf("asset = %q", r.URL.Query().Get("asset"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {"asset_id":"native","quote":"fiat:USD","price":"0.07142","price_type":"vwap","observed_at":"2026-04-28T10:00:00Z","window_seconds":60},
			"as_of": "2026-04-28T10:00:00Z",
			"sources": ["binance","kraken"],
			"flags": {"stale":false,"reduced_redundancy":false,"triangulated":false,"divergence_warning":false}
		}`))
	})

	env, err := c.Price(context.Background(), client.PriceQuery{Asset: "native", Quote: "fiat:USD"})
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if env.Data.Price != "0.07142" {
		t.Errorf("Price = %q", env.Data.Price)
	}
	if env.Data.PriceType != "vwap" {
		t.Errorf("PriceType = %q", env.Data.PriceType)
	}
	if len(env.Sources) != 2 {
		t.Errorf("Sources = %v", env.Sources)
	}
}

// TestPrice_AssetRequired — empty Asset is a client-side 400 (no
// HTTP roundtrip, no server cost).
func TestPrice_AssetRequired(t *testing.T) {
	c := client.New(client.Options{})
	_, err := c.Price(context.Background(), client.PriceQuery{Asset: ""})
	if err == nil {
		t.Fatal("expected error for empty asset, got nil")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Errorf("error type = %T, want *APIError 400", err)
	}
}

// TestAPIError_DecodesProblemJSON — server 4xx response with
// problem+json body decodes into a typed APIError.
func TestAPIError_DecodesProblemJSON(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{
			"type": "https://api.stellarindex.io/errors/asset-not-found",
			"title": "Asset not found",
			"status": 404,
			"detail": "USDC-G... has no known issuer",
			"instance": "/v1/assets/USDC-GBAD",
			"request_id": "req_abc123"
		}`))
	})

	_, err := c.Asset(context.Background(), "USDC-GBAD")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("not an APIError: %T %v", err, err)
	}
	if apiErr.Status != 404 {
		t.Errorf("Status = %d, want 404", apiErr.Status)
	}
	if !apiErr.IsNotFound() {
		t.Error("IsNotFound() should be true for 404")
	}
	if apiErr.Title != "Asset not found" {
		t.Errorf("Title = %q", apiErr.Title)
	}
	if apiErr.RequestID != "req_abc123" {
		t.Errorf("RequestID = %q", apiErr.RequestID)
	}
	// Error string includes title + request id for support visibility
	msg := apiErr.Error()
	if !strings.Contains(msg, "Asset not found") || !strings.Contains(msg, "req_abc123") {
		t.Errorf("Error() = %q (should include title + request_id)", msg)
	}
}

// TestAPIError_NonProblemJSONFallback — server returned 502 with
// text/plain body (e.g. reverse-proxy error). Surface a status-only
// APIError with the body text in Detail.
func TestAPIError_NonProblemJSONFallback(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream timeout"))
	})

	_, err := c.Price(context.Background(), client.PriceQuery{Asset: "native"})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 {
		t.Fatalf("expected 502 APIError, got %T %v", err, err)
	}
	if apiErr.Detail != "upstream timeout" {
		t.Errorf("Detail = %q", apiErr.Detail)
	}
}

// TestAuthorizationHeader — APIKey on Options translates to a
// Bearer header on every request.
func TestAuthorizationHeader(t *testing.T) {
	var sawAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"as_of":"2026-01-01T00:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_test_xyz"})
	_, _ = c.Me(context.Background())
	if sawAuth != "Bearer rek_test_xyz" {
		t.Errorf("Authorization = %q, want %q", sawAuth, "Bearer rek_test_xyz")
	}
}

// TestNoAuthHeaderWhenAPIKeyEmpty — anonymous client should NOT
// send a Bearer header (otherwise the server might 401 on a
// malformed empty bearer token).
func TestNoAuthHeaderWhenAPIKeyEmpty(t *testing.T) {
	var sawAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"asset_id":"native","quote":"fiat:USD","price":"0","price_type":"vwap","observed_at":"2026-01-01T00:00:00Z"},"as_of":"2026-01-01T00:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL})
	_, _ = c.Price(context.Background(), client.PriceQuery{Asset: "native"})
	if sawAuth != "" {
		t.Errorf("Authorization sent without API key: %q", sawAuth)
	}
}

// TestUserAgent — every request carries the SDK user-agent.
func TestUserAgent(t *testing.T) {
	var sawUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"asset_id":"native","quote":"fiat:USD","price":"0","price_type":"vwap","observed_at":"2026-01-01T00:00:00Z"},"as_of":"2026-01-01T00:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL})
	_, _ = c.Price(context.Background(), client.PriceQuery{Asset: "native"})
	if !strings.HasPrefix(sawUA, "stellarindex-go-sdk/") {
		t.Errorf("User-Agent = %q, want stellarindex-go-sdk/* prefix", sawUA)
	}
}

// TestUserAgentOverride — operator-supplied UA replaces the default.
func TestUserAgentOverride(t *testing.T) {
	var sawUA string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"asset_id":"native","quote":"fiat:USD","price":"0","price_type":"vwap","observed_at":"2026-01-01T00:00:00Z"},"as_of":"2026-01-01T00:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL, UserAgent: "myapp/2.5.0"})
	_, _ = c.Price(context.Background(), client.PriceQuery{Asset: "native"})
	if sawUA != "myapp/2.5.0" {
		t.Errorf("User-Agent = %q, want myapp/2.5.0", sawUA)
	}
}

// TestCreateKey_RoundTrip — POST body marshals correctly; response
// surfaces the plaintext.
func TestCreateKey_RoundTrip(t *testing.T) {
	var bodyReq client.CreateKeyRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&bodyReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {"key_id":"kid_new","plaintext":"rek_freshly_minted","label":"ci-bot"},
			"as_of": "2026-04-28T10:00:00Z",
			"flags": {}
		}`))
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_admin"})
	env, err := c.CreateKey(context.Background(), client.CreateKeyRequest{Label: "ci-bot"})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if env.Data.Plaintext != "rek_freshly_minted" {
		t.Errorf("Plaintext = %q", env.Data.Plaintext)
	}
	if bodyReq.Label != "ci-bot" {
		t.Errorf("server saw label = %q, want ci-bot", bodyReq.Label)
	}
}

// TestCreateKey_LabelRequired — client-side validation catches
// empty label without sending the request.
func TestCreateKey_LabelRequired(t *testing.T) {
	c := client.New(client.Options{})
	_, err := c.CreateKey(context.Background(), client.CreateKeyRequest{})
	if err == nil {
		t.Fatal("expected error for empty label")
	}
}

// TestDegenerate2xxEnvelopeIsRejected — PC-4: a well-formed but
// degenerate 2xx body ({}, {"data":null}) decodes without a JSON
// error into a zero Envelope. Without the as_of invariant guard the
// client would hand the caller a silent zero-value success; with it
// the call fails loudly. The server stamps as_of on every real
// envelope, so a missing as_of unambiguously flags a non-envelope.
func TestDegenerate2xxEnvelopeIsRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "null data, no as_of", body: `{"data":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			})

			_, err := c.Price(context.Background(), client.PriceQuery{Asset: "native", Quote: "fiat:USD"})
			if err == nil {
				t.Fatalf("degenerate body %q decoded to a nil-error zero envelope; want an error", tc.body)
			}
			if !strings.Contains(err.Error(), "envelope") {
				t.Errorf("error = %q, want it to mention the missing envelope invariant", err.Error())
			}
		})
	}
}

// TestValid2xxEnvelopeWithZeroDataStillPasses — the as_of guard must
// only reject genuinely-degenerate bodies, not a legitimate envelope
// whose data is empty. A response carrying a real as_of but a
// zero-value data object stays a success.
func TestValid2xxEnvelopeWithZeroDataStillPasses(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"as_of":"2026-01-01T00:00:00Z","flags":{}}`))
	})
	if _, err := c.Price(context.Background(), client.PriceQuery{Asset: "native", Quote: "fiat:USD"}); err != nil {
		t.Fatalf("valid envelope with empty data was rejected: %v", err)
	}
}

// TestContextCancellation — a cancelled context aborts the request.
func TestContextCancellation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	c := client.New(client.Options{BaseURL: ts.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Price(ctx, client.PriceQuery{Asset: "native"})
	if err == nil {
		t.Fatal("expected cancelled-context error")
	}
}

const meEnvelope = `{"data":{},"as_of":"2026-01-01T00:00:00Z","flags":{}}`

// authRecorder is an httptest server that records every Authorization
// header it receives and answers with a valid /v1/me envelope.
type authRecorder struct {
	mu   sync.Mutex
	seen []string
	srv  *httptest.Server
}

func newAuthRecorder(t *testing.T) *authRecorder {
	t.Helper()
	rec := &authRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(meEnvelope))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *authRecorder) hits() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusFound)
	}
}

// TestRedirect_RefusesSchemeDowngradeWithAPIKey — an https origin that
// redirects to plain http must not have the Bearer key follow it.
func TestRedirect_RefusesSchemeDowngradeWithAPIKey(t *testing.T) {
	plain := newAuthRecorder(t)
	origin := httptest.NewTLSServer(redirectTo(plain.srv.URL))
	t.Cleanup(origin.Close)

	c := client.New(client.Options{BaseURL: origin.URL, APIKey: "rek_test_xyz", HTTPClient: origin.Client()})
	_, err := c.Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("err = %v, want a refused-redirect error", err)
	}
	if got := plain.hits(); len(got) != 0 {
		t.Errorf("plain-http target received %d request(s) (Authorization %q); want none", len(got), got)
	}
}

// TestRedirect_RefusesPortChangeWithAPIKey — same hostname, different
// port is a different origin; the key must not follow.
func TestRedirect_RefusesPortChangeWithAPIKey(t *testing.T) {
	other := newAuthRecorder(t)
	origin := httptest.NewServer(redirectTo(other.srv.URL))
	t.Cleanup(origin.Close)

	c := client.New(client.Options{BaseURL: origin.URL, APIKey: "rek_test_xyz"})
	_, err := c.Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("err = %v, want a refused-redirect error", err)
	}
	if got := other.hits(); len(got) != 0 {
		t.Errorf("other-origin target received %d request(s) (Authorization %q); want none", len(got), got)
	}
}

// TestRedirect_SameOriginStillFollowed — the guard must not break an
// ordinary same-origin redirect, and must still run a caller's own
// CheckRedirect without mutating the caller's client.
func TestRedirect_SameOriginStillFollowed(t *testing.T) {
	var (
		mu       sync.Mutex
		finalHit string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("hop") == "" {
			http.Redirect(w, r, r.URL.Path+"?hop=1", http.StatusFound)
			return
		}
		mu.Lock()
		finalHit = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(meEnvelope))
	}))
	t.Cleanup(ts.Close)

	callerPolicyRan := false
	callerPolicy := func(*http.Request, []*http.Request) error {
		callerPolicyRan = true
		return nil
	}
	hc := &http.Client{CheckRedirect: callerPolicy}
	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_test_xyz", HTTPClient: hc})
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if finalHit != "Bearer rek_test_xyz" {
		t.Errorf("Authorization after same-origin redirect = %q, want %q", finalHit, "Bearer rek_test_xyz")
	}
	if !callerPolicyRan {
		t.Error("caller-supplied CheckRedirect was not consulted")
	}
	callerPolicyRan = false
	_ = hc.CheckRedirect(nil, nil)
	if !callerPolicyRan {
		t.Error("New replaced the caller's CheckRedirect in place")
	}
}

type recordingTransport struct {
	mu    sync.Mutex
	calls int
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.calls++
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(meEnvelope)),
		Request:    r,
	}, nil
}

func (rt *recordingTransport) count() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.calls
}

// TestPlaintextBaseURL_WithAPIKey — a non-loopback http:// BaseURL
// carrying a key fails before any request leaves the process unless the
// caller opts in; anonymous plaintext calls are unaffected.
func TestPlaintextBaseURL_WithAPIKey(t *testing.T) {
	cases := []struct {
		name      string
		opts      client.Options
		wantErr   bool
		wantCalls int
	}{
		{"keyed plaintext refused", client.Options{APIKey: "rek_test_xyz"}, true, 0},
		{"keyed plaintext opted in", client.Options{APIKey: "rek_test_xyz", AllowInsecureHTTP: true}, false, 1},
		{"anonymous plaintext allowed", client.Options{}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &recordingTransport{}
			tc.opts.BaseURL = "http://api.example.test"
			tc.opts.HTTPClient = &http.Client{Transport: rt}
			_, err := client.New(tc.opts).Me(context.Background())
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "AllowInsecureHTTP")) {
				t.Errorf("err = %v, want a refusal naming AllowInsecureHTTP", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			if got := rt.count(); got != tc.wantCalls {
				t.Errorf("transport calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// createKeyCapture records what one POST /v1/account/keys delivered.
type createKeyCapture struct {
	header http.Header
	body   map[string]any
}

func captureCreateKey(t *testing.T, req client.CreateKeyRequest) createKeyCapture {
	t.Helper()
	var got createKeyCapture
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"data":{"key_id":"kid_new","label":"ci"},"as_of":"2026-04-28T10:00:00Z","flags":{}}`))
	}))
	t.Cleanup(ts.Close)
	c := client.New(client.Options{BaseURL: ts.URL, APIKey: "rek_caller"})
	if _, err := c.CreateKey(context.Background(), req); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	return got
}

// TestCreateKey_SendsIdempotencyKeyHeader: a retried CreateKey is only
// safe if the server can recognise it, so the SDK must carry the
// caller's key on the Idempotency-Key header (and never in the body).
func TestCreateKey_SendsIdempotencyKeyHeader(t *testing.T) {
	got := captureCreateKey(t, client.CreateKeyRequest{Label: "ci", IdempotencyKey: "mint-7f3a"})
	if v := got.header.Get("Idempotency-Key"); v != "mint-7f3a" {
		t.Errorf("server saw Idempotency-Key = %q, want mint-7f3a", v)
	}
	for k := range got.body {
		if k != "label" {
			t.Errorf("unexpected body member %q (header-only fields leaked into JSON): %+v", k, got.body)
		}
	}

	bare := captureCreateKey(t, client.CreateKeyRequest{Label: "ci"})
	if _, set := bare.header["Idempotency-Key"]; set {
		t.Error("Idempotency-Key sent although the request left it empty")
	}
}

// TestCreateKey_SendsReasonHeader: an operator-tier caller of
// POST /v1/account/keys is 400'd without X-Reason, so CreateKey must be
// able to send it — header only, never in the body.
func TestCreateKey_SendsReasonHeader(t *testing.T) {
	got := captureCreateKey(t, client.CreateKeyRequest{Label: "ci", Reason: "rotating staff credential per ticket 88"})
	if v := got.header.Get("X-Reason"); v != "rotating staff credential per ticket 88" {
		t.Errorf("server saw X-Reason = %q, want the request's Reason", v)
	}
	if _, leaked := got.body["reason"]; leaked {
		t.Errorf("reason leaked into the JSON body: %+v", got.body)
	}
	bare := captureCreateKey(t, client.CreateKeyRequest{Label: "ci"})
	if _, set := bare.header["X-Reason"]; set {
		t.Error("X-Reason sent although the request left it empty")
	}
}
