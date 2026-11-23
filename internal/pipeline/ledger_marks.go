package pipeline

import (
	"context"
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// LedgerMarks counts, per ledger, the rows the producer enqueued that the sink lane has not yet landed, so a resume
// cursor can stop at the last ledger every row of which is durable. Nil-safe: a nil *LedgerMarks tracks nothing.
type LedgerMarks struct {
	mu        sync.Mutex
	processed uint32         // last ledger whose events were all enqueued
	pending   map[uint32]int // ledger -> rows enqueued, not yet landed
}

// NewLedgerMarks starts tracking after lastDurable, the ledger the persisted cursor already covers.
func NewLedgerMarks(lastDurable uint32) *LedgerMarks {
	return &LedgerMarks{processed: lastDurable, pending: make(map[uint32]int)}
}

// Processed returns the last ledger whose events were all enqueued.
func (m *LedgerMarks) Processed() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processed
}

// DurableThrough returns the highest ledger, at most [LedgerMarks.Processed], at or below which every enqueued row
// has landed. It never decreases: rows are only enqueued for ledgers above it.
func (m *LedgerMarks) DurableThrough() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.processed
	for l := range m.pending {
		if l <= d {
			d = l - 1
		}
	}
	return d
}

// AllLanded records that the sink lane drained every enqueued row with nothing abandoned, so every processed ledger
// is durable. Call it only after the lane returned with a zero [ShutdownLoss] from a closed channel.
func (m *LedgerMarks) AllLanded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.pending)
}

func (m *LedgerMarks) enqueued(ledger uint32) {
	if m == nil || ledger == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[ledger]++
}

func (m *LedgerMarks) landed(ledger uint32) {
	if m == nil || ledger == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending[ledger] <= 1 {
		delete(m.pending, ledger)
		return
	}
	m.pending[ledger]--
}

func (m *LedgerMarks) finished(ledger uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ledger > m.processed {
		m.processed = ledger
	}
}

// markLedger is the ledger an event is counted under: a trade's own ledger (the sink acks trades from the flushed
// batch, where only the trade survives), else the ledger it was decoded from.
func markLedger(ev consumer.Event, ledger uint32) uint32 {
	if t, ok := tradeFromEvent(ev); ok {
		return t.Ledger
	}
	return ledger
}

// enqueueLedger sends one ledger's events to the sink, counting each against marks before the send.
func enqueueLedger(ctx context.Context, events chan<- consumer.Event, outputs []consumer.Event, ledger uint32, marks *LedgerMarks) error {
	for _, ev := range outputs {
		marks.enqueued(markLedger(ev, ledger))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case events <- ledgerEvent{Event: ev, ledger: ledger, marks: marks}:
		}
	}
	if marks != nil {
		marks.finished(ledger)
	}
	return nil
}

// markAcks is one worker's side of [LedgerMarks]. It acks only while the lane's ctx is live: after cancel the drain
// paths write rows it never counted, so the marks freeze and the next start re-walks from there.
type markAcks struct {
	ctx    context.Context //nolint:containedctx // the lane's lifetime, checked per ack, never used for I/O
	marks  *LedgerMarks
	trades map[uint32]int // tracked trades buffered, per ledger
}

func newMarkAcks(ctx context.Context) *markAcks {
	return &markAcks{ctx: ctx, trades: make(map[uint32]int)}
}

// event acks one landed (or projector-owned, so skipped) non-trade row.
func (a *markAcks) event(marks *LedgerMarks, ledger uint32) {
	if a.ctx.Err() != nil {
		return
	}
	marks.landed(ledger)
}

// bufferTrade records that t, counted against marks, is waiting in the worker's batch.
func (a *markAcks) bufferTrade(marks *LedgerMarks, t canonical.Trade) {
	if marks == nil || t.Ledger == 0 {
		return
	}
	a.marks = marks
	a.trades[t.Ledger]++
}

// flushed acks the tracked trades of batch that are not in abandoned, and returns abandoned.
func (a *markAcks) flushed(batch, abandoned []canonical.Trade) []canonical.Trade {
	if a.marks == nil || a.ctx.Err() != nil {
		return abandoned
	}
	lost := make(map[uint32]int, len(abandoned))
	for _, t := range abandoned {
		lost[t.Ledger]++
	}
	for _, t := range batch {
		if lost[t.Ledger] > 0 {
			lost[t.Ledger]--
			continue
		}
		if a.trades[t.Ledger] == 0 {
			continue
		}
		a.trades[t.Ledger]--
		if a.trades[t.Ledger] == 0 {
			delete(a.trades, t.Ledger)
		}
		a.marks.landed(t.Ledger)
	}
	return abandoned
}
