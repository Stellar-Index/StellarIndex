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
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// dustFloorUSDMicros is the streamer dust guard's threshold: 1_000 µUSD = $0.001. Sub-microcent CEX fills have a meaningless quote/base
// and would set absurd /v1/ohlc wicks. The floor is in USD, not quote units: a flat 0.001-quote floor on XLM/BTC (~$100) would drop
// small prints and size-bias VWAP.
const dustFloorUSDMicros = 1_000

// externalQuoteScale is the fixed-point scale streamed CEX amounts arrive at (10^8 per whole quote
// asset). FX pollers (10^6) emit no dust, so the guard stays scoped to the streamer path.
const externalQuoteScale = 100_000_000

// fiatQuoteUSDMicros is the USD reference for ANY fiat quote leg (one unit ~ $1). It understates high-value currencies like KWD, making
// their floor stricter (the size-biased direction); bounded because every streamed fiat leg is hard-coded (USD/EUR/GBP, at most
// ~$1.35). Revisit if pairs become runtime-configurable.
const fiatQuoteUSDMicros = 1_000_000

// cryptoQuoteUSDMicros is a coarse USD value of ONE whole unit of each streamed crypto quote ticker, in [fiatQuoteUSDMicros] units. NOT
// prices: they only convert the $0.001 threshold to quote units, floored at 1. An absent ticker means no floor ([minStreamQuoteUnits]).
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

// noDustFloor applies when a quote asset has no USD reference: only a zero quote leg is dust. Failing open is deliberate: inventing a
// magnitude recreates the size bias and dropping real prints skews served volume/VWAP. [Run] warns once per such pair.
var noDustFloor = canonical.NewAmount(big.NewInt(1))

// streamDustFloors caches the floor per distinct USD reference so the streamer hot path is a map lookup, not a big.Int divide per
// trade.
var streamDustFloors = buildStreamDustFloors()

func buildStreamDustFloors() map[uint64]canonical.Amount {
	out := make(map[uint64]canonical.Amount, len(cryptoQuoteUSDMicros)+1)
	out[fiatQuoteUSDMicros] = dustFloorUnits(fiatQuoteUSDMicros)
	for _, ref := range cryptoQuoteUSDMicros {
		out[ref] = dustFloorUnits(ref)
	}
	return out
}

// dustFloorUnits converts the USD dust threshold into quote units at the external scale: units = $0.001 / usdPerWholeUnit, clamped to a
// minimum of 1 so a very-high-value quote still drops a zero quote leg (price 0 is never a real print).
func dustFloorUnits(usdPerUnitMicros uint64) canonical.Amount {
	units := new(big.Int).SetUint64(dustFloorUSDMicros)
	units.Mul(units, big.NewInt(externalQuoteScale))
	units.Div(units, new(big.Int).SetUint64(usdPerUnitMicros))
	if units.Sign() <= 0 {
		units.SetInt64(1)
	}
	return canonical.NewAmount(units)
}

// quoteUSDReferenceMicros resolves the coarse USD value of one whole unit of a quote asset; ok=false means no defensible magnitude.
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

// minStreamQuoteUnits is the quote-leg floor, in the QUOTE asset's smallest unit at the external 10^8 scale, below which a streamed CEX
// trade is dropped as dust. It equals $0.001 of notional; [noDustFloor] when there is no USD reference.
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

// StreamerSpec is a configured streamer plus the pair list it should subscribe to, returned by each venue's builder.
type StreamerSpec struct {
	Streamer Streamer
	Pairs    []canonical.Pair
}

// PollerSpec pairs a Poller with the pairs it fetches each tick; the runner ticks at the poller's own PollInterval and fans PollOnce outputs into the shared sink.
type PollerSpec struct {
	Poller Poller
	Pairs  []canonical.Pair
}

// Run starts every streamer and poller in its own goroutine, fans output into sink, and returns a cleanup that blocks until all stop; a streamer dropped on an unrecoverable error leaves the rest running, and Start config errors return synchronously.
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

	// Pre-flight every Start so config errors (empty pair list, bad URL) surface before anything is left
	// running. Each Start spawns a reconnect-forever goroutine bound to the ctx it gets; under a derived,
	// cancellable ctx, a LATER Start failure cancels the earlier streamers instead of leaking them until the
	// parent ctx ends (often never, on a config error).
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
			// A panic while forwarding one streamer's trades must not crash
			// the whole ingest process — it unwinds this connector's
			// goroutine (logged at Error with its stack); the other
			// connectors keep running.
			defer worker.Recover(logger, "external-forward:"+name)
			defer wg.Done()
			forwardTrades(streamerCtx, name, ch, sink, logger)
		}(r.name, r.ch)
	}

	// Pollers: one goroutine per poller running a ticker at the
	// declared PollInterval. Each tick calls PollOnce; returned
	// trades + updates are wrapped and fanned to the shared sink.
	for _, p := range pollers {
		if err := preflightPoller(p); err != nil {
			return nil, teardown(cancelStreamers, &wg, err)
		}
		wg.Add(1)
		go func(spec PollerSpec) {
			// A panic inside a poller's tick must not crash the whole
			// ingest process — it unwinds this poller's goroutine (logged
			// at Error with its stack); the other connectors keep running.
			defer worker.Recover(logger, "external-poller:"+spec.Poller.Name())
			defer wg.Done()
			// The poller runs under streamerCtx so teardown() (a later poller failing validation) can cancel it;
			// bound to the raw ctx, wg.Wait() would block until the caller's ctx ends. streamerCtx inherits ctx's
			// cancellation on normal shutdown.
			runPoller(streamerCtx, spec, sink, logger)
		}(p)
	}

	// wait() blocks until every connector goroutine has shut down, then
	// releases the derived streamer context. cancelStreamers is also
	// the safety valve the error paths above use to avoid leaking the
	// already-launched streamer goroutines.
	wait = func() {
		wg.Wait()
		cancelStreamers()
	}
	return wait, nil
}

// preflightPoller rejects a spec the runner cannot tick and, before the first update reaches the sink, declares its oracle staleness
// resolution.
func preflightPoller(p PollerSpec) error {
	if p.Poller == nil {
		return errors.New("external.Run: nil Poller in spec")
	}
	interval := p.Poller.PollInterval()
	if interval <= 0 {
		return fmt.Errorf("external.Run: %q declares non-positive PollInterval %v",
			p.Poller.Name(), interval)
	}
	declareOracleResolution(p.Poller.Name(), interval)
	return nil
}

// declareOracleResolution gives every polled source a finite stellarindex_oracle_stale budget: the registry's upstream cadence, floored
// at the poll interval. Without a registry cadence it falls back to the poll interval, since undeclared +Inf can never exceed the
// alert's comparison.
func declareOracleResolution(source string, pollInterval time.Duration) {
	resolution := max(Lookup(source).OracleResolution, pollInterval)
	obs.DeclareOracleResolution(source, resolution.Seconds())
}

// warnUnreferencedDustQuotes logs once per streamed pair whose quote has no [cryptoQuoteUSDMicros] entry (the dust guard is inert for
// it). A start-up WARN, not fatal and not per-trade.
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

// teardown cancels the derived streamer context, waits for the already-launched streamer goroutines
// to exit, then returns the original error, so a late config error in Run does not leak them.
func teardown(cancel context.CancelFunc, wg *sync.WaitGroup, err error) error {
	cancel()
	wg.Wait()
	return err
}

// drainUntilClosed discards from a streamer's trade channel until it closes, so on the Start error path the goroutine exit never blocks
// on a pending send.
func drainUntilClosed(ch <-chan canonical.Trade) {
	for range ch {
	}
}

// maxTradeFutureSkew absorbs host/venue clock skew; minTradeTime is the Stellar network's genesis. A trade outside [minTradeTime,
// now+skew] comes from a unit mismatch or vendor fault and would corrupt time-ordered data.
const maxTradeFutureSkew = 5 * time.Minute

var minTradeTime = time.Date(2015, 9, 30, 0, 0, 0, 0, time.UTC)

func plausibleTradeTime(ts, now time.Time) bool {
	return !ts.Before(minTradeTime) && !ts.After(now.Add(maxTradeFutureSkew))
}

// shutdownDrainGrace bounds how long forwardTrades keeps pushing already-buffered trades after ctx cancel, so a stalled consumer cannot
// hold up shutdown.
const shutdownDrainGrace = 2 * time.Second

// forwardTrades drains one streamer's channel into the shared sink as TradeEvents until it closes or ctx is cancelled; on cancel it
// first flushes buffered trades within shutdownDrainGrace, since live CEX streams have no backfill.
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
			flushBuffered(ctx, source, in, sink)
			return
		case trade, ok := <-in:
			if !ok {
				logger.Info("external streamer closed",
					"source", source)
				return
			}
			if !forwardTrade(ctx, source, trade, sink) {
				flushBuffered(ctx, source, in, sink)
				return
			}
		}
	}
}

// forwardTrade applies the dust and timestamp filters and sends one trade to the sink. It
// returns false when ctx was cancelled before the send completed.
func forwardTrade(ctx context.Context, source string, trade canonical.Trade, sink chan<- consumer.Event) bool {
	// Drop sub-$0.001 dust fills (see minStreamQuoteUnits): their integer-quantised ratios carry no
	// meaningful price and corrupt OHLC high/low. The floor is resolved per QUOTE ASSET so it means
	// the same $0.001 on XLM/BTC as on XLM/USDT.
	if trade.QuoteAmount.Cmp(minStreamQuoteUnits(trade.Pair.Quote)) < 0 {
		obs.ExternalDustDroppedTotal.WithLabelValues(source).Inc()
		return true
	}
	if !plausibleTradeTime(trade.Timestamp, time.Now()) {
		obs.ExternalBadTimestampDroppedTotal.WithLabelValues(source).Inc()
		slog.Warn("dropping trade with implausible timestamp",
			"source", source, "timestamp", trade.Timestamp)
		return true
	}
	obs.CEXStreamLastTradeUnix.WithLabelValues(source).Set(float64(time.Now().Unix()))
	ev := TradeEvent{Trade: trade}
	select {
	case sink <- ev:
		return true
	case <-ctx.Done():
	}
	// The trade is already off the streamer channel: give the sink one
	// bounded chance to take it rather than losing it to the cancel.
	select {
	case sink <- ev:
	case <-time.After(shutdownDrainGrace):
	}
	return false
}

// flushBuffered forwards whatever is already queued in in, without waiting
// for more, until the channel is empty/closed or shutdownDrainGrace elapses.
func flushBuffered(ctx context.Context, source string, in <-chan canonical.Trade, sink chan<- consumer.Event) {
	grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownDrainGrace)
	defer cancel()
	for {
		select {
		case trade, ok := <-in:
			if !ok || !forwardTrade(grace, source, trade, sink) {
				return
			}
		default:
			return
		}
	}
}

// throttled is implemented by pollers that skip upstream calls while backing off a rate limit. A skip inside that window is not proof
// the upstream is reachable, so it must not refresh the staleness clock.
type throttled interface {
	CooldownRemaining() time.Duration
}

func inCooldown(p Poller) bool {
	t, ok := p.(throttled)
	return ok && t.CooldownRemaining() > 0
}

// pollOutcome scores one PollOnce result for polls_total. Only "success" and "skipped" refresh the staleness clock: (nil, nil, nil) is
// an upstream that answered with nothing new; "idle" (no requestable pair) is a config problem, never fresh.
func pollOutcome(trades []canonical.Trade, updates []canonical.OracleUpdate, err error) string {
	switch {
	case errors.Is(err, ErrNoApplicablePairs):
		return "idle"
	case err != nil:
		return "error"
	case trades == nil && updates == nil:
		return "skipped"
	case len(trades) == 0 && len(updates) == 0:
		return "empty"
	default:
		return "success"
	}
}

func emitPollResults(ctx context.Context, source string, sink chan<- consumer.Event, trades []canonical.Trade, updates []canonical.OracleUpdate) {
	for _, t := range trades {
		if !plausibleTradeTime(t.Timestamp, time.Now()) {
			obs.ExternalBadTimestampDroppedTotal.WithLabelValues(source).Inc()
			continue
		}
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

// runPoller drives one Poller at its declared cadence, firing immediately. A PollOnce error is logged and retried next tick, since REST
// sources hit transient errors routinely.
func runPoller(
	ctx context.Context,
	spec PollerSpec,
	sink chan<- consumer.Event,
	logger *slog.Logger,
) {
	name := spec.Poller.Name()
	interval := spec.Poller.PollInterval()
	warnedIdle := false

	doPoll := func() {
		trades, updates, err := spec.Poller.PollOnce(ctx, spec.Pairs)
		outcome := pollOutcome(trades, updates, err)
		obs.ExternalPollerPollsTotal.WithLabelValues(name, outcome).Inc()
		switch outcome {
		case "error":
			logger.Warn("poller error", "source", name, "err", err)
			return
		case "empty":
			logger.Warn("poller reached upstream but produced no rows", "source", name)
			return
		case "idle":
			if !warnedIdle {
				logger.Warn("poller has no applicable pairs; it will never produce rows", "source", name)
				warnedIdle = true
			}
			return
		}
		if outcome == "skipped" && inCooldown(spec.Poller) {
			return
		}
		obs.ExternalPollerLastSuccessUnix.WithLabelValues(name).Set(float64(time.Now().Unix()))
		emitPollResults(ctx, name, sink, trades, updates)
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
