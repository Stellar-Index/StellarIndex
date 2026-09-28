package customerwebhook

import "net/http"

// unguardedClient passes the caller's client through untouched so a test can
// deliver to an httptest server on loopback, which the SSRF guard refuses.
func unguardedClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}
	}
	return c
}

// NewUnguardedForTest is New without the delivery-time SSRF guard. It exists
// only in test binaries.
func NewUnguardedForTest(store DeliveryStore, opts Options) *Worker {
	return newWorker(store, opts, unguardedClient)
}

// GuardedTransportForTest exposes the *http.Transport production New()
// builds, so a test can assert its pool-bounding fields without a live
// network call.
func GuardedTransportForTest(c *http.Client) *http.Transport {
	return guardedClient(c).Transport.(*http.Transport)
}
