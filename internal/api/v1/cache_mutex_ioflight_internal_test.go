package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blockingReadyCheck is a ReadyChecker that ignores ctx and blocks on `release`: the wedged-dependency shape, a check round that
// outlasts any caller's patience.
type blockingReadyCheck struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReadyCheck) Ping(context.Context) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}
func (b *blockingReadyCheck) Name() string   { return "blocking" }
func (b *blockingReadyCheck) Critical() bool { return false }

// TestHandleReadyz_ConcurrentCallerAbandonsViaContext: handleReadyz must not hold readyzMu across the whole check round, or each
// concurrent probe on this unauthenticated, rate-limit-exempt route blocks for the round's full budget. The round runs detached
// and waiters select on their own ctx, so an expired caller returns promptly.
func TestHandleReadyz_ConcurrentCallerAbandonsViaContext(t *testing.T) {
	blocker := &blockingReadyCheck{started: make(chan struct{}), release: make(chan struct{})}
	srv := New(Options{ReadyChecks: []ReadyChecker{blocker}})
	t.Cleanup(func() { close(blocker.release) })

	// First caller starts the round; its only checker blocks forever
	// (until cleanup), so the round never completes on its own.
	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/readyz", nil)
		srv.handleReadyz(w, req)
	}()
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first round's check never started")
	}

	// Second caller's context expires well inside the still-running
	// round.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/readyz", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleReadyz(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleReadyz did not return when the caller's own context expired — blocked on readyzMu across the in-flight round (GH-587)")
	}
}

// blockingLakeCheck is a ClickHouse ReadyChecker (Name()=="clickhouse")
// whose Ping ignores ctx and blocks on `release`, for
// TestHandleLivezLake_ConcurrentCallerAbandonsViaContext.
type blockingLakeCheck struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingLakeCheck) Ping(context.Context) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}
func (b *blockingLakeCheck) Name() string   { return "clickhouse" }
func (b *blockingLakeCheck) Critical() bool { return false }

// TestHandleLivezLake_ConcurrentCallerAbandonsViaContext mirrors the readyz test for
// /v1/livez/lake, whose 1s TTL against a 5s ping budget let every probe block on livezLakeMu
// across the ping, on the route an operator polls to decide whether to drain a region.
func TestHandleLivezLake_ConcurrentCallerAbandonsViaContext(t *testing.T) {
	blocker := &blockingLakeCheck{started: make(chan struct{}), release: make(chan struct{})}
	srv := New(Options{ReadyChecks: []ReadyChecker{blocker}})
	t.Cleanup(func() { close(blocker.release) })

	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/livez/lake", nil)
		srv.handleLivezLake(w, req)
	}()
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first round's ping never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/livez/lake", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleLivezLake(w, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleLivezLake did not return when the caller's own context expired — blocked on livezLakeMu across the in-flight ping (GH-587)")
	}
}
