package customerwebhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestWorker_StalledEndpointDoesNotBlockOthers — GH-663. The batch was
// delivered strictly serially, so one endpoint that holds its connection
// open delayed every other customer's delivery by the full attempt
// timeout per row. Here endpoint A stalls on both its rows, which are
// claimed AHEAD of endpoint B's; B must still be delivered while A is
// stalled, and A must never have two POSTs in flight at once.
func TestWorker_StalledEndpointDoesNotBlockOthers(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unstall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unstall)

	var aInFlight, aMaxInFlight atomic.Int32
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := aInFlight.Add(1)
		defer aInFlight.Add(-1)
		for {
			m := aMaxInFlight.Load()
			if n <= m || aMaxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer stalled.Close()

	bDelivered := make(chan struct{}, 1)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case bDelivered <- struct{}{}:
		default:
		}
	}))
	defer healthy.Close()

	store := newFakeStore()
	secret := []byte("test-secret-bytes")
	a := platform.CustomerWebhook{ID: uuid.New(), URL: stalled.URL, SecretHash: secret, Enabled: true}
	b := platform.CustomerWebhook{ID: uuid.New(), URL: healthy.URL, SecretHash: secret, Enabled: true}
	store.addWebhook(a)
	store.addWebhook(b)
	due := time.Now().Add(-time.Second)
	for _, hook := range []uuid.UUID{a.ID, a.ID, b.ID} {
		store.enqueue(platform.WebhookDelivery{
			ID: uuid.New(), WebhookID: hook, EventType: string(platform.WebhookEventIncidentSEV1),
			Payload: []byte(`{}`), NextAttemptAt: due,
		})
	}

	w := customerwebhook.NewUnguardedForTest(store, customerwebhook.Options{
		PollInterval: time.Hour,
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = w.Run(ctx); close(done) }()

	select {
	case <-bDelivered:
	case <-time.After(3 * time.Second):
		t.Error("endpoint B was not delivered while endpoint A stalled: one slow endpoint is blocking the queue")
	}
	unstall()
	deadline := time.Now().Add(5 * time.Second)
	for deliveredCount(store) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if got := aMaxInFlight.Load(); got != 1 {
		t.Errorf("endpoint A had %d POSTs in flight at once, want 1 (per-endpoint deliveries must stay serial)", got)
	}
	if got := deliveredCount(store); got != 3 {
		t.Errorf("delivered %d of 3 rows once A recovered", got)
	}
}

func deliveredCount(s *fakeStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.delivered)
}
