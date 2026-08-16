package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rosterCacheStubReader counts ListProtocolContracts calls per source and
// can be made to fail for a chosen source — the two behaviours the W1.3
// roster-cache tests need (served-from-cache, and fail-omits-not-zeros).
type rosterCacheStubReader struct {
	mu        sync.Mutex
	bySource  map[string][]timescale.ProtocolContract
	proj      map[string][]string
	errFor    map[string]bool // sources whose registry read returns an error
	listCalls map[string]int  // per-source ListProtocolContracts call count
}

func (r *rosterCacheStubReader) ListProtocolContracts(_ context.Context, source string) ([]timescale.ProtocolContract, error) {
	r.mu.Lock()
	if r.listCalls == nil {
		r.listCalls = map[string]int{}
	}
	r.listCalls[source]++
	fail := r.errFor[source]
	rows := r.bySource[source]
	r.mu.Unlock()
	if fail {
		return nil, errors.New("served-tier roster read failed")
	}
	return rows, nil
}

func (r *rosterCacheStubReader) ListSourceContractsFromProjection(_ context.Context, source string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proj[source], nil
}

func (r *rosterCacheStubReader) ProtocolContractIndex(_ context.Context) (map[string]string, error) {
	return nil, nil
}

func (r *rosterCacheStubReader) callCount(source string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls[source]
}

// TestProtocolsList_RosterServedFromCache proves the W1.3 perf fix: a
// second /v1/protocols request does NOT re-scan the served tier for a
// source's contract roster — the count is served from the per-server SWR
// cache. Pre-fix (handleProtocolsList called protocolRoster inline per
// request) the reader was hit once PER request; the assertion that blend's
// registry read ran exactly once across two requests fails against that
// code and passes against the cache.
func TestProtocolsList_RosterServedFromCache(t *testing.T) {
	reader := &rosterCacheStubReader{
		bySource: map[string][]timescale.ProtocolContract{
			"blend": {
				{Source: "blend", ContractID: "CPOOL1", FactoryID: "CFACTORY1", FirstLedger: 51_500_000},
				{Source: "blend", ContractID: "CPOOL2", FactoryID: "CFACTORY2", FirstLedger: 60_000_123},
			},
		},
	}
	ts := httpTestServer(t, v1.New(v1.Options{ProtocolContracts: reader}))

	for i := 0; i < 2; i++ {
		resp := mustGet(t, ts.URL+"/v1/protocols")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		// Confirm the count is actually served (2 pools folded with blend's
		// module extras) so the test is not passing on an omitted source.
		var env struct {
			Data struct {
				Protocols []struct {
					Name          string `json:"name"`
					ContractCount int    `json:"contract_count"`
				} `json:"protocols"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("request %d: decode: %v", i+1, err)
		}
		blendCount, found := 0, false
		for _, p := range env.Data.Protocols {
			if p.Name == "blend" {
				blendCount, found = p.ContractCount, true
			}
		}
		if !found || blendCount < 2 {
			t.Fatalf("request %d: blend contract_count = %d (found=%v), want the real roster count", i+1, blendCount, found)
		}
	}

	if got := reader.callCount("blend"); got != 1 {
		t.Fatalf("blend registry read ran %d times across two requests, want 1 (roster must be served from cache, not re-scanned)", got)
	}
}

// TestProtocolsList_RosterFailureOmitsNotZeros proves the W1.3 honesty fix:
// when a source's roster read FAILS and no last-good count is cached, the
// source is OMITTED from the directory and named in coverage_note — never
// serialised with a fabricated contract_count: 0. Pre-fix the failing read
// was swallowed to an empty roster, so blend appeared with contract_count 0
// and no coverage_note; both assertions fail against that code.
func TestProtocolsList_RosterFailureOmitsNotZeros(t *testing.T) {
	reader := &rosterCacheStubReader{
		bySource: map[string][]timescale.ProtocolContract{
			"blend": {
				{Source: "blend", ContractID: "CPOOL1", FactoryID: "CFACTORY1", FirstLedger: 51_500_000},
			},
		},
		errFor: map[string]bool{"blend": true},
	}
	ts := httpTestServer(t, v1.New(v1.Options{ProtocolContracts: reader}))

	resp := mustGet(t, ts.URL+"/v1/protocols")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Decode the raw JSON (not v1.ProtocolsView) so the test compiles against
	// the pre-fix wire shape too — the redness proof must not depend on the
	// new coverage_note Go field existing.
	var env struct {
		Data struct {
			Protocols []struct {
				Name          string `json:"name"`
				ContractCount int    `json:"contract_count"`
			} `json:"protocols"`
			CoverageNote string `json:"coverage_note"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, p := range env.Data.Protocols {
		if p.Name == "blend" {
			t.Fatalf("blend has a failing roster read but was serialised with contract_count=%d — a failed read must be omitted, not published as a real zero", p.ContractCount)
		}
	}
	if !strings.Contains(env.Data.CoverageNote, "blend") {
		t.Fatalf("coverage_note = %q, want it to name the omitted degraded source blend", env.Data.CoverageNote)
	}
}
