package chops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// The soroswap pair seed must fail closed: a CONFIGURED seed that fails (no endpoint, RPC error, mid-sweep error)
// returns an error. A DISABLED seed (empty factory_contract, the default) is not a failure, so hosts without a seed
// still get verdicts.

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

// One transient response on the first call of a clean one-pair sweep, with real backoff (~2s per shape): a proxy's HTML
// 503 and a rate limit as HTTP 429 over a JSON-RPC error envelope. The retry must classify status from the response, not
// the client's error text, which is the bare envelope error.
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

// Fail-closed seeding makes the nightly only as reliable as ~640 sequential public-RPC calls. The retry is pinned in
// internal/sources/soroswap; this proves seedSoroswapForRecon reaches it with the client and timeouts it builds, and
// that the helper's notices name no command.

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

func assertNoCommandPrefix(t *testing.T, out string) {
	t.Helper()
	for _, cmd := range []string{"verify-reconciliation", "compute-completeness"} {
		if strings.Contains(out, cmd) {
			t.Errorf("seed notice names the command %q, but the helper is shared by both: %q", cmd, out)
		}
	}
}

// The recognition census must seed the soroswap decoder with the pair registry (the same set attribution folds into
// ownerOf); unseeded, pairTokens is empty and Matches() rejects every real pair event as a false gap. Both directions
// run on the same event: unseeded rejects, seeded recognizes.
func TestRecognitionCensus_SoroswapPairSeedRequired(t *testing.T) {
	// A real registered soroswap pair contract (from the r1 pair registry).
	const pairID = "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A"
	// A pair "sync" flow event — topic[0]=String("SoroswapPair"), topic[1]=Symbol("sync").
	pairEvent := events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}

	// Control: an UNSEEDED census dispatcher must NOT recognize the pair event.
	unseeded, err := pipeline.BuildDispatcher([]string{soroswap.SourceName}, config.OracleConfig{}, nil)
	if err != nil {
		t.Fatalf("build unseeded dispatcher: %v", err)
	}
	if _, ok := unseeded.Recognize(pairEvent); ok {
		t.Fatal("unseeded census recognized a soroswap pair event — the bug is not reproduced; " +
			"this test would pass vacuously and prove nothing")
	}

	// The fix: a census dispatcher SEEDED with the pair registry recognizes it.
	seed := map[string]soroswap.PairTokens{pairID: {}} // Matches() needs only key presence
	seeded, err := pipeline.BuildDispatcher(
		[]string{soroswap.SourceName}, config.OracleConfig{}, nil,
		soroswap.WithSeededPairTokensDecoder(seed),
	)
	if err != nil {
		t.Fatalf("build seeded dispatcher: %v", err)
	}
	if _, ok := seeded.Recognize(pairEvent); !ok {
		t.Fatal("seeded census did NOT recognize a real soroswap pair event — the fix is ineffective")
	}
}
