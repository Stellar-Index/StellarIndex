package v1

import (
	"context"
	"sync"
	"time"
)

// slowWatermark models the production lake reader: LakeWatermark is a plain QueryRow with no internal deadline,
// so delay elapses regardless of the context handed in.
type slowWatermark struct {
	mu       sync.Mutex
	calls    int
	finished int

	delay    time.Duration
	ledger   uint32
	closedAt time.Time
	err      error
}

func (s *slowWatermark) LakeWatermark(context.Context) (uint32, time.Time, error) {
	s.mu.Lock()
	s.calls++
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished++
	return s.ledger, s.closedAt, s.err
}

func (s *slowWatermark) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *slowWatermark) finishedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finished
}
