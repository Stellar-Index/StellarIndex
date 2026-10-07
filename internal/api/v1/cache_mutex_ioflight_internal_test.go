package v1

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// blockingReadyCheck is a ReadyChecker that ignores ctx and blocks on
// `release` — the "wedged, misbehaving dependency" shape server.go's
// ReadyChecker doc warns about. Simulates a check round that runs far
// longer than any one caller is willing to wait.
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

// TestHandleReadyz_ConcurrentCallerAbandonsViaContext is the regression
// test that handleReadyz does not hold readyzMu across the
// whole check round (computeReadyz), so a second caller would queue
// synchronously on the mutex with no way to honour its own request
// context. On an unauthenticated, rate-limit-exempt route, that meant
// every concurrent probe blocked for up to the round's full budget
// regardless of its own deadline. The fix runs the round detached and
// has waiters select on it alongside their own ctx — a caller whose
// context expires must get its handler call back promptly, without
// waiting for the round in flight to finish.
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

// TestHandleLivezLake_ConcurrentCallerAbandonsViaContext mirrors
// TestHandleReadyz_ConcurrentCallerAbandonsViaContext for the
// second endpoint: /v1/livez/lake's 1s TTL against a 5s ping budget was
// exactly the head-of-line-blocking gap the finding named — every
// probe blocked on livezLakeMu across the ping, up to 5s, on the route
// an operator polls to decide whether to drain a region.
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
