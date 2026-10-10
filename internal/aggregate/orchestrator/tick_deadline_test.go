package orchestrator

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// wedgedStore is a Store whose trades query for one pair stops
// answering, the way a query on a half-open connection does: it returns
// only when its context ends. Every other pair is served normally.
type wedgedStore struct {
	*mockStore
	mu     sync.Mutex
	wedged string // pair.String(); "" = healed
}

func (s *wedgedStore) setWedged(pair string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wedged = pair
}

func (s *wedgedStore) TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	s.mu.Lock()
	wedged := s.wedged
	s.mu.Unlock()
	if p.String() == wedged {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.mockStore.TradesInRange(ctx, p, from, to, limit)
}

// fxCallRecord is what one FXQuoteAtOrBefore call looked like.
type fxCallRecord struct {
	cutoff    time.Time
	bounded   bool
	remaining time.Duration
}

// deadlineRecordingFX answers every FX query and records whether the
// context it was given carried a deadline.
type deadlineRecordingFX struct {
	mu         sync.Mutex
	observedAt time.Time
	calls      []fxCallRecord
}

func (f *deadlineRecordingFX) FXQuoteAtOrBefore(ctx context.Context, _ canonical.Pair, cutoff time.Time, _ []string) (*big.Rat, time.Time, string, error) {
	dl, ok := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, fxCallRecord{cutoff: cutoff, bounded: ok, remaining: time.Until(dl)})
	f.mu.Unlock()
	return big.NewRat(80, 100), f.observedAt, "massive", nil
}
