package v1

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Shared tip-stream producers (real-time program RT-1; audit 2026-08-04
// "tip stream = 6 DB queries/s PER CONNECTION"). The legacy shape ran
// one compute loop per CONNECTION, so N viewers of the same pair cost
// N× the tip computation and the shared pool saturated at ~2300
// streams. One producer now runs per DISTINCT (asset, quote, window)
// and publishes into the streaming Hub's topic ring; every connection
// is a plain Hub subscriber. Cost scales with distinct pairs being
// watched, not with viewers — the precondition for making every asset
// page hold a live stream ("this should feel alive", 2026-08-08).

// tipProducerLinger keeps a producer alive briefly after its last
// subscriber leaves, absorbing page reloads/reconnects without a
// stop/start churn of the compute loop.
const tipProducerLinger = 30 * time.Second

// defaultMaxTipProducers caps how many distinct tip producers may run at
// once (wave-D UNAUTH-DOS-1).
//
// The SSE caps count CONNECTIONS. A tip-stream connection also mints a
// DETACHED producer: its context comes from context.Background(), it
// outlives the request by design, and it survives the connection's
// release for tipProducerLinger. So the connection cap does not bound
// this — an unauthenticated client could open and immediately abort
// streams in a loop and leave an unbounded set of compute loops running,
// each polling the database on its own ticker, with no connection left
// to attribute them to.
//
// The key is (asset, quote, window_seconds), and window_seconds is
// CLIENT-CHOSEN in [1,60] — so the key space is pairs × 60, and an
// attacker does not even need distinct assets to enumerate it.
//
// The Hub's own topic reaper cannot shed this load either: it only
// evicts subscriber-less topics, and a live producer re-publishes to its
// topic every window, recreating it.
//
// 512 is generous against legitimate use — real fan-out is the set of
// pairs actually being watched, in the hundreds at most, and the
// explorer requests a single default window — while capping the worst
// case well short of what could starve the DB pool. Tune with
// [Server.SetMaxTipProducers].
const defaultMaxTipProducers = 512

type tipProducerKey struct {
	asset  string
	quote  string
	window int
}

// tipTopic is the Hub topic name for a producer key.
func (k tipProducerKey) topic() string {
	return "tip:" + k.asset + "/" + k.quote + "/" + strconv.Itoa(k.window)
}

type tipProducer struct {
	refs   int
	cancel context.CancelFunc
	linger *time.Timer
}

type tipProducerRegistry struct {
	mu     sync.Mutex
	active map[tipProducerKey]*tipProducer
	// lingerFor overrides tipProducerLinger when > 0 (tests).
	lingerFor time.Duration
	// maxProducers overrides defaultMaxTipProducers when > 0. A negative
	// value disables the ceiling (an operator's explicit choice, and the
	// escape hatch if the default is ever wrong for a deployment).
	maxProducers int
	// refused counts acquire calls turned away by the ceiling, so a
	// flood is visible rather than merely survived.
	refused uint64
}

// limit is the effective producer ceiling; <= 0 from the operator means
// "no ceiling".
func (r *tipProducerRegistry) limit() int {
	if r.maxProducers != 0 {
		return r.maxProducers
	}
	return defaultMaxTipProducers
}

// acquire ensures a producer runs for key (calling start in a fresh
// goroutine with a registry-owned context if none is running) and
// holds a reference to it. Returns the release func — call exactly
// once; idempotence is the caller's job.
//
// ok is false when the registry is at its ceiling and this key would
// need a NEW producer. Joining an ALREADY-RUNNING producer is always
// allowed: refusing that would turn a popular pair's own viewers away
// while costing nothing to serve, which is the opposite of the
// protection intended. Callers must not use release when ok is false.
func (r *tipProducerRegistry) acquire(
	key tipProducerKey, start func(ctx context.Context),
) (release func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[tipProducerKey]*tipProducer)
	}
	p, exists := r.active[key]
	if exists {
		if p.linger != nil {
			p.linger.Stop()
			p.linger = nil
		}
		p.refs++
	} else {
		if lim := r.limit(); lim > 0 && len(r.active) >= lim {
			r.refused++
			return nil, false
		}
		// The cancel func is NOT lost (gosec G118 false positive): it is
		// stored on the producer record and invoked by the linger timer
		// in release() once the last reference is gone.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec
		p = &tipProducer{refs: 1, cancel: cancel}
		r.active[key] = p
		go start(ctx)
	}
	return func() { r.release(key) }, true
}

func (r *tipProducerRegistry) release(key tipProducerKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.active[key]
	if !ok {
		return
	}
	p.refs--
	if p.refs > 0 {
		return
	}
	linger := r.lingerFor
	if linger <= 0 {
		linger = tipProducerLinger
	}
	// Last subscriber gone: linger, then stop — unless someone re-acquires.
	p.linger = time.AfterFunc(linger, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		cur, still := r.active[key]
		if !still || cur != p || cur.refs > 0 {
			return
		}
		cur.cancel()
		delete(r.active, key)
	})
}

// running reports how many producers are currently registered
// (running or lingering). Diagnostic + test hook.
func (r *tipProducerRegistry) running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active)
}

// refusedCount reports how many acquires the ceiling has turned away
// since process start. Cumulative, like streaming.StreamsRejected.
func (r *tipProducerRegistry) refusedCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refused
}

// TipProducersRunning reports the number of live shared tip producers.
// Exported for the operator diagnostics surface — a producer count that
// climbs while connections do not is the signature of the abort-loop
// flood the ceiling exists to stop.
func (s *Server) TipProducersRunning() int { return s.tipProducers.running() }

// TipProducersRefused reports the cumulative count of tip producers
// refused by the ceiling.
func (s *Server) TipProducersRefused() uint64 { return s.tipProducers.refusedCount() }

// SetMaxTipProducers overrides the shared-tip-producer ceiling. Pass a
// negative value to disable it. Call once at startup.
func (s *Server) SetMaxTipProducers(n int) {
	s.tipProducers.mu.Lock()
	defer s.tipProducers.mu.Unlock()
	s.tipProducers.maxProducers = n
}

// acquireTipProducer ensures a shared producer runs for the pair and
// holds a reference to it. Returns the Hub topic to subscribe to and
// the release func.
// ok is false when the producer ceiling is reached and this pair has no
// producer already running; the caller must refuse the stream rather
// than fall through to a per-connection loop, which would reintroduce
// exactly the unbounded compute the ceiling is there to prevent.
func (s *Server) acquireTipProducer(
	asset, quote canonical.Asset, window int,
) (topic string, release func(), ok bool) {
	key := tipProducerKey{asset: asset.String(), quote: quote.String(), window: window}
	release, ok = s.tipProducers.acquire(key, func(ctx context.Context) {
		s.runSharedTipProducer(ctx, key, asset, quote, window)
	})
	if !ok {
		return "", nil, false
	}
	return key.topic(), release, true
}

// runSharedTipProducer is the ONE compute loop for a pair: computes the
// tip every `window` seconds and publishes to the Hub topic. The Hub's
// ring buffer gives late subscribers the most recent event immediately
// (resume semantics), so a fresh page paints without waiting a tick.
func (s *Server) runSharedTipProducer(ctx context.Context, key tipProducerKey, asset, quote canonical.Asset, window int) {
	defer s.recoverStreamProducer("price_tip_shared")
	var gen streaming.Generator
	emit := func() {
		tickCtx, cancel := context.WithTimeout(ctx, tipStreamTickTimeout)
		snap, sources, err := s.computeTip(tickCtx, asset, quote, window)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("shared tip producer compute failed — skipping emit",
					"err", err, "asset", key.asset, "quote", key.quote)
			}
			return
		}
		if ev, ok := tipStreamEvent(&gen, snap, sources); ok {
			s.hub.Publish(key.topic(), ev.Type, ev.Data)
		}
	}
	emit() // immediate first publish — the ring serves it to every joiner
	ticker := time.NewTicker(time.Duration(window) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			emit()
		}
	}
}
