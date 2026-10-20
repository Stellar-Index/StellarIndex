package timescale

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Both bounds of [since, recent) are midnight-floored, so the range is fixed past history: only a backfill or re-derive changes it, and
// the TTL bounds how long that stays unseen. A wide read plans every trades chunk in range (~150ms per call on r1), and a batch client
// returning after an idle gap longer than the TTL pays it again on every row.
const (
	olderTradesTTL        = 6 * time.Hour
	olderTradesMaxEntries = 4096
)

type olderTradesEntry struct {
	trades []canonical.Trade // the range's newest trades, newest first; empty when it has none
	at     time.Time
}

func (s *Store) cachedOlderTrades(key string) ([]canonical.Trade, bool) {
	s.olderTradesMu.Lock()
	defer s.olderTradesMu.Unlock()
	e, ok := s.olderTrades[key]
	if !ok || time.Since(e.at) >= olderTradesTTL {
		return nil, false
	}
	return e.trades, true
}

func (s *Store) rememberOlderTrades(key string, trades []canonical.Trade) {
	s.olderTradesMu.Lock()
	defer s.olderTradesMu.Unlock()
	if s.olderTrades == nil || len(s.olderTrades) >= olderTradesMaxEntries {
		s.olderTrades = map[string]olderTradesEntry{}
	}
	s.olderTrades[key] = olderTradesEntry{trades: trades, at: time.Now()}
}
