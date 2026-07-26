package external

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// dustFloorUSDMicros is the dust threshold the streamer guard
// enforces, in micro-dollars: 1_000 µUSD = $0.001. This — not a
// count of quote units — is the guard's actual intent. A STREAMED
// CEX fill worth less than a tenth of a cent carries no usable
// price: CEX feeds emit sub-microcent fills whose amounts are tiny
// integers (e.g. 8 base for 1 quote), making quote/base a
// meaningless round fraction (1/8, 1/10, …). Kept, those single
// dust prints set the UNWEIGHTED OHLC high/low (max/min of
// quote/base) and produced absurd wicks on the served /v1/ohlc API
// — e.g. an XLM/USD low of $0.125 from a $0.00000001 fill — while
// contributing ~zero real volume.
//
// C2-016 (audit-2026-07-23): this used to be a flat 100_000 units
// at the external 10^8 scale — i.e. 0.001 of whatever the QUOTE
// asset happens to be — with a docstring asserting "CEX reference
// pairs are USD-quoted". They are not: binance/pairs.yaml and
// bitstamp/pairs.go both configure XLM/BTC. 0.001 BTC is ~$100, so
// the flat floor silently dropped every XLM/BTC print below roughly
// a thousand XLM — and the drop is SIZE-BIASED, so the surviving
// XLM/BTC volume and VWAP were systematically skewed toward large
// trades. Denominating the floor in USD removes the bias at its
// source instead of papering over it per-pair.
const dustFloorUSDMicros = 1_000

// externalQuoteScale is the fixed-point scale streamed CEX amounts
// arrive at (10^8 units per whole quote asset). Streamers are all
// CEX at this scale; the FX pollers (10^6) don't emit dust, so this
// guard stays scoped to the streamer path.
const externalQuoteScale = 100_000_000

// fiatQuoteUSDMicros is the USD reference used for ANY fiat quote
// leg: one whole unit ≈ $1. Deliberately one number rather than a
// per-currency FX table — a real table would be false precision for
// an order-of-magnitude dust threshold, and would rot.
//
// It is safe because $1 is an UPPER BOUND across the whole ADR-0010
// fiat allow-list (canonical.knownFiatCodes): the richest codes on
// it (GBP, CHF) are ≈ $1.3, so the floor is at most ~1.3× the
// intended $0.001; the poorest (IDR, VND, KRW) are worth far less
// than a dollar, which makes their floor far BELOW $0.001. Both
// error directions are acceptable, and the second is the safe one —
// over-stating the reference under-states the floor, so we keep
// marginal prints rather than silently discard real ones.
const fiatQuoteUSDMicros = 1_000_000

// cryptoQuoteUSDMicros is a coarse, order-of-magnitude USD value of
// ONE whole unit of each crypto ticker that can appear as a QUOTE
// leg on a venue we stream. Micro-dollars, same units as
// [fiatQuoteUSDMicros].
//
// These are NOT prices. Nothing served, stored, or aggregated reads
// them; they exist solely to convert the $0.001 dust threshold into
// the quote asset's own units, and the result is floored at 1 unit.
// That makes them robust to enormous drift — BTC anywhere between
// ~$1k and ~$10M still resolves to a 1-unit floor, and a 10× error
// on ETH moves the threshold between $0.0001 and $0.01, both still
// orders of magnitude below any real fill. There is no rate source
// at the ingest runner (the USD-valuation path lives downstream in
// the aggregator, deliberately — see internal/aggregate/stablecoin.go
// on why peg/rate policy must not run at decode time), so a static
// anchor table is the honest option; it is kept here beside the
// guard it serves, matching this package's Go-map source-of-truth
// convention (see [Registry]).
//
// A ticker that is absent resolves to "no floor" — see
// [minStreamQuoteUnits].
var cryptoQuoteUSDMicros = map[string]uint64{
	// USD-pegged stablecoins — the dominant CEX quote leg.
	"USDT":  1_000_000,
	"USDC":  1_000_000,
	"DAI":   1_000_000,
	"PYUSD": 1_000_000,
	"USDP":  1_000_000,
	// EUR-pegged stablecoins (≈ $1.1; the extra 10% is noise at this
	// threshold).
	"EURC":  1_000_000,
	"EUROC": 1_000_000,
	"EUROB": 1_000_000,
	// Real crypto quote legs. BTC is live today (binance XLMBTC,
	// bitstamp xlmbtc); ETH and XLM are the other plausible ones.
	"BTC": 100_000 * 1_000_000,
	"ETH": 3_000 * 1_000_000,
	"XLM": 300_000,
}

// noDustFloor is the floor applied when no USD reference exists for
// a quote asset: 1 unit, i.e. only a strictly-zero quote leg counts
// as dust. Fail-open on the DATA is the conservation-correct choice
// here — inventing a magnitude for an unknown asset is exactly how
// C2-016 happened, and silently discarding real prints biases every
// served volume/VWAP downstream, whereas keeping a marginal print
// merely leaves a visible outlier. Operators are told at start-up:
// [Run]'s pre-flight warns once per unreferenced streamed pair.
var noDustFloor = canonical.NewAmount(big.NewInt(1))

// streamDustFloors caches the computed floor per distinct USD
// reference so the streamer hot path is a map lookup rather than a
// big.Int divide per trade.
var streamDustFloors = buildStreamDustFloors()

func buildStreamDustFloors() map[uint64]canonical.Amount {
	out := make(map[uint64]canonical.Amount, len(cryptoQuoteUSDMicros)+1)
	out[fiatQuoteUSDMicros] = dustFloorUnits(fiatQuoteUSDMicros)
	for _, ref := range cryptoQuoteUSDMicros {
		out[ref] = dustFloorUnits(ref)
	}
	return out
}

// dustFloorUnits converts the USD dust threshold into quote-asset
// units at the external scale: units = $0.001 / usdPerWholeUnit.
// Clamped to a minimum of 1 so a very-high-value quote asset still
// drops a zero quote leg (price 0 is never a real print).
func dustFloorUnits(usdPerUnitMicros uint64) canonical.Amount {
	units := new(big.Int).SetUint64(dustFloorUSDMicros)
	units.Mul(units, big.NewInt(externalQuoteScale))
	units.Div(units, new(big.Int).SetUint64(usdPerUnitMicros))
	if units.Sign() <= 0 {
		units.SetInt64(1)
	}
	return canonical.NewAmount(units)
}

// quoteUSDReferenceMicros resolves the coarse USD value of one whole
// unit of a quote asset. ok=false means we have no defensible
// magnitude for it.
func quoteUSDReferenceMicros(quote canonical.Asset) (uint64, bool) {
	switch quote.Type {
	case canonical.AssetFiat:
		return fiatQuoteUSDMicros, true
	case canonical.AssetCrypto:
		ref, ok := cryptoQuoteUSDMicros[quote.Code]
		return ref, ok
	default:
		// Streamers are all CEX, whose legs are fiat / crypto
		// tickers. An on-chain (native/classic/soroban) quote here
		// would be a routing surprise — no floor rather than a
		// guessed one.
		return 0, false
	}
}

// minStreamQuoteUnits is the quote-leg floor, in the QUOTE asset's
// smallest unit at the external 10^8 scale, below which a streamed
// CEX trade is dropped as dust. Equivalent to $0.001 of notional
// under [cryptoQuoteUSDMicros] / [fiatQuoteUSDMicros]; [noDustFloor]
// when the quote asset has no USD reference.
func minStreamQuoteUnits(quote canonical.Asset) canonical.Amount {
	ref, ok := quoteUSDReferenceMicros(quote)
	if !ok {
		return noDustFloor
	}
	if floor, ok := streamDustFloors[ref]; ok {
		return floor
	}
	return dustFloorUnits(ref)
}

// StreamerSpec is a configured streamer + the pair list it should
// subscribe to. Returned by each venue package's builder so the
// indexer doesn't need to know how to wire pair maps per venue.
type StreamerSpec struct {
	Streamer Streamer
	Pairs    []canonical.Pair
}

// PollerSpec pairs a Poller with the list of pairs it should fetch
// on each tick. Pollers declare their own PollInterval; the runner
// ticks at that cadence and fans the PollOnce outputs into the
// shared sink.
type PollerSpec struct {
	Poller Poller
	Pairs  []canonical.Pair
}

// Run launches every streamer (and later, poller) in its own
// goroutine, fans trade output into the supplied consumer.Event
// channel wrapping each as a TradeEvent, and returns a cleanup
// function that blocks until all connectors have shut down. The
// cleanup is normally called from the indexer's shutdown sequence;
// ctx cancellation is the primary stop signal.
//
// On a streamer's own error channel close (unrecoverable error),
// Run logs and lets it drop — the other connectors continue. Fatal
// config errors at Start time are returned synchronously before any
// goroutine spawns.
//
// The signature intentionally mirrors the dispatcher's
// processAndPersist goroutine pattern so the indexer can wire them
// symmetrically.
func Run(
	ctx context.Context,
	streamers []StreamerSpec,
	pollers []PollerSpec,
	sink chan<- consumer.Event,
	logger *slog.Logger,
) (wait func(), err error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(streamers) == 0 && len(pollers) == 0 {
		// No external sources configured — still return a valid
		// wait() so the caller's shutdown path doesn't need
		// special-case logic.
		return func() {}, nil
	}

	// Pre-flight every Start. Fatal config errors (empty pair list,
	// bad endpoint URL) surface here before we spawn anything.
	//
	// G10-11: each Streamer.Start spawns a reconnect-forever goroutine
	// bound to the context we hand it. If a LATER Start fails, the
	// earlier streamers' goroutines would otherwise leak — Run returns
	// an error, the caller never gets a wait()/cancel handle, and those
	// goroutines run until the parent ctx is cancelled (often never, on
	// a startup-config error). We start every streamer under a DERIVED,
	// cancellable context and cancel it on the error path so the
	// already-launched streamers shut down before Run returns. On the
	// happy path the derived context lives as long as the parent (it's
	// cancelled when ctx is, via context.WithCancel propagation).
	streamerCtx, cancelStreamers := context.WithCancel(ctx)
	type running struct {
		name string
		ch   <-chan canonical.Trade
	}
	launched := make([]running, 0, len(streamers))
	for _, s := range streamers {
		warnUnreferencedDustQuotes(s.Streamer.Name(), s.Pairs, logger)
		ch, err := s.Streamer.Start(streamerCtx, s.Pairs)
		if err != nil {
			// Tear down every streamer we already launched, then wait
			// for their goroutines to observe the cancellation and
			// close their channels so we don't return with goroutines
			// still draining.
			cancelStreamers()
			for _, r := range launched {
				drainUntilClosed(r.ch)
			}
			return nil, fmt.Errorf("external.Run: start %q: %w", s.Streamer.Name(), err)
		}
		launched = append(launched, running{name: s.Streamer.Name(), ch: ch})
	}

	var wg sync.WaitGroup
	for _, r := range launched {
		wg.Add(1)
		go func(name string, ch <-chan canonical.Trade) {
			defer wg.Done()
			forwardTrades(streamerCtx, name, ch, sink, logger)
		}(r.name, r.ch)
	}

	// Pollers: one goroutine per poller running a ticker at the
	// declared PollInterval. Each tick calls PollOnce; returned
	// trades + updates are wrapped and fanned to the shared sink.
	for _, p := range pollers {
		if p.Poller == nil {
			return nil, teardown(cancelStreamers, &wg, errors.New("external.Run: nil Poller in spec"))
		}
		interval := p.Poller.PollInterval()
		if interval <= 0 {
			return nil, teardown(cancelStreamers, &wg, fmt.Errorf(
				"external.Run: %q declares non-positive PollInterval %v",
				p.Poller.Name(), interval))
		}
		wg.Add(1)
		go func(spec PollerSpec) {
			defer wg.Done()
			// REL-05 (audit-2026-07-23): streamerCtx, not the raw
			// parent ctx. teardown() (used when a LATER poller in
			// this same loop fails config validation) only cancels
			// streamerCtx — a poller goroutine bound to the raw ctx
			// would ignore that cancellation and wg.Wait() below (and
			// in teardown) would block until the caller's own ctx is
			// separately cancelled, which on a startup-config error
			// may never happen. streamerCtx is a child of ctx, so on
			// the normal shutdown path (ctx cancelled) it propagates
			// exactly the same as before.
			runPoller(streamerCtx, spec, sink, logger)
		}(p)
	}

	// wait() blocks until every connector goroutine has shut down, then
	// releases the derived streamer context. cancelStreamers is also
	// the safety valve the error paths above use to avoid leaking the
	// already-launched streamer goroutines (G10-11).
	wait = func() {
		wg.Wait()
		cancelStreamers()
	}
	return wait, nil
}

// warnUnreferencedDustQuotes logs once per configured streamed pair
// whose quote asset has no USD reference in [cryptoQuoteUSDMicros].
// Those pairs run with [noDustFloor] — correct (we never silently
// drop a real print) but it means the dust guard is inert for them,
// which an operator adding a pair should know. Deliberately a
// start-up WARN, not a fatal: one unreferenced pair must not take
// the whole external ingest down, and not a per-trade signal:
// cardinality is bounded by the configured pair list.
func warnUnreferencedDustQuotes(source string, pairs []canonical.Pair, logger *slog.Logger) {
	for _, p := range pairs {
		if _, ok := quoteUSDReferenceMicros(p.Quote); ok {
			continue
		}
		logger.Warn("streamed pair quote asset has no USD dust-floor reference; dust filter inert for this pair",
			"source", source,
			"quote", p.Quote.String(),
			"remedy", "add the ticker to cryptoQuoteUSDMicros in internal/sources/external/runner.go")
	}
}

// teardown cancels the derived streamer context and blocks until the
// already-launched streamer goroutines have observed the cancellation
// and exited, then returns the original error. Used by Run's poller-
// validation error paths so a late config error doesn't leak the
// streamer goroutines started earlier in the same call (G10-11).
func teardown(cancel context.CancelFunc, wg *sync.WaitGroup, err error) error {
	cancel()
	wg.Wait()
	return err
}

// drainUntilClosed reads and discards from a streamer's trade channel
// until it closes. Used on the streamer-Start error path (G10-11):
// the earlier streamers were started with the now-cancelled derived
// context, so their run loops will close their channels promptly; we
// drain so the close — and thus the goroutine exit — is not blocked on
// a pending send, then return.
func drainUntilClosed(ch <-chan canonical.Trade) {
	for range ch {
	}
}

// forwardTrades drains one streamer's channel into the shared sink,
// wrapping each trade as a TradeEvent. Returns when the source
// channel closes (streamer shutdown) or ctx is cancelled.
func forwardTrades(
	ctx context.Context,
	source string,
	in <-chan canonical.Trade,
	sink chan<- consumer.Event,
	logger *slog.Logger,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case trade, ok := <-in:
			if !ok {
				logger.Info("external streamer closed",
					"source", source)
				return
			}
			// Drop sub-$0.001 dust fills — see minStreamQuoteUnits. They
			// carry no meaningful price (integer-quantised round-fraction
			// ratios) and corrupt the OHLC high/low if ingested. The
			// floor is resolved per QUOTE ASSET so the threshold means
			// the same $0.001 on XLM/BTC as on XLM/USDT (C2-016).
			if trade.QuoteAmount.Cmp(minStreamQuoteUnits(trade.Pair.Quote)) < 0 {
				obs.ExternalDustDroppedTotal.WithLabelValues(source).Inc()
				continue
			}
			select {
			case <-ctx.Done():
				return
			case sink <- TradeEvent{Trade: trade}:
			}
		}
	}
}

// runPoller drives a single Poller at its declared cadence. On each
// tick, PollOnce is called; returned trades land as TradeEvents and
// updates as UpdateEvents on the shared sink. An error from PollOnce
// is logged + counted (future: per-source metrics hook) but doesn't
// stop the loop — poll-based sources regularly hit transient REST
// errors (network blip, venue rate-limit) and should just retry at
// the next tick.
//
// First poll fires immediately on startup rather than waiting a full
// interval — operators want fresh data visible within seconds of
// starting the indexer, not only after the ticker elapses.
func runPoller(
	ctx context.Context,
	spec PollerSpec,
	sink chan<- consumer.Event,
	logger *slog.Logger,
) {
	name := spec.Poller.Name()
	interval := spec.Poller.PollInterval()

	doPoll := func() {
		trades, updates, err := spec.Poller.PollOnce(ctx, spec.Pairs)
		if err != nil {
			obs.ExternalPollerPollsTotal.WithLabelValues(name, "error").Inc()
			logger.Warn("poller error",
				"source", name, "err", err)
			return
		}
		// (nil trades, nil updates, nil err) is the convention for
		// "poller skipped this tick" — used by per-poller cooldown
		// after rate-limit (e.g. coingecko backoff) AND by sources
		// like chainlink that frequently see "no new round" between
		// 1-hour feed updates. Pre-2026-06-01 this branch returned
		// without updating LastSuccessUnix, so a healthy chainlink
		// poller (polling every 30s, but feeds updating hourly)
		// looked stale to `stellarindex_external_poller_stale`
		// within ~10-15 min. The outcome counter still bumps
		// "skipped" so operators can tell skip from success; but
		// the timestamp bumps too because the poller is alive +
		// reaching upstream — a skip means "we polled and there
		// was nothing new", not "we couldn't poll."
		if trades == nil && updates == nil {
			obs.ExternalPollerPollsTotal.WithLabelValues(name, "skipped").Inc()
			obs.ExternalPollerLastSuccessUnix.WithLabelValues(name).Set(float64(time.Now().Unix()))
			return
		}
		obs.ExternalPollerPollsTotal.WithLabelValues(name, "success").Inc()
		obs.ExternalPollerLastSuccessUnix.WithLabelValues(name).Set(float64(time.Now().Unix()))
		for _, t := range trades {
			select {
			case <-ctx.Done():
				return
			case sink <- TradeEvent{Trade: t}:
			}
		}
		for _, u := range updates {
			select {
			case <-ctx.Done():
				return
			case sink <- UpdateEvent{Update: u}:
			}
		}
	}

	// Fire once on start, then on the ticker cadence.
	doPoll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("poller stopping", "source", name)
			return
		case <-ticker.C:
			doPoll()
		}
	}
}
