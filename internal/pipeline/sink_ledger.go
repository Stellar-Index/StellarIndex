package pipeline

import (
	"sync"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// ledgerEvent tags an on-chain event with the ledger it was decoded from, so a row the sink abandons at shutdown
// names a ledger the cursor can be moved below. Only [ProcessLedger] makes one; the sink takes the tag off on
// receipt (eventLedger) because every later step switches on the concrete event type.
type ledgerEvent struct {
	consumer.Event
	ledger uint32
}

func withLedger(ev consumer.Event, ledger uint32) consumer.Event {
	return ledgerEvent{Event: ev, ledger: ledger}
}

// eventLedger returns the event as its producer made it and its ledger, 0 for an event that was never tagged.
func eventLedger(ev consumer.Event) (consumer.Event, uint32) {
	if le, ok := ev.(ledgerEvent); ok {
		return le.Event, le.ledger
	}
	return ev, 0
}

// ShutdownLoss is what one [PersistEvents] run abandoned (rows no drain pass landed); a caller checkpointing a resume cursor (backfill) MUST NOT move it past them.
type ShutdownLoss struct {
	// Rows is the number of abandoned rows.
	Rows int
	// MinLedger is the lowest ledger among abandoned on-chain rows; 0 when none.
	MinLedger uint32
	// LedgerUnknown is set when an abandoned row carried no ledger (an off-chain trade or event), so no cursor
	// position covers it.
	LedgerUnknown bool
}

// lossTracker accumulates one PersistEvents run's [ShutdownLoss] across
// its workers. Nil-safe so the drain helpers' direct unit tests can pass nil.
type lossTracker struct {
	mu   sync.Mutex
	loss ShutdownLoss
}

func (l *lossTracker) trades(abandoned []canonical.Trade) {
	if l == nil || len(abandoned) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loss.Rows += len(abandoned)
	for _, t := range abandoned {
		switch {
		case t.Ledger == 0: // off-chain trade: no ledger to cap on
			l.loss.LedgerUnknown = true
		case l.loss.MinLedger == 0 || t.Ledger < l.loss.MinLedger:
			l.loss.MinLedger = t.Ledger
		}
	}
}

func (l *lossTracker) event(ledger uint32) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loss.Rows++
	switch {
	case ledger == 0:
		l.loss.LedgerUnknown = true
	case l.loss.MinLedger == 0 || ledger < l.loss.MinLedger:
		l.loss.MinLedger = ledger
	}
}

func (l *lossTracker) snapshot() ShutdownLoss {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loss
}
