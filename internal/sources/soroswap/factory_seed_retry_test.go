package soroswap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
)

// RLT-416 residual. compute-completeness and verify-reconciliation fail CLOSED
// on a seed error, and on r1 the seed sweeps a PUBLIC third-party RPC with
// 1+3N sequential calls. With no retry, one transient failure anywhere in the
// sweep aborted the nightly pass for every source. This file is the black-box
// half of the regression: it uses only the exported seed and a real
// stellarrpc.Client, so it COMPILES against the pre-fix code and fails there on
// behaviour. The schedule/budget/classification half is in
// factory_seed_retry_internal_test.go.

// seedFault is one scripted failure the fake RPC serves INSTEAD of an answer.
type seedFault struct {
	status  int    // non-zero: reply with this HTTP status (and, alone, a non-JSON body)
	body    string // body for status; a generic HTML stub when empty, unless exact
	exact   bool   // write status and body VERBATIM — an empty body stays empty, JSON stays JSON
	simErr  string // non-empty: a SUCCESSFUL round-trip whose result carries a contract error
	result  string // non-empty: a 200 envelope whose "result" is this raw JSON (wrong shape)
	rpcCode int    // non-zero: reply with a JSON-RPC error envelope, under status (200 when zero)
	rpcMsg  string // message for rpcCode
	drop    bool   // hijack the connection and close it mid-request
}

// flakySorobanRPC answers the sweep's logical calls in order from `answers`
// (base64 SCVals). Before answering logical call i it first serves every fault
// in faults[i], one per HTTP request; a fault marked forever in `stuck` is
// served on every request and the call never succeeds. hits counts HTTP
// requests, per logical call.
type flakySorobanRPC struct {
	mu      sync.Mutex
	answers []string
	faults  map[int][]seedFault
	stuck   map[int]seedFault
	logical int
	hits    map[int]int
}

func newFlakySorobanRPC(t *testing.T, f *flakySorobanRPC) *httptest.Server {
	t.Helper()
	f.hits = map[int]int{}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

func (f *flakySorobanRPC) next() (fault *seedFault, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.logical
	f.hits[i]++
	if s, ok := f.stuck[i]; ok {
		return &s, ""
	}
	if pending := f.faults[i]; len(pending) > 0 {
		f.faults[i] = pending[1:]
		return &pending[0], ""
	}
	f.logical++
	if i < len(f.answers) {
		return nil, f.answers[i]
	}
	return nil, ""
}

func (f *flakySorobanRPC) hitsFor(i int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[i]
}

func (f *flakySorobanRPC) totalHits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, h := range f.hits {
		n += h
	}
	return n
}

func (f *flakySorobanRPC) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID int `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	fault, answer := f.next()
	switch {
	case fault == nil:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"latestLedger": 100, "results": []map[string]any{{"xdr": answer}}},
		})
	case fault.drop:
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	case fault.exact:
		// http.Error always writes a non-empty text body, which is the one
		// shape a real rate limiter or proxy is least likely to send. This
		// arm serves what they do send: nothing at all, or JSON.
		w.WriteHeader(fault.status)
		_, _ = w.Write([]byte(fault.body))
	case fault.result != "":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(fault.result),
		})
	case fault.rpcCode != 0:
		if fault.status != 0 {
			w.WriteHeader(fault.status)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": fault.rpcCode, "message": fault.rpcMsg},
		})
	case fault.simErr != "":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"latestLedger": 100, "error": fault.simErr},
		})
	default:
		body := fault.body
		if body == "" {
			body = "<html>upstream says no</html>"
		}
		http.Error(w, body, fault.status)
	}
}

// onePairAnswers is a clean one-pair sweep: length, all_pairs(0), token_0,
// token_1 — logical calls 0..3.
func onePairAnswers(t *testing.T, pair, tok0, tok1 string) []string {
	t.Helper()
	return []string{
		b64ScVal(t, u32SV(1)),
		b64ScVal(t, contractAddrSV(t, pair)),
		b64ScVal(t, contractAddrSV(t, tok0)),
		b64ScVal(t, contractAddrSV(t, tok1)),
	}
}

// A sweep that hits one transient failure of EACH retryable class — an HTTP
// 503, an HTTP 429, and a connection dropped mid-request — on three different
// calls must still seed the pair. Real backoff (1s per first retry), so ~4s.
func TestSeedFromFactoryRPC_TransientFailuresAreRetriedPerCall(t *testing.T) {
	factory := makeContractStrkey(t, 0x01)
	pair, tok0, tok1 := makeContractStrkey(t, 0x02), makeContractStrkey(t, 0x03), makeContractStrkey(t, 0x04)
	rpc := &flakySorobanRPC{
		answers: onePairAnswers(t, pair, tok0, tok1),
		faults: map[int][]seedFault{
			0: {{status: http.StatusServiceUnavailable}},
			2: {{status: http.StatusTooManyRequests}},
			3: {{drop: true}},
		},
	}
	srv := newFlakySorobanRPC(t, rpc)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dec := NewDecoder()
	n, err := dec.SeedFromFactoryRPC(ctx, stellarrpc.New(srv.URL), factory)
	if err != nil {
		t.Fatalf("seed failed on a transient RPC error: %v — one 503/429/reset in a ~640-call sweep must not "+
			"fail the seed, because the recon callers fail the whole nightly pass closed on it (RLT-416)", err)
	}
	if n != 1 {
		t.Fatalf("seeded %d pair(s); want 1", n)
	}
	got, ok := dec.pairTokensFor(pair)
	if !ok {
		t.Fatalf("pair %s not in the registry after a recovered sweep", pair)
	}
	if got.Token0.ContractID != tok0 || got.Token1.ContractID != tok1 {
		t.Errorf("registry holds (%s, %s); want (%s, %s) — a retried call must not shift which answer "+
			"lands in which slot", got.Token0.ContractID, got.Token1.ContractID, tok0, tok1)
	}
	// 4 logical calls + exactly one re-issue for each of the three faults.
	// The retry is per CALL: the sweep is never restarted (that would be 4+
	// extra hits and a second all_pairs_length).
	if total := rpc.totalHits(); total != 7 {
		t.Errorf("RPC saw %d request(s); want 7 (4 calls + 3 single retries)", total)
	}
	for call, want := range map[int]int{0: 2, 1: 1, 2: 2, 3: 2} {
		if h := rpc.hitsFor(call); h != want {
			t.Errorf("logical call %d was issued %d time(s); want %d", call, h, want)
		}
	}
}
