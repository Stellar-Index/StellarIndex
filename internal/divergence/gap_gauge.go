package divergence

import (
	"math"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// Gap fractions the pairs-over gauge counts, with their threshold label.
const (
	gapWarnFraction = 0.05
	gapCritFraction = 0.10
)

// gapTracker keeps each pair's latest per-reference |ours−ref|/ref and
// publishes the bounded Prometheus view of it: the worst gap per reference and
// how many pairs sit over 5 % / 10 %. Per-pair values stay in Redis and
// Postgres; only these aggregates reach the registry.
type gapTracker struct {
	mu     sync.Mutex
	ttl    time.Duration
	refs   []string
	byPair map[string]pairGaps
}

type pairGaps struct {
	at    time.Time
	byRef map[string]float64
}

func newGapTracker(refs []string, ttl time.Duration) *gapTracker {
	g := &gapTracker{ttl: ttl, refs: refs, byPair: map[string]pairGaps{}}
	g.publish(time.Time{})
	return g
}

// observe records one refresh. A pinned or unpriced refresh carries no
// verdict, so it clears the pair rather than keeping a stale gap alive.
func (g *gapTracker) observe(pair string, ourPrice float64, sources map[string]float64, pinned bool, now time.Time) {
	byRef := map[string]float64{}
	if !pinned {
		for ref, refPrice := range sources {
			gap := math.Abs(ourPrice-refPrice) / refPrice
			if refPrice > 0 && !math.IsNaN(gap) && !math.IsInf(gap, 0) {
				byRef[ref] = gap
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(byRef) == 0 {
		delete(g.byPair, pair)
	} else {
		g.byPair[pair] = pairGaps{at: now, byRef: byRef}
	}
	g.publishLocked(now)
}

func (g *gapTracker) publish(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.publishLocked(now)
}

func (g *gapTracker) publishLocked(now time.Time) {
	maxByRef := make(map[string]float64, len(g.refs))
	for _, r := range g.refs {
		maxByRef[r] = 0
	}
	var warn, crit int
	for pair, pg := range g.byPair {
		// A pair that stopped refreshing must not hold the gauge up forever.
		if g.ttl > 0 && !now.IsZero() && now.Sub(pg.at) > g.ttl {
			delete(g.byPair, pair)
			continue
		}
		worst := 0.0
		for ref, gap := range pg.byRef {
			maxByRef[ref] = math.Max(maxByRef[ref], gap)
			worst = math.Max(worst, gap)
		}
		if worst > gapWarnFraction {
			warn++
		}
		if worst > gapCritFraction {
			crit++
		}
	}
	for ref, v := range maxByRef {
		obs.DivergenceMaxAbsFraction.WithLabelValues(ref).Set(v)
	}
	obs.DivergencePairsOver.WithLabelValues("5pct").Set(float64(warn))
	obs.DivergencePairsOver.WithLabelValues("10pct").Set(float64(crit))
}
