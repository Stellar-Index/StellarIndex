package divergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
)

// This file cross-checks the served `circulating_supply` against external references, mirroring
// the price path in worker.go. When every reference is dark or stale it degrades to a non-paging
// `no_reference` outcome, not a false divergence alert. See docs/methodology/xlm-circulating-supply.md.

// Supply-check sentinel errors. ErrAssetUnsupported (reference.go) is
// reused for "this reference doesn't publish the asset"; the two below
// are supply-specific.
var (
	// ErrSupplyUnavailable means the reference supports the asset but has no usable circulating
	// figure right now (outage, 429, malformed body): a transient miss, like ErrPriceUnavailable.
	ErrSupplyUnavailable = errors.New("divergence: circulating supply unavailable")

	// ErrNoServedSupply: our served tier has no circulating snapshot yet (bootstrap), so nothing to compare; recorded as `refresh_error`, not a divergence.
	ErrNoServedSupply = errors.New("divergence: no served circulating supply for asset")
)

// SupplyReference is a pluggable external circulating-supply source, the analogue of Reference; implementations MUST be safe for concurrent LookupCirculatingSupply calls and SHOULD return ErrAssetUnsupported for an unpublished asset.
type SupplyReference interface {
	// Name returns a stable, lowercase, hyphenated label for the Prometheus `reference` label and
	// JSON keys (e.g. "coingecko"). Renaming is a wire break against the supply-divergence alert.
	Name() string

	// LookupCirculatingSupply returns the supply in WHOLE-TOKEN units (NOT minor units / stroops); [ErrAssetUnsupported] if unpublished, [ErrSupplyUnavailable] on a transient outage.
	LookupCirculatingSupply(ctx context.Context, asset canonical.Asset) (float64, error)
}

// ServedSupply is the narrow view of our own served circulating-supply snapshot the cross-check consumes.
type ServedSupply struct {
	// Circulating is the served figure in MINOR units (stroops for XLM
	// at 7 dp; per-asset decimals otherwise) — the *big.Int form the
	// supply pipeline stores, never truncated (ADR-0003).
	Circulating *big.Int
	// LedgerSequence + ObservedAt carry the snapshot's provenance for
	// logging; the check itself only reads Circulating.
	LedgerSequence uint32
	ObservedAt     time.Time
}

// ServedSupplyReader is the read seam onto OUR served circulating supply (production: timescale LatestSupply); package-local so divergence stays decoupled from the supply pipeline.
type ServedSupplyReader interface {
	// LatestCirculatingSupply returns the latest snapshot for a supply.AssetKey ("XLM" for native). Implementations MUST return a wrapped
	// [ErrNoServedSupply] when none exists, so bootstrap is distinguishable from a read failure.
	LatestCirculatingSupply(ctx context.Context, assetKey string) (ServedSupply, error)
}

// SupplyCheck binds one flagship asset to the identifiers each side of the comparison needs; the service iterates a fixed, operator-bounded slice of these.
type SupplyCheck struct {
	// Asset is the canonical asset passed to each [SupplyReference].
	Asset canonical.Asset
	// AssetKey is the supply.AssetKey form the [ServedSupplyReader]
	// reads by ("XLM" for native lumens).
	AssetKey string
	// Decimals is the minor-unit scale used to convert the served
	// *big.Int (stroops) to whole tokens for comparison (7 for XLM).
	Decimals int
	// Label is the stable value stamped on the metric `asset` label.
	// Uses the canonical wire form ("native") so dashboards join
	// against the same vocabulary the API uses.
	Label string
}

// SupplyOutcomeKind is the per-(asset, tick) outcome, used directly as the `outcome` Prometheus label, mirroring the price path's vocabulary.
type SupplyOutcomeKind string

const (
	// SupplyOutcomeOK — our served figure agreed with every responding
	// reference within the threshold.
	SupplyOutcomeOK SupplyOutcomeKind = "ok"
	// SupplyOutcomeDivergent — at least one responding reference
	// disagreed by more than the threshold. The ratio gauge carries the
	// magnitude; the supply-divergence alert fires off the gauge.
	SupplyOutcomeDivergent SupplyOutcomeKind = "divergent"
	// SupplyOutcomeNoReference: our figure loaded but every reference was unreachable or didn't publish the asset. Not a divergence and deliberately not paged.
	SupplyOutcomeNoReference SupplyOutcomeKind = "no_reference"
	// SupplyOutcomeRefreshError — OUR served snapshot couldn't be read
	// (bootstrap, storage error). Nothing to compare against.
	SupplyOutcomeRefreshError SupplyOutcomeKind = "refresh_error"
)

// SupplyEmitter is the metric-emission seam, keeping the service Prometheus-agnostic; wiring lives in cmd/stellarindex-aggregator/main.go so this package has no obs dependency.
type SupplyEmitter interface {
	// Ratio sets the per-(asset, reference) gauge to the absolute
	// relative divergence |our − ref| / ref. Called once per responding
	// reference.
	Ratio(asset, reference string, ratio float64)
	// Outcome increments the per-outcome counter. Called exactly once
	// per (asset, tick) with the aggregate outcome.
	Outcome(kind SupplyOutcomeKind)
	// Duration records the per-(asset, tick) evaluation latency under
	// the aggregate outcome label.
	Duration(kind SupplyOutcomeKind, seconds float64)
}

// SupplyServiceOptions configures a [SupplyService].
type SupplyServiceOptions struct {
	// Checks is the flagship asset set to cross-check. Empty falls back
	// to [DefaultSupplyChecks] (XLM only).
	Checks []SupplyCheck
	// References is the external reference set. Empty is rejected by
	// NewSupplyService — the aggregator gates on
	// `[divergence.supply].enabled` + at least one reference before
	// wiring, so a dark service is never constructed.
	References []SupplyReference
	// Reader is the served-tier read seam. Required.
	Reader ServedSupplyReader
	// Emitter is the metric seam. Required.
	Emitter SupplyEmitter
	// Threshold is the relative-divergence ratio above which a
	// reference is "divergent" (0.01 = 1%). <= 0 falls back to
	// [DefaultSupplyThreshold].
	Threshold float64
	// PerReferenceTimeout bounds each reference lookup. <= 0 falls back
	// to 10s (supply endpoints are slower + rarer than price ones).
	PerReferenceTimeout time.Duration
	// Logger receives per-tick diagnostics. Required (nil-checked).
	Logger *slog.Logger
	// NowFn overrides time.Now for deterministic tests.
	NowFn func() time.Time
}

// DefaultSupplyMaxAge is the staleness ceiling for a supply reference's upstream publication time; deliberately generous (24h), since supply drifts glacially and the gate only catches a FROZEN upstream.
const DefaultSupplyMaxAge = 24 * time.Hour

// supplyStaleness gates a reference against its own injected clock ([SupplyReference] carries no comparison time). A zero publishedAt
// fails closed: an unverifiable figure is excluded, never served as fresh.
func supplyStaleness(now func() time.Time, maxAge time.Duration, refName string, publishedAt time.Time) error {
	if publishedAt.IsZero() {
		return fmt.Errorf("%w: %s returned no parseable publication time (freshness unverifiable)",
			ErrSupplyUnavailable, refName)
	}
	if maxAge <= 0 {
		maxAge = DefaultSupplyMaxAge
	}
	asOf := now().UTC()
	if age := asOf.Sub(publishedAt); age > maxAge {
		return fmt.Errorf("%w: %s supply figure is stale (published %s ago, max %s)",
			ErrSupplyUnavailable, refName, age.Truncate(time.Second), maxAge)
	}
	return nil
}

// DefaultSupplyThreshold is the 1% relative-divergence ceiling, far above the ~0.03% XLM fee-pool noise floor (docs/methodology/xlm-circulating-supply.md), so only a real drift alerts.
const DefaultSupplyThreshold = 0.01

// DefaultSupplyPerReferenceTimeout bounds each supply-reference HTTP call when the caller sets none (supply endpoints are slower than price ones).
const DefaultSupplyPerReferenceTimeout = 10 * time.Second

// SupplyService runs one supply cross-check cycle per [Tick]: load our figure, fan out to references, emit ratio, outcome and duration; safe for concurrent Tick with concurrent-safe dependencies.
type SupplyService struct {
	checks    []SupplyCheck
	refs      []SupplyReference
	reader    ServedSupplyReader
	emitter   SupplyEmitter
	threshold float64
	timeout   time.Duration
	logger    *slog.Logger
	nowFn     func() time.Time
}

// NewSupplyService errors on a missing required option or no references; the caller should not wire a service with nothing to compare (it would emit only `no_reference`).
func NewSupplyService(opts SupplyServiceOptions) (*SupplyService, error) {
	if opts.Reader == nil {
		return nil, errors.New("divergence: supply service Reader is required")
	}
	if opts.Emitter == nil {
		return nil, errors.New("divergence: supply service Emitter is required")
	}
	if opts.Logger == nil {
		return nil, errors.New("divergence: supply service Logger is required")
	}
	if len(opts.References) == 0 {
		return nil, errors.New("divergence: supply service needs at least one reference")
	}
	checks := opts.Checks
	if len(checks) == 0 {
		checks = DefaultSupplyChecks()
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = DefaultSupplyThreshold
	}
	timeout := opts.PerReferenceTimeout
	if timeout <= 0 {
		timeout = DefaultSupplyPerReferenceTimeout
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &SupplyService{
		checks:    checks,
		refs:      opts.References,
		reader:    opts.Reader,
		emitter:   opts.Emitter,
		threshold: threshold,
		timeout:   timeout,
		logger:    opts.Logger,
		nowFn:     nowFn,
	}, nil
}

// DefaultSupplyChecks is the built-in flagship set: XLM only, the one figure the Stellar Dashboard covers authoritatively.
func DefaultSupplyChecks() []SupplyCheck {
	return []SupplyCheck{
		{
			Asset:    canonical.NativeAsset(),
			AssetKey: "XLM", // supply.AssetKey(native) — see internal/supply/xlm.go
			Decimals: 7,     // XLM stroops
			Label:    "native",
		},
	}
}

// Tick runs one cross-check cycle across every configured check; per-check failures are isolated (logged and counted) so one asset's hiccup doesn't drop the rest.
func (s *SupplyService) Tick(ctx context.Context) {
	for _, c := range s.checks {
		if ctx.Err() != nil {
			return
		}
		s.tickOne(ctx, c)
	}
}

// tickOne evaluates a single check and emits exactly one outcome +
// duration for it, plus one ratio gauge per responding reference.
func (s *SupplyService) tickOne(ctx context.Context, c SupplyCheck) {
	start := s.nowFn()
	emit := func(kind SupplyOutcomeKind) {
		s.emitter.Outcome(kind)
		s.emitter.Duration(kind, s.nowFn().Sub(start).Seconds())
	}

	served, err := s.reader.LatestCirculatingSupply(ctx, c.AssetKey)
	if err != nil {
		// Bootstrap (no snapshot yet) and real read errors both mean "nothing to compare"
		// (refresh_error); bootstrap logs at debug (expected on a cold deploy), a real error at warn.
		if errors.Is(err, ErrNoServedSupply) {
			s.logger.Debug("supply divergence: no served snapshot yet",
				"asset", c.Label, "asset_key", c.AssetKey)
		} else {
			s.logger.Warn("supply divergence: served-supply read failed",
				"asset", c.Label, "asset_key", c.AssetKey, "err", err)
		}
		emit(SupplyOutcomeRefreshError)
		return
	}
	ourTokens, ok := scaleServedSupply(served.Circulating, c.Decimals)
	if !ok {
		s.logger.Warn("supply divergence: served circulating is non-positive or unscalable",
			"asset", c.Label, "asset_key", c.AssetKey)
		emit(SupplyOutcomeRefreshError)
		return
	}

	responded := 0
	divergent := false
	for _, ref := range s.refs {
		refVal, rerr := s.lookupOne(ctx, ref, c.Asset)
		if errors.Is(rerr, ErrAssetUnsupported) {
			// Reference doesn't publish this asset — no contribution,
			// not a failure (information for the operator).
			continue
		}
		if rerr != nil {
			// Transient miss (429, timeout, malformed body). Debug-level: the aggregate
			// no_reference/ok outcome carries the operator signal, and a per-reference warn would be
			// noisy on the CoinGecko-429 path.
			s.logger.Debug("supply divergence: reference lookup failed",
				"asset", c.Label, "reference", ref.Name(), "err", rerr)
			continue
		}
		if !isFinitePositive(refVal) {
			continue
		}
		responded++
		ratio := absFloat(ourTokens-refVal) / refVal
		s.emitter.Ratio(c.Label, ref.Name(), ratio)
		if ratio > s.threshold {
			divergent = true
			s.logger.Warn("supply divergence: over threshold",
				"asset", c.Label, "reference", ref.Name(),
				"our", ourTokens, "reference_value", refVal,
				"ratio", ratio, "threshold", s.threshold)
		}
	}

	switch {
	case responded == 0:
		// Served figure loaded but no reference answered — graceful
		// degrade, NOT a divergence. Deliberately not paged.
		s.logger.Debug("supply divergence: no reference responded",
			"asset", c.Label, "references", len(s.refs))
		emit(SupplyOutcomeNoReference)
	case divergent:
		emit(SupplyOutcomeDivergent)
	default:
		emit(SupplyOutcomeOK)
	}
}

// lookupOne bounds a single reference call with the per-reference
// timeout so one slow vendor can't stall the whole tick.
func (s *SupplyService) lookupOne(ctx context.Context, ref SupplyReference, asset canonical.Asset) (float64, error) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return ref.LookupCirculatingSupply(cctx, asset)
}

// scaleServedSupply divides the minor-unit *big.Int by 10^decimals via big.Rat (never int64 truncation, ADR-0003) and collapses to float64 only at the percentage comparison boundary.
// Returns ok=false for a nil, non-positive or out-of-range input.
func scaleServedSupply(raw *big.Int, decimals int) (float64, bool) {
	if raw == nil || raw.Sign() <= 0 {
		return 0, false
	}
	if decimals < 0 || decimals > 38 {
		return 0, false
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Rat).SetFrac(raw, div).Float64() // i128:ok reference supply for a percentage cross-check; one correctly-rounded conversion of the exact ratio
	if !isFinitePositive(f) {
		return 0, false
	}
	return f, true
}

// ─── Stellar Network Dashboard reference ─────────────────────────────

// StellarDashboardReference derives XLM circulating supply as the Dashboard does: totalSupply - sdfMandate - upgradeReserve - feePool (docs/methodology/xlm-circulating-supply.md). XLM ONLY; others are [ErrAssetUnsupported].
type StellarDashboardReference struct {
	httpClient *http.Client
	baseURL    string
	maxAge     time.Duration
	nowFn      func() time.Time
}

// StellarDashboardOptions configures [NewStellarDashboardReference].
type StellarDashboardOptions struct {
	// HTTPClient — nil falls back to a 10s-timeout client.
	HTTPClient *http.Client
	// BaseURL overrides the API base. Empty defaults to
	// "https://dashboard.stellar.org/api/v3". Tests pass an
	// httptest.Server URL. The reference GETs BaseURL + "/lumens".
	BaseURL string
	// MaxAge is the staleness ceiling for the `/lumens`
	// `updatedAt` field. <= 0 falls back to [DefaultSupplyMaxAge].
	MaxAge time.Duration
	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// NewStellarDashboardReference constructs the Dashboard-backed reference.
func NewStellarDashboardReference(opts StellarDashboardOptions) *StellarDashboardReference {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = "https://dashboard.stellar.org/api/v3"
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultSupplyMaxAge
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &StellarDashboardReference{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		maxAge:     maxAge,
		nowFn:      nowFn,
	}
}

// Name implements [SupplyReference].
func (r *StellarDashboardReference) Name() string { return "stellar-dashboard" }

// dashboardLumens is the consumed subset of `/lumens` (decimal strings, whole XLM); prefers `circulatingSupply` when present, else the component formula.
type dashboardLumens struct {
	TotalSupply       string `json:"totalSupply"`
	SDFMandate        string `json:"sdfMandate"`
	UpgradeReserve    string `json:"upgradeReserve"`
	FeePool           string `json:"feePool"`
	CirculatingSupply string `json:"circulatingSupply"`
	// UpdatedAt is the upstream publication time (RFC 3339). Present on
	// the live v3 body; used for the staleness gate. A body without it
	// fails the gate closed.
	UpdatedAt string `json:"updatedAt"`
}

// LookupCirculatingSupply implements [SupplyReference]. XLM only.
func (r *StellarDashboardReference) LookupCirculatingSupply(ctx context.Context, asset canonical.Asset) (float64, error) {
	if !isNativeLumensSupplyForm(asset) {
		return 0, fmt.Errorf("%w: stellar-dashboard publishes XLM supply only, got %q", ErrAssetUnsupported, asset.String())
	}

	body, err := r.getLumens(ctx)
	if err != nil {
		return 0, err
	}

	var parsed dashboardLumens
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("%w: stellar-dashboard decode: %w", ErrSupplyUnavailable, err)
	}

	// Staleness gate: a frozen `/lumens` upstream must read as
	// "reference unavailable", never drive a supply divergence. A
	// missing/unparseable updatedAt fails closed too.
	if err := supplyStaleness(r.nowFn, r.maxAge, r.Name(), parseUpstreamTime(parsed.UpdatedAt)); err != nil {
		return 0, err
	}

	// Prefer an explicit circulatingSupply field when the endpoint
	// version publishes one.
	if parsed.CirculatingSupply != "" {
		if v, perr := strconv.ParseFloat(parsed.CirculatingSupply, 64); perr == nil && isFinitePositive(v) {
			return v, nil
		}
	}

	// Otherwise derive it the way the Dashboard's own lumens.js does.
	total, terr := strconv.ParseFloat(parsed.TotalSupply, 64)
	mandate, merr := strconv.ParseFloat(parsed.SDFMandate, 64)
	upgrade, uerr := strconv.ParseFloat(parsed.UpgradeReserve, 64)
	feePool, ferr := strconv.ParseFloat(parsed.FeePool, 64)
	if terr != nil || merr != nil || uerr != nil || ferr != nil {
		return 0, fmt.Errorf("%w: stellar-dashboard missing/unparseable supply components", ErrSupplyUnavailable)
	}
	circulating := total - mandate - upgrade - feePool
	if !isFinitePositive(circulating) {
		return 0, fmt.Errorf("%w: stellar-dashboard derived non-positive circulating %g", ErrSupplyUnavailable, circulating)
	}
	return circulating, nil
}

// getLumens issues the single GET against BaseURL + "/lumens" and returns the raw body, mapping HTTP 429 / non-200 to
// [ErrSupplyUnavailable] so an outage degrades to no_reference rather than a hard error.
func (r *StellarDashboardReference) getLumens(ctx context.Context) ([]byte, error) {
	endpoint := r.baseURL + "/lumens"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("stellar-dashboard: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stellarindex-divergence/0.1")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: stellar-dashboard: %w", ErrSupplyUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%w: stellar-dashboard rate-limited (HTTP 429)", ErrSupplyUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: stellar-dashboard HTTP %d", ErrSupplyUnavailable, resp.StatusCode)
	}
	const maxBody = 64 << 10 // the /lumens body is < 1 KiB; cap generously
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%w: stellar-dashboard read body: %w", ErrSupplyUnavailable, err)
	}
	return body, nil
}

// ─── CoinGecko supply reference ──────────────────────────────────────

// coinGeckoDefaultBaseURL is the public / free-tier REST base.
const coinGeckoDefaultBaseURL = "https://api.coingecko.com/api/v3"

// coinGeckoProBaseURL is the paid-tier base; a Pro key 404s against the public host, so it is auto-selected when APIKey is set and BaseURL is not.
const coinGeckoProBaseURL = "https://pro-api.coingecko.com/api/v3"

// CoinGeckoSupplyReference reads `market_data.circulating_supply` from `/coins/{id}`; OFF by default (the free tier is 429-throttled, yielding only `no_reference`), and distinct from the price-path [CoinGeckoReference].
type CoinGeckoSupplyReference struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	// idMap maps canonical asset_id → CoinGecko coin id ("native" →
	// "stellar"). Any asset absent from the map yields
	// ErrAssetUnsupported.
	idMap  map[string]string
	maxAge time.Duration
	nowFn  func() time.Time
}

// CoinGeckoSupplyOptions configures [NewCoinGeckoSupplyReference].
type CoinGeckoSupplyOptions struct {
	// HTTPClient — nil falls back to a 10s-timeout client. One without a
	// CheckRedirect is used with [httpx.KeyedSameOriginRedirect] added.
	HTTPClient *http.Client
	// BaseURL overrides the API base; empty defaults to the public host, or the Pro host when APIKey is set. An explicit BaseURL always wins.
	BaseURL string
	// APIKey, when non-empty, is sent as the `x-cg-pro-api-key` header
	// (the Pro-tier auth that lifts the free-tier 429 ceiling).
	APIKey string
	// IDMap maps canonical asset_id → CoinGecko coin id. Empty falls
	// back to the built-in default ("native" / "crypto:XLM" → "stellar";
	// not the XLM SAC, see [isNativeLumensSupplyForm]).
	IDMap map[string]string
	// MaxAge is the staleness ceiling for the response's
	// `market_data.last_updated`. <= 0 falls back to [DefaultSupplyMaxAge].
	MaxAge time.Duration
	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// NewCoinGeckoSupplyReference constructs the CoinGecko-backed supply reference.
func NewCoinGeckoSupplyReference(opts CoinGeckoSupplyOptions) *CoinGeckoSupplyReference {
	baseURL := opts.BaseURL
	if baseURL == "" {
		// A Pro key 404s the public host: auto-switch to the Pro host unless the operator set an explicit base_url (as the ingest poller does).
		if opts.APIKey != "" {
			baseURL = coinGeckoProBaseURL
		} else {
			baseURL = coinGeckoDefaultBaseURL
		}
	}
	idMap := opts.IDMap
	if len(idMap) == 0 {
		idMap = map[string]string{
			"native":     "stellar",
			"crypto:XLM": "stellar",
		}
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultSupplyMaxAge
	}
	nowFn := opts.NowFn
	if nowFn == nil {
		nowFn = time.Now
	}
	return &CoinGeckoSupplyReference{
		httpClient: keyedCoinGeckoClient("coingecko-supply", opts.HTTPClient),
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     opts.APIKey,
		idMap:      idMap,
		maxAge:     maxAge,
		nowFn:      nowFn,
	}
}

// keyedCoinGeckoClient gives a CoinGecko reference a client whose redirect
// policy keeps the key on the origin; an injected client without its own
// CheckRedirect is copied rather than trusted to follow any Location.
func keyedCoinGeckoClient(tag string, injected *http.Client) (keyed *http.Client) {
	if injected == nil {
		return httpx.NewKeyedClient(tag, 10*time.Second)
	}
	if injected.CheckRedirect != nil {
		return injected
	}
	cp := *injected
	cp.CheckRedirect = httpx.KeyedSameOriginRedirect(tag)
	return &cp
}

// Name implements [SupplyReference].
func (c *CoinGeckoSupplyReference) Name() string { return "coingecko" }

// coinGeckoResponse is the subset of `/coins/{id}` we consume.
type coinGeckoResponse struct {
	MarketData struct {
		//floatmoney:ok CoinGecko wire decode, used only as an external cross-check reference in LookupCirculatingSupply (SupplyReference interface), never stored or served as canonical money
		CirculatingSupply float64 `json:"circulating_supply"`
		// LastUpdated is the upstream publication time (RFC 3339) used
		// for the staleness gate. Absent → the gate fails closed.
		LastUpdated string `json:"last_updated"`
	} `json:"market_data"`
}

// LookupCirculatingSupply implements [SupplyReference].
func (c *CoinGeckoSupplyReference) LookupCirculatingSupply(ctx context.Context, asset canonical.Asset) (float64, error) {
	cgID, ok := c.idMap[asset.String()]
	if !ok {
		return 0, fmt.Errorf("%w: asset %q has no CoinGecko coin id", ErrAssetUnsupported, asset.String())
	}

	endpoint := c.baseURL + "/coins/" + cgID +
		"?localization=false&tickers=false&market_data=true&community_data=false&developer_data=false&sparkline=false"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("coingecko: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "stellarindex-divergence/0.1")
	if c.apiKey != "" {
		req.Header.Set("x-cg-pro-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%w: coingecko: %w", ErrSupplyUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("%w: coingecko rate-limited (HTTP 429)", ErrSupplyUnavailable)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: coingecko HTTP %d", ErrSupplyUnavailable, resp.StatusCode)
	}
	const maxBody = 512 << 10 // /coins/{id} is a fat body even trimmed
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, fmt.Errorf("%w: coingecko read body: %w", ErrSupplyUnavailable, err)
	}
	var parsed coinGeckoResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("%w: coingecko decode: %w", ErrSupplyUnavailable, err)
	}
	if !isFinitePositive(parsed.MarketData.CirculatingSupply) {
		return 0, fmt.Errorf("%w: coingecko circulating_supply non-positive for %q", ErrSupplyUnavailable, cgID)
	}
	// Staleness gate: a frozen CoinGecko supply must read as
	// "reference unavailable". A missing/unparseable last_updated fails
	// closed too.
	if err := supplyStaleness(c.nowFn, c.maxAge, c.Name(), parseUpstreamTime(parsed.MarketData.LastUpdated)); err != nil {
		return 0, err
	}
	return parsed.MarketData.CirculatingSupply, nil
}

// parseUpstreamTime parses an RFC 3339 upstream timestamp (as `/lumens`.updatedAt and `/coins/{id}`.market_data.last_updated use),
// returning the zero time on empty or unparseable input, which [supplyStaleness] rejects as unverifiable.
func parseUpstreamTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// isNativeLumensSupplyForm reports whether a is `native` or `crypto:XLM`. The XLM SAC is excluded on purpose: supply.AssetKey keys it on the contract (wrapped amount), so matching it would page a false divergence.
func isNativeLumensSupplyForm(a canonical.Asset) bool {
	return a.Type != canonical.AssetSoroban &&
		canonical.CanonicalAsset(a).Type == canonical.AssetNative
}

// Compile-time checks.
var (
	_ SupplyReference = (*StellarDashboardReference)(nil)
	_ SupplyReference = (*CoinGeckoSupplyReference)(nil)
)
