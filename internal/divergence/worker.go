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
)

// Cache is the Redis subset the [Service] needs. Declared as an
// interface so tests can substitute miniredis or a fake without
// pulling the full redis.UniversalClient surface.
//
// SAdd / SMembers / Expire were added for F-1344: the worker keys
// divergence results per-pair (`div:<base>/<quote>`) and maintains a
// per-base index SET (`div:idx:<base>`) so the by-asset reader can
// discover and OR every quote's WarningFired flag without a
// blocking KEYS/SCAN on the hot path. redis.UniversalClient
// satisfies all five methods.
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
	OurPrice      float64 `json:"our_price"`
	Median        float64 `json:"median"`
	DivergencePct float64 `json:"divergence_pct"`

	// WarningFired is evaluated by the worker and cached so API
	// readers don't need to know the threshold values. It fires when
	// SuccessCount >= MinSourcesForWarning AND EITHER
	//
	//   - DivergencePct > Threshold — the median of the references
	//     disagrees with our price; or
	//   - AgreementCount == 0 — NO responding reference corroborates
	//     our price within Threshold (MNY-22).
	//
	// The second leg exists because the median gate alone is blind to
	// symmetric disagreement: references straddling our price (one
	// +8%, one −8%) produce a median equal to our price and a
	// DivergencePct of ~0, so total disagreement read as agreement.
	// See [Service.RefreshPair] for why the leg is "nobody agrees"
	// rather than "somebody disagrees".
	//
	// W3-guards-2: the raw condition above must additionally have
	// PERSISTED for at least ServiceOptions.WarningPersistence (default
	// 5m) before this flips true. OurPrice is a shortest-window VWAP
	// while the references are instantaneous spot quotes, so on a fast
	// price move the VWAP legitimately lags the spot and the raw
	// condition trips for up to one window even though nothing is wrong;
	// that transient self-clears as the average rolls past the move. A
	// genuine divergence persists past the window and still fires. See
	// [Service.warningPersists].
	//
	// A below-quorum refresh reaches no verdict, so it carries the last
	// evaluated value forward instead of asserting false; SuccessCount
	// below the quorum is what marks the entry unchecked.
	WarningFired bool `json:"warning_fired"`

	// FiringSince is the comparison time the current uninterrupted raw
	// divergence began, zero while the raw condition is clear. It is the
	// durable half of the WarningPersistence debounce: a restarted worker
	// resumes the streak from it instead of restarting the clock, which
	// would republish WarningFired=false for a pair that never stopped
	// diverging. See [Service.restoreWarningState].
	FiringSince time.Time `json:"firing_since,omitzero"`

	// Sources / Failures mirror Result, kept for operator
	// dashboards.
	Sources  map[string]float64 `json:"sources,omitempty"`
	Failures map[string]string  `json:"failures,omitempty"`

	// SuccessCount + FailureCount counters for the run.
	SuccessCount int `json:"success_count"`
	FailureCount int `json:"failure_count"`

	// AgreementCount is how many successful references corroborated
	// OurPrice within the worker's threshold — the ADR-0019 Phase 3
	// cross-oracle agreement input ([CountAgreeing]). Distinct from
	// SuccessCount ("how many responded"): SuccessCount=5,
	// AgreementCount=4 reads "five references answered, four agree
	// with us".
	//
	// CS-087 semantics: failed references neither agree nor
	// disagree, so consumers MUST gate on SuccessCount before
	// interpreting this — SuccessCount=0 ⇒ AgreementCount=0 means
	// "unchecked", not "unanimous disagreement".
	AgreementCount int `json:"agreement_count"`

	// Pinned marks an entry whose OurPrice was a frozen pair's pinned
	// last-known-good, not a fresh VWAP ([Service.RefreshPinnedPair]).
	// The references and Median are fresh; DivergencePct and
	// AgreementCount measure the pinned value and are no verdict or
	// confidence input, and WarningFired/FiringSince are carried forward
	// from the last evaluated refresh.
	Pinned bool `json:"our_price_pinned,omitempty"`

	// ComputedAt is when the worker wrote this result. RFC 3339 UTC.
	ComputedAt time.Time `json:"computed_at"`
}

// ObservationSink is the optional durable-mirror seam for
// divergence observations. The Service calls RecordObservation
// once per (pair, reference) tuple every refresh tick, capturing
// the our_price / ref_price / delta_pct triple plus a firing/clear
// status.
//
// Today the worker writes only the aggregate result + a boolean
// firing flag to Redis with a TTL. The historical per-reference
// deltas are lost. The durable mirror persists them so the
// explorer /divergences page (explorer-data-inventory.md
// §7.19) can plot the actual divergence over time and so incident
// post-mortems can verify "Reflector drifted N% from us at ledger
// X" against ground truth.
//
// Implementations must NOT block the worker's hot path on network
// failures. Production wires
// `internal/storage/timescale.DivergenceSink`.
type ObservationSink interface {
	// RecordObservation persists one (pair, reference) comparison.
	// firing = true when |delta_pct| exceeded the per-reference
	// threshold at observation time.
	RecordObservation(ctx context.Context, obs ObservationRecord) error
}

// ObservationRecord is the per-(pair, reference) observation passed
// to ObservationSink. Decoupled from the internal CachedResult shape
// so the sink can evolve without the Service changing.
//
// Canonical definition lives in [domain.DivergenceObservationRecord]
// (D8 M0-1: internal/storage/timescale reads/writes this shape and
// must not import upward into this package to do so); this is a
// transparent alias so every existing caller of
// divergence.ObservationRecord is unaffected.
type ObservationRecord = domain.DivergenceObservationRecord

// DefaultWarningPersistence is the debounce window applied to
// WarningFired when [ServiceOptions.WarningPersistence] is unset. It is
// one shortest-window VWAP horizon — the same 5m as the default VWAP
// window ([orchestrator.DefaultWindows][0]), [cachekeys.DivergenceTTL],
// and the default divergence-refresh interval. That is exactly the
// horizon over which a fast-move gap between our windowed VWAP and a
// reference spot self-clears, so a divergence that outlives it is real
// rather than the artefact of the two values pricing different instants
// (W3-guards-2).
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

	// WarningPersistence is the minimum wall-clock duration a raw
	// divergence (DivergencePct > Threshold, or nobody agreeing) must
	// hold — across at least two refreshes — before WarningFired is
	// published true. It debounces the structural mismatch between our
	// side (a shortest-window VWAP) and the references (instantaneous
	// spot quotes): on a fast price move the VWAP lags the spot by up
	// to one window, so a legitimate move momentarily reads as a
	// divergence that self-clears within a window, while a genuine
	// divergence persists past it (W3-guards-2). It is a debounce, not
	// a threshold bump, so it suppresses ONLY transient gaps and never
	// blinds a sustained one — however small.
	//
	// Zero (unset) defaults to [DefaultWarningPersistence] (5m). A
	// NEGATIVE value disables the gate (immediate firing, the
	// pre-debounce behaviour) for operators who accept the
	// false-positive trade-off, and for unit tests isolating the
	// threshold/agreement logic from the debounce.
	WarningPersistence time.Duration

	// RefreshInterval is the expected spacing between two consecutive
	// refreshes of the same pair (the caller's cadence). A firing streak
	// whose previous firing observation is more than twice
	// max(RefreshInterval, WarningPersistence) old has an unobserved gap
	// in it, so it restarts instead of maturing on one post-gap sample.
	// Zero means the cadence is at most WarningPersistence.
	RefreshInterval time.Duration

	// PerReferenceTimeout is forwarded to [Compare] via
	// [CompareOptions]. Default 5s.
	PerReferenceTimeout time.Duration

	// ObservationSink, when non-nil, receives one record per (pair,
	// reference) tuple every refresh. Persists the per-reference
	// delta history that the Redis cache discards. Optional — nil
	// keeps legacy Redis-only behaviour.
	ObservationSink ObservationSink

	// Logger, when non-nil, receives WARN-level log lines for sink
	// failures. Optional — nil silences the path (legacy behaviour).
	// The aggregator passes its component logger so failures land
	// in the same journal stream as the rest of the orchestrator.
	Logger *slog.Logger

	// OnWarningFired, when non-nil, is invoked from RefreshPair on
	// the EDGE — a refresh that flips a pair from "below
	// threshold" → "above threshold" (or fires for the first
	// time). Best-effort: errors / panics inside the hook do not
	// propagate. F-1249 (codex audit-2026-05-12): the aggregator
	// wires this to customerwebhook.Fanout.Publish so dashboard
	// hooks subscribed to `divergence.firing` get a callback.
	//
	// Edge-only firing (vs every-refresh-while-firing) prevents
	// the API binary's delivery queue from re-spamming subscribers
	// on every aggregator tick a divergence stays above the
	// threshold. The fanout service is itself idempotent on the
	// subscriber list but the callbacks would still pile up.
	OnWarningFired WarningHook
}

// WarningHook is the callback shape for edge-triggered divergence
// warnings. `cached` is the same CachedResult Redis has just
// stored; reuse it to build the webhook payload.
type WarningHook func(ctx context.Context, pair canonical.Pair, cached CachedResult)

// Service wraps a set of References + a cache writer, exposing a
// single [Service.RefreshPair] method the aggregator hooks into
// after writing each fresh VWAP. Writes the cached result to
// Redis at the `div:<base>/<quote>` key per ADR-0007.
//
// Service is safe for concurrent RefreshPair calls — the
// underlying Cache and References must also be concurrent-safe
// (they all are by contract).
type Service struct {
	refs        []Reference
	cache       Cache
	threshold   float64
	minSources  int
	timeout     time.Duration
	persistence time.Duration
	maxGap      time.Duration
	sink        ObservationSink
	// logger is optional — nil-safe. When set, sink failures are
	// logged at WARN per (pair, reference) instead of being
	// silently dropped. Pre-2026-05-10 the missing-log meant
	// Postgres write failures (e.g. during the disk-full SEV-2
	// cascade) silently dropped every divergence_observations row
	// — operators only saw it when the explorer's /divergences
	// page surfaced a gap, days later.
	logger *slog.Logger

	// onWarning + warningState power the edge-triggered fan-out
	// hook (F-1249 codex audit-2026-05-12). `warningState` maps
	// pair.String() → the WarningFired of the most recent EVALUATED
	// refresh; the hook fires only on `false → true` transitions, and a
	// below-quorum refresh carries this value forward untouched.
	//
	// firingSince (W3-guards-2) maps pair.String() → the current
	// uninterrupted raw-firing streak (its first and latest firing
	// comparison times), and powers the WarningPersistence debounce in
	// [Service.warningPersists]. It is cleared the moment a refresh
	// finds the raw condition clear.
	//
	// restored records the pairs whose maps were seeded from the previous
	// process's cached result ([Service.restoreWarningState]). warningMu
	// guards all three maps.
	onWarning    WarningHook
	warningMu    sync.Mutex
	warningState map[string]bool
	firingSince  map[string]firingStreak
	restored     map[string]bool
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
		timeout = 5 * time.Second
	}
	// WarningPersistence: 0 (unset) → default debounce; a NEGATIVE
	// value is the explicit "disable the gate" opt-out and is left
	// intact for warningPersists to treat as off. Unlike the fields
	// above, 0 here is a meaningful value (= fire immediately), so we
	// deliberately map it to the safe default rather than clamp-to-off.
	persistence := opts.WarningPersistence
	if persistence == 0 {
		persistence = DefaultWarningPersistence
	}
	return &Service{
		maxGap:       2 * max(persistence, opts.RefreshInterval),
		refs:         independentReferences(opts.References),
		cache:        opts.Cache,
		threshold:    threshold,
		minSources:   minSources,
		timeout:      timeout,
		persistence:  persistence,
		sink:         opts.ObservationSink,
		logger:       opts.Logger,
		onWarning:    opts.OnWarningFired,
		warningState: map[string]bool{},
		firingSince:  map[string]firingStreak{},
		restored:     map[string]bool{},
	}, nil
}

// RefreshPair runs one divergence check for the supplied pair +
// our-price, then writes the cached result to Redis at
// div:<base>/<quote> and records the quote in the per-base index
// set. The aggregator calls this from the bucket-close path AFTER
// the VWAP has been written to its own Redis key.
//
// Returns nil when the worker has no References configured (silent
// no-op so an operator who hasn't enabled divergence yet doesn't
// see a torrent of "skipped" log lines). Returns the underlying
// error when Compare's network calls all fail, but cache-write
// errors are returned separately so the caller can decide whether
// to retry.
// ErrNoReferenceResponded is returned by RefreshPair when references ARE
// configured but every one failed for this pair (SuccessCount == 0). The
// cache is still written (recording the outage state), but the caller gets a
// distinct signal so a total reference outage can be alerted on instead of
// silently counting as a successful refresh (CS-088). It is NOT returned when
// no references are configured at all — that's an intentional-disabled state.
var ErrNoReferenceResponded = errors.New("divergence: no reference responded for pair")

func (s *Service) RefreshPair(ctx context.Context, pair canonical.Pair, ourPrice float64, observedAt time.Time) error {
	return s.refresh(ctx, pair, ourPrice, observedAt, false)
}

// RefreshPinnedPair is [Service.RefreshPair] for a pair whose served
// price is frozen: pinnedPrice is the last-known-good the freeze keeps
// serving, not a fresh observation. The references are still polled, so
// the entry's Median stays current for the freeze's release corroboration,
// but the comparison against the pinned value reaches no verdict — a
// freeze on a genuine repricing would otherwise be reported as our price
// diverging from the market it refused to follow. So the entry is marked
// [CachedResult.Pinned], the warning verdict and its persistence streak are
// carried forward as on a below-quorum refresh, and no warning hook fires.
// Per-reference observations are still persisted: each row carries the
// pinned price it was measured against, and is the operator's evidence of
// whether the market moved away from the frozen value.
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

	// MNY-22: the warning gate is median-vs-ourPrice OR nobody-agrees.
	// The median leg alone masks symmetric disagreement — two
	// references at ±8% put the median exactly on our price, so
	// DivergencePct ≈ 0 and the warning stayed silent while NO
	// reference actually corroborated us. AgreementCount was already
	// computed for the confidence score and captured precisely that,
	// but nothing gated on it.
	//
	// The leg is "AgreementCount == 0" (no responding reference
	// corroborates us), NOT "any reference disagrees": with three or
	// more references a single flaky one would otherwise pin the
	// warning on permanently, which is a false-positive machine rather
	// than a signal. Both legs are gated on SuccessCount >= minSources
	// per CS-087 — with no responses, AgreementCount == 0 means
	// "unchecked", not "unanimous disagreement".
	checked := res.SuccessCount >= s.minSources
	evaluated := checked && !pinned

	// W3-guards-2: our value is a shortest-window VWAP; the references
	// are instantaneous spot quotes. On a fast price move the VWAP lags
	// the spot for up to one window, so rawFiring trips on a legitimate
	// move that self-clears within that window. Only publish the warning
	// once the raw condition has PERSISTED beyond that horizon — a
	// genuine divergence does, a fast-move artefact does not. The
	// comparison time (the instant each reference priced) is the clock;
	// fall back to wall time when the caller supplies none.
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
		rawFiring := res.DivergencePct > s.threshold || agreeing == 0
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
		ComputedAt:     time.Now().UTC(),
	}

	body, err := json.Marshal(cached)
	if err != nil {
		// Should be unreachable — CachedResult has no func/chan
		// fields. Wrap for diagnostic completeness.
		return fmt.Errorf("divergence: marshal cached result: %w", err)
	}

	// F-1344 (G16-03): write a PER-PAIR key, not a per-base key. The
	// orchestrator calls RefreshPair once per configured pair; a
	// per-base key let the last pair in iteration order clobber the
	// asset's divergence verdict. The per-pair key keeps each pair's
	// result independent; the by-asset reader (LookupCached) ORs them.
	if err := s.cache.Set(ctx, key.String(), body, cachekeys.DivergenceTTL).Err(); err != nil {
		return fmt.Errorf("divergence: cache set %s: %w", key, err)
	}

	// Maintain the per-base quote index so LookupCached can discover
	// which per-pair keys to OR for a given base. SADD is idempotent;
	// the Expire refreshes the set's TTL on every write so it drains
	// in lock-step with the value keys (a base whose pairs stop
	// refreshing loses its index after DivergenceTTL rather than
	// pinning dead quote members forever).
	idxKey := cachekeys.DivergenceBaseIndex(pair.Base)
	if err := s.cache.SAdd(ctx, idxKey.String(), pair.Quote.String()).Err(); err != nil {
		return fmt.Errorf("divergence: index sadd %s: %w", idxKey, err)
	}
	if err := s.cache.Expire(ctx, idxKey.String(), cachekeys.DivergenceTTL).Err(); err != nil {
		return fmt.Errorf("divergence: index expire %s: %w", idxKey, err)
	}

	// Durable per-reference mirror. Best-effort: a sink failure must
	// not surface to the caller because the Redis cache write — the
	// load-bearing operation that drives flags.divergence_warning
	// on the API response — has already succeeded.
	if s.sink != nil {
		// COR-12: stamp the durable observation with the COMPARISON
		// time — the same instant handed to Compare, and therefore the
		// instant each reference priced — not the wall clock at write
		// time, which trails it by the whole reference fan-out (up to
		// PerReferenceTimeout). observed_at is also part of the row's
		// conflict key, so this additionally makes a re-run of the same
		// comparison idempotent instead of inserting a near-duplicate.
		// A caller that supplies no comparison time falls back to the
		// previous behaviour rather than persisting a zero timestamp.
		stampedAt := observedAt.UTC()
		if observedAt.IsZero() {
			stampedAt = cached.ComputedAt
		}
		s.flushObservations(ctx, pair, ourPrice, res, stampedAt)
	}

	if evaluated {
		s.recordWarning(ctx, pair, cached)
	}
	// CS-088: references were configured but none responded — the cache now
	// holds a SuccessCount=0 result carrying the last verdict forward, which
	// nothing on the wire distinguishes from a fresh one except the quorum.
	// Signal the outage so the refresh loop can emit a distinct outcome and
	// page on a dark checker.
	if res.SuccessCount == 0 {
		return ErrNoReferenceResponded
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

// recordWarning latches an evaluated refresh's verdict and runs the
// F-1249 edge-triggered hook: it fires only on `false → true`, so the
// customer-webhook queue gets one POST per episode rather than one per
// refresh-while-firing, and a return to false re-arms it.
func (s *Service) recordWarning(ctx context.Context, pair canonical.Pair, cached CachedResult) {
	s.warningMu.Lock()
	prev := s.warningState[pair.String()]
	s.warningState[pair.String()] = cached.WarningFired
	s.warningMu.Unlock()
	if s.onWarning != nil && cached.WarningFired && !prev {
		s.onWarning(ctx, pair, cached)
	}
}

// warningPersists is the W3-guards-2 debounce that separates a genuine
// sustained divergence from the transient artefact of comparing our
// shortest-window VWAP against an instantaneous reference spot. A raw
// firing only becomes a published WarningFired once it has held for at
// least s.persistence, measured on the comparison clock (`at`), across
// at least two refreshes.
//
// It is time-based rather than tick-count based so the suppression is
// robust to the aggregator's refresh cadence. A refresh that finds the
// raw condition CLEAR resets the streak, so each fresh onset starts its
// own persistence clock (and a real divergence that briefly dips below
// threshold restarts, which is the conservative choice). A not-yet-
// published streak whose last firing observation is older than s.maxGap
// also restarts: the pair went unevaluated (no VWAP, parse error, below
// quorum, process down) and elapsed time across that gap is not evidence
// the condition held. A published warning keeps its streak across a gap
// so an outage does not flap it off and re-fire the webhook. Returns
// rawFiring unchanged when the gate is disabled (s.persistence <= 0).
// The second result is the streak start to persist, zero when clear.
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

// restoreWarningState seeds a pair's debounce streak and webhook edge
// latch from the result the previous process cached at cacheKey, once per
// pair per process. Without it a restart starts both maps empty: the
// first refresh of a still-diverging pair overwrites the cached
// WarningFired=true with false (an affirmative all-clear) for a whole
// persistence window, then re-fires divergence.firing for a divergence
// that never stopped. The cache TTL bounds how stale the seeded streak
// can be. A read error leaves the pair unrestored so the next refresh
// retries.
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
	}
}

// flushObservations persists one durable row per (pair, reference)
// for the current refresh. Each successful reference contributes
// its observed price + delta + firing flag.
//
// Failures (references that errored out) are deliberately skipped
// — there's no observation to record, and the failure is already
// surfaced via the Redis cache's Failures map for operator
// dashboards.
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
		// ADR-0003: the record's price/delta fields are decimal
		// strings, not float64 — format at this boundary using the
		// shortest round-trip representation (same idiom as
		// api/v1.moneyStr) so RecordObservation never binds a raw
		// float64 into a NUMERIC column.
		if err := s.sink.RecordObservation(ctx, ObservationRecord{
			Pair:       pair,
			Reference:  refName,
			OurPrice:   strconv.FormatFloat(ourPrice, 'f', -1, 64),
			RefPrice:   strconv.FormatFloat(refPrice, 'f', -1, 64),
			DeltaPct:   strconv.FormatFloat(deltaPct, 'f', -1, 64),
			Firing:     firing,
			ObservedAt: observedAt,
		}); err != nil && s.logger != nil {
			// Best-effort write — the Redis cache (load-bearing for
			// flags.divergence_warning) already succeeded. Log so
			// operators see the durable-mirror gap; pre-2026-05-10
			// this was a fully-silent drop.
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

// LookupCached returns the divergence verdict for a BASE asset,
// aggregated across every quote that asset trades against (F-1344).
// Both verdicts are ORs over the per-pair entries, so neither depends
// on the order pairs are refreshed in nor on which pair is picked as
// [AssetVerdict.Detail]: a below-quorum quote with a large delta cannot
// make a cross-checked asset read as unchecked.
//
// Returns ([AssetVerdict{}], false, nil) when the base has no live
// per-pair entries (the worker hasn't run for it yet, or every
// pair's TTL has elapsed). API hot-path consumers call this when
// serving /v1/price to decide whether to set flags.divergence_warning.
//
// Cache-read errors other than redis.Nil are surfaced — callers
// should NOT silently set flags.divergence_warning=false on a
// transient cache outage; better to keep the previous response's
// flag value (or fail-open).
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

// lookupCachedQuote reads and decodes one BASE/quote pair's cached
// divergence result. ok is false when the pair contributes nothing to
// the aggregate (unparseable index member or expired cache entry);
// err is non-nil only for a real cache error that should abort the
// whole lookup.
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

// LookupCachedPair reads the cached divergence result for exactly one
// (base, quote) pair — the quote-specific counterpart to [Service.LookupCached],
// which ORs every quote of the base together. A caller serving a value
// for a SPECIFIC quote (the API's per-pair read paths) must use this:
// ORing in another quote's verdict attaches a warning computed against
// a market the served value never touched (GH-1045 — a GBP divergence
// warning was shown on a clean USD price because LookupCached folded
// every quote of the base into one flag).
//
// Returns (_, false, nil) on a cache miss (no key written yet, or the
// entry TTL'd out) — same posture as LookupCached. Read/decode errors
// are surfaced rather than swallowed, also matching LookupCached.
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

// LookupCachedPairVerdict is [Service.LookupCachedPair] plus the SAME
// quorum gate LookupCached applies per quote (SuccessCount >=
// minSources) — the quote-specific counterpart callers serving one
// exact pair need, so `checked` can never drift from the threshold
// WarningFired itself was gated on. It can only move the flag
// true→false: a below-quorum RefreshPair write never sets
// WarningFired true in the first place, so (firing=true, checked=false)
// cannot occur.
func (s *Service) LookupCachedPairVerdict(ctx context.Context, pair canonical.Pair) (firing, checked bool, err error) {
	cached, found, err := s.LookupCachedPair(ctx, pair)
	if err != nil || !found {
		return false, false, err
	}
	return cached.WarningFired, cached.SuccessCount >= s.minSources, nil
}
