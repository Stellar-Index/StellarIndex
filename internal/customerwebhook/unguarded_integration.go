//go:build integration

package customerwebhook

import "net/http"

// NewUnguardedForIntegrationTest is New without the delivery-time SSRF guard so tests can hit loopback httptest servers; the integration build tag keeps it out of production binaries.
func NewUnguardedForIntegrationTest(store DeliveryStore, opts Options) *Worker {
	return newWorker(store, opts, func(c *http.Client) *http.Client {
		if c == nil {
			return &http.Client{Timeout: defaultHTTPTimeout}
		}
		return c
	})
}
