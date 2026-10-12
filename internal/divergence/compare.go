package divergence

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Result is the outcome of one divergence comparison, with enough detail for the aggregator's flag-gating decision and operator dashboards.
type Result struct {
	// Pair is the pair being compared.
	Pair canonical.Pair

	// OurPrice is what our aggregator computed.
	//floatmoney:ok comparator-only diagnostic result (doc comment above), never stored or served directly — RecordObservation formats to decimal strings at the sink boundary
	OurPrice float64

	// Median is the median of references that returned a price.
	// Zero when SuccessCount == 0 (no references reported);
	// caller MUST check SuccessCount before reading Median.
	Median float64

	// DivergencePct is `abs(OurPrice - Median) / Median * 100`,
	// or 0 when SuccessCount == 0.
	DivergencePct float64

	// Sources maps reference Name() → reported price. Only
	// successful references appear here.
	Sources map[string]float64

	// SourcesAsOf maps reference Name() → when that reference's upstream
	// observed its Sources price (the quote's AsOf). Same keys as Sources.
	SourcesAsOf map[string]time.Time

	// Failures maps reference Name() → error message. Empty when
	// every reference succeeded; non-empty entries indicate which
	// sources we couldn't reach this run.
	Failures map[string]string

	// SuccessCount is len(Sources). Convenience for callers that
	// gate "is this divergence signal trustworthy?" — typically
	// require SuccessCount >= 2 before raising flags.divergence_warning.
	SuccessCount int

	// FailureCount is len(Failures). Combined with SuccessCount,
	// the operator can see the full reference universe outcome.
	FailureCount int

	// Outcomes maps EVERY reference Name() → a bounded outcome class
	// (one of [ReferenceOutcomes]): "ok" for a Sources entry, a failure
	// class for a Failures entry. Failures carries verbatim error text,
	// which is unbounded; this is its metric-label-safe twin.
	Outcomes map[string]string
}

// Bounded per-reference outcome classes recorded in [Result.Outcomes].
const (
	OutcomeOK                = "ok"
	OutcomeAssetUnsupported  = "asset_unsupported"
	OutcomePriceUnavailable  = "price_unavailable"
	OutcomeTooStale          = "too_stale_to_compare"
	OutcomeInvalidPrice      = "invalid_price"
	OutcomeTimeout           = "timeout"
	OutcomeDeadlineExceeded  = "overall_deadline_exceeded"
	OutcomePanicked          = "panicked"
	OutcomeError             = "error"
	invalidPriceFailureLabel = "non-positive or non-finite price"
)

// ReferenceOutcomes is the complete [Result.Outcomes] vocabulary.
var ReferenceOutcomes = []string{
	OutcomeOK, OutcomeAssetUnsupported, OutcomePriceUnavailable, OutcomeTooStale,
	OutcomeInvalidPrice, OutcomeTimeout, OutcomeDeadlineExceeded, OutcomePanicked, OutcomeError,
}

// errReferencePanicked wraps a recovered reference panic so
// [errorOutcome] can class it without parsing the message.
var errReferencePanicked = errors.New("reference panicked")

// CompareOptions tunes [Compare]'s behaviour.
type CompareOptions struct {
	// PerReferenceTimeout caps the time spent on each reference's
	// LookupQuote call. Default 5s. Set lower for hot-path use
	// where divergence is best-effort.
	PerReferenceTimeout time.Duration

	// MinSuccessForMedian — Compare returns a zero Median when
	// fewer than this many references returned successfully.
	// Default 1 (any reference is better than none); operators
	// can require 2+ for stronger consensus.
	MinSuccessForMedian int

	// OverallTimeout caps the WHOLE comparison; <= 0 defaults to [overallTimeoutFactor] x PerReferenceTimeout. The per-reference timeout is only advisory (a reference
	// that ignores ctx never returns), so this is the wall-clock ceiling; see [Compare] for how a reference that blows it is recorded.
	OverallTimeout time.Duration
}

// DefaultPerReferenceTimeout bounds each price-reference call when the caller sets none.
const DefaultPerReferenceTimeout = 5 * time.Second

// overallTimeoutFactor scales PerReferenceTimeout into the default overall budget; references run concurrently, so 2x leaves slack while still bounding the call.
const overallTimeoutFactor = 2

// overallDeadlineLabel is the Failures value for a reference that had not reported by the overall deadline, distinct from a per-reference timeout so "slow upstream" differs from "ignores cancellation".
const overallDeadlineLabel = OutcomeDeadlineExceeded

// Compare queries every reference in parallel and returns the divergence between ourPrice and the median of the successes.
func Compare(
	ctx context.Context,
	refs []Reference,
	pair canonical.Pair,
	ourPrice float64,
	observedAt time.Time,
	opts CompareOptions,
) Result {
	if opts.PerReferenceTimeout <= 0 {
		opts.PerReferenceTimeout = DefaultPerReferenceTimeout
	}
	if opts.MinSuccessForMedian <= 0 {
		opts.MinSuccessForMedian = 1
	}
	if opts.OverallTimeout <= 0 {
		opts.OverallTimeout = overallTimeoutFactor * opts.PerReferenceTimeout
	}

	res := Result{
		Pair:        pair,
		OurPrice:    ourPrice,
		Sources:     map[string]float64{},
		SourcesAsOf: map[string]time.Time{},
		Failures:    map[string]string{},
		Outcomes:    map[string]string{},
	}

	if len(refs) == 0 {
		return res
	}

	results := make(chan fetchOutcome, len(refs))

	// Names are resolved on THIS goroutine, before the fan-out, so a
	// reference that never reports can still be attributed at the
	// overall deadline below.
	pending := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		pending[safeName(ref)] = struct{}{}
	}

	for _, ref := range refs {
		go func(r Reference) {
			// Capture the reference's name once up-front so we can
			// report the failure even if r.Name() itself is what
			// panics (cheap defence; references are operator-supplied
			// in some deployments).
			name := safeName(r)

			// Recover a LookupQuote panic into Failures so one broken reference cannot take the comparison down, and also report it via worker.Report so
			// stellarindex_worker_panics_total (which the page rule reads) moves.
			defer func() {
				if rv := recover(); rv != nil {
					worker.Report(nil, "divergence-reference-lookup", rv)
					// Best-effort send; channel is buffered to len(refs)
					// so this is a non-blocking write.
					results <- fetchOutcome{
						name: name,
						err:  fmt.Errorf("%w: %v", errReferencePanicked, rv),
					}
				}
			}()

			perCtx, cancel := context.WithTimeout(ctx, opts.PerReferenceTimeout)
			defer cancel()
			q, err := r.LookupQuote(perCtx, pair, observedAt)
			if err == nil {
				err = checkComparable(q, pair, observedAt)
			}
			results <- fetchOutcome{name: name, price: q.Price, asOf: q.AsOf, err: err}
		}(ref)
	}
	prices := collectOutcomes(results, pending, len(refs), opts.OverallTimeout, &res)

	res.SuccessCount = len(res.Sources)
	res.FailureCount = len(res.Failures)

	if res.SuccessCount < opts.MinSuccessForMedian {
		return res
	}
	sort.Float64s(prices)
	res.Median = median(prices)
	if res.Median > 0 {
		res.DivergencePct = math.Abs(ourPrice-res.Median) / res.Median * 100.0
	}
	return res
}

// fetchOutcome is one reference's reply on [Compare]'s fan-in channel.
type fetchOutcome struct {
	name string
	//floatmoney:ok one reference's fan-in reply price, comparator-internal only (same class as Quote.Price / Result.OurPrice above)
	price float64
	asOf  time.Time
	err   error
}

// collectOutcomes drains `total` replies into res, or after `overall` records every reference still in `pending` as an overall-deadline failure. Returns the successful prices, unsorted.
// The channel is NEVER closed and is buffered to the reference count, so a goroutine abandoned at the deadline can send late without blocking, leaking or panicking.
func collectOutcomes(
	results <-chan fetchOutcome,
	pending map[string]struct{},
	total int,
	overall time.Duration,
	res *Result,
) []float64 {
	deadline := time.NewTimer(overall)
	defer deadline.Stop()

	prices := make([]float64, 0, total)
	for outstanding := total; outstanding > 0; outstanding-- {
		select {
		case o := <-results:
			delete(pending, o.name)
			switch {
			case o.err != nil:
				res.Failures[o.name] = classifyError(o.err)
				res.Outcomes[o.name] = errorOutcome(o.err)
			case !isFinitePositive(o.price):
				res.Failures[o.name] = invalidPriceFailureLabel
				res.Outcomes[o.name] = OutcomeInvalidPrice
			default:
				res.Sources[o.name] = o.price
				res.SourcesAsOf[o.name] = o.asOf
				res.Outcomes[o.name] = OutcomeOK
				prices = append(prices, o.price)
			}
		case <-deadline.C:
			// Partial result: record everyone still outstanding as a
			// deadline failure and serve what did arrive.
			for name := range pending {
				res.Failures[name] = overallDeadlineLabel
				res.Outcomes[name] = OutcomeDeadlineExceeded
			}
			return prices
		}
	}
	return prices
}

// CountAgreeing returns how many `sources` corroborate `ourPrice` within `thresholdPct` percent (a delta exactly AT the threshold agrees); an empty map returns 0, which consumers MUST read as "unchecked", never "every reference disagrees".
func CountAgreeing(ourPrice float64, sources map[string]float64, thresholdPct float64) int {
	if !isFinitePositive(ourPrice) || math.IsNaN(thresholdPct) || thresholdPct < 0 {
		return 0
	}
	n := 0
	for _, refPrice := range sources {
		if !isFinitePositive(refPrice) {
			continue
		}
		deltaPct := math.Abs(ourPrice-refPrice) / refPrice * 100.0
		if deltaPct <= thresholdPct {
			n++
		}
	}
	return n
}

// countOutcome returns how many entries of [Result.Outcomes] equal want —
// e.g. how many references were OutcomeAssetUnsupported, to separate
// structural non-coverage from a genuine reference failure.
func countOutcome(outcomes map[string]string, want string) int {
	n := 0
	for _, o := range outcomes {
		if o == want {
			n++
		}
	}
	return n
}

// errorOutcome is classifyError's bounded twin for [Result.Outcomes]:
// the same sentinels, but every unrecognised error collapses to one
// class instead of passing its text through.
func errorOutcome(err error) string {
	switch {
	case errors.Is(err, ErrAssetUnsupported):
		return OutcomeAssetUnsupported
	case errors.Is(err, ErrPriceUnavailable):
		return OutcomePriceUnavailable
	case errors.Is(err, ErrTooStaleToCompare):
		return OutcomeTooStale
	case errors.Is(err, errReferencePanicked):
		return OutcomePanicked
	case errors.Is(err, context.DeadlineExceeded):
		return OutcomeTimeout
	default:
		return OutcomeError
	}
}

// classifyError maps known sentinels to short stable dashboard labels; a recovered panic surfaces as "panicked: <text>" and unknown errors pass through verbatim.
func classifyError(err error) string {
	switch {
	case errors.Is(err, ErrAssetUnsupported):
		return "asset_unsupported"
	case errors.Is(err, ErrPriceUnavailable):
		return "price_unavailable"
	case errors.Is(err, ErrTooStaleToCompare):
		return "too_stale_to_compare"
	default:
		// Drop the "reference panicked:" prefix when the underlying
		// goroutine wrapped a panic — operator-facing label reads
		// "panicked: foo" rather than "reference panicked: foo".
		msg := err.Error()
		const prefix = "reference panicked: "
		if strings.HasPrefix(msg, prefix) {
			return "panicked: " + msg[len(prefix):]
		}
		return msg
	}
}

// safeName returns r.Name() under a panic guard, or "_unknown" if it panics (operator-supplied references might), so the failure label still surfaces.
func safeName(r Reference) (out string) {
	defer func() {
		if rv := recover(); rv != nil {
			out = "_unknown"
		}
	}()
	return r.Name()
}

// isFinitePositive guards against +/-Inf and NaN making it through
// JSON parsing of upstream responses, plus zero/negative prices.
// Any of those land us in the failure bucket.
func isFinitePositive(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0) && x > 0
}

// median returns the median of a sorted slice (mean of the two middle values for even length). The caller MUST sort ascending; it does
// not, keeping the comparator's hot path O(1).
func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2.0
}
