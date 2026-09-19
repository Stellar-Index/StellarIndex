package stellarrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// MaxResponseBytes caps how much we will read off the wire per call.
// stellar-rpc's getEvents / getTransactions / getLedgers can return
// several MB on a big page; 64 MiB leaves comfortable headroom while
// bounding memory if an upstream proxy ever misbehaves (returns a
// streaming error page, gets man-in-the-middled by a captive portal,
// etc.). Exceeding the cap is an error, not a silent truncation.
const MaxResponseBytes = 64 << 20

// Client is a JSON-RPC client for a single stellar-rpc endpoint.
// Safe for concurrent use.
type Client struct {
	endpoint string
	http     *http.Client
	nextID   atomic.Int64
}

// Option configures a [Client] at construction time.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client (useful for
// custom timeouts, TLS configs, transport-level tracing).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithTimeout sets an overall request timeout. Ignored if
// [WithHTTPClient] has already been applied.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		if c.http == nil {
			c.http = &http.Client{Timeout: d, Transport: newDefaultTransport()}
		}
	}
}

// New returns a client pointing at endpoint (e.g.
// "http://localhost:8000"). Default timeout: 30 s.
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

// newDefaultTransport returns an http.Transport tuned for the
// indexer's access pattern: one RPC endpoint shared by many source
// goroutines. The stdlib default MaxIdleConnsPerHost=2 forces
// most sources to dial fresh TCP on every call, which burns
// connection setup + TLS handshake cost 100×/s under load. Raising
// it lets keep-alive reuse kick in the way it's supposed to.
func newDefaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 200
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// Endpoint returns the URL the client talks to.
func (c *Client) Endpoint() string { return c.endpoint }

// HTTPStatusError is returned by every call whose HTTP response carried
// a status >= 400, whatever the body looked like: empty, an HTML or
// plain-text proxy page, JSON that is not a JSON-RPC envelope, or a
// JSON-RPC error envelope. The status is the one thing a caller needs
// to tell "the endpoint is rate limiting or briefly unwell" from "this
// request is wrong", and before this type existed it was dropped on
// the empty-body and error-envelope paths and only present as text on
// the others — so classify with errors.As on this type, never by
// matching the message.
//
// When the body was a JSON-RPC error envelope, Err is the
// *[JSONRPCError] and Unwrap exposes it, so errors.As for
// *[JSONRPCError] keeps working on a non-2xx response. Otherwise Err
// is nil.
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

// ResponseDecodeError is returned when a response with a status below
// 400 had a body that does not decode as a JSON-RPC envelope: empty,
// cut short, or a proxy's HTML interstitial served with a 200. It is a
// property of that one response, not of the request, which is why it
// is a type of its own — a caller that retries can tell it from a
// result that decoded as an envelope but did not fit the target type
// (that one stays a plain error: re-asking returns the same shape).
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

// newHTTPStatusError builds the error for a status >= 400. The body is
// decoded on a best-effort basis only to recover an error envelope; a
// body that is empty, not JSON, or JSON of another shape still yields
// an *HTTPStatusError, never a decode error that loses the status.
func newHTTPStatusError(method string, status int, body []byte) *HTTPStatusError {
	e := &HTTPStatusError{Method: method, StatusCode: status, Body: truncate(string(body), 256)}
	var envelope jsonrpcResponse
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != nil {
		e.Err = envelope.Error
	}
	return e
}

// call is the low-level JSON-RPC round-trip. Callers unmarshal the
// result into their own target. If the remote returned an error
// envelope with a status below 400, call returns the *[JSONRPCError].
// Any status >= 400 returns an *[HTTPStatusError] (wrapping the
// *[JSONRPCError] when the body carried one); an undecodable body with
// a status below 400 returns a *[ResponseDecodeError].
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	req := jsonrpcRequest{Version: "2.0", ID: int(id), Method: method, Params: params}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("stellarrpc: marshal %s: %w", method, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("stellarrpc: new request %s: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	// Identifiable User-Agent so stellar-rpc operators can correlate
	// traffic in their logs (mirrors internal/metadata/sep1.go).
	httpReq.Header.Set("User-Agent", "stellar-index/stellarrpc (+https://stellarindex.io)")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("stellarrpc: %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read full body so callers see JSON-level errors even on !=200,
	// but cap the read so a runaway upstream can't OOM us. The +1
	// lets us detect overshoot cleanly — if we read MaxResponseBytes+1
	// bytes, the stream wasn't done.
	limited := io.LimitReader(resp.Body, MaxResponseBytes+1)
	respBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("stellarrpc: %s: read body: %w", method, err)
	}
	if int64(len(respBody)) > MaxResponseBytes {
		return fmt.Errorf("stellarrpc: %s: response exceeded %d bytes (upstream misbehaving?)",
			method, MaxResponseBytes)
	}

	// A status >= 400 is a failure whatever the body is — an upstream
	// proxy's HTML page, nothing at all, JSON that happens to parse, or
	// a JSON-RPC error envelope — and the caller must not be able to
	// treat it as success. Decided BEFORE the body is decoded so the
	// status is never lost to a decode error or replaced by the
	// envelope's own code.
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

// truncate cuts `s` to at most `n` bytes plus a trailing "…",
// walking back to the nearest UTF-8 rune boundary at or before
// byte n so multi-byte codepoints aren't sliced in half. Used
// for log + error messages where the input is typically an HTTP
// response body — those routinely contain UTF-8 (vendor error
// pages, JSON-with-unicode), and a naive byte slice produced
// invalid UTF-8 in journalctl + Loki output.
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

// Health calls getHealth. Note: a healthy stale node returns a
// JSON-RPC error envelope rather than a 200 with status=stale —
// callers should handle both paths.
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

// LatestLedgerSequence is a convenience wrapper that returns just
// the ledger sequence number. Source implementations seed their
// startLedger from this on first poll — stellar-rpc's getEvents
// rejects startLedger=0, so sources MUST pick a real number before
// their first subscription.
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

// GetEvents calls getEvents with the given filters + pagination.
// Pass nil for pagination to use server defaults.
//
// The response is sanity-checked (see EventsResponse.sanityCheck)
// before being returned — a node serving inconsistent ledger
// bounds or out-of-order events surfaces as an error here, not as
// a silent ingestion bug downstream.
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

// GetTransaction calls getTransaction.
//
// hash is the tx envelope hash as a hex string (no "0x" prefix).
// Returns status=NOT_FOUND when the tx is outside the RPC node's
// retention window — NOT an error. Callers should branch on Status
// rather than relying on error to signal "not found".
func (c *Client) GetTransaction(ctx context.Context, hash string) (*TransactionResponse, error) {
	var r TransactionResponse
	return &r, c.call(ctx, "getTransaction", transactionParams{Hash: hash}, &r)
}

// GetTransactions calls getTransactions (paginated batch lookup).
//
// startLedger=0 uses the pagination cursor only. Cursor format is
// an opaque stellar-rpc value — pass through from a prior response.
func (c *Client) GetTransactions(ctx context.Context, startLedger uint32, pag *Pagination) (*TransactionsResponse, error) {
	p := transactionsParams{StartLedger: startLedger, Pagination: pag}
	var r TransactionsResponse
	return &r, c.call(ctx, "getTransactions", p, &r)
}

// SimulateTransaction calls simulateTransaction.
//
// txEnvelope is a base64-encoded xdr.TransactionEnvelope. For read-
// only Soroban view calls (all_pairs_length, token_0, etc.) the
// envelope is unsigned — stellar-rpc doesn't validate signatures
// during simulation. Build one with [InvokeContractTxEnvelope].
//
// Returns the SimulationResponse verbatim; callers inspect
// response.Results[0].XDR (base64 SCVal) for the function return
// value. response.Error is non-empty when the contract call itself
// failed (e.g. panicking, out-of-gas); a nil Go error from this
// method only means "the RPC round-trip succeeded."
func (c *Client) SimulateTransaction(ctx context.Context, txEnvelope string) (*SimulationResponse, error) {
	p := simulateParams{Transaction: txEnvelope}
	var r SimulationResponse
	return &r, c.call(ctx, "simulateTransaction", p, &r)
}
