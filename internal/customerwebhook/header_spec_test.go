// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package customerwebhook_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/customerwebhook"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestWorker_EveryDeliveryHeaderIsInSpec delivers one webhook and requires
// every X-StellarIndex-* header the receiver saw to be named in the
// OpenAPI spec, so a customer building a verifier from the spec is told
// about every header the sender sets.
func TestWorker_EveryDeliveryHeaderIsInSpec(t *testing.T) {
	spec, err := os.ReadFile("../../openapi/stellar-index.v1.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	specLower := strings.ToLower(string(spec))

	var (
		mu   sync.Mutex
		seen = map[string]bool{}
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-stellarindex-") {
				seen[name] = true
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	store := newFakeStore()
	webhookID, secret := makeWebhook(t, ts.URL, true)
	store.addWebhook(platform.CustomerWebhook{ID: webhookID, URL: ts.URL, SecretHash: secret, Enabled: true})
	store.enqueue(platform.WebhookDelivery{
		ID:            uuid.New(),
		WebhookID:     webhookID,
		EventType:     string(platform.WebhookEventDivergenceFiring),
		Payload:       []byte(`{}`),
		NextAttemptAt: time.Now().Add(-time.Second),
	})
	runOneTick(t, store, customerwebhook.Options{PollInterval: 30 * time.Millisecond})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 4 {
		t.Fatalf("receiver saw %d X-StellarIndex-* headers (%v), want at least 4 — the delivery did not run", len(seen), seen)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.Contains(specLower, "`"+strings.ToLower(name)+"`") {
			t.Errorf("delivery header %s is sent but not documented in openapi/stellar-index.v1.yaml", name)
		}
	}
}
