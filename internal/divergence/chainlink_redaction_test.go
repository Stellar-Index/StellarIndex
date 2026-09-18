package divergence

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// chainlinkSecretPath is the key-bearing part of a keyed RPC endpoint.
// Keyed providers (Alchemy, Infura, QuickNode) put the API key in the
// URL PATH, which is why config.go documents the whole rpc_url as a
// secret. Deliberately an obviously-fake, low-entropy string.
const chainlinkSecretPath = "/v2/not-a-real-key-ns12" // gitleaks:allow

// hangUpRPC returns a server that accepts the connection and then
// closes it without answering, so the real http.Client produces a real
// *url.Error — the exact shape a timeout / TLS failure / dropped
// connection against the operator's RPC endpoint produces in
// production. Hijacking (rather than pointing at a closed port) keeps
// the test hermetic: no reliance on a port staying unbound.
func hangUpRPC(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestChainlink_TransportError_RedactsKeyedEndpoint — NS12.
//
// r.rpcURL is a secret (same CHAINLINK_RPC_URL as the ingest poller;
// keyed providers embed the API key in the path). A transport failure
// yields a *url.Error whose Error() quotes the full request URL, so
// wrapping it raw put the operator's API key into the error string that
// Compare copies verbatim into Result.Failures. The error must name the
// endpoint host — that is the operator's whole diagnostic — with the
// path redacted.
func TestChainlink_TransportError_RedactsKeyedEndpoint(t *testing.T) {
	srv := hangUpRPC(t)
	ref := NewChainlinkReference(ChainlinkOptions{
		HTTPClient: srv.Client(),
		RPCURL:     srv.URL + chainlinkSecretPath,
	})

	_, err := ref.LookupPrice(context.Background(), mustPair(t, "crypto:BTC", "fiat:USD"), time.Now())
	if err == nil {
		t.Fatal("LookupPrice against a hung-up RPC: want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, chainlinkSecretPath) {
		t.Errorf("transport error leaks the keyed endpoint path: %s", msg)
	}
	if want := srv.URL + "/<redacted>"; !strings.Contains(msg, want) {
		t.Errorf("transport error = %q, want it to name the redacted endpoint %q", msg, want)
	}
	if !strings.Contains(msg, "chainlink: rpc transport: ") {
		t.Errorf("transport error = %q, want the rpc-transport prefix preserved", msg)
	}
}

// TestChainlink_BadRequestURL_RedactsKeyedEndpoint — NS12, second URL
// -bearing path in the same function: http.NewRequestWithContext runs
// url.Parse, and its failure is itself a *url.Error carrying the raw
// (secret) URL.
func TestChainlink_BadRequestURL_RedactsKeyedEndpoint(t *testing.T) {
	// A control character makes url.Parse fail without changing what
	// the operator configured in any other way.
	const badURL = "https://eth-mainnet.example.test" + chainlinkSecretPath + "\n"
	ref := NewChainlinkReference(ChainlinkOptions{RPCURL: badURL})

	_, err := ref.LookupPrice(context.Background(), mustPair(t, "crypto:BTC", "fiat:USD"), time.Now())
	if err == nil {
		t.Fatal("LookupPrice with an unparseable rpc_url: want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, chainlinkSecretPath) {
		t.Errorf("request-build error leaks the keyed endpoint path: %s", msg)
	}
	if !strings.Contains(msg, "chainlink: new request: ") {
		t.Errorf("request-build error = %q, want the new-request prefix preserved", msg)
	}
}

// TestChainlink_TransportError_SecretNeverReachesDivergenceCache —
// NS12 end to end, through the production entry point.
//
// This is the artifact the finding is about: RefreshPair's CachedResult
// is JSON-marshalled into the per-pair divergence key in Redis, which
// ha-plan.md documents as a single internal-bind, no-AUTH instance.
// Every refresh cycle re-writes it while the endpoint keeps erroring,
// so an ordinary timeout parked the operator's API key at rest with no
// attacker action at all. Assert on the raw cached bytes, not on the
// error, because the bytes are what an unauthenticated reader gets.
func TestChainlink_TransportError_SecretNeverReachesDivergenceCache(t *testing.T) {
	srv := hangUpRPC(t)
	ref := NewChainlinkReference(ChainlinkOptions{
		HTTPClient: srv.Client(),
		RPCURL:     srv.URL + chainlinkSecretPath,
	})

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	svc, err := NewService(ServiceOptions{
		References: []Reference{ref},
		Cache:      rdb,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	pair := mustPair(t, "crypto:BTC", "fiat:USD")
	ctx := context.Background()
	// Every reference failed, so RefreshPair reports the outage
	// (CS-088) — but it still writes the cache entry, which is the
	// point of this test.
	if err := svc.RefreshPair(ctx, pair, 65_000, time.Now()); err != nil && !errors.Is(err, ErrNoReferenceResponded) {
		t.Fatalf("RefreshPair: %v", err)
	}

	body, err := rdb.Get(ctx, cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("redis get: %v", err)
	}
	if strings.Contains(string(body), chainlinkSecretPath) {
		t.Fatalf("divergence cache entry carries the keyed rpc endpoint at rest: %s", body)
	}
	// Non-vacuous: the failure label must still be there (and name the
	// redacted endpoint), so this is not passing because the cache is
	// empty or the failure was swallowed. Decoded, because the encoder
	// escapes the angle brackets of "<redacted>" on the wire.
	var cached CachedResult
	if err := json.Unmarshal(body, &cached); err != nil {
		t.Fatalf("unmarshal cached result: %v", err)
	}
	label := cached.Failures[ChainlinkSourceName]
	if !strings.HasPrefix(label, "chainlink: rpc transport: ") {
		t.Fatalf("cached chainlink failure label = %q, want the rpc-transport prefix", label)
	}
	if want := srv.URL + "/<redacted>"; !strings.Contains(label, want) {
		t.Fatalf("cached chainlink failure label = %q, want the redacted endpoint %q", label, want)
	}
}
