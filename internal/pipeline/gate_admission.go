package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// gateAdmissionRetryEvery is how often unpersisted admissions are retried.
const gateAdmissionRetryEvery = 5 * time.Second

// gateAdmissions persists live factory-child gate admissions (protocol_contracts, soroswap_pairs,
// sushiswap_v3_pools). The in-memory gate already holds the child, so ingest looks healthy when the write fails;
// but after a restart the creation event is behind the cursor and only the table re-admits the child. A failed
// write is therefore kept and retried until it lands, counted, and exposed as a gauge an alert watches.
type gateAdmissions struct {
	ctx        context.Context
	logger     *slog.Logger
	retryEvery time.Duration

	mu      sync.Mutex
	pending map[string]pendingAdmission
	running bool
}

type pendingAdmission struct {
	source, id string
	write      func(context.Context) error
}

func newGateAdmissions(ctx context.Context, logger *slog.Logger) *gateAdmissions {
	return &gateAdmissions{ctx: ctx, logger: logger, retryEvery: gateAdmissionRetryEvery, pending: map[string]pendingAdmission{}}
}

// persist makes one attempt on the caller's goroutine, bounded by upsertHookTimeout because that goroutine is the
// dispatcher's. write must be idempotent: a kept write is retried until it succeeds or the process stops.
func (g *gateAdmissions) persist(source, id string, write func(context.Context) error) {
	if err := g.attempt(g.ctx, write); err != nil {
		obs.GateAdmissionPersistFailuresTotal.WithLabelValues(source).Inc()
		g.logger.Warn("gate admission not persisted; it will be retried",
			"source", source, "contract", id, "err", err)
		g.keep(pendingAdmission{source: source, id: id, write: write})
	}
}

func (g *gateAdmissions) attempt(ctx context.Context, write func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(ctx, upsertHookTimeout)
	defer cancel()
	return write(wctx)
}

func (g *gateAdmissions) keep(p pendingAdmission) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending[p.source+"/"+p.id] = p
	g.publishLocked(p.source)
	if !g.running {
		g.running = true
		go g.retryLoop()
	}
}

func (g *gateAdmissions) publishLocked(source string) {
	n := 0
	for _, p := range g.pending {
		if p.source == source {
			n++
		}
	}
	obs.GateAdmissionsUnpersisted.WithLabelValues(source).Set(float64(n))
}

func (g *gateAdmissions) retryLoop() {
	// A panic here leaves the gauge above zero, so the alert still fires.
	defer worker.Recover(g.logger, "gate-admission-retry")
	t := time.NewTicker(g.retryEvery)
	defer t.Stop()
	for {
		select {
		case <-g.ctx.Done():
			// One last try on a context the shutdown has not cancelled; what still fails is lost to the next start.
			g.retry(context.WithoutCancel(g.ctx), true)
			return
		case <-t.C:
			if g.retry(g.ctx, false) {
				return
			}
		}
	}
}

// retry attempts every kept write and reports whether none is left, in which case the loop has stopped.
func (g *gateAdmissions) retry(ctx context.Context, last bool) (drained bool) {
	g.mu.Lock()
	batch := make(map[string]pendingAdmission, len(g.pending))
	for k, p := range g.pending {
		batch[k] = p
	}
	g.mu.Unlock()

	for k, p := range batch {
		err := g.attempt(ctx, p.write)
		if err != nil {
			obs.GateAdmissionPersistFailuresTotal.WithLabelValues(p.source).Inc()
			if last {
				g.logger.Error("gate admission LOST at shutdown: after the restart this contract's events are rejected "+
					"as foreign until `stellarindex-ops seed-protocol-contracts` re-walks its factory",
					"source", p.source, "contract", p.id, "err", err)
			}
			continue
		}
		g.logger.Info("gate admission persisted on retry", "source", p.source, "contract", p.id)
		g.mu.Lock()
		delete(g.pending, k)
		g.publishLocked(p.source)
		g.mu.Unlock()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) == 0 {
		g.running = false
		return true
	}
	return false
}
