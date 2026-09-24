package customerwebhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A caller-supplied Options.HTTPClient must not be a way around the
// delivery-time SSRF guard: the guard belongs to every Worker, not only to
// the one built from the nil default.
func TestNew_SuppliedClientStillRefusesInternalDestinations(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	w := New(nopStore{}, Options{HTTPClient: &http.Client{Timeout: 2 * time.Second}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := w.opts.HTTPClient.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("POST to loopback %s through a caller-supplied client = %v, want an SSRF-defence refusal", ts.URL, err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("loopback receiver was hit %d times, want 0", n)
	}
	if got := w.opts.HTTPClient.Timeout; got != 2*time.Second {
		t.Errorf("worker client Timeout = %s, want the caller's 2s preserved", got)
	}
}

func TestNew_SuppliedClientCannotFollowRedirects(t *testing.T) {
	follow := func(*http.Request, []*http.Request) error { return nil }
	w := New(nopStore{}, Options{HTTPClient: &http.Client{CheckRedirect: follow}})
	c := w.opts.HTTPClient
	if c.CheckRedirect == nil {
		t.Fatal("worker client follows redirects — a 302 to an internal host would route around the dialer")
	}
	if err := c.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// A supplied Transport owns its own dialer (and proxy), so honouring it
// would bypass the guard; New must refuse it rather than drop it silently.
func TestNew_RejectsSuppliedTransport(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a caller-supplied Transport, whose dialer bypasses the SSRF guard")
		}
	}()
	New(nopStore{}, Options{HTTPClient: &http.Client{Transport: http.DefaultTransport}})
}
