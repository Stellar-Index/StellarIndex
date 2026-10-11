package divergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Cache is the Redis subset the [Service] needs; SAdd/SMembers/Expire maintain a per-base index so readers avoid KEYS/SCAN.
type Cache interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	SAdd(ctx context.Context, key string, members ...any) *redis.IntCmd
	SMembers(ctx context.Context, key string) *redis.StringSliceCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
}

// CachedResult is the wire shape stored at the `div:<base>/<quote>`
// Redis key per ADR-0007. Mirrors most of [Result] but with a couple
// of derived fields for API-side consumers that don't want to redo
// the threshold logic.
type CachedResult struct {
	// PairID is the canonical pair string the result is for.
	PairID string `json:"pair_id"`

	// OurPrice / Median / DivergencePct mirror the comparator output.
	//floatmoney:ok internal Redis cache wire shape read only by the aggregator's cross-oracle signal (confidence.go lookupCrossOracle); never re-served — the durable ObservationRecord path formats to decimal strings at the sink boundary (worker.go RecordObservation)
	OurPrice      float64 `json:"our_price"`
	Median        float64 `json:"median"`
	DivergencePct float64 `json:"divergence_pct"`

	// WarningFired fires when SuccessCount >= MinSourcesForWarning AND (DivergencePct > Threshold OR AgreementCount == 0), once persisted for WarningPersistence.
	// The AgreementCount==0 leg catches symmetric disagreement the median hides; see [Service.RefreshPair]. A below-quorum refresh carries the last value forward.
	WarningFired bool `json:"warning_fired"`

	// FiringSince is when the current uninterrupted raw divergence began (zero when clear); the durable half of the WarningPersistence
	// debounce, so a restart resumes the streak. See [Service.restoreWarningState].
	FiringSince time.Time `json:"firing_since,omitzero"`

	// Sources / Failures mirror Result, kept for operator
	// dashboards.
	Sources  map[string]float64 `json:"sources,omitempty"`
	Failures map[string]string  `json:"failures,omitempty"`

	// SuccessCount + FailureCount counters for the run.
	SuccessCount int `json:"success_count"`
	FailureCount int `json:"failure_count"`

	// AgreementCount is how many successful references corroborated OurPrice within the threshold (ADR-0019 Phase 3; [CountAgreeing]).
	// Failed references neither agree nor disagree: gate on SuccessCount first, since SuccessCount=0 means "unchecked", not unanimous disagreement.
	AgreementCount int `json:"agreement_count"`

	// Pinned marks an entry whose OurPrice was a frozen pair's pinned last-known-good ([Service.RefreshPinnedPair]); DivergencePct and
	// AgreementCount are then no verdict, and WarningFired/FiringSince are carried forward.
	Pinned bool `json:"our_price_pinned,omitempty"`

	// WindowSeconds is the aggregation window of the VWAP OurPrice was
	// ([ServiceOptions.Window]); 0 when the writer did not record one.
	WindowSeconds int `json:"window_seconds,omitempty"`

	// ComputedAt is when the worker wrote this result. RFC 3339 UTC.
	ComputedAt time.Time `json:"computed_at"`
}

// ObservationSink is the optional durable mirror of per-(pair, reference) observations; implementations must not block the hot path on network failures.
type ObservationSink interface {
	// RecordObservation persists one (pair, reference) comparison.
	// firing = true when |delta_pct| exceeded the per-reference
	// threshold at observation time.
	RecordObservation(ctx context.Context, obs ObservationRecord) error
}

// ObservationRecord aliases [domain.DivergenceObservationRecord] so storage need not import upward into this package.
type ObservationRecord = domain.DivergenceObservationRecord

// DefaultWarningPersistence is the WarningFired debounce when unset: one shortest-window VWAP horizon (5m), over which a fast-move VWAP/spot gap self-clears.
const DefaultWarningPersistence = 5 * time.Minute

// ServiceOptions configures a [Service].
type ServiceOptions struct {
	// References is the list of external sources to compare against.
	// Empty list disables divergence checking (Service.RefreshPair
	// returns nil without writing).
	References []Reference

	// Cache is the Redis client used to store CachedResult JSON
	// at div:<base>/<quote> keys. Required.
	Cache Cache

	// Threshold is the divergence percentage above which
	// WarningFired is true on the cached result. Default 5.0
	// (5%). Operators tune higher for noisier asset classes.
	Threshold float64

	// MinSourcesForWarning is the minimum number of successful
	// references required before WarningFired can be true. Default
	// 2 — a single dissenting source isn't enough to call divergence.
	MinSourcesForWarning int

	// WarningPersistence is how long a raw divergence must hold (across two refreshes) before WarningFired is true; it debounces VWAP-vs-spot lag and never blinds a sustained gap.
	// Zero defaults to [DefaultWarningPersistence]; a NEGATIVE value disables the gate (immediate firing).
	WarningPersistence time.Duration

	// RefreshInterval is the caller's expected cadence per pair; a firing streak with a gap over 2x max(RefreshInterval, WarningPersistence)
	// restarts instead of maturing on one post-gap sample. Zero means at most WarningPersistence.
	RefreshInterval time.Duration

	// Window is the aggregation window whose VWAP the caller passes as
	// ourPrice (the aggregator's shortest). It is stamped on every cached
	// verdict so a reader serving a value over another window can tell the
	// verdict does not speak to it. Zero leaves the verdict unscoped.
	Window time.Duration

	// PairCount is how many pairs one refresh pass visits. The caller
	// refreshes them sequentially, so a pair's div: key is rewritten up
	// to RefreshInterval plus a whole pass after its previous write;
	// with RefreshInterval it sizes the key's TTL ([Service.cacheTTL]).
	PairCount int

	// PerReferenceTimeout is forwarded to [Compare] via
	// [CompareOptions]. Default 5s.
	PerReferenceTimeout time.Duration

	// ObservationSink, when non-nil, receives one record per (pair,
	// reference) tuple every refresh. Persists the per-reference
	// delta history that the Redis cache discards. Optional — nil
	// leaves the Redis cache as the only record.
	ObservationSink ObservationSink

	// Logger, when non-nil, receives WARN-level log lines for sink
	// failures. Optional — nil silences the path.
	// The aggregator passes its component logger so failures land
	// in the same journal stream as the rest of the orchestrator.
	Logger *slog.Logger

	// OnWarningFired, when non-nil, runs on the false-to-true edge only (not every refresh while firing), so subscribers are not re-spammed.
	// A non-nil error releases the latch so the next refresh retries; panics in the hook do not propagate.
	OnWarningFired WarningHook
}

// WarningHook is the edge-triggered warning callback; a non-nil error releases the edge latch so the next firing refresh retries.
type WarningHook func(ctx context.Context, pair canonical.Pair, cached CachedResult) error

// Service refreshes divergence per pair and caches it at `div:<base>/<quote>` (ADR-0007); safe for concurrent RefreshPair, so Cache and References must be concurrent-safe.
type Service struct {
	refs        []Reference
	cache       Cache
	threshold   float64
	minSources  int
	window      time.Duration
	timeout     time.Duration
	persistence time.Duration
	maxGap      time.Duration
	cacheTTL    time.Duration
	sink        ObservationSink
	// logger is optional (nil-safe); when set, sink failures are logged at WARN instead of silently dropping observation rows.
	logger *slog.Logger

	// warningState maps pair.String() to the last EVALUATED refresh's WarningFired (the hook fires on false-to-true only; below-quorum carries forward).
	// firingSince is the raw-firing streak for the debounce; notified marks pairs whose episode reached onWarning (warningState stays true while a failed delivery retries);
	// restored marks pairs seeded by [Service.restoreWarningState]. warningMu guards all four maps.
	onWarning    WarningHook
	warningMu    sync.Mutex
	warningState map[string]bool
	notified     map[string]bool
	firingSince  map[string]firingStreak
	restored     map[string]bool
	gaps         *gapTracker
}

// firingStreak is one pair's raw-firing run: since is its first firing
// observation, last its most recent one. last is what distinguishes a
// run observed throughout from one with an unevaluated gap in it.
type firingStreak struct {
	since time.Time
	last  time.Time
}

// independentReferences drops references that cannot corroborate our
// VWAP because they price the same book (see [CorrelatedWithOurVWAP]).
// Filtering here, the one constructor, keeps them out of the median, the
// quorum and the agreement count whichever binary built the list.
func independentReferences(refs []Reference) []Reference {
	out := make([]Reference, 0, len(refs))
	for _, r := range refs {
		if !CorrelatedWithOurVWAP(safeName(r)) {
			out = append(out, r)
		}
	}
	return out
}

// NewService constructs a divergence service. Returns an error when
// required options are missing.
func NewService(opts ServiceOptions) (*Service, error) {
	if opts.Cache == nil {
		return nil, errors.New("divergence: Cache is required")
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = 5.0
	}
	minSources := opts.MinSourcesForWarning
	if minSources <= 0 {
		minSources = 2
	}
	timeout := opts.PerReferenceTimeout
	if timeout <= 0 {
		timeout = DefaultPerReferenceTimeout
	}
	// A NEGATIVE WarningPersistence is the explicit opt-out and stays intact; 0 is meaningful here (fire immediately),
	// so map it to the safe default rather than clamp-to-off.
	persistence := opts.WarningPersistence
	if persistence == 0 {
		persistence = DefaultWarningPersistence
	}
	refs := independentReferences(opts.References)
	seedReferenceOutcomes(refs)
	ttl := cacheTTL(opts.RefreshInterval, opts.PairCount, timeout)
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, safeName(r))
	}
	return &Service{
		maxGap:       2 * max(persistence, opts.RefreshInterval),
		cacheTTL:     ttl,
		gaps:         newGapTracker(names, ttl),
		refs:         refs,
		cache:        opts.Cache,
		threshold:    threshold,
		minSources:   minSources,
		window:       opts.Window,
		timeout:      timeout,
		persistence:  persistence,
		sink:         opts.ObservationSink,
		logger:       opts.Logger,
		onWarning:    opts.OnWarningFired,
		warningState: map[string]bool{},
		notified:     map[string]bool{},
		firingSince:  map[string]firingStreak{},
		restored:     map[string]bool{},
	}, nil
}

// cacheTTLSlack covers what a pass does besides Compare — the VWAP
// read, the Redis writes and the observation sink — per pass.
const cacheTTLSlack = time.Minute

// cacheTTL must outlive the longest gap between a pair's writes (pass cadence plus every pair spending Compare's whole budget),
// or consumers read "no cross-oracle data" between; never below [cachekeys.DivergenceTTL].
func cacheTTL(refreshInterval time.Duration, pairs int, perReference time.Duration) time.Duration {
	if refreshInterval <= 0 {
		return cachekeys.DivergenceTTL
	}
	worstPass := time.Duration(max(pairs, 1)) * overallTimeoutFactor * perReference
	return max(cachekeys.DivergenceTTL, refreshInterval+worstPass+cacheTTLSlack)
}

// seedReferenceOutcomes pre-creates every (reference, outcome) child so a
// failure class's first occurrence moves a rate() instead of creating it.
func seedReferenceOutcomes(refs []Reference) {
	for _, r := range refs {
		for _, outcome := range ReferenceOutcomes {
			obs.DivergenceReferenceTotal.WithLabelValues(safeName(r), outcome)
		}
	}
}

// recordReferenceHealth exports per-reference outcomes and quorum, because an outage leaving fewer than minSources responding disarms detection while the pass still counts ok.
func recordReferenceHealth(pair canonical.Pair, res Result, quorumMet bool) {
	for name, outcome := range res.Outcomes {
		obs.DivergenceReferenceTotal.WithLabelValues(name, outcome).Inc()
	}
	met := 0.0
	if quorumMet {
		met = 1
	}
	obs.DivergencePairQuorumMet.WithLabelValues(pair.String()).Set(met)
}

// ErrNoReferenceResponded is returned by RefreshPair when no configured reference answered
// and at least one genuinely failed; the outage result is still cached, so it can be paged on.
var ErrNoReferenceResponded = errors.New("divergence: no reference responded for pair")

// RefreshPair compares ourPrice with the references and caches the result at div:<base>/<quote>.
// It returns nil when no references are configured, ErrNoReferenceResponded, or a cache error.
func (s *Service) RefreshPair(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time) error {
	return s.refresh(ctx, pair, ourPrice, observedAt, false)
}

// RefreshPinnedPair is [Service.RefreshPair] for a frozen pair: references are polled but reach no verdict against pinnedPrice, so the entry is marked Pinned and no hook fires.
func (s *Service) RefreshPinnedPair(ctx context.Context, pair canonical.Pair, pinnedPrice float64, observedAt time.Time) error {
	return s.refresh(ctx, pair, pinnedPrice, observedAt, true)
}

func (s *Service) refresh(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time, pinned bool) error {
	if len(s.refs) == 0 {
		return nil
	}
	res := Compare(ctx, s.refs, pair, ourPrice, observedAt, CompareOptions{
		PerReferenceTimeout: s.timeout,
		MinSuccessForMedian: 1, // surface even single-source signals; threshold gate handles trustworthiness
	})

	// Agreement uses the same threshold as the per-reference firing
	// test in flushObservations, so "agrees" is exactly "would not
	// fire" for that reference.
	agreeing := CountAgreeing(ourPrice, res.Sources, s.threshold)

	// The gate is median-vs-ourPrice OR nobody-agrees: references at +/-8% put the median on our price, so the median leg alone is blind.
	// Use AgreementCount==0, not "any reference disagrees" (one flaky reference would pin the warning on). Both legs need SuccessCount >= minSources:
	// with no responses, 0 means unchecked.
	checked := res.SuccessCount >= s.minSources
	evaluated := checked && !pinned
	recordReferenceHealth(pair, res, checked)
	s.gaps.observe(pair.String(), ourPrice, res.Sources, pinned, time.Now())

	// Our VWAP lags spot by up to one window on a fast move, so publish only once the raw condition persisted past that horizon.
	// The clock is the comparison time, falling back to wall time when none is supplied.
	gateAt := observedAt
	if gateAt.IsZero() {
		gateAt = time.Now().UTC()
	}
	key := cachekeys.Divergence(pair)
	s.restoreWarningState(ctx, pair.String(), key.String(), gateAt)

	// A below-quorum or pinned refresh is unevaluable: it must not assert
	// "no divergence", restart the persistence streak or reset the webhook
	// latch, so it carries the last evaluated verdict forward.
	warningFired, firingSince := s.lastWarning(pair.String()), s.lastFiringSince(pair.String())
	if evaluated {
		// At the SuccessCount==2 quorum floor the median is a mean, so one outlier fires the median leg though the other agrees exactly; require agreement
		// in the MINORITY (agreeing < ceil(SuccessCount/2)). The agreeing==0 leg above is independent of DivergencePct (symmetric references put the median AT ourPrice).
		medianLegFiring := res.DivergencePct > s.threshold && agreeing < (res.SuccessCount+1)/2
		rawFiring := medianLegFiring || agreeing == 0
		warningFired, firingSince = s.warningPersists(pair.String(), rawFiring, gateAt)
	}

	cached := CachedResult{
		PairID:         pair.String(),
		OurPrice:       ourPrice,
		Median:         res.Median,
		DivergencePct:  res.DivergencePct,
		WarningFired:   warningFired,
		FiringSince:    firingSince,
		Sources:        res.Sources,
		Failures:       res.Failures,
		SuccessCount:   res.SuccessCount,
		FailureCount:   res.FailureCount,
		AgreementCount: agreeing,
		Pinned:         pinned,
		WindowSeconds:  int(s.window / time.Second),
		ComputedAt:     time.Now().UTC(),
	}

	body, err := json.Marshal(cached)
	if err != nil {
		// Should be unreachable — CachedResult has no func/chan
		// fields. Wrap for diagnostic completeness.
		return fmt.Errorf("divergence: marshal cached result: %w", err)
	}

	// Per-PAIR key, not per-base: a per-base key lets the last pair in iteration order clobber the asset's verdict; LookupCached ORs the pairs.
	if err := s.cache.Set(ctx, key.String(), body, s.cacheTTL).Err(); err != nil {
		return fmt.Errorf("divergence: cache set %s: %w", key, err)
	}

	// The per-base quote index lets LookupCached find the pair keys; Expire on every write drains it with the value keys so dead quotes do not linger.
	idxKey := cachekeys.DivergenceBaseIndex(pair.Base)
	if err := s.cache.SAdd(ctx, idxKey.String(), pair.Quote.String()).Err(); err != nil {
		return fmt.Errorf("divergence: index sadd %s: %w", idxKey, err)
	}
	if err := s.cache.Expire(ctx, idxKey.String(), s.cacheTTL).Err(); err != nil {
		return fmt.Errorf("divergence: index expire %s: %w", idxKey, err)
	}

	// Durable per-reference mirror. Best-effort: a sink failure must
	// not surface to the caller because the Redis cache write — the
	// load-bearing operation that drives flags.divergence_warning
	// on the API response — has already succeeded.
	if s.sink != nil {
		// Stamp with the COMPARISON time, not wall clock (which trails by the reference fan-out); observed_at is in the conflict key,
		// so a re-run stays idempotent. Zero falls back to ComputedAt.
		stampedAt := observedAt.UTC()
		if observedAt.IsZero() {
			stampedAt = cached.ComputedAt
		}
		s.flushObservations(ctx, pair, ourPrice, res, stampedAt)
	}

	if evaluated {
		s.recordWarning(ctx, pair, cached)
	}
	// No reference responded: signal the outage so the refresh loop can emit a distinct outcome. asset_unsupported is structural non-coverage but counts in FailureCount,
	// so page only when a GENUINE failure exists beyond the unsupported ones, else an FX-only oracle map pages "checker running blind" forever.
	if res.SuccessCount == 0 {
		unsupported := countOutcome(res.Outcomes, OutcomeAssetUnsupported)
		if res.FailureCount > unsupported {
			return ErrNoReferenceResponded
		}
	}
	return nil
}

// lastWarning returns the WarningFired of the pair's most recent
// evaluated refresh (false when there has been none).
func (s *Service) lastWarning(pairKey string) bool {
	s.warningMu.Lock()
	defer s.warningMu.Unlock()
	return s.warningState[pairKey]
}

// lastFiringSince returns the persistence-streak start recorded for the
// pair (zero when there is none, i.e. the raw condition last evaluated
// clear). It mirrors lastWarning so a below-quorum refresh can carry the
// streak forward into the cached result without calling warningPersists.
func (s *Service) lastFiringSince(pairKey string) time.Time {
	s.warningMu.Lock()
	defer s.warningMu.Unlock()
	return s.firingSince[pairKey].since
}

// recordWarning latches the verdict and fires the hook once per episode (not per refresh); a hook error leaves the episode undelivered for retry, the published verdict unaffected.
func (s *Service) recordWarning(ctx context.Context, pair canonical.Pair, cached CachedResult) {
	key := pair.String()
	s.warningMu.Lock()
	s.warningState[key] = cached.WarningFired
	if !cached.WarningFired {
		delete(s.notified, key)
	}
	deliver := s.onWarning != nil && cached.WarningFired && !s.notified[key]
	if deliver {
		s.notified[key] = true
	}
	s.warningMu.Unlock()
	if !deliver {
		return
	}
	if err := s.onWarning(ctx, pair, cached); err != nil {
		s.warningMu.Lock()
		if s.warningState[key] {
			delete(s.notified, key)
		}
		s.warningMu.Unlock()
	}
}

// warningPersists debounces a raw firing until it held for s.persistence on the comparison clock `at`, across two refreshes; a clear refresh resets the streak.
// An unpublished streak older than s.maxGap restarts (time across an unevaluated gap is no evidence); a published one keeps its streak so an outage does not flap the webhook.
// Returns rawFiring when disabled (persistence <= 0), plus the streak start to persist (zero when clear).
func (s *Service) warningPersists(pairKey string, rawFiring bool, at time.Time) (bool, time.Time) {
	if s.persistence <= 0 {
		return rawFiring, time.Time{}
	}
	s.warningMu.Lock()
	defer s.warningMu.Unlock()
	if !rawFiring {
		delete(s.firingSince, pairKey)
		return false, time.Time{}
	}
	streak, ok := s.firingSince[pairKey]
	gapped := ok && at.Sub(streak.last) > s.maxGap && !s.warningState[pairKey]
	if !ok || at.Before(streak.since) || gapped {
		// First firing of a new streak, a non-monotonic comparison clock,
		// or an unobserved gap: start the persistence clock here and hold.
		s.firingSince[pairKey] = firingStreak{since: at, last: at}
		return false, at
	}
	if at.After(streak.last) {
		streak.last = at
		s.firingSince[pairKey] = streak
	}
	return at.Sub(streak.since) >= s.persistence, streak.since
}

// restoreWarningState seeds the debounce streak and edge latch from the previous process's cached result, once per pair, so a restart neither publishes a false all-clear
// nor re-fires divergence.firing. A read error leaves the pair unrestored for retry.
func (s *Service) restoreWarningState(ctx context.Context, pairKey, cacheKey string, at time.Time) {
	s.warningMu.Lock()
	done := s.restored[pairKey]
	s.warningMu.Unlock()
	if done {
		return
	}
	var prior CachedResult
	body, err := s.cache.Get(ctx, cacheKey).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		if s.logger != nil {
			s.logger.Warn("divergence: restore warning state failed; retrying next refresh",
				"pair", pairKey, "err", err)
		}
		return
	default:
		if uerr := json.Unmarshal(body, &prior); uerr != nil {
			prior = CachedResult{}
		}
	}
	since := prior.FiringSince
	if since.IsZero() && prior.WarningFired && s.persistence > 0 {
		// Written before FiringSince existed: it was published firing, so
		// its streak had already persisted a full window.
		since = at.Add(-s.persistence)
	}
	s.warningMu.Lock()
	defer s.warningMu.Unlock()
	if s.restored[pairKey] {
		return
	}
	s.restored[pairKey] = true
	if _, live := s.firingSince[pairKey]; !live && !since.IsZero() {
		// The cache holds no last-firing time; since is the conservative
		// stand-in (it can only make the gap test stricter).
		s.firingSince[pairKey] = firingStreak{since: since, last: since}
	}
	if _, live := s.warningState[pairKey]; !live && prior.WarningFired {
		s.warningState[pairKey] = true
		s.notified[pairKey] = true
	}
}

// flushObservations persists one row per successful (pair, reference); failed references are skipped (already surfaced via the cache's Failures map).
func (s *Service) flushObservations(
	ctx context.Context,
	pair canonical.Pair,
	ourPrice float64,
	res Result,
	observedAt time.Time,
) {
	for refName, refPrice := range res.Sources {
		if refPrice == 0 {
			// Defensive: a reference reporting zero would produce a
			// divide-by-zero on delta. Skip with no log; this is
			// the comparator's job to surface.
			continue
		}
		deltaPct := (ourPrice - refPrice) / refPrice * 100.0
		firing := absFloat(deltaPct) > s.threshold
		// ADR-0003: price/delta fields are decimal strings (shortest round-trip, as api/v1.moneyStr), so no raw float64 is bound into a NUMERIC column.
		if err := s.sink.RecordObservation(ctx, ObservationRecord{
			Pair:          pair,
			Reference:     refName,
			OurPrice:      strconv.FormatFloat(ourPrice, 'f', -1, 64),
			RefPrice:      strconv.FormatFloat(refPrice, 'f', -1, 64),
			DeltaPct:      strconv.FormatFloat(deltaPct, 'f', -1, 64),
			Firing:        firing,
			ObservedAt:    observedAt,
			RefObservedAt: res.SourcesAsOf[refName],
		}); err != nil && s.logger != nil {
			// Best-effort write — the Redis cache (load-bearing for
			// flags.divergence_warning) already succeeded. Log so
			// operators see the durable-mirror gap.
			s.logger.Warn("divergence: sink RecordObservation failed",
				"pair", pair.String(),
				"reference", refName,
				"err", err)
		}
	}
}

// absFloat is a tiny helper kept here (math.Abs imports the heavier
// math package the rest of this file doesn't need).
func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// AssetVerdict is [Service.LookupCached]'s by-asset answer. Firing and
// Checked are asset-level facts aggregated across every quote of the
// base; Detail is one pair's row for display, and its per-pair counts
// must not be read as an asset-level verdict.
type AssetVerdict struct {
	// Firing is the OR of the per-pair WarningFired flags.
	Firing bool
	// Checked is true when ANY quote's comparison met the source
	// quorum, i.e. the asset was cross-checked at least once.
	Checked bool
	// Detail is the representative pair: the firing pair with the
	// largest divergence when any fires, else the largest divergence.
	Detail CachedResult
}

// LookupCached returns the BASE asset's verdict ORed across its quotes; cache errors other than redis.Nil are surfaced, so callers must not assume warning=false on an outage.
func (s *Service) LookupCached(ctx context.Context, asset canonical.Asset) (AssetVerdict, bool, error) {
	idxKey := cachekeys.DivergenceBaseIndex(asset)
	quotes, err := s.cache.SMembers(ctx, idxKey.String()).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return AssetVerdict{}, false, fmt.Errorf("divergence: index smembers %s: %w", idxKey, err)
	}
	if len(quotes) == 0 {
		return AssetVerdict{}, false, nil
	}

	var (
		agg       CachedResult
		found     bool
		warning   bool
		checked   bool
		bestFire  bool    // whether `agg` currently holds a firing pair
		bestDelta float64 // |DivergencePct| of the representative pair held in `agg`
	)
	for _, q := range quotes {
		cached, ok, qerr := s.lookupCachedQuote(ctx, asset, q)
		if qerr != nil {
			return AssetVerdict{}, false, qerr
		}
		if !ok {
			continue
		}
		if cached.WarningFired {
			warning = true
		}
		// The same quorum RefreshPair gates WarningFired on.
		if cached.SuccessCount >= s.minSources {
			checked = true
		}

		// Pick the representative detail row: prefer firing pairs over
		// non-firing, and within the same firing-status prefer the
		// larger |divergence|. The first contributing pair always
		// wins its slot (firstContrib short-circuits the comparison).
		delta := absFloat(cached.DivergencePct)
		switch {
		case !found:
			agg, bestFire, bestDelta = cached, cached.WarningFired, delta
		case cached.WarningFired && !bestFire:
			agg, bestFire, bestDelta = cached, true, delta
		case cached.WarningFired == bestFire && delta >= bestDelta:
			agg, bestDelta = cached, delta
		}
		found = true
	}
	if !found {
		return AssetVerdict{}, false, nil
	}
	return AssetVerdict{Firing: warning, Checked: checked, Detail: agg}, true, nil
}

// lookupCachedQuote reads one pair's cached result; ok=false means it contributes nothing, err is a real cache error that aborts the whole lookup.
func (s *Service) lookupCachedQuote(ctx context.Context, asset canonical.Asset, q string) (result CachedResult, ok bool, err error) {
	quote, perr := canonical.ParseAsset(q)
	if perr != nil {
		// A malformed member can't be turned back into a key; skip it
		// rather than fail the whole lookup. The index is
		// worker-written from canonical assets, so this is a
		// defensive guard, not an expected path.
		return CachedResult{}, false, nil //nolint:nilerr // malformed index member is skipped, not a propagated error
	}
	pair := canonical.Pair{Base: asset, Quote: quote}
	key := cachekeys.Divergence(pair)
	raw, gerr := s.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(gerr, redis.Nil) {
		// Value expired but the index member lingered (the set's own
		// TTL hasn't fired yet). Treat as "no contribution", not an
		// error — redis.Nil is an expected miss, per LookupCached's
		// doc comment.
		return CachedResult{}, false, nil //nolint:nilerr // redis.Nil is a cache miss, not a propagated error
	}
	if gerr != nil {
		return CachedResult{}, false, fmt.Errorf("divergence: cache get %s: %w", key, gerr)
	}
	var cached CachedResult
	if uerr := json.Unmarshal(raw, &cached); uerr != nil {
		return CachedResult{}, false, fmt.Errorf("divergence: unmarshal cached result: %w", uerr)
	}
	return cached, true, nil
}

// LookupCachedPair reads one exact (base, quote) entry; per-pair read paths must use it, since LookupCached ORs in other quotes' verdicts (a GBP warning on a clean USD price).
func (s *Service) LookupCachedPair(ctx context.Context, pair canonical.Pair) (CachedResult, bool, error) {
	key := cachekeys.Divergence(pair)
	raw, err := s.cache.Get(ctx, key.String()).Bytes()
	if errors.Is(err, redis.Nil) {
		return CachedResult{}, false, nil
	}
	if err != nil {
		return CachedResult{}, false, fmt.Errorf("divergence: cache get %s: %w", key, err)
	}
	var cached CachedResult
	if err := json.Unmarshal(raw, &cached); err != nil {
		return CachedResult{}, false, fmt.Errorf("divergence: unmarshal cached result: %w", err)
	}
	return cached, true, nil
}

// LookupCachedPairVerdict is [Service.LookupCachedPair] plus the same SuccessCount >= minSources gate as LookupCached, so `checked` cannot drift from the threshold WarningFired used.
func (s *Service) LookupCachedPairVerdict(ctx context.Context, pair canonical.Pair) (firing, checked bool, window time.Duration, err error) {
	cached, found, err := s.LookupCachedPair(ctx, pair)
	if err != nil || !found {
		return false, false, 0, err
	}
	return cached.WarningFired, cached.SuccessCount >= s.minSources,
		time.Duration(cached.WindowSeconds) * time.Second, nil
}
