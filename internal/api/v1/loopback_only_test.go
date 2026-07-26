package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLoopbackOnly pins C3-029/C3-106 (audit-2026-07-23). The /metrics
// gate checked only that RemoteAddr was loopback — but the documented
// topology is Caddy on the SAME HOST proxying to 127.0.0.1:3000, so a
// misconfigured proxy that forwards public traffic presents a loopback
// RemoteAddr and the guard passed it. It was inert against exactly the
// failure it was written for.
func TestLoopbackOnly(t *testing.T) {
	served := false
	h := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		wantServed bool
	}{
		{"local_scraper_v4", "127.0.0.1:54321", nil, true},
		{"local_scraper_v6", "[::1]:54321", nil, true},
		{"no_port", "127.0.0.1", nil, true},

		{"public_remote", "203.0.113.5:443", nil, false},
		{"private_lan_remote", "10.0.0.7:443", nil, false},
		{"unparseable_remote", "not-an-ip", nil, false},

		// The C3-029 half: loopback RemoteAddr because the proxy is on
		// the same host, but the request was RELAYED for a remote client.
		{
			"same_host_proxy_xff", "127.0.0.1:54321",
			map[string]string{"X-Forwarded-For": "203.0.113.5"},
			false,
		},
		{
			"same_host_proxy_real_ip", "127.0.0.1:54321",
			map[string]string{"X-Real-Ip": "203.0.113.5"},
			false,
		},
		{
			"same_host_proxy_fwd_host", "127.0.0.1:54321",
			map[string]string{"X-Forwarded-Host": "api.stellarindex.io"},
			false,
		},
		{
			"same_host_proxy_rfc7239", "127.0.0.1:54321",
			map[string]string{"Forwarded": "for=203.0.113.5;proto=https"},
			false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			served = false
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if served != tc.wantServed {
				t.Errorf("handler served = %v, want %v", served, tc.wantServed)
			}
			// 404 (not 403) is deliberate: 403 confirms the route exists.
			if !tc.wantServed && rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (403 would confirm the route exists)", rec.Code)
			}
		})
	}
}
