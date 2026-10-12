package soroswap

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── Event envelope ─────────────────────────────────────────────

// TradeEvent is the [consumer.Event] Soroswap's Decoder emits on a completed swap+sync pair, type-switched by the sink.
type TradeEvent struct {
	Trade canonical.Trade
}

// EventKind implements [consumer.Event].
func (TradeEvent) EventKind() string { return "soroswap.trade" }

// Source implements [consumer.Event] — matches [SourceName].
func (TradeEvent) Source() string { return SourceName }

// Compile-time check that TradeEvent satisfies consumer.Event.
var _ consumer.Event = TradeEvent{}

// SkimEvent is the [consumer.Event] emitted on a pair `skim` event (soroswap_skim_events row); the optional `to` is empty when the WASM omits it.
type SkimEvent struct {
	ContractID string // pair contract C-strkey
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	EventIndex uint32 // position within the op; part of the migration 0043 PK so multiple skims per op don't collapse
	ObservedAt time.Time
	To         string // optional recipient strkey; "" → NULL
	Amount0    canonical.Amount
	Amount1    canonical.Amount
}

// EventKind implements [consumer.Event].
func (SkimEvent) EventKind() string { return "soroswap.skim" }

// Source implements [consumer.Event] — matches [SourceName].
func (SkimEvent) Source() string { return SourceName }

// Compile-time check that SkimEvent satisfies consumer.Event.
var _ consumer.Event = SkimEvent{}

// LiquidityEvent is the [consumer.Event] emitted on a pair `deposit` or `withdraw` (soroswap_liquidity row); Token0/Token1 come from the factory new_pair registry, are empty when the pair is unseeded (row still emitted), and it does NOT feed the swap+sync buffer.
type LiquidityEvent struct {
	ContractID  string // pair contract C-strkey (emitter)
	Ledger      uint32
	TxHash      string
	OpIndex     uint32
	EventIndex  uint32 // position within the op; part of the migration 0127 PK
	ObservedAt  time.Time
	Action      string // EventDeposit | EventWithdraw
	To          string // LP provider address (the body's `to`)
	Token0      string // canonical asset_id from the pair registry; "" if unseeded
	Token1      string // canonical asset_id; "" if unseeded
	Amount0     canonical.Amount
	Amount1     canonical.Amount
	Liquidity   canonical.Amount // LP shares minted (deposit) / burned (withdraw)
	NewReserve0 canonical.Amount
	NewReserve1 canonical.Amount
}

// EventKind implements [consumer.Event].
func (LiquidityEvent) EventKind() string { return "soroswap.liquidity" }

// Source implements [consumer.Event] — matches [SourceName].
func (LiquidityEvent) Source() string { return SourceName }

// Compile-time check that LiquidityEvent satisfies consumer.Event.
var _ consumer.Event = LiquidityEvent{}

// ─── Correlation buffer ─────────────────────────────────────────
// Groups swap + sync by groupKey and emits complete pairs; incompletes wait for their partner
// until ClosedAt is older than maxAge, then are returned as orphans and dropped. Age-based
// eviction bounds memory when a swap never gets its sync.

// defaultOrphanMaxAge: how long an incomplete entry waits for its partner. Swap+sync share a tx and arrive adjacently,
// so 5m is a generous ceiling for out-of-order quirks.
const defaultOrphanMaxAge = 5 * time.Minute

type buffer struct {
	m      map[groupKey]*RawPair
	maxAge time.Duration
	nowFn  func() time.Time
}

func newBuffer() *buffer {
	return &buffer{
		m:      map[groupKey]*RawPair{},
		maxAge: defaultOrphanMaxAge,
		nowFn:  time.Now,
	}
}

// absorb records an event and returns the pairs that just completed, including a swap-only group rotated out by a same-pair
// re-entry. Stale orphans are returned separately as evicted, never as completed.
func (b *buffer) absorb(e *events.Event, kind string, closedAt time.Time) (completed []RawPair, evicted []RawPair) {
	// Evict stale orphans first to keep the map bounded. The reference time is the incoming
	// event's ClosedAt, not wall-clock, so backfill compares against the replayed timeline.
	evicted = b.sweepStale(closedAt)

	k := keyOf(e)
	p, ok := b.m[k]
	if ok && p.rotates(kind, e) {
		// A swap or sync landing in an occupied slot with a DIFFERENT EventIndex is a second swap
		// through the SAME pair in the same op; overwriting would lose the first trade uncounted.
		// Rotate the group out: swap-only is final (the trade reads from the swap body alone),
		// sync-only is an orphan. The same EventIndex is a redelivery: overwrite.
		if p.Swap != nil {
			completed = append(completed, *p)
		} else {
			evicted = append(evicted, *p)
		}
		ok = false
	}
	if !ok {
		p = &RawPair{
			Ledger: e.Ledger, TxHash: e.TxHash, OpIndex: uint32(e.OperationIndex),
			Pair: e.ContractID, ClosedAt: closedAt,
		}
		b.m[k] = p
	}
	switch kind {
	case EventSwap:
		p.Swap = e
	case EventSync:
		p.Sync = e
	}
	if p.Complete() {
		delete(b.m, k)
		completed = append(completed, *p)
	}
	return completed, evicted
}

// rotates reports whether e lands in an occupied slot of p as a DIFFERENT event (another EventIndex): a second swap/sync
// through the same pair in one op, not a redelivery.
func (p *RawPair) rotates(kind string, e *events.Event) bool {
	var prior *events.Event
	switch kind {
	case EventSwap:
		prior = p.Swap
	case EventSync:
		prior = p.Sync
	}
	return prior != nil && prior.EventIndex != e.EventIndex
}

// sweepStale removes entries whose ClosedAt is older than maxAge relative to ref (the latest event's ClosedAt).
// Never use nowFn() in backfill: historical ClosedAt is far behind wall-clock, so every entry would evict early.
func (b *buffer) sweepStale(ref time.Time) []RawPair {
	if b.maxAge <= 0 {
		return nil
	}
	if ref.IsZero() {
		ref = b.nowFn()
	}
	cutoff := ref.Add(-b.maxAge)
	var evicted []RawPair
	for k, p := range b.m {
		if p.ClosedAt.Before(cutoff) {
			evicted = append(evicted, *p)
			delete(b.m, k)
		}
	}
	return evicted
}

// orphans returns every incomplete entry, without mutating the buffer; called after a bounded-range ingest so metrics
// can attribute the leak.
func (b *buffer) orphans() []RawPair {
	out := make([]RawPair, 0, len(b.m))
	for _, p := range b.m {
		out = append(out, *p)
	}
	return out
}

// size returns the number of in-flight (incomplete) pairs.
func (b *buffer) size() int { return len(b.m) }
