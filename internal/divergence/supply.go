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
)

// This file adds a SUPPLY-divergence cross-check that mirrors the
// price-divergence path in worker.go, but for the served
// `circulating_supply` figure instead of VWAP price.
//
// Motivation: the price path (CoinGecko / Chainlink / on-chain
// oracles) catches a wrong PRICE, but nothing cross-checked our
// SUPPLY. A genuinely-stale SDF-reserve exclusion list — or a supply
// bug — would silently drift `/v1/assets/native` circulating away
// from the market with no automated signal, and the manual "is our
// supply right?" investigation (see
// docs/methodology/xlm-circulating-supply.md) is the only line of
// defense. This automates that check.
//
// The reference universe is deliberately different from the price
// path's: circulating supply is published by the Stellar Network
// Dashboard (authoritative for XLM, free, no auth) and by CoinGecko
// (`/coins/{id}` → `market_data.circulating_supply`, off by default
// because the free tier has been 429-throttled since 2026-06-19).
// The check degrades gracefully to a `no_reference` outcome when every
// reference is dark — exactly the CS-088/089 discipline the price path
// applies, so a dead reference is a distinct (non-paging) signal, not
// a false divergence alert.

// Supply-check sentinel errors. ErrAssetUnsupported (reference.go) is
// reused for "this reference doesn't publish the asset"; the two below
// are supply-specific.
var (
	// ErrSupplyUnavailable — the reference supports the asset but has
	// no usable circulating figure right now (vendor outage, 429,
	// malformed body). Treated as a transient miss, mirroring
	// ErrPriceUnavailable on the price path.
	ErrSupplyUnavailable = errors.New("divergence: circulating supply unavailable")

	// ErrNoServedSupply — OUR served tier has no circulating snapshot
	// for the asset yet (bootstrap state, before the supply refresher
	// has produced its first `asset_supply_history` row). Distinct from
	// a reference miss: it means we have nothing to compare, so the
	// check records `refresh_error` for the asset rather than a
	// (misleading) divergence.
	ErrNoServedSupply = errors.New("divergence: no served circulating supply for asset")
)

// SupplyReference is a pluggable external circulating-supply
// reference. One implementation per external source (Stellar
// Dashboard, CoinGecko). Analogue of [Reference] on the price path.
//
// Implementations MUST be safe for concurrent LookupCirculatingSupply
// calls and SHOULD return [ErrAssetUnsupported] for an asset they
// don't publish (so the service records the gap without treating it as
// a transport failure).
type SupplyReference interface {
	// Name returns a stable, lowercase, hyphenated label suitable for
	// the Prometheus `reference` label and JSON keys (e.g.
	// "stellar-dashboard", "coingecko"). Stable across versions —
	// renaming is a wire break against the supply-divergence alert.
	Name() string

	// LookupCirculatingSupply returns the reference's reported
	// circulating supply for the asset, in WHOLE-TOKEN units (NOT minor
	// units / stroops). Returns [ErrAssetUnsupported] when the source
	// doesn't publish the asset, and [ErrSupplyUnavailable] on a
	// transient outage.
	LookupCirculatingSupply(ctx context.Context, asset canonical.Asset) (float64, error)
}

// ServedSupply is the narrow view of our own served circulating-supply
// snapshot the cross-check consumes.
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

// ServedSupplyReader is the read seam onto OUR served circulating
// supply. Production wires an adapter over
// `internal/storage/timescale.(*Store).LatestSupply`; tests substitute
// a fake. Kept as a package-local interface (not an import of
// internal/supply) so the divergence package stays decoupled from the
// supply pipeline, same discipline as [OracleReader].
type ServedSupplyReader interface {
	// LatestCirculatingSupply returns the most-recent served snapshot
	// for the supply asset_key (supply.AssetKey form — "XLM" for native
	// lumens). Implementations MUST return a wrapped [ErrNoServedSupply]
	// when no snapshot exists yet so the service distinguishes the
	// bootstrap state from a real read failure.
	LatestCirculatingSupply(ctx context.Context, assetKey string) (ServedSupply, error)
}

// SupplyCheck binds one flagship asset to the identifiers each side of
// the comparison needs. The service iterates a fixed, operator-bounded
// slice of these (XLM first — see [DefaultSupplyChecks]).
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

// SupplyOutcomeKind is the per-(asset, tick) outcome, used directly as
// the `outcome` Prometheus label. Mirrors the price path's
// ok/divergent/no_reference/refresh_error vocabulary.
type SupplyOutcomeKind string

const (
	// SupplyOutcomeOK — our served figure agreed with every responding
	// reference within the threshold.
	SupplyOutcomeOK SupplyOutcomeKind = "ok"
	// SupplyOutcomeDivergent — at least one responding reference
	// disagreed by more than the threshold. The ratio gauge carries the
	// magnitude; the supply-divergence alert fires off the gauge.
	SupplyOutcomeDivergent SupplyOutcomeKind = "divergent"
	// SupplyOutcomeNoReference — our served figure loaded fine but every
	// configured reference was unreachable / didn't publish the asset
	// (CoinGecko 429, Dashboard outage). Graceful-degrade state: NOT a
	// divergence, deliberately not paged (a dead reference must not
	// masquerade as a real supply drift).
	SupplyOutcomeNoReference SupplyOutcomeKind = "no_reference"
	// SupplyOutcomeRefreshError — OUR served snapshot couldn't be read
	// (bootstrap, storage error). Nothing to compare against.
	SupplyOutcomeRefreshError SupplyOutcomeKind = "refresh_error"
)

// SupplyEmitter is the metric-emission seam — kept as an interface so
// the service stays Prometheus-agnostic and unit tests capture emitted
// values without a registry. Production wraps
// obs.SupplyDivergenceRatio / obs.SupplyDivergenceTotal /
// obs.SupplyDivergenceDurationSeconds; the wiring lives in
// cmd/stellarindex-aggregator/main.go so this package keeps no obs
// dependency (same split as [supply.CrossCheckEmitter]).
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

// DefaultSupplyMaxAge is the CS-089 staleness ceiling for a supply
// reference's upstream publication time. Circulating supply moves
// glacially (XLM inflation was disabled in 2019; the figure now drifts
// only by fee burns), so the gate is deliberately generous — its job
// is to catch a genuinely FROZEN upstream (a reference that has stopped
// updating for a day-plus), not normal cadence. The Stellar Dashboard
// recomputes `updatedAt` roughly per-ledger and CoinGecko refreshes
// `market_data.last_updated` frequently, so 24h means "the reference
// has been dark for a full day" — well beyond any healthy gap.
//
// VALUE DECISION: 24h is a defensible engineering default, not a
// business rule.
const DefaultSupplyMaxAge = 24 * time.Hour

// supplyStaleness enforces the CS-089 gate for a supply reference's
// upstream publication time. Unlike the price path (which threads the
// bucket-end observedAt through [Compare]) the [SupplyReference]
// interface carries no comparison time, so each reference gates against
// its own injected clock. A zero publishedAt no-ops (the upstream
// didn't report a timestamp) — we only reject a figure we can PROVE is
// stale.
func supplyStaleness(now func() time.Time, maxAge time.Duration, refName string, publishedAt time.Time) error {
	if publishedAt.IsZero() {
		return nil
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

// DefaultSupplyThreshold is the 1% relative-divergence ceiling. It sits
// two-plus orders of magnitude above the ~0.03% XLM noise floor (the
// Fee-Pool delta between our figure and the Dashboard's, see
// docs/methodology/xlm-circulating-supply.md), so the alert only fires
// on a REAL drift (e.g. a stale SDF exclusion account) — never on that
// structural noise.
const DefaultSupplyThreshold = 0.01

// SupplyService runs one supply cross-check cycle per [Tick]. For each
// configured [SupplyCheck] it loads our served circulating figure,
// fans out to every reference, computes the relative divergence, and
// emits the ratio gauge + outcome counter + duration histogram.
//
// Safe for concurrent Tick calls after construction; the References,
// Reader, and Emitter must also be concurrent-safe (they are by
// contract).
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

// NewSupplyService constructs the cross-check service. Returns an error
// when a required option is missing OR when no references are
// configured — a service with nothing to compare against would emit
// only `no_reference` forever, so the caller should simply not wire it
// (the aggregator gates on `[divergence.supply].enabled` + at least one
// reference before calling this).
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
		timeout = 10 * time.Second
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

// DefaultSupplyChecks is the built-in flagship set: XLM only. XLM is
// the single most-viewed supply figure and the one the Stellar
// Dashboard covers authoritatively; USDC + the top verified currencies
// are a mechanical follow-up (add a SupplyCheck + a CoinGecko slug)
// once a reference that actually publishes them today is wired.
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

// Tick runs one cross-check cycle across every configured check.
// Per-check failures are isolated (logged + surfaced via the outcome
// metric) so one asset's storage hiccup doesn't drop the rest.
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
		// Bootstrap (no snapshot yet) and real read errors both mean
		// "nothing to compare" — refresh_error. Log at the level that
		// matches: bootstrap is debug (expected on a cold deploy), a
		// real error is warn.
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
			// Transient miss (429, timeout, malformed body). Debug-level:
			// the aggregate no_reference/ok outcome carries the operator
			// signal; a per-reference warn would be noisy on the known
			// CoinGecko-429 path.
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

// scaleServedSupply divides the served minor-unit *big.Int by
// 10^decimals via big.Rat (never int64 truncation, ADR-0003) and
// collapses to whole-token float64 only at the comparison boundary —
// the divergence is percentage-based so float64 at the boundary is
// fine, same trade-off as scaleOracleAmount. Returns ok=false for a
// nil / non-positive / out-of-range input.
func scaleServedSupply(raw *big.Int, decimals int) (float64, bool) {
	if raw == nil || raw.Sign() <= 0 {
		return 0, false
	}
	if decimals < 0 || decimals > 38 {
		return 0, false
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Rat).SetFrac(raw, div).Float64()
	if !isFinitePositive(f) {
		return 0, false
	}
	return f, true
}

// ─── Stellar Network Dashboard reference ─────────────────────────────

// StellarDashboardReference reads the authoritative XLM supply
// breakdown the Stellar Network Dashboard publishes at
// `dashboard.stellar.org/api/v3/lumens` and derives circulating supply
// the same way the Dashboard does:
//
//	circulating = totalSupply − sdfMandate − upgradeReserve − feePool
//
// This is the market-standard "lumens in circulation" figure (see
// docs/methodology/xlm-circulating-supply.md). It covers XLM ONLY;
// every other asset returns [ErrAssetUnsupported]. Free, no auth,
// reliable — the primary reference for the XLM check (CoinGecko's free
// tier has been 429-throttled since 2026-06-19).
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
	// MaxAge is the CS-089 staleness ceiling for the `/lumens`
	// `updatedAt` field. <= 0 falls back to [DefaultSupplyMaxAge].
	MaxAge time.Duration
	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// NewStellarDashboardReference constructs the Dashboard-backed
// reference.
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

// dashboardLumens is the subset of the `/lumens` response body we
// consume. All figures are decimal strings in whole XLM units. Newer
// endpoint versions also publish `circulatingSupply` directly; we
// prefer it when present and fall back to the component formula
// otherwise (the v3 body computes it client-side).
type dashboardLumens struct {
	TotalSupply       string `json:"totalSupply"`
	SDFMandate        string `json:"sdfMandate"`
	UpgradeReserve    string `json:"upgradeReserve"`
	FeePool           string `json:"feePool"`
	CirculatingSupply string `json:"circulatingSupply"`
	// UpdatedAt is the upstream publication time (RFC 3339). Present on
	// the live v3 body; used for the CS-089 staleness gate. Absent on
	// older/compat bodies — the gate then no-ops.
	UpdatedAt string `json:"updatedAt"`
}

// LookupCirculatingSupply implements [SupplyReference]. XLM only.
func (r *StellarDashboardReference) LookupCirculatingSupply(ctx context.Context, asset canonical.Asset) (float64, error) {
	if !isXLMAsset(asset) {
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

	// CS-089 staleness gate: a frozen `/lumens` upstream must read as
	// "reference unavailable", never drive a supply divergence. A
	// missing/unparseable updatedAt no-ops (zero time).
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

// getLumens issues the single GET against BaseURL + "/lumens" and
// returns the raw body, mapping HTTP 429 / non-200 to
// [ErrSupplyUnavailable] so an outage degrades to no_reference rather
// than a hard error.
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

// CoinGeckoSupplyReference reads `market_data.circulating_supply` from
// CoinGecko's `/coins/{id}` endpoint. OFF by default: the free tier has
// been 429-throttled since 2026-06-19 (pending the Pro key), so
// enabling it without a working key just produces the graceful
// no_reference outcome. Distinct from the price path's
// [CoinGeckoReference] (which uses `/simple/price`) — kept separate
// because the endpoints, response shapes, and quota profiles differ.
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
	// HTTPClient — nil falls back to a 10s-timeout client.
	HTTPClient *http.Client
	// BaseURL overrides the API base. Empty defaults to
	// "https://api.coingecko.com/api/v3".
	BaseURL string
	// APIKey, when non-empty, is sent as the `x-cg-pro-api-key` header
	// (the Pro-tier auth that lifts the free-tier 429 ceiling).
	APIKey string
	// IDMap maps canonical asset_id → CoinGecko coin id. Empty falls
	// back to the built-in default ("native" / "crypto:XLM" → "stellar").
	IDMap map[string]string
	// MaxAge is the CS-089 staleness ceiling for the response's
	// `market_data.last_updated`. <= 0 falls back to [DefaultSupplyMaxAge].
	MaxAge time.Duration
	// NowFn overrides time.Now for deterministic tests. Nil = time.Now.
	NowFn func() time.Time
}

// NewCoinGeckoSupplyReference constructs the CoinGecko-backed supply
// reference.
func NewCoinGeckoSupplyReference(opts CoinGeckoSupplyOptions) *CoinGeckoSupplyReference {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = "https://api.coingecko.com/api/v3"
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
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     opts.APIKey,
		idMap:      idMap,
		maxAge:     maxAge,
		nowFn:      nowFn,
	}
}

// Name implements [SupplyReference].
func (c *CoinGeckoSupplyReference) Name() string { return "coingecko" }

// coinGeckoResponse is the subset of `/coins/{id}` we consume.
type coinGeckoResponse struct {
	MarketData struct {
		CirculatingSupply float64 `json:"circulating_supply"`
		// LastUpdated is the upstream publication time (RFC 3339) used
		// for the CS-089 staleness gate. Absent → gate no-ops.
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
	// CS-089 staleness gate: a frozen CoinGecko supply must read as
	// "reference unavailable". Missing last_updated no-ops (zero time).
	if err := supplyStaleness(c.nowFn, c.maxAge, c.Name(), parseUpstreamTime(parsed.MarketData.LastUpdated)); err != nil {
		return 0, err
	}
	return parsed.MarketData.CirculatingSupply, nil
}

// parseUpstreamTime parses an RFC 3339 upstream timestamp (the shape
// both `/lumens`.updatedAt and `/coins/{id}`.market_data.last_updated
// use), returning the zero time on empty/unparseable input so the
// staleness gate no-ops rather than rejecting a usable figure.
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

// isXLMAsset reports whether the asset is native lumens in either
// canonical form (the on-chain `native` form or the abstract
// `crypto:XLM` ticker).
func isXLMAsset(a canonical.Asset) bool {
	if a.Type == canonical.AssetNative {
		return true
	}
	return a.String() == "crypto:XLM"
}

// Compile-time checks.
var (
	_ SupplyReference = (*StellarDashboardReference)(nil)
	_ SupplyReference = (*CoinGeckoSupplyReference)(nil)
)
