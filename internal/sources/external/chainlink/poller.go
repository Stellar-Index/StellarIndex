package chainlink

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// Poller implements external.Poller for Chainlink Data Feeds, emitting one OracleUpdate per new (feed_address, roundId); the dedup cache is in-memory, so a restart re-emits the latest round, which the storage PK rejects idempotently.
type Poller struct {
	// Client is the EVM JSON-RPC client for the operator-supplied endpoint.
	Client *Client

	// Logger receives per-feed warn/info messages. nil → slog.Default.
	Logger *slog.Logger

	// Interval overrides DefaultPollInterval. Zero = default.
	Interval time.Duration

	// FeedMap maps a canonical pair string ("crypto:BTC/fiat:USD") to the AggregatorV3 contract
	// address, decimals and invert flag. Operator-curated.
	FeedMap map[string]FeedSpec

	// Cache holds the last roundId emitted per feed address.
	Cache *roundCache

	// Concurrency caps in-flight feed eth_call requests per PollOnce tick (default 8), a polite
	// ceiling for shared RPC endpoints.
	Concurrency int

	// decimals is the per-feed on-chain decimals() verification state (see decimals.go).
	decimals *decimalsCache

	// now is the injectable clock for the decimals() refresh/retry cadence and the updatedAt
	// future-skew guard. nil → time.Now.
	now func() time.Time
}

// clock returns the poller's injectable wall clock (time.Now when unset).
func (p *Poller) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// NewPoller builds a Poller with defaults; rpcURL must include the API key for keyed providers.
func NewPoller(rpcURL string, feedMap map[string]FeedSpec) *Poller {
	// Seed zero-valued series per configured feed (the label set is bounded), so a healthy feed
	// is distinguishable from one whose counter was never wired.
	for k := range feedMap {
		obs.ChainlinkFeedDecimalsMismatchTotal.WithLabelValues("ingest", k)
		obs.ChainlinkFeedDecimalsVerifyFailedTotal.WithLabelValues("ingest", k)
		for _, o := range feedOutcomes {
			obs.ChainlinkFeedPollsTotal.WithLabelValues(k, o)
		}
		obs.ChainlinkFeedLastSuccessUnix.WithLabelValues(k)
	}
	return &Poller{
		Client:      NewClient(rpcURL, nil),
		Interval:    DefaultPollInterval,
		FeedMap:     feedMap,
		Cache:       newRoundCache(),
		Concurrency: 8,
		decimals:    newDecimalsCache(),
	}
}

// Name implements external.Connector.
func (p *Poller) Name() string { return SourceName }

// Class implements external.Connector.
func (p *Poller) Class() external.Class { return external.ClassOracle }

// PollInterval implements external.Poller.
func (p *Poller) PollInterval() time.Duration {
	if p.Interval <= 0 {
		return DefaultPollInterval
	}
	return p.Interval
}

// pollResult is one feed's outcome for a single PollOnce tick.
type pollResult struct {
	update canonical.OracleUpdate
	err    error
	pair   canonical.Pair
	round  Round
}

// Per-feed poll outcomes, the label values of obs.ChainlinkFeedPollsTotal.
const (
	outcomeEmitted        = "emitted"
	outcomeUnchanged      = "unchanged"
	outcomeStale          = "stale"
	outcomeCarriedForward = "carried_forward"
	outcomeError          = "error"
)

var feedOutcomes = []string{outcomeEmitted, outcomeUnchanged, outcomeStale, outcomeCarriedForward, outcomeError}

// recordFeedOutcome counts one feed's poll outcome and advances its last-success gauge when the
// feed was current.
func (p *Poller) recordFeedOutcome(pair canonical.Pair, outcome string) {
	obs.ChainlinkFeedPollsTotal.WithLabelValues(pair.String(), outcome).Inc()
	if outcome == outcomeEmitted || outcome == outcomeUnchanged {
		obs.ChainlinkFeedLastSuccessUnix.WithLabelValues(pair.String()).Set(float64(p.clock().Unix()))
	}
}

// failureOutcome maps a per-feed poll error to its outcome label.
func failureOutcome(err error) string {
	switch {
	case errors.Is(err, ErrStaleRound):
		return outcomeStale
	case errors.Is(err, ErrCarriedForwardRound):
		return outcomeCarriedForward
	default:
		return outcomeError
	}
}

// checkRoundCurrent refuses a carried-forward answer or one older than the feed's MaxAge. It runs
// before dedup so a frozen feed keeps reporting stale instead of reading as "unchanged".
func checkRoundCurrent(pair canonical.Pair, spec FeedSpec, rnd Round, now time.Time) error {
	if rnd.AnsweredInRound != nil && rnd.RoundID != nil && rnd.AnsweredInRound.Cmp(rnd.RoundID) < 0 {
		return fmt.Errorf("%w: %s round=%s answeredInRound=%s",
			ErrCarriedForwardRound, pair.String(), rnd.RoundID, rnd.AnsweredInRound)
	}
	maxAge := spec.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge(pair.String())
	}
	if age := now.Sub(rnd.UpdatedAt); age > maxAge {
		return fmt.Errorf("%w: %s round=%s updated %s ago, max %s",
			ErrStaleRound, pair.String(), rnd.RoundID, age.Truncate(time.Second), maxAge)
	}
	return nil
}

// pollFeed resolves decimals, fetches the latest round, refuses it if not current, and projects and commits it if new. ok=false means
// "already emitted this round", the one path PollOnce's fan-in must not receive a result for. Every path records the feed's outcome.
func (p *Poller) pollFeed(ctx context.Context, pair canonical.Pair, spec FeedSpec) (pollResult, bool) {
	res, ok := p.pollFeedOnce(ctx, pair, spec)
	switch {
	case !ok:
		p.recordFeedOutcome(pair, outcomeUnchanged)
	case res.err != nil:
		p.recordFeedOutcome(pair, failureOutcome(res.err))
	default:
		p.recordFeedOutcome(pair, outcomeEmitted)
	}
	return res, ok
}

func (p *Poller) pollFeedOnce(ctx context.Context, pair canonical.Pair, spec FeedSpec) (pollResult, bool) {
	// Scale first: a disagreeing or unknown on-chain decimals() refuses the feed before its price
	// is read.
	dec, err := p.resolveDecimals(ctx, pair, spec)
	if err != nil {
		return pollResult{err: err, pair: pair}, true
	}
	spec.Decimals = dec
	rnd, err := p.fetchLatest(ctx, pair, spec)
	if err != nil {
		return pollResult{err: err, pair: pair}, true
	}
	if err := checkRoundCurrent(pair, spec, rnd, p.clock()); err != nil {
		return pollResult{err: err, pair: pair, round: rnd}, true
	}
	if !p.Cache.wouldEmit(rnd.FeedAddress, rnd.RoundID) {
		// Already emitted this round (or a newer one) — no-op.
		return pollResult{}, false
	}
	u, err := p.project(pair, spec, rnd)
	if err != nil {
		// Do NOT mark the round emitted: the round id only advances on a new answer, so committing
		// here would wedge the feed at this round until restart.
		return pollResult{err: err, pair: pair, round: rnd}, true
	}
	// Commit the dedup high-water mark only once the update is built, so no later failure can
	// silently lose this round.
	p.Cache.commitEmit(rnd.FeedAddress, rnd.RoundID)
	return pollResult{update: u, pair: pair, round: rnd}, true
}

// PollOnce implements external.Poller: one OracleUpdate per pair whose round is new, never trades; per-feed failures are logged and counted, not bubbled, and at most p.Concurrency eth_calls run at once.
func (p *Poller) PollOnce(ctx context.Context, pairs []canonical.Pair) ([]canonical.Trade, []canonical.OracleUpdate, error) { //nolint:gocognit,gocyclo // dispatch-heavy; splitting hurts readability
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}

	conc := p.Concurrency
	if conc <= 0 {
		conc = 8
	}
	sem := make(chan struct{}, conc)
	results := make(chan pollResult, len(pairs))

	// anyCurrent tracks whether ANY feed was current (emitted or unchanged). Unchanged feeds send
	// nothing to results, so without it the fan-in cannot tell "nothing new" from "every feed failed".
	var anyCurrent atomic.Bool

	var wg sync.WaitGroup
	for _, pr := range pairs {
		spec, ok := p.FeedMap[pr.String()]
		if !ok {
			// Skip silently: logging every miss every tick would drown the journal.
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pair canonical.Pair, spec FeedSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			// An unrecovered panic in any goroutine kills the process; report the feed as failed
			// rather than letting it vanish from `updates`.
			defer func() {
				if rec := recover(); rec != nil {
					worker.Report(logger, "external-chainlink-feed-poll", rec)
					p.recordFeedOutcome(pair, outcomeError)
					results <- pollResult{err: fmt.Errorf("chainlink feed poll panicked: %v", rec), pair: pair}
				}
			}()
			res, ok := p.pollFeed(ctx, pair, spec)
			if !ok {
				// Unchanged: no-op, but still a feed that is current.
				anyCurrent.Store(true)
				return
			}
			results <- res
		}(pr, spec)
	}
	go func() {
		// Close from a DEFER: if the join panicked and were merely contained, the fan-in range
		// would block on a channel nobody closes, and a wedged tick is worse than a crash.
		defer close(results)
		defer worker.Recover(logger, "external-chainlink-feed-join")
		wg.Wait()
	}()

	var updates []canonical.OracleUpdate
	var firstErr error
	for r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			logger.Warn("chainlink feed poll failed",
				"source", SourceName,
				"pair", r.pair.String(),
				"outcome", failureOutcome(r.err),
				"err", r.err)
			continue
		}
		// Empty update means "no new round" (not an error).
		if r.update.Source == "" {
			continue
		}
		updates = append(updates, r.update)
	}

	// nil/nil/nil means "skipped"; an error would mark the whole tick failed even when most feeds
	// succeeded. An all-feeds-failed cycle is not a healthy skip, though: the staleness gauge
	// would stay green while the poller is wedged (bad endpoint, revoked key, RPC down), so
	// firstErr surfaces when no feed was current.
	if len(updates) == 0 {
		if firstErr != nil && !anyCurrent.Load() {
			return nil, nil, firstErr
		}
		return nil, nil, nil
	}
	return nil, updates, nil
}

// fetchLatest calls latestRoundData on the feed and decodes the 5-tuple, wrapping errors with the
// pair.
func (p *Poller) fetchLatest(ctx context.Context, pair canonical.Pair, spec FeedSpec) (Round, error) {
	rawHex, err := p.Client.EthCall(ctx, spec.Address, SelLatestRoundData, "latest")
	if err != nil {
		return Round{}, fmt.Errorf("eth_call %s: %w", pair.String(), err)
	}
	rnd, err := decodeLatestRoundData(rawHex, spec.Address, p.clock())
	if err != nil {
		return Round{}, fmt.Errorf("decode %s: %w", pair.String(), err)
	}
	return rnd, nil
}

// project converts a decoded Round into a canonical.OracleUpdate, applying per-feed Decimals and Invert. spec.Decimals must be the
// RESOLVED scale: a literal 0 is refused (ErrDecimalsUnresolved), as 10^0 would store a BTC/USD row 10^8 too large. Identity is
// synthesized (Ledger=0, OpIndex=0, TxHash=sha256(feed||roundId)[:64], Timestamp=Round.UpdatedAt) so re-emits are idempotent.
func (p *Poller) project(pair canonical.Pair, spec FeedSpec, rnd Round) (canonical.OracleUpdate, error) {
	decimals := spec.Decimals
	if decimals == 0 {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: %s feed=%s has no resolved decimals", ErrDecimalsUnresolved, pair.String(), spec.Address)
	}

	// Invert is 1/answer at the same decimal scale, on the big.Int (ADR-0003).
	rawAnswer, ok := new(big.Int).SetString(rnd.Answer, 10)
	if !ok {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: bad decimal answer %q", ErrMalformedResult, rnd.Answer)
	}
	answer := rawAnswer
	var published *canonical.Amount
	if spec.Invert {
		if rawAnswer.Sign() == 0 {
			return canonical.OracleUpdate{}, fmt.Errorf("%w: cannot invert zero", ErrMalformedResult)
		}
		// Round half-up: a truncating Quo biases every inverted price low.
		answer = scale.InvertScaled(rawAnswer, int(decimals))
		// The reciprocal is lossy; keep the feed's own answer (ADR-0003).
		p := canonical.NewAmount(rawAnswer)
		published = &p
	}
	if answer.Sign() <= 0 {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: post-invert non-positive %s", ErrNonPositivePrice, answer.String())
	}

	return canonical.OracleUpdate{
		Source:         SourceName,
		ContractID:     "", // off-chain; the ETH address is not a canonical-row field
		Ledger:         0,
		TxHash:         syntheticTxHash(spec.Address, rnd.RoundID),
		OpIndex:        0,
		Timestamp:      rnd.UpdatedAt,
		Asset:          pair.Base,
		Quote:          pair.Quote,
		Price:          canonical.NewAmount(answer),
		PublishedPrice: published,
		Decimals:       decimals,
		Observer:       "",
	}, nil
}

// wouldEmit reports whether (feedAddr, roundID) is newer than the last committed round, without
// mutating the cache (the commit waits for commitEmit).
func (c *roundCache) wouldEmit(feedAddr string, roundID *big.Int) bool {
	if roundID == nil {
		roundID = new(big.Int)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.last[feedAddr]
	return !ok || roundID.Cmp(prev) > 0
}

// commitEmit marks (feedAddr, roundID) as emitted. Call it only after project succeeded, or a
// failure would wedge the feed at that round until restart.
func (c *roundCache) commitEmit(feedAddr string, roundID *big.Int) {
	if roundID == nil {
		roundID = new(big.Int)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.last[feedAddr]
	if ok && roundID.Cmp(prev) <= 0 {
		return
	}
	// Copy so a caller's later mutation cannot corrupt the high-water mark.
	c.last[feedAddr] = new(big.Int).Set(roundID)
}

// syntheticTxHash derives a deterministic 64-char hex tx_hash from (feed address, roundId). roundID is the FULL uint80 proxy round id:
// rounds from different phases sharing a low-64-bit aggregatorRoundId must hash distinctly or they collide on the storage PK.
func syntheticTxHash(feedAddress string, roundID *big.Int) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strings.ToLower(feedAddress)))
	_, _ = h.Write([]byte(":"))
	if roundID == nil {
		roundID = new(big.Int)
	}
	// The decimal string is unique per distinct wide id, so the hash input stays stable and
	// collision-free across phase rollovers.
	_, _ = h.Write([]byte(roundID.String()))
	return hex.EncodeToString(h.Sum(nil))
}
