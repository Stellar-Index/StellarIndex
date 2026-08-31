package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the public Stellar Index endpoint. Override via
// [Options.BaseURL] for staging / self-hosted deployments.
const DefaultBaseURL = "https://api.stellarindex.io"

// DefaultTimeout is the per-request timeout applied when
// [Options.HTTPClient] is nil. It is a HARD wall-clock cap on the
// whole request — set as http.Client.Timeout — and is independent of
// the per-call context: a context deadline can only shorten a
// request, never extend it past this 30s. Hot-path calls (Price,
// Observations) are well under this. To permit a longer request
// (e.g. a wide-range History call), supply your own transport via
// [Options.HTTPClient] with a larger (or zero = unbounded) Timeout;
// raising the per-call context deadline alone has no effect.
const DefaultTimeout = 30 * time.Second

// userAgent is sent on every request so server-side telemetry can
// distinguish SDK callers from raw HTTP clients. Bump the version
// in tandem with the SDK module's tag.
//
// Bumped 0.1.0 -> 0.2.0 for the Client.Asset() breaking change
// (ADR-0042 LC-040: return type Envelope[AssetDetail] ->
// Envelope[AssetLookup]). This is the first time this constant has
// moved since the package was created — including through the
// Unit-D wire-collapse breaking change that already shipped
// (9442d311, 2026-06-16) — because no pkg/client/vX.Y.Z tag has ever
// actually been cut; see docs/architecture/semver-policy.md's
// tagging mechanics, which this change is the first real exercise of.
const userAgent = "stellarindex-go-sdk/0.2.0"

// Options configures a [Client] at construction.
type Options struct {
	// BaseURL is the API root. Trailing slash is stripped if
	// present. Defaults to [DefaultBaseURL].
	BaseURL string

	// APIKey is sent as `Authorization: Bearer <key>` on every
	// request when non-empty. Empty = anonymous (rate-limited
	// at the per-IP tier per the server's APIConfig).
	APIKey string

	// HTTPClient is the underlying transport. Non-nil callers
	// supply their own *http.Client to control timeouts, transport
	// pooling, instrumentation, etc. Nil falls through to a default
	// client with [DefaultTimeout].
	HTTPClient *http.Client

	// UserAgent overrides the SDK's User-Agent header. Empty leaves
	// the default. Useful for embedding the SDK in a higher-level
	// product that wants its own identifier surfaced server-side.
	UserAgent string
}

// Client is the entry point for every API call. Construct via
// [New] and reuse across goroutines.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	userAgent  string
}

// New constructs a [Client] from the supplied [Options]. Returns a
// usable client even with a zero Options (anonymous calls against
// the public endpoint).
func New(opts Options) *Client {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}

	ua := opts.UserAgent
	if ua == "" {
		ua = userAgent
	}

	return &Client{
		baseURL:    baseURL,
		apiKey:     opts.APIKey,
		httpClient: httpClient,
		userAgent:  ua,
	}
}

// doJSON performs an HTTP request against the server, decoding the
// response into out (which should be a *Envelope[T] pointer for the
// 200 path). Centralised here so every endpoint method gets the
// same auth header, user-agent, problem+json error decoding, and
// context propagation behaviour.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("client: parse url: %w", err)
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: marshal body: %w", err)
		}
		bodyReader = strings.NewReader(string(raw))
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("client: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap response read so a misbehaving server can't wedge the
	// caller. 16 MiB is far above any single envelope we serve.
	//
	// Read maxResponseBytes+1 and error on `>`, rather than reading
	// exactly the cap: LimitReader returns (n, nil) AT the limit, so a
	// truncated body was previously indistinguishable from a complete
	// one and got parsed as JSON — surfacing as a confusing decode error
	// about the payload rather than the truth, which is that the
	// response was too large (wave-D F-SDK-08). The +1 makes overshoot
	// detectable; same idiom as internal/stellarrpc.
	//
	// The message deliberately offers NO escape hatch. maxResponseBytes
	// is a function-local const applied to every request regardless of
	// transport, so no Options.HTTPClient can raise or remove it —
	// telling the caller otherwise would ship false remediation advice.
	const maxResponseBytes = 16 << 20
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("client: read response: %w", err)
	}
	if len(bodyBytes) > maxResponseBytes {
		return fmt.Errorf(
			"client: response body exceeds the %d-byte cap this SDK reads; "+
				"narrow the query (a smaller limit, or a shorter time range)",
			maxResponseBytes)
	}

	if resp.StatusCode >= 400 {
		return parseAPIError(
			resp.StatusCode,
			resp.Header.Get("Content-Type"),
			resp.Header.Get("Retry-After"),
			bodyBytes,
		)
	}

	if out != nil {
		if err := json.Unmarshal(bodyBytes, out); err != nil {
			return fmt.Errorf("client: decode response: %w", err)
		}
		// A well-formed but degenerate 2xx body ({}, {"data":null})
		// unmarshals without error into a zero Envelope, which would
		// otherwise surface to the caller as a silent zero-value
		// result. Enforce the one invariant every server envelope
		// carries — a populated as_of — so such a response fails loudly
		// instead. Endpoints that pass a non-Envelope out (or nil) skip
		// this via the type assertion.
		if v, ok := out.(interface{ validateEnvelope() error }); ok {
			if err := v.validateEnvelope(); err != nil {
				return fmt.Errorf("client: %s %s: %w", method, path, err)
			}
		}
	}
	return nil
}

// errEmptyJSON is returned by parseAPIError when the body is
// well-formed JSON but doesn't contain the expected problem+json
// fields. Internal — surfaced through APIError.Detail.
var errEmptyJSON = errors.New("server returned non-problem+json error body")

// errNotAnEnvelope is returned by doJSON when a 2xx body decodes
// without error but is missing the as_of timestamp every server
// envelope carries — i.e. a degenerate `{}` / `{"data":null}` that
// would otherwise yield a silent zero-value result. Reached via
// [Envelope.validateEnvelope].
var errNotAnEnvelope = errors.New("server returned a response without an as_of timestamp (not a valid envelope)")
