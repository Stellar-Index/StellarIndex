package pipeline

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// ledgerEvent tags an on-chain event with the ledger it was decoded from, so a row the sink abandons at shutdown
// names a ledger the cursor can be moved below. Only [ProcessLedger] makes one; the sink takes the tag off on
// receipt (eventLedger) because every later step switches on the concrete event type.
type ledgerEvent struct {
	consumer.Event
	ledger uint32
	// marks, when set, is told once the event's row has landed (see [LedgerMarks]).
	marks *LedgerMarks
}

func withLedger(ev consumer.Event, ledger uint32) consumer.Event {
	return ledgerEvent{Event: ev, ledger: ledger}
}

// eventMarks returns the [LedgerMarks] the producer counted ev against, nil for an untracked event.
func eventMarks(ev consumer.Event) *LedgerMarks {
	if le, ok := ev.(ledgerEvent); ok {
		return le.marks
	}
	return nil
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
	// OffChainFrom and OffChainTo bound the timestamps of the abandoned ledgerless trades, the window to refetch
	// from the venues; zero when none.
	OffChainFrom, OffChainTo time.Time
	// Venues lists the sources of the abandoned ledgerless trades, sorted and comma-separated (a string keeps
	// ShutdownLoss comparable): with the window, what to refetch.
	Venues string
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
			l.offChain(t.Timestamp)
			l.venue(t.Source)
		case l.loss.MinLedger == 0 || t.Ledger < l.loss.MinLedger:
			l.loss.MinLedger = t.Ledger
		}
	}
}

// offChain widens the off-chain window to ts. Caller holds l.mu.
func (l *lossTracker) offChain(ts time.Time) {
	if ts.IsZero() {
		return
	}
	if l.loss.OffChainFrom.IsZero() || ts.Before(l.loss.OffChainFrom) {
		l.loss.OffChainFrom = ts
	}
	if ts.After(l.loss.OffChainTo) {
		l.loss.OffChainTo = ts
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

// venue adds source to the sorted venue list. Caller holds l.mu.
func (l *lossTracker) venue(source string) {
	var venues []string
	if l.loss.Venues != "" {
		venues = strings.Split(l.loss.Venues, ",")
	}
	if !slices.Contains(venues, source) {
		venues = append(venues, source)
		slices.Sort(venues)
		l.loss.Venues = strings.Join(venues, ",")
	}
}
