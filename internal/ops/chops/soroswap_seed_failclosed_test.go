package chops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// The soroswap pair seed is a pre-loop input of compute-completeness (and
// verify-reconciliation) that must not log and continue on failure while every
// sibling precondition returns. These tests pin BOTH
// halves of the corrected contract, through the production function and a real
// stellar-rpc client against a scripted JSON-RPC server:
//
//   - a seed that is CONFIGURED and fails (no endpoint, RPC error, error
//     part-way through the sweep) returns an error, and both commands return
//     that error instead of printing it;
//   - a seed that is DISABLED (factory_contract empty — the documented disable,
//     the config default, and the shape both test nets run) is not a failure,
//     so failing closed on the first half does not abort every source's verdict
//     on hosts that never had a seed.

// seedFixtureStrkey returns a deterministic C-strkey.
func seedFixtureStrkey(t *testing.T, b byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = b
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func seedFixtureU32(t *testing.T, v uint32) string {
	t.Helper()
	u := xdr.Uint32(v)
	return seedFixtureB64(t, xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u})
}

func seedFixtureAddr(t *testing.T, strk string) string {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", strk, err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return seedFixtureB64(t, xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr})
}

func seedFixtureB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// scriptedSorobanRPC answers the i-th simulateTransaction with results[i]; any
// call at or past len(results) is rejected with a simulate error. It counts
// calls so a test can prove the RPC was (or was not) reached.
func scriptedSorobanRPC(t *testing.T, results []string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		i := int(calls.Add(1)) - 1
		sim := map[string]any{"latestLedger": 100}
		if i < len(results) {
			sim["results"] = []map[string]any{{"xdr": results[i]}}
		} else {
			sim["error"] = "scripted rpc outage"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": sim})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func seedFixtureConfig(factory, endpoint string) config.Config {
	var cfg config.Config
	cfg.Oracle.Soroswap.FactoryContract = factory
	cfg.Oracle.Soroswap.SeedRPCEndpoint = endpoint
	return cfg
}

func pairSyncEvent(pairID string) events.Event {
	return events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}
}

// (c) factory empty — the documented disable and the zero-value default. Must
// NOT be an error: the callers now return every error, so an error here aborts
// the whole nightly pass on a seed-less host before any source is evaluated.
func TestSeedSoroswapForRecon_DisabledSeedIsNotAFailure(t *testing.T) {
	if err := seedSoroswapForRecon(context.Background(), config.Config{}, soroswap.NewDecoder()); err != nil {
		t.Fatalf("seed with oracle.soroswap.factory_contract empty returned %q; want nil — an empty factory "+
			"DISABLES the seed (config.SoroswapConfig), it does not fail it, and both commands abort on a seed error", err)
	}

	// Disabled means disabled even when an RPC endpoint is available: the sweep
	// must not run against an empty factory id.
	srv, calls := scriptedSorobanRPC(t, nil)
	cfg := seedFixtureConfig("", srv.URL)
	cfg.Stellar.RPCEndpoints = []string{srv.URL}
	if err := seedSoroswapForRecon(context.Background(), cfg, soroswap.NewDecoder()); err != nil {
		t.Fatalf("disabled seed with an endpoint configured returned %q; want nil", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("disabled seed made %d RPC call(s); want 0", n)
	}
}

// (b) factory set, no endpoint anywhere — a misconfiguration, not a disable.
func TestSeedSoroswapForRecon_FactoryWithoutEndpointFails(t *testing.T) {
	err := seedSoroswapForRecon(context.Background(), seedFixtureConfig(seedFixtureStrkey(t, 0x01), ""), soroswap.NewDecoder())
	if err == nil {
		t.Fatal("factory set with no RPC endpoint returned nil; want an error — the seed is configured and cannot run")
	}
	if !strings.Contains(err.Error(), "no RPC endpoint") {
		t.Errorf("error %q does not name the missing endpoint", err)
	}
}

// (a) factory set, RPC fails — on the first call, and part-way through the
// sweep (the partial-registry case: SeedFromFactoryRPC returns mid-loop).
func TestSeedSoroswapForRecon_RPCFailureIsReturnedWithItsCause(t *testing.T) {
	factory, pair := seedFixtureStrkey(t, 0x01), seedFixtureStrkey(t, 0x02)
	cases := []struct {
		name    string
		results []string
		wantIn  string
	}{
		{"first call", nil, "all_pairs_length"},
		{"mid sweep", []string{seedFixtureU32(t, 2), seedFixtureAddr(t, pair)}, "token_0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := scriptedSorobanRPC(t, tc.results)
			err := seedSoroswapForRecon(context.Background(), seedFixtureConfig(factory, srv.URL), soroswap.NewDecoder())
			if err == nil {
				t.Fatal("seed returned nil after the RPC rejected a call; want the error")
			}
			if calls.Load() == 0 {
				t.Fatal("RPC never reached — the error did not come from the sweep")
			}
			for _, want := range []string{tc.wantIn, "scripted rpc outage"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry the cause fragment %q", err, want)
				}
			}
		})
	}
}

// (d) clean sweep — nil, and the decoder the caller passed in is the one seeded
// (it is the same pointer the catalogue's soroswap source re-derives with).
func TestSeedSoroswapForRecon_CleanSweepSeedsTheDecoder(t *testing.T) {
	factory, pair := seedFixtureStrkey(t, 0x01), seedFixtureStrkey(t, 0x02)
	srv, calls := scriptedSorobanRPC(t, []string{
		seedFixtureU32(t, 1),
		seedFixtureAddr(t, pair),
		seedFixtureAddr(t, seedFixtureStrkey(t, 0x03)),
		seedFixtureAddr(t, seedFixtureStrkey(t, 0x04)),
	})
	dec := soroswap.NewDecoder()
	if dec.Matches(pairSyncEvent(pair)) {
		t.Fatal("unseeded decoder already matches the pair — the seeded assertion below would be vacuous")
	}
	// Endpoint via the stellar.rpc_endpoints[0] fallback.
	cfg := seedFixtureConfig(factory, "")
	cfg.Stellar.RPCEndpoints = []string{srv.URL}
	if err := seedSoroswapForRecon(context.Background(), cfg, dec); err != nil {
		t.Fatalf("clean sweep returned %q; want nil", err)
	}
	if n := calls.Load(); n != 4 {
		t.Errorf("clean one-pair sweep made %d RPC calls; want 4", n)
	}
	if !dec.Matches(pairSyncEvent(pair)) {
		t.Error("decoder does not match the swept pair after a clean seed")
	}
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
