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

// Poller implements external.Poller for Chainlink Data Feeds.
//
// It emits one canonical.OracleUpdate per new (feed_address, roundId). The dedup cache is
// in-memory, so the first poll after a restart re-emits the latest round per feed; the storage
// PK's ON CONFLICT rejects that idempotently. The runner serialises PollOnce per source, so the
// internal mutexes only guard against future concurrent paths.
type Poller struct {
	// Client is the EVM JSON-RPC client. Constructor injects the
	// operator-supplied endpoint (Alchemy / Infura / public).
	Client *Client

	// Logger receives per-feed warn/info messages. nil → slog.Default.
	Logger *slog.Logger

	// Interval overrides DefaultPollInterval. Zero = default.
	Interval time.Duration

	// FeedMap maps canonical pair string ("crypto:BTC/fiat:USD")
	// to the AggregatorV3 contract address + decimals + invert.
	// Operator-curated; constructor injects from config.
	FeedMap map[string]FeedSpec

	// Cache holds the last roundId emitted per feed address. Empty
	// at construction; populated on each successful PollOnce.
	Cache *roundCache

	// concurrency caps how many feed eth_call requests we have in
	// flight at once during one PollOnce tick. Defaults to 8 — well
	// inside Alchemy's free-tier per-second budget for our pair
	// counts and a polite ceiling for public RPCs.
	Concurrency int

	// decimals is the per-feed on-chain decimals() verification state
	// (see decimals.go). Built by NewPoller, like Cache.
	decimals *decimalsCache

	// now is the injectable clock for the decimals() refresh / retry
	// cadence and the updatedAt future-skew guard. nil → time.Now.
	now func() time.Time
}

// clock returns the poller's injectable wall clock (time.Now when unset).
func (p *Poller) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// NewPoller builds a Poller with sensible defaults. Caller supplies
// the RPC endpoint (must include the API key for keyed providers
// like Alchemy) and the feed map.
func NewPoller(rpcURL string, feedMap map[string]FeedSpec) *Poller {
	// Seed zero-valued mismatch/verify-failed series per configured feed. FeedMap is fixed at
	// construction so the label set is bounded, and without seeding a healthy feed is
	// indistinguishable from one whose counter was never wired.
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

// Per-feed poll outcomes, the label values of
// obs.ChainlinkFeedPollsTotal.
const (
	outcomeEmitted        = "emitted"
	outcomeUnchanged      = "unchanged"
	outcomeStale          = "stale"
	outcomeCarriedForward = "carried_forward"
	outcomeError          = "error"
)

var feedOutcomes = []string{outcomeEmitted, outcomeUnchanged, outcomeStale, outcomeCarriedForward, outcomeError}

// recordFeedOutcome counts one feed's poll outcome and, for a fresh
// round, advances its last-success gauge.
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

// checkRoundCurrent refuses a round that is not a current publication:
// a carried-forward answer, or one older than the feed's MaxAge. It
// runs before dedup so a frozen feed keeps reporting stale every tick
// instead of reading as "unchanged".
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

// pollFeed resolves decimals, fetches the latest round, refuses it if it is not current, and
// projects and commits it if new. ok=false means "already emitted this round": the one path
// PollOnce's fan-in must not receive a result for. Every path records the feed's outcome.
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
	// Scale first: the on-chain decimals() verified against the
	// configured value (decimals.go). A disagreeing or unknown
	// scale refuses the feed before its price is read.
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
		// Do NOT mark the round emitted: a project() failure can be transient, but the round id
		// only advances when the feed publishes a new answer, so committing here would wedge the
		// feed at this round until restart.
		return pollResult{err: err, pair: pair, round: rnd}, true
	}
	// Commit the dedup high-water mark only now that the update has actually
	// been built — the earliest point a failure downstream of this line can
	// no longer cause silent, permanent data loss for this round.
	p.Cache.commitEmit(rnd.FeedAddress, rnd.RoundID)
	return pollResult{update: u, pair: pair, round: rnd}, true
}

// PollOnce implements external.Poller: one OracleUpdate per pair whose round is new, never
// trades (Chainlink is oracle-class).
//
// Per-feed failures (including stale and carried-forward rounds) are logged and counted on
// stellarindex_chainlink_feed_polls_total, not bubbled, so one bad feed cannot stop the others.
// At most p.Concurrency eth_call requests run at once, to stay polite to shared RPC endpoints.
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

	// anyCurrent tracks whether ANY feed was current this tick — emitted
	// OR unchanged. "Unchanged" (ok=false from pollFeed) sends nothing to
	// results, so without this the fan-in below can't tell "a sibling is
	// fine, it just has nothing new" from "every feed failed".
	var anyCurrent atomic.Bool

	var wg sync.WaitGroup
	for _, pr := range pairs {
		spec, ok := p.FeedMap[pr.String()]
		if !ok {
			// Pair isn't in our feed map — operator misconfig or
			// the framework passed us a non-Chainlink-ish pair.
			// Skip silently; logging every miss every 30s would
			// drown the journal.
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pair canonical.Pair, spec FeedSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			// An unrecovered panic in ANY goroutine kills the whole
			// process; report the feed as failed rather than letting it
			// silently vanish from `updates`.
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
		// Close from a DEFER, not as a trailing statement: if the join
		// panicked and the panic were merely contained, the fan-in range
		// below would block on a channel nobody ever closes — a wedged
		// poll tick is worse than the crash it replaces.
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

	// nil/nil/nil means "skipped": returning an error would mark the whole tick "error" even when
	// most feeds succeeded. Per-feed errors are already logged and counted above.
	//
	// An all-feeds-failed cycle is not a healthy skip: (nil,nil,nil) would let the runner bump
	// ExternalPollerLastSuccessUnix and keep the staleness gauge green while the poller is wedged
	// (bad endpoint, revoked key, RPC down). An unchanged sibling (ok=false) is current, not a
	// failure, so firstErr surfaces only when no feed was current this tick.
	if len(updates) == 0 {
		if firstErr != nil && !anyCurrent.Load() {
			return nil, nil, firstErr
		}
		return nil, nil, nil
	}
	return nil, updates, nil
}

// fetchLatest calls latestRoundData on the given feed and decodes
// the 5-tuple. Wraps the network + decode errors with the pair
// context the caller logs.
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

// project converts a decoded Round into a canonical.OracleUpdate, applying the per-feed
// Decimals and Invert transforms.
//
// spec.Decimals must be the RESOLVED scale (on-chain decimals() verified against config). A
// literal 0 is refused as ErrDecimalsUnresolved: dividing by 10^0 would store a BTC/USD row
// 10^8 too large, and guessing 8 would hide the scale drift the resolver exists to catch.
//
// Identity is synthesized (Ledger=0, OpIndex=0, TxHash=sha256(feed||roundId)[:64],
// Timestamp=Round.UpdatedAt), so re-emitting the same round hits the same PK and inserts
// idempotently under restart and retry.
func (p *Poller) project(pair canonical.Pair, spec FeedSpec, rnd Round) (canonical.OracleUpdate, error) {
	decimals := spec.Decimals
	if decimals == 0 {
		return canonical.OracleUpdate{}, fmt.Errorf("%w: %s feed=%s has no resolved decimals", ErrDecimalsUnresolved, pair.String(), spec.Address)
	}

	// Apply Invert: 1/answer at the same decimal scale, on the big.Int
	// (oracle_updates stores raw integers per ADR-0003).
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
		// Round half-up like every other inverted feed; a truncating
		// Quo biases every inverted price low.
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
		ContractID:     "", // off-chain; ETH contract address belongs in a separate observability surface, not the canonical row
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

// wouldEmit reports whether (feedAddr, roundID) is newer than the last committed round without
// mutating the cache, so an already-seen round is skipped before any work and the commit waits
// until the update has survived every failure point (see commitEmit).
func (c *roundCache) wouldEmit(feedAddr string, roundID *big.Int) bool {
	if roundID == nil {
		roundID = new(big.Int)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.last[feedAddr]
	return !ok || roundID.Cmp(prev) > 0
}

// commitEmit marks (feedAddr, roundID) as emitted. Call it only after project succeeded: marking
// earlier would let a project() failure wedge the feed at that round, because the round id only
// advances when the feed publishes a new answer, so later polls would emit nothing until restart.
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
	// Store a copy so a later mutation of the caller's *big.Int can't
	// corrupt the cached high-water mark.
	c.last[feedAddr] = new(big.Int).Set(roundID)
}

// syntheticTxHash derives a deterministic 64-char hex tx_hash (tx_hash is CHAR(64)) from the
// (feed address, roundId) pair via SHA-256.
//
// roundID is the FULL uint80 proxy round id: rounds from different phases that share a low-64-bit
// aggregatorRoundId must hash to distinct values so they do not collide on the storage PK.
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
