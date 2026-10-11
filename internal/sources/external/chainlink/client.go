package chainlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Client is a minimal Ethereum JSON-RPC client for eth_call (latestRoundData), eth_blockNumber
// (backfill range probe) and eth_getLogs (backfill walk).
//
// It is deliberately tiny: go-ethereum's dependency tree is heavy and we need three methods, two
// ABI shapes and no signing. Stateless and goroutine-safe: each method does one HTTP round-trip;
// connection pooling is configured via http.Client.Transport.
type Client struct {
	HTTPClient *http.Client
	Endpoint   string

	// id is a monotonic JSON-RPC request id, useful for log correlation.
	id atomic.Uint64
}

// NewClient constructs a Client. An empty endpoint falls back to [DefaultEndpoint]; a nil
// http.Client falls back to a 30s timeout, generous because eth_getLogs over 5k blocks can take
// seconds under load and timing out mid-call is worse than waiting.
func NewClient(endpoint string, httpClient *http.Client) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		HTTPClient: httpClient,
		Endpoint:   strings.TrimRight(endpoint, "/"),
	}
}

// jsonRPCResponse is the lowest-common-denominator success/error
// envelope. Specific RPC methods unmarshal `Result` further.
type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// EthCall performs eth_call against `to` with raw 0x-prefixed `data` (selector + ABI args) at
// `block` (a tag or hex block number) and returns the raw hex result for the caller to decode.
// It returns ErrEmptyResult for `0x` (reverted call or missing contract) and ErrMalformedResult
// when the JSON-RPC layer succeeded but the response shape is wrong.
func (c *Client) EthCall(ctx context.Context, to, data, block string) (string, error) {
	if block == "" {
		block = "latest"
	}
	params := []any{
		map[string]string{"to": to, "data": data},
		block,
	}
	var resultHex string
	if err := c.do(ctx, "eth_call", params, &resultHex); err != nil {
		return "", err
	}
	if resultHex == "" || resultHex == "0x" {
		return "", fmt.Errorf("%w: to=%s data=%s", ErrEmptyResult, to, data)
	}
	return resultHex, nil
}

// EthBlockNumber returns the current head block as a uint64. Used
// by the backfill harness to bound its eth_getLogs walk at "now".
func (c *Client) EthBlockNumber(ctx context.Context) (uint64, error) {
	var resultHex string
	if err := c.do(ctx, "eth_blockNumber", []any{}, &resultHex); err != nil {
		return 0, err
	}
	n, err := parseHexUint(resultHex)
	if err != nil {
		return 0, fmt.Errorf("%w: eth_blockNumber: %w", ErrMalformedResult, err)
	}
	return n, nil
}

// LogEntry is the eth_getLogs response shape we care about for
// AnswerUpdated decoding. Other fields (logIndex, removed, etc)
// are present in the JSON but ignored by us.
type LogEntry struct {
	Address     string   `json:"address"`     // 0x-prefixed feed contract
	Topics      []string `json:"topics"`      // [topic0, ...indexed]
	Data        string   `json:"data"`        // 0x-prefixed hex of non-indexed args
	BlockNumber string   `json:"blockNumber"` // 0x-prefixed hex
	TxHash      string   `json:"transactionHash"`
	TxIndex     string   `json:"transactionIndex"`
	LogIndex    string   `json:"logIndex"`
}

// EthGetLogs performs eth_getLogs for `addresses` and the `topics` filter (a topic hash or nil
// wildcard per entry) over the inclusive `fromBlock`..`toBlock` range.
//
// Providers cap range and response size (Alchemy free: 10MB / 10k logs), so callers chunk
// historical walks by 5k blocks.
func (c *Client) EthGetLogs(
	ctx context.Context,
	addresses []string,
	topics []any, // [topic0_hex, topic1_or_nil, ...]
	fromBlock, toBlock uint64,
) ([]LogEntry, error) {
	filter := map[string]any{
		"fromBlock": fmt.Sprintf("0x%x", fromBlock),
		"toBlock":   fmt.Sprintf("0x%x", toBlock),
	}
	if len(addresses) > 0 {
		filter["address"] = addresses
	}
	if len(topics) > 0 {
		filter["topics"] = topics
	}
	var logs []LogEntry
	if err := c.do(ctx, "eth_getLogs", []any{filter}, &logs); err != nil {
		return nil, err
	}
	return logs, nil
}

// maxRPCBodyBytes caps a JSON-RPC response read. Providers cap
// eth_getLogs near 10 MB / 10k logs; the headroom covers envelope
// overhead while stopping a wedged endpoint from OOMing the ingester.
const maxRPCBodyBytes = 32 << 20

// do is the one-shot HTTP wrapper shared by every RPC method.
// Marshals the JSON-RPC request, posts, decodes the response, and
// unmarshals `result` into `out`.
func (c *Client) do(ctx context.Context, method string, params []any, out any) error {
	id := c.id.Add(1)
	reqBody, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("chainlink: marshal %s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("chainlink: new request %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := DoWithoutRedirects(c.HTTPClient, req)
	if err != nil {
		// A *url.Error embeds the full request URL, and keyed RPC providers carry the API key in
		// the path, so redact before wrapping to keep the key out of logs.
		return fmt.Errorf("chainlink: %s transport: %w", method, NewRedactedTransportError(err, c.Endpoint))
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRPCBodyBytes+1))
	if err != nil {
		return fmt.Errorf("chainlink: %s read body: %w", method, err)
	}
	if int64(len(body)) > maxRPCBodyBytes {
		return fmt.Errorf("chainlink: %s body exceeds %d bytes", method, maxRPCBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// Bound the snippet so a giant HTML error page doesn't
		// flood logs. 256 bytes is enough to identify "Cloudflare
		// rate-limited us" vs "Alchemy returned 401" without
		// turning a single failed call into 50KB of journal output.
		snippet := string(body)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "…"
		}
		return fmt.Errorf("chainlink: %s status %d: %s", method, resp.StatusCode, snippet)
	}

	var env jsonRPCResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("chainlink: %s decode: %w", method, err)
	}
	if env.Error != nil {
		return fmt.Errorf("chainlink: %s rpc error %d: %s", method, env.Error.Code, env.Error.Message)
	}
	if len(env.Result) == 0 || string(env.Result) == "null" {
		return fmt.Errorf("%w: %s returned null", ErrEmptyResult, method)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("chainlink: %s unmarshal result: %w", method, err)
	}
	return nil
}

// ErrRedirectRefused is the transport error for an RPC reply that tries
// to redirect: the endpoint URL may carry an API key, and Go re-sends
// the full previous URL as the next hop's Referer.
var ErrRedirectRefused = errors.New("redirect refused: rpc endpoint URL may carry an API key")

// DoWithoutRedirects sends req through a shallow copy of c that never
// follows a redirect; c itself (timeout, transport) is left untouched.
func DoWithoutRedirects(c *http.Client, req *http.Request) (*http.Response, error) {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrRedirectRefused }
	return cp.Do(req) //nolint:gosec // G704: both callers build req.URL from an operator-configured RPC endpoint (Client.Endpoint / CHAINLINK_RPC_URL), not request-tainted input
}

// RedactURLError converts a transport error into a string with the secret-bearing URL scrubbed.
// Keyed RPC providers embed the API key in the endpoint path, so a *url.Error's default Error()
// would leak it; the URL is replaced with a host-only, path-redacted form. Non-*url.Error inputs
// carry no request URL and are returned unchanged.
//
// Exported because internal/divergence's ChainlinkReference reads the same CHAINLINK_RPC_URL and
// must scrub it identically; one implementation avoids a second copy drifting out of coverage.
func RedactURLError(err error, endpoint string) string {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err.Error()
	}
	// Keep the original "<op> <url>: <underlying>" shape, minus the secret.
	return fmt.Sprintf("%s %q: %v", ue.Op, RedactEndpoint(endpoint), ue.Err)
}

// RedactedTransportError pairs a URL-redacted message with the original transport error. Unwrap
// keeps errors.Is/As reaching context.DeadlineExceeded, net.Error and the *url.Error; formatting
// RedactURLError into a new error with %s would turn err into a leaf and lose that classification.
type RedactedTransportError struct {
	msg string
	err error
}

func (e *RedactedTransportError) Error() string { return e.msg }
func (e *RedactedTransportError) Unwrap() error { return e.err }

// NewRedactedTransportError builds a [RedactedTransportError], redacting endpoint via
// [RedactURLError] for the message while keeping err reachable through Unwrap.
func NewRedactedTransportError(err error, endpoint string) *RedactedTransportError {
	return &RedactedTransportError{msg: RedactURLError(err, endpoint), err: err}
}

// RedactEndpoint returns a log-safe RPC endpoint: scheme and host only, with the path/query (where
// keyed providers stash the API key) replaced by "/<redacted>". It never echoes an unparseable
// input, which might be the secret. Exported for non-transport error paths that name the endpoint.
func RedactEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "[redacted-endpoint]"
	}
	return u.Scheme + "://" + u.Host + "/<redacted>"
}
