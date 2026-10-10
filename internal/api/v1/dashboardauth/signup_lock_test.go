package dashboardauth

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// fakeEmailLocker is the in-memory analogue of [auth.RedisSignupEmailLocker]
// for unit tests. Acquire returns true exactly once per key with a
// per-acquire token; Release removes the key only when the token still
// matches, so a subsequent Acquire
// wins again. Mirrors the SETNX+CAD ownership the Redis adapter implements.
type fakeEmailLocker struct {
	mu   sync.Mutex
	held map[string]string // key -> current holder's token
	seq  int
}

func newFakeEmailLocker() *fakeEmailLocker {
	return &fakeEmailLocker{held: map[string]string{}}
}

func (l *fakeEmailLocker) Acquire(_ context.Context, key string, _ time.Duration) (bool, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, held := l.held[key]; held {
		return false, "", nil
	}
	l.seq++
	token := fmt.Sprintf("tok-%d", l.seq)
	l.held[key] = token
	return true, token, nil
}

func (l *fakeEmailLocker) Release(_ context.Context, key, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if token == "" {
		return nil
	}
	if l.held[key] == token {
		delete(l.held, key)
	}
	return nil
}
