package v1_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// TestStreamEndpointsKeepTheRouteCacheControl asserts the Cache-Control
// a client actually receives on a stream, through the full middleware
// stack: the CacheControl middleware's per-route policy, not the SSE
// writer's fallback. The writer must not overwrite it with a bare
// `no-cache`, which permits storage and drops `private`.
func TestStreamEndpointsKeepTheRouteCacheControl(t *testing.T) {
	cases := []struct {
		name, path, want string
		opts             v1.Options
	}{
		{"ledger", "/v1/ledger/stream", "private, no-store", v1.Options{Cursors: &advancingCursorsReader{ledger: 1000}}},
		{"price", "/v1/price/stream?asset=native&quote=fiat:USD", "no-store", v1.Options{Hub: streaming.NewHub(8)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(v1.New(tc.opts).Handler())
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != tc.want {
				t.Errorf("Cache-Control = %q, want the route policy %q", got, tc.want)
			}
		})
	}
}
