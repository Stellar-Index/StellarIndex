package stellarrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Stellar-Index/StellarIndex/internal/redact"
)

// MaxResponseBytes caps the bytes read per call (64 MiB) to bound memory if an upstream misbehaves; exceeding it is an error, not a silent truncation.
const MaxResponseBytes = 64 << 20

// Client is a JSON-RPC client for a single stellar-rpc endpoint, safe for concurrent use.
type Client struct {
	endpoint string
	http     *http.Client
	nextID   atomic.Int64
}

// Option configures a [Client] at construction time.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (timeouts, TLS, tracing).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithTimeout sets an overall request timeout, ignored if [WithHTTPClient] was already applied.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if c.http == nil {
			c.http = &http.Client{Timeout: d, Transport: newDefaultTransport()}
		}
	}
}

// New returns a client for endpoint with a 30 s default timeout.
func New(endpoint string, opts ...Option) *Client {
	c := &Client{endpoint: endpoint}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = &http.Client{
			Timeout:   30 * time.Second,
			Transport: newDefaultTransport(),
		}
	}
	return c
}

// newDefaultTransport returns an http.Transport for one RPC endpoint shared by many source
// goroutines. The stdlib MaxIdleConnsPerHost=2 would force a fresh TCP and TLS handshake on most
// calls under load, so it is raised to allow keep-alive reuse.
func newDefaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// Endpoint returns the URL the client talks to.
func (c *Client) Endpoint() string { return c.endpoint }

// HTTPStatusError is returned for any response status >= 400; classify with errors.As, never by message; Err is the *[JSONRPCError] when the body was an error envelope, else nil.
type HTTPStatusError struct {
	// Method is the JSON-RPC method that was called.
	Method string
	// StatusCode is the HTTP status, always >= 400.
	StatusCode int
	// Body is the response body, truncated for logging. Empty when the
	// response had no body.
	Body string
	// Err is the *[JSONRPCError] from the body's error envelope, or nil
	// when the body carried none.
	Err error
}

func (e *HTTPStatusError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("stellarrpc: %s: HTTP %d: %v", e.Method, e.StatusCode, e.Err)
	case e.Body == "":
		return fmt.Sprintf("stellarrpc: %s: HTTP %d (empty body)", e.Method, e.StatusCode)
	case e.Body[0] == '{':
		return fmt.Sprintf("stellarrpc: %s: HTTP %d (no JSON-RPC error envelope): %s", e.Method, e.StatusCode, e.Body)
	default:
		return fmt.Sprintf("stellarrpc: %s: HTTP %d: %s", e.Method, e.StatusCode, e.Body)
	}
}

// Unwrap returns the JSON-RPC error envelope the response carried, if any.
func (e *HTTPStatusError) Unwrap() error { return e.Err }

// ResponseDecodeError is returned when a status below 400 carries a body that is not a JSON-RPC envelope (empty, truncated, proxy HTML); per-response, so a retrying caller can tell it from a well-formed envelope that did not fit the target type.
type ResponseDecodeError struct {
	// Method is the JSON-RPC method that was called.
	Method string
	// Body is the response body, truncated for logging.
	Body string
	// Err is the underlying encoding/json error.
	Err error
}

func (e *ResponseDecodeError) Error() string {
	return fmt.Sprintf("stellarrpc: %s: decode: %v (body: %s)", e.Method, e.Err, e.Body)
}

// Unwrap returns the underlying encoding/json error.
func (e *ResponseDecodeError) Unwrap() error { return e.Err }

// redactedTransportError wraps an http.Client.Do error with the request URL scrubbed from its
// text. It still unwraps to the original, so callers classifying by type (e.g. soroswap retry via
// errors.As(err, &net.Error{})) keep working.
type redactedTransportError struct {
	msg string
	err error
}

func (e *redactedTransportError) Error() string { return e.msg }
func (e *redactedTransportError) Unwrap() error { return e.err }

// endpointFailure describes a request-construction failure without the
// endpoint's text: net/url's error quotes both the URL and the fragment
// it choked on, which for a key in the userinfo is part of the key.
func endpointFailure(endpoint string) string {
	if _, err := url.Parse(endpoint); err != nil {
		return "invalid endpoint: " + redact.ParseFailure(endpoint, err)
	}
	return "invalid request to " + redact.Endpoint(endpoint)
}

// newHTTPStatusError builds the error for a status >= 400. The body is decoded only to recover an
// error envelope, so an undecodable body never replaces the status with a decode error.
func newHTTPStatusError(method string, status int, body []byte) *HTTPStatusError {
	e := &HTTPStatusError{Method: method, StatusCode: status, Body: truncate(string(body), 256)}
	var envelope jsonrpcResponse
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		e.Err = envelope.Error
	}
	return e
}

// call is the low-level JSON-RPC round-trip. An error envelope with status below 400 returns *[JSONRPCError]; status >= 400 returns
// *[HTTPStatusError] (wrapping the *[JSONRPCError] when present); an undecodable body below 400 returns *[ResponseDecodeError].
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	req := jsonrpcRequest{Version: "2.0", ID: int(id), Method: method, Params: params}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("stellarrpc: marshal %s: %w", method, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return &redactedTransportError{
			msg: fmt.Sprintf("stellarrpc: new request %s: %s", method, endpointFailure(c.endpoint)),
			err: err,
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Identifiable User-Agent so stellar-rpc operators can correlate
	// traffic in their logs (mirrors internal/metadata/sep1.go).
	httpReq.Header.Set("User-Agent", "stellar-index/stellarrpc (+https://stellarindex.io)")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// An operator may point rpc_endpoints at a keyed third-party provider, and a raw %w wrap would
		// log the *url.Error's full URL, key included. Scrub it with the shared RedactURLError but
		// keep unwrapping to the real error for retry classification.
		return &redactedTransportError{
			msg: fmt.Sprintf("stellarrpc: %s: %s", method, redact.URLError(err, c.endpoint)),
			err: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	// Read the full body so JSON-level errors show even on non-200, capped so a runaway upstream
	// cannot OOM us; the +1 detects overshoot.
	limited := io.LimitReader(resp.Body, MaxResponseBytes+1)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("stellarrpc: %s: read body: %w", method, err)
	}
	if int64(len(respBody)) > MaxResponseBytes {
		return fmt.Errorf("stellarrpc: %s: response exceeded %d bytes (upstream misbehaving?)",
			method, MaxResponseBytes)
	}

	// A status >= 400 is a failure whatever the body is. Decide it before decoding the body so the
	// status is never lost to a decode error or replaced by the envelope's own code.
	if resp.StatusCode >= 400 {
		return newHTTPStatusError(method, resp.StatusCode, respBody)
	}

	var envelope jsonrpcResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return &ResponseDecodeError{Method: method, Body: truncate(string(respBody), 256), Err: err}
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("stellarrpc: %s: decode result: %w", method, err)
		}
	}
	return nil
}

// truncate cuts `s` to at most `n` bytes plus a trailing "…", walking back to a UTF-8 rune
// boundary so multi-byte codepoints in HTTP response bodies are not sliced into invalid UTF-8
// in logs and error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

// ─── Public methods ────────────────────────────────────────────────

// Health calls getHealth; a healthy stale node returns a JSON-RPC error envelope rather than status=stale, so callers must handle both paths.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	err := c.call(ctx, "getHealth", nil, &h)
	return &h, err
}

// LatestLedger calls getLatestLedger.
func (c *Client) LatestLedger(ctx context.Context) (*LatestLedger, error) {
	var l LatestLedger
	return &l, c.call(ctx, "getLatestLedger", nil, &l)
}

// LatestLedgerSequence returns the latest ledger sequence; getEvents rejects startLedger=0, so sources must seed a real startLedger from this.
func (c *Client) LatestLedgerSequence(ctx context.Context) (uint32, error) {
	l, err := c.LatestLedger(ctx)
	if err != nil {
		return 0, err
	}
	return l.Sequence, nil
}

// Network calls getNetwork.
func (c *Client) Network(ctx context.Context) (*Network, error) {
	var n Network
	return &n, c.call(ctx, "getNetwork", nil, &n)
}

// VersionInfo calls getVersionInfo.
func (c *Client) VersionInfo(ctx context.Context) (*VersionInfo, error) {
	var v VersionInfo
	return &v, c.call(ctx, "getVersionInfo", nil, &v)
}

// FeeStats calls getFeeStats.
func (c *Client) FeeStats(ctx context.Context) (*FeeStats, error) {
	var f FeeStats
	return &f, c.call(ctx, "getFeeStats", nil, &f)
}

// GetEvents calls getEvents (nil pagination for server defaults); the response is sanity-checked so inconsistent ledger bounds or out-of-order events error here instead of silently corrupting ingestion.
func (c *Client) GetEvents(ctx context.Context, startLedger, endLedger uint32, filters []EventFilter, pag *Pagination) (*EventsResponse, error) {
	p := eventsParams{StartLedger: startLedger, EndLedger: endLedger, Filters: filters, Pagination: pag}
	var r EventsResponse
	if err := c.call(ctx, "getEvents", p, &r); err != nil {
		return nil, err
	}
	if err := r.sanityCheck(); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetLedgers calls getLedgers.
func (c *Client) GetLedgers(ctx context.Context, startLedger uint32, pag *Pagination) (*LedgersResponse, error) {
	p := ledgersParams{StartLedger: startLedger, Pagination: pag}
	var r LedgersResponse
	return &r, c.call(ctx, "getLedgers", p, &r)
}

// GetTransaction calls getTransaction with the hex envelope hash (no "0x"); a tx outside the retention window returns status=NOT_FOUND, not an error, so callers must branch on Status.
func (c *Client) GetTransaction(ctx context.Context, hash string) (*TransactionResponse, error) {
	var r TransactionResponse
	return &r, c.call(ctx, "getTransaction", transactionParams{Hash: hash}, &r)
}

// GetTransactions calls getTransactions; startLedger=0 uses the pagination cursor only, which is an opaque value passed through from a prior response.
func (c *Client) GetTransactions(ctx context.Context, startLedger uint32, pag *Pagination) (*TransactionsResponse, error) {
	p := transactionsParams{StartLedger: startLedger, Pagination: pag}
	var r TransactionsResponse
	return &r, c.call(ctx, "getTransactions", p, &r)
}

// SimulateTransaction calls simulateTransaction with a base64 TransactionEnvelope (unsigned is fine, build via [InvokeContractTxEnvelope]); the response is verbatim, so Error non-empty means the contract call failed while a nil Go error only means the round-trip succeeded.
func (c *Client) SimulateTransaction(ctx context.Context, txEnvelope string) (*SimulationResponse, error) {
	p := simulateParams{Transaction: txEnvelope}
	var r SimulationResponse
	return &r, c.call(ctx, "simulateTransaction", p, &r)
}
