package timescale

import "time"

// Live writes land inside the recent window, so only a backfill or re-derive can fill [since, recent) after a miss; the TTL bounds how
// long that stays unseen. A miss there plans every trades chunk in range (~165ms per arm on a fresh connection on r1).
const (
	olderTradesEmptyTTL        = time.Minute
	olderTradesEmptyMaxEntries = 4096
)

func (s *Store) olderTradesKnownEmpty(key string) bool {
	s.olderTradesMu.Lock()
	defer s.olderTradesMu.Unlock()
	at, ok := s.olderTradesEmpty[key]
	return ok && time.Since(at) < olderTradesEmptyTTL
}

func (s *Store) markOlderTradesEmpty(key string) {
	s.olderTradesMu.Lock()
	defer s.olderTradesMu.Unlock()
	if s.olderTradesEmpty == nil || len(s.olderTradesEmpty) >= olderTradesEmptyMaxEntries {
		s.olderTradesEmpty = map[string]time.Time{}
	}
	s.olderTradesEmpty[key] = time.Now()
}
