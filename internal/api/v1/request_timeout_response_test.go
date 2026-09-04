package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// blockingLendingReader stalls until the caller's context is done, then
// returns that context's error — the shape a cold contract_data scan
// takes when the request deadline beats it.
type blockingLendingReader struct{}

func (blockingLendingReader) ListBlendPools(ctx context.Context) ([]timescale.BlendPoolSummary, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingLendingReader) BlendPoolAssets(ctx context.Context, _ string) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingLendingReader) BlendReserveConfigs(ctx context.Context, _ string) (map[string]blend.ReserveConfig, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRequestTimeout_GlobalDeadlineWritesProblem pins the wire contract
// when the RequestTimeout middleware's own deadline — not a tighter
// per-handler one — is what fires.
//
// Pre-fix this produced a BODYLESS HTTP 200: clientAborted keyed on
// `r.Context().Err() != nil`, which the middleware's deadline satisfies
// exactly as a client disconnect does, so the handler returned silently
// and net/http emitted an implicit 200 with content-length 0. A
// dashboard reads resp.ok as true, parses an empty body, and renders
// "0 reserves / $0 TVL" for a pool holding real supply — a wrong answer
// served with full confidence, which is worse than an error.
//
// The middleware deadline is a SERVER-side budget: the client is still
// on the wire and is owed an RFC 9457 problem document it can retry on.
func TestRequestTimeout_GlobalDeadlineWritesProblem(t *testing.T) {
	pool := mkCStrkey(t, 7)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{},
		Lending:  blockingLendingReader{},
		// Below every per-handler budget, so the middleware's deadline
		// is the one the reader observes.
		RequestTimeout: 150 * time.Millisecond,
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	body, _ := readAll(resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 with body %q — a request-deadline expiry must never look like success", body)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a request deadline is retryable capacity, not an internal fault)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (a transient timeout must not be cached and replayed)", cc)
	}
	if !strings.Contains(body, "lending-timeout") {
		t.Errorf("expected `lending-timeout` problem type in body, got: %s", body)
	}
}
