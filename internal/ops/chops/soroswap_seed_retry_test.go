package chops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// RLT-416 residual, through the production entry point both commands call.
// Failing closed on a seed error (soroswap_seed_failclosed_test.go) made the
// nightly pass only as reliable as ~640 sequential calls to a public RPC. The
// retry itself is pinned in internal/sources/soroswap; this proves it is
// REACHED from seedSoroswapForRecon — with the client and timeouts that
// function builds — and that the helper's notices name no command.

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })
	fn()
	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// One transient response on the very first call of an otherwise clean one-pair
// sweep. Real backoff, so each shape takes ~2s (1s retry wait + three 300ms
// throttles). The shapes are the two ends of what a hosted endpoint sends: a
// proxy's HTML 503, and a rate limit as providers usually word it — HTTP 429
// over a JSON-RPC error envelope with a server-range code. The second one
// still failed the seed closed after a single request when the retry read the
// status out of the client's error text, because the client returned the bare
// envelope error and the status never reached the classification.
func TestSeedSoroswapForRecon_TransientRPCFailureDoesNotFailTheSeed(t *testing.T) {
	shapes := map[string]func(w http.ResponseWriter, id int){
		"HTTP 503, HTML body": func(w http.ResponseWriter, _ int) {
			http.Error(w, "<html>503 upstream unavailable</html>", http.StatusServiceUnavailable)
		},
		"HTTP 429, envelope code -32005": func(w http.ResponseWriter, id int) {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": id,
				"error": map[string]any{"code": -32005, "message": "rate limit exceeded"},
			})
		},
	}
	for name, transient := range shapes {
		t.Run(name, func(t *testing.T) {
			assertSeedSurvivesOneTransient(t, transient)
		})
	}
}

func assertSeedSurvivesOneTransient(t *testing.T, transient func(w http.ResponseWriter, id int)) {
	t.Helper()
	factory, pair := seedFixtureStrkey(t, 0x01), seedFixtureStrkey(t, 0x02)
	answers := []string{
		seedFixtureU32(t, 1),
		seedFixtureAddr(t, pair),
		seedFixtureAddr(t, seedFixtureStrkey(t, 0x03)),
		seedFixtureAddr(t, seedFixtureStrkey(t, 0x04)),
	}
	var hits, answered atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if hits.Add(1) == 1 {
			transient(w, req.ID)
			return
		}
		i := int(answered.Add(1)) - 1
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"latestLedger": 100, "results": []map[string]any{{"xdr": answers[i]}}},
		})
	}))
	t.Cleanup(srv.Close)

	dec := soroswap.NewDecoder()
	var err error
	out := captureStderr(t, func() {
		err = seedSoroswapForRecon(context.Background(), seedFixtureConfig(factory, srv.URL), dec)
	})
	if err != nil {
		t.Fatalf("seed returned %q after ONE transient response (%d request(s) made); want nil — both commands "+
			"abort every source's verdict on this error, so a single blip must be retried, not returned", err, hits.Load())
	}
	if n := hits.Load(); n != 5 {
		t.Errorf("RPC saw %d request(s); want 5 (4 calls + 1 retry of the first)", n)
	}
	if !dec.Matches(pairSyncEvent(pair)) {
		t.Error("decoder does not match the swept pair after a recovered seed")
	}
	if !strings.Contains(out, "soroswap pair seed: seeded 1 pairs") {
		t.Errorf("stderr %q is missing the success notice", out)
	}
	assertNoCommandPrefix(t, out)
}

// The helper serves compute-completeness as well as verify-reconciliation; a
// notice that names one of them is wrong for the other.
func TestSeedSoroswapForRecon_NoticesNameNoCommand(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = seedSoroswapForRecon(context.Background(), config.Config{}, soroswap.NewDecoder())
	})
	if err != nil {
		t.Fatalf("disabled seed returned %q; want nil", err)
	}
	if !strings.Contains(out, "soroswap pair seed: disabled") {
		t.Errorf("stderr %q is missing the disabled-seed notice", out)
	}
	assertNoCommandPrefix(t, out)
}

func assertNoCommandPrefix(t *testing.T, out string) {
	t.Helper()
	for _, cmd := range []string{"verify-reconciliation", "compute-completeness"} {
		if strings.Contains(out, cmd) {
			t.Errorf("seed notice names the command %q, but the helper is shared by both: %q", cmd, out)
		}
	}
}
