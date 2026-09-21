package v1_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubMEVReader is the in-memory test seam for /v1/mev. It records the
// context each call receives (for deadline capture) and can be made to
// return an arbitrary error.
type stubMEVReader struct {
	mu      sync.Mutex
	lastCtx context.Context
	err     error
	rows    []timescale.MEVEventRow
}

func (r *stubMEVReader) ListMEVEvents(ctx context.Context, _ string, _ int) ([]timescale.MEVEventRow, error) {
	r.mu.Lock()
	r.lastCtx = ctx
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return r.rows, nil
}

func (r *stubMEVReader) deadline() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastCtx == nil {
		return time.Time{}, false
	}
	return r.lastCtx.Deadline()
}

// TestMEVEvents_InvalidKindIs400 pins Q204: an unrecognised ?kind= must
// 400, not pass straight through to the storage query (which would just
// index-scan zero rows and mask the caller's typo as "nothing detected
// yet" — the same silent-empty-page anti-pattern /v1/markets guards
// against on ?source=).
func TestMEVEvents_InvalidKindIs400(t *testing.T) {
	reader := &stubMEVReader{}
	srv := v1.New(v1.Options{MEV: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/mev?kind=not_a_real_kind")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invalid-kind") {
		t.Errorf("expected the `invalid-kind` problem type in the body, got: %s", body)
	}
}

// TestMEVEvents_PerHandlerTimeoutCeiling pins NS24: ListMEVEvents must
// be called against a context bounded by a per-handler ceiling well
// under the blanket RequestTimeout — the same pattern /v1/pools and
// /v1/markets use (#1082). Without it, a cold mev_events scan holds the
// connection until the 60s blanket deadline (or the ingress) gives up
// instead of returning a fast, retryable 503.
func TestMEVEvents_PerHandlerTimeoutCeiling(t *testing.T) {
	reader := &stubMEVReader{}
	srv := v1.New(v1.Options{MEV: reader, RequestTimeout: 60 * time.Second})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/mev")
	_, _ = readAll(resp)

	dl, ok := reader.deadline()
	if !ok {
		t.Fatal("ListMEVEvents received a context with no deadline at all")
	}
	if until := time.Until(dl); until > 9*time.Second {
		t.Fatalf("ListMEVEvents ctx deadline is %s out — no per-handler ceiling distinct from "+
			"the 60s blanket RequestTimeout; a cold mev_events scan will hang instead of "+
			"getting a fast retryable 503", until)
	}
}

// TestMEVEvents_DeadlineExceededIs503 pins the other half of NS24: once
// a per-handler ceiling fires, the handler must map it to a specific,
// retryable 503 — not the generic 500 an unhandled deadline produces.
func TestMEVEvents_DeadlineExceededIs503(t *testing.T) {
	reader := &stubMEVReader{err: context.DeadlineExceeded}
	srv := v1.New(v1.Options{MEV: reader})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/mev")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "mev-timeout") {
		t.Errorf("expected the `mev-timeout` problem type in the body, got: %s", body)
	}
}
