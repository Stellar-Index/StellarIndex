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
}

// acquire ensures a producer runs for key (calling start in a fresh
// goroutine with a registry-owned context if none is running) and
// holds a reference to it. Returns the release func — call exactly
// once; idempotence is the caller's job.
func (r *tipProducerRegistry) acquire(key tipProducerKey, start func(ctx context.Context)) (release func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = make(map[tipProducerKey]*tipProducer)
	}
	p, ok := r.active[key]
	if ok {
		if p.linger != nil {
			p.linger.Stop()
			p.linger = nil
		}
		p.refs++
	} else {
		// The cancel func is NOT lost (gosec G118 false positive): it is
		// stored on the producer record and invoked by the linger timer
		// in release() once the last reference is gone.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec
		p = &tipProducer{refs: 1, cancel: cancel}
		r.active[key] = p
		go start(ctx)
	}
	return func() { r.release(key) }
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

// acquireTipProducer ensures a shared producer runs for the pair and
// holds a reference to it. Returns the Hub topic to subscribe to and
// the release func.
func (s *Server) acquireTipProducer(asset, quote canonical.Asset, window int) (topic string, release func()) {
	key := tipProducerKey{asset: asset.String(), quote: quote.String(), window: window}
	release = s.tipProducers.acquire(key, func(ctx context.Context) {
		s.runSharedTipProducer(ctx, key, asset, quote, window)
	})
	return key.topic(), release
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
