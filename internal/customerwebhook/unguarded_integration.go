//go:build integration

package customerwebhook

import "net/http"

// NewUnguardedForIntegrationTest is New without the delivery-time SSRF guard,
// so an integration test can deliver to an httptest server on loopback. The
// integration build tag keeps it out of every production binary.
func NewUnguardedForIntegrationTest(store DeliveryStore, opts Options) *Worker {
	return newWorker(store, opts, func(c *http.Client) *http.Client {
		if c == nil {
			return &http.Client{Timeout: defaultHTTPTimeout}
		}
		return c
	})
}
